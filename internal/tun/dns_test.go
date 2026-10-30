package tun

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

func queryWire(t *testing.T) []byte {
	t.Helper()
	q := dnsmessage.Message{Header: dnsmessage.Header{ID: 42, RecursionDesired: true}, Questions: []dnsmessage.Question{{Name: dnsmessage.MustNewName("example.com."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}}}
	b, err := q.Pack()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func dnsAnswer(t *testing.T, b []byte, large bool) []byte {
	t.Helper()
	var m dnsmessage.Message
	if err := m.Unpack(b); err != nil {
		t.Error(err)
		return nil
	}
	m.Header.Response = true
	m.Answers = []dnsmessage.Resource{{Header: dnsmessage.ResourceHeader{Name: m.Questions[0].Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 60}, Body: &dnsmessage.AResource{A: [4]byte{203, 0, 113, 7}}}}
	if large {
		m.Answers = append(m.Answers, dnsmessage.Resource{Header: dnsmessage.ResourceHeader{Name: m.Questions[0].Name, Type: dnsmessage.TypeTXT, Class: dnsmessage.ClassINET}, Body: &dnsmessage.TXTResource{TXT: []string{string(bytes.Repeat([]byte("x"), 250)), string(bytes.Repeat([]byte("y"), 250)), string(bytes.Repeat([]byte("z"), 250))}}})
	}
	wire, err := m.Pack()
	if err != nil {
		t.Error(err)
	}
	return wire
}

// A real SOCKS handshake forwards to a local TLS server while asserting that
// the production dialer bootstraps DoH by literal IP instead of resolving it.
func mockSOCKS(t *testing.T, target string) string {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, e := ln.Accept()
			if e != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer c.Close()
				c.SetDeadline(time.Now().Add(10 * time.Second))
				var greeting [3]byte
				if _, e := io.ReadFull(c, greeting[:]); e != nil {
					return
				}
				if greeting != [3]byte{5, 1, 0} {
					t.Errorf("SOCKS greeting: %v", greeting)
					return
				}
				c.Write([]byte{5, 0})
				var request [10]byte
				if _, e := io.ReadFull(c, request[:]); e != nil {
					return
				}
				if !bytes.Equal(request[:], []byte{5, 1, 0, 1, 1, 1, 1, 1, 1, 187}) {
					t.Errorf("DoH must use literal 1.1.1.1:443, got %v", request)
					return
				}
				remote, e := net.DialTimeout("tcp", target, time.Second)
				if e != nil {
					t.Error(e)
					return
				}
				defer remote.Close()
				c.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 1})
				copied := make(chan struct{})
				go func() { io.Copy(remote, c); close(copied) }()
				io.Copy(c, remote)
				c.Close()
				remote.Close()
				<-copied
			}()
		}
	}()
	t.Cleanup(func() { ln.Close(); <-done; wg.Wait() })
	return ln.Addr().String()
}

func TestDNSOverSOCKSWithUDPAndTCP(t *testing.T) {
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			t.Error("DoH must negotiate HTTP/2 through SOCKS")
		}
		if r.Method != "POST" || r.URL.Path != "/dns-query" || r.Header.Get("Content-Type") != "application/dns-message" {
			t.Error("unexpected DoH request")
		}
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		w.Header().Set("Content-Type", "application/dns-message")
		w.Write(dnsAnswer(t, b, true))
	}))
	server.EnableHTTP2 = true
	server.StartTLS()
	defer server.Close()
	socks := mockSOCKS(t, server.Listener.Addr().String())
	p, err := startDNS(t.Context(), "127.0.0.1:0", socks, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	trusted := server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	trusted.ServerName = "example.com"
	trusted.MinVersion = tls.VersionTLS12
	p.transport.TLSClientConfig = trusted
	if err = p.check(t.Context()); err != nil {
		t.Fatal(err)
	}
	wire := queryWire(t)
	udp, err := net.Dial("udp4", p.udp.LocalAddr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer udp.Close()
	udp.SetDeadline(time.Now().Add(3 * time.Second))
	if _, err = udp.Write(wire); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4096)
	n, err := udp.Read(buf)
	if err != nil {
		t.Fatal(err)
	}
	var a dnsmessage.Message
	if err = a.Unpack(buf[:n]); err != nil || !a.Header.Truncated || len(a.Answers) != 0 {
		t.Fatalf("UDP answer should request TCP retry: %+v %v", a, err)
	}
	tcp, err := net.Dial("tcp4", p.tcp.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer tcp.Close()
	tcp.SetDeadline(time.Now().Add(3 * time.Second))
	for i := 0; i < 2; i++ { // DNS TCP permits multiple framed queries per connection.
		var size [2]byte
		binary.BigEndian.PutUint16(size[:], uint16(len(wire)))
		if _, err = tcp.Write(append(size[:], wire...)); err != nil {
			t.Fatal(err)
		}
		if _, err = io.ReadFull(tcp, size[:]); err != nil {
			t.Fatal(err)
		}
		answer := make([]byte, int(binary.BigEndian.Uint16(size[:])))
		if _, err = io.ReadFull(tcp, answer); err != nil {
			t.Fatal(err)
		}
		if err = a.Unpack(answer); err != nil || a.Header.Truncated || len(a.Answers) != 2 || a.Header.ID != 42 {
			t.Fatalf("invalid full TCP answer: %+v %v", a, err)
		}
	}
}

type roundTripper func(*http.Request) (*http.Response, error)

func (f roundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestDNSFailureAndInvalidResponse(t *testing.T) {
	wire := queryWire(t)
	p := &dnsProxy{endpoint: "https://cloudflare-dns.com/dns-query", client: &http.Client{Transport: roundTripper(func(*http.Request) (*http.Response, error) { return nil, fmt.Errorf("tunnel unavailable") })}}
	var a dnsmessage.Message
	if err := a.Unpack(p.answer(t.Context(), wire, true)); err != nil || a.Header.RCode != dnsmessage.RCodeServerFailure {
		t.Fatalf("must return SERVFAIL without direct DNS fallback: %v %+v", err, a)
	}
	answer := dnsAnswer(t, wire, false)
	answer[1]++ // Mismatched transaction ID must never be delivered to the client.
	p.client.Transport = roundTripper(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(answer))}, nil
	})
	if _, err := p.exchange(context.Background(), wire); err == nil {
		t.Fatal("accepted unrelated answer")
	}
	if got := p.answer(t.Context(), []byte{0, 1}, true); got != nil {
		t.Fatal("replied to malformed DNS")
	}
}

func TestDNSHealthCheckRetriesOnlyTransientNetworkFailures(t *testing.T) {
	for _, tc := range []struct {
		name       string
		failure    error
		alwaysFail bool
		wantCalls  int
		wantErr    bool
	}{
		{"TLS timeout then success", &net.OpError{Op: "TLS handshake", Net: "tcp", Err: timeoutError{}}, false, 2, false},
		{"EOF then success", io.EOF, false, 2, false},
		{"persistent timeout is bounded", timeoutError{}, true, 2, true},
		{"invalid answer is not retried", fmt.Errorf("invalid DoH answer"), true, 1, true},
		{"cancellation is not retried", context.Canceled, true, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			p := &dnsProxy{endpoint: "https://cloudflare-dns.com/dns-query", transport: &http.Transport{}}
			p.client = &http.Client{Transport: roundTripper(func(r *http.Request) (*http.Response, error) {
				calls++
				if calls == 1 || tc.alwaysFail {
					return nil, tc.failure
				}
				query, err := io.ReadAll(r.Body)
				if err != nil {
					return nil, err
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(dnsAnswer(t, query, false)))}, nil
			})}
			err := p.check(t.Context())
			if (err != nil) != tc.wantErr || calls != tc.wantCalls {
				t.Fatalf("calls=%d error=%v, want calls=%d error=%v", calls, err, tc.wantCalls, tc.wantErr)
			}
		})
	}
}

type timeoutError struct{}

func (timeoutError) Error() string   { return "TLS handshake timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func TestIPv4OnlyDNSPolicy(t *testing.T) {
	for _, typ := range []dnsmessage.Type{dnsmessage.TypeAAAA, 64, 65} {
		for _, udp := range []bool{false, true} {
			t.Run(fmt.Sprintf("type%d/udp%v", typ, udp), func(t *testing.T) {
				var q dnsmessage.Message
				if err := q.Unpack(queryWire(t)); err != nil {
					t.Fatal(err)
				}
				q.Questions[0].Type = typ
				q.Header.CheckingDisabled = true
				wire, err := q.Pack()
				if err != nil {
					t.Fatal(err)
				}
				calls := 0
				p := &dnsProxy{ipv4Only: true, endpoint: "https://cloudflare-dns.com/dns-query", client: &http.Client{Transport: roundTripper(func(*http.Request) (*http.Response, error) {
					calls++
					return nil, fmt.Errorf("upstream called")
				})}}
				var a dnsmessage.Message
				if err := a.Unpack(p.answer(t.Context(), wire, udp)); err != nil {
					t.Fatal(err)
				}
				if calls != 0 || !a.Header.Response || a.Header.RCode != dnsmessage.RCodeSuccess || a.Header.ID != q.Header.ID || len(a.Questions) != 1 || a.Questions[0] != q.Questions[0] || len(a.Answers) != 0 || a.Header.AuthenticData {
					t.Fatalf("expected unauthenticated local NODATA without upstream: calls=%d answer=%+v", calls, a)
				}
				// Enabling IPv6 must forward these types rather than silently filter.
				p.ipv4Only = false
				if _, err := p.exchange(t.Context(), wire); err == nil || calls != 1 {
					t.Fatalf("IPv6 opt-in did not forward: calls=%d err=%v", calls, err)
				}
				// A queries must continue to use the tunnel in IPv4-only mode.
				p.ipv4Only = true
				if _, err := p.exchange(t.Context(), queryWire(t)); err == nil || calls != 2 {
					t.Fatalf("A query did not forward: calls=%d err=%v", calls, err)
				}
			})
		}
	}
}
