package tun

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

func TestDNSCacheAgesTTLAndExpires(t *testing.T) {
	now := time.Now()
	wire := dnsAnswer(t, queryWire(t), false)
	entry, ok := cacheEntry(wire, now)
	if !ok {
		t.Fatal("positive response not cached")
	}
	var answer dnsmessage.Message
	if err := answer.Unpack(entry.answer(77, now.Add(17*time.Second))); err != nil {
		t.Fatal(err)
	}
	if answer.Header.ID != 77 || answer.Answers[0].Header.TTL != 43 {
		t.Fatalf("ID/TTL not updated: %+v", answer)
	}
	if entry.answer(78, now.Add(time.Minute)) != nil {
		t.Fatal("served expired answer")
	}
	if !bytes.Equal(entry.wire, wire) {
		t.Fatal("shared cache entry modified")
	}
	for _, kind := range []string{"zero TTL", "truncated", "failure", "empty"} {
		var m dnsmessage.Message
		if err := m.Unpack(wire); err != nil {
			t.Fatal(err)
		}
		switch kind {
		case "zero TTL":
			m.Answers[0].Header.TTL = 0
		case "truncated":
			m.Header.Truncated = true
		case "failure":
			m.Header.RCode = dnsmessage.RCodeServerFailure
		case "empty":
			m.Answers = nil
		}
		b, _ := m.Pack()
		if _, ok := cacheEntry(b, now); ok {
			t.Fatalf("cached %s answer", kind)
		}
	}
}

func TestDNSCoalescesLookupsAndCancellationDoesNotPoisonOthers(t *testing.T) {
	query := queryWire(t)
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	p := &dnsProxy{endpoint: "https://example.test/dns-query", client: &http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		select {
		case <-release:
		case <-r.Context().Done():
			return nil, r.Context().Err()
		}
		q, _ := io.ReadAll(r.Body)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(dnsAnswer(t, q, false)))}, nil
	})}}
	ctx, cancel := context.WithCancel(t.Context())
	first := make(chan error, 1)
	go func() { _, err := p.exchange(ctx, query); first <- err }()
	<-started
	cancel()
	if err := <-first; err != context.Canceled {
		t.Fatalf("cancelled caller: %v", err)
	}
	const clients = 20
	var wg sync.WaitGroup
	errs := make(chan error, clients)
	for i := 0; i < clients; i++ {
		wg.Add(1)
		go func(id uint16) {
			defer wg.Done()
			q := append([]byte(nil), query...)
			binary.BigEndian.PutUint16(q, id)
			a, err := p.exchange(t.Context(), q)
			if err != nil {
				errs <- err
				return
			}
			if binary.BigEndian.Uint16(a) != id {
				errs <- fmt.Errorf("answer ID does not match %d", id)
			}
		}(uint16(100 + i))
	}
	close(release)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if n := calls.Load(); n != 1 {
		t.Fatalf("duplicate upstream requests: %d", n)
	}
	if _, err := p.exchange(t.Context(), query); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("warm lookup did not use cache")
	}
	// A different query type must not reuse the A answer.
	var aaaa dnsmessage.Message
	if err := aaaa.Unpack(query); err != nil {
		t.Fatal(err)
	}
	aaaa.Questions[0].Type = dnsmessage.TypeAAAA
	other, _ := aaaa.Pack()
	if _, err := p.exchange(t.Context(), other); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatal("A and AAAA cache keys collided")
	}
	p.lookupWG.Wait()
}

func TestDNSCacheKeepsEDNSFlagsAndBoundsMemory(t *testing.T) {
	query := queryWire(t)
	var m dnsmessage.Message
	if err := m.Unpack(query); err != nil {
		t.Fatal(err)
	}
	m.Header.Response = true
	m.Answers = []dnsmessage.Resource{{Header: dnsmessage.ResourceHeader{Name: m.Questions[0].Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 60}, Body: &dnsmessage.AResource{A: [4]byte{1, 2, 3, 4}}}}
	m.Additionals = []dnsmessage.Resource{{Header: dnsmessage.ResourceHeader{Name: dnsmessage.MustNewName("."), Type: dnsmessage.TypeOPT, Class: 1232, TTL: 0x8000}, Body: &dnsmessage.OPTResource{}}}
	wire, _ := m.Pack()
	now := time.Now()
	e, ok := cacheEntry(wire, now)
	if !ok {
		t.Fatal("OPT mistaken for zero TTL")
	}
	var cached dnsmessage.Message
	if err := cached.Unpack(e.answer(2, now.Add(10*time.Second))); err != nil {
		t.Fatal(err)
	}
	if cached.Additionals[0].Header.TTL != 0x8000 {
		t.Fatal("EDNS DNSSEC flags changed")
	}
	p := &dnsProxy{endpoint: "https://example.test/dns-query", client: &http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		q, _ := io.ReadAll(r.Body)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(dnsAnswer(t, q, false)))}, nil
	})}}
	for i := 0; i < dnsCacheCapacity+3; i++ {
		q := dnsmessage.Message{Header: dnsmessage.Header{ID: 1}, Questions: []dnsmessage.Question{{Name: dnsmessage.MustNewName(fmt.Sprintf("host%d.example.", i)), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}}}
		b, _ := q.Pack()
		if _, err := p.exchange(t.Context(), b); err != nil {
			t.Fatal(err)
		}
	}
	p.lookupWG.Wait()
	if len(p.cache) > dnsCacheCapacity {
		t.Fatalf("unbounded cache: %d", len(p.cache))
	}
}

func TestDNSHealthCheckCannotBeSatisfiedByCache(t *testing.T) {
	query := queryWire(t)
	fail := false
	p := &dnsProxy{endpoint: "https://example.test/dns-query", client: &http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
		if fail {
			return nil, fmt.Errorf("transport unreachable")
		}
		q, _ := io.ReadAll(r.Body)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(dnsAnswer(t, q, false)))}, nil
	})}}
	if _, err := p.exchange(t.Context(), query); err != nil {
		t.Fatal(err)
	}
	p.lookupWG.Wait()
	fail = true
	if _, err := p.exchange(t.Context(), query); err != nil {
		t.Fatal("expected cached answer", err)
	}
	if err := p.check(t.Context()); err == nil {
		t.Fatal("cached answer concealed a broken route")
	}
}
