package socks

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"strconv"
	"sync"
	"testing"
	"time"

	cryptox "github.com/roca/h264-tunnel-lab/internal/crypto"
	"github.com/roca/h264-tunnel-lab/internal/logging"
	"github.com/roca/h264-tunnel-lab/internal/mux"
	"github.com/roca/h264-tunnel-lab/internal/muxproto"
)

type packetPipe struct {
	ch                 chan []byte
	done               chan struct{}
	mu                 sync.Mutex
	dropOpen, dropData bool
}

func (p *packetPipe) WritePayload(b []byte, _ bool) error {
	h, _ := muxproto.DecodeHeader(b)
	p.mu.Lock()
	drop := false
	if h.Type == muxproto.FrameOpen && !p.dropOpen {
		p.dropOpen = true
		drop = true
	}
	if h.Type == muxproto.FrameData && !p.dropData {
		p.dropData = true
		drop = true
	}
	p.mu.Unlock()
	if drop {
		return nil
	}
	select {
	case p.ch <- bytes.Clone(b):
		return nil
	case <-p.done:
		return io.ErrClosedPipe
	}
}
func (p *packetPipe) ReadPayload() ([]byte, bool, error) {
	select {
	case b := <-p.ch:
		return b, false, nil
	case <-p.done:
		return nil, false, io.EOF
	}
}

func proxyPair(t *testing.T, idle time.Duration, options ...func(*Config)) (string, *mux.Session, *mux.Session) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	a := &packetPipe{ch: make(chan []byte, 8192), done: make(chan struct{})}
	b := &packetPipe{ch: make(chan []byte, 8192), done: make(chan struct{})}
	key, _ := cryptox.GenerateKey()
	log := logging.NewTo("socks-test", io.Discard)
	l, e := mux.New(mux.Config{SessionID: 123, Role: mux.RoleListener, Mode: cryptox.ModeAEAD, Master: key, Sink: a, Source: b, Log: log})
	if e != nil {
		t.Fatal(e)
	}
	d, e := mux.New(mux.Config{SessionID: 123, Role: mux.RoleDialer, Mode: cryptox.ModeAEAD, Master: key, Sink: b, Source: a, Log: log})
	if e != nil {
		t.Fatal(e)
	}
	ln, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	cfg := Config{HandshakeTimeout: 2 * time.Second, DialTimeout: time.Second, IdleTimeout: idle, Log: log}
	for _, option := range options {
		option(&cfg)
	}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); ServeGateway(ctx, l, cfg) }()
	go func() { defer wg.Done(); Serve(ctx, ln, d, cfg) }()
	t.Cleanup(func() {
		cancel()
		l.Close()
		d.Close()
		ln.Close()
		close(a.done)
		close(b.done)
		wg.Wait()
	})
	return ln.Addr().String(), l, d
}

func connectSOCKS(addr, target string, domain bool) (*net.TCPConn, byte, error) {
	c, e := net.DialTimeout("tcp", addr, time.Second)
	if e != nil {
		return nil, 0, e
	}
	tcp := c.(*net.TCPConn)
	tcp.SetDeadline(time.Now().Add(15 * time.Second))
	fail := func(e error) (*net.TCPConn, byte, error) { tcp.Close(); return nil, 0, e }
	// Fragment negotiation to exercise ReadFull rather than packet assumptions.
	for _, b := range []byte{5, 1, 0} {
		if _, e = tcp.Write([]byte{b}); e != nil {
			return fail(e)
		}
	}
	var auth [2]byte
	if _, e = io.ReadFull(tcp, auth[:]); e != nil {
		return fail(e)
	}
	if auth != [2]byte{5, 0} {
		return fail(fmt.Errorf("auth %v", auth))
	}
	host, port, _ := net.SplitHostPort(target)
	p, _ := strconv.Atoi(port)
	req := []byte{5, 1, 0}
	if domain {
		req = append(req, 3, byte(len(host)))
		req = append(req, host...)
	} else if ip := net.ParseIP(host).To4(); ip != nil {
		req = append(req, 1)
		req = append(req, ip...)
	} else {
		req = append(req, 4)
		req = append(req, net.ParseIP(host).To16()...)
	}
	req = binary.BigEndian.AppendUint16(req, uint16(p))
	if e = writeAll(tcp, req); e != nil {
		return fail(e)
	}
	code, _, e := readReply(tcp)
	if e != nil {
		return fail(e)
	}
	return tcp, code, nil
}

func TestSOCKSConcurrentHalfCloseAndRemoteDNS(t *testing.T) {
	proxy, l, d := proxyPair(t, 5*time.Second)
	target, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer target.Close()
	const n = 4
	var targetWG sync.WaitGroup
	targetWG.Add(n)
	go func() {
		for i := 0; i < n; i++ {
			c, e := target.Accept()
			if e != nil {
				return
			}
			go func() {
				defer targetWG.Done()
				defer c.Close()
				body, e := io.ReadAll(c)
				if e != nil {
					return
				}
				sum := sha256.Sum256(body)
				writeAll(c, append(sum[:], body...))
			}()
		}
	}()
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			address := target.Addr().String()
			if i%2 == 1 {
				_, p, _ := net.SplitHostPort(address)
				address = net.JoinHostPort("localhost", p)
			}
			c, code, e := connectSOCKS(proxy, address, i%2 == 1)
			if e != nil {
				errs <- e
				return
			}
			defer c.Close()
			if code != 0 {
				errs <- fmt.Errorf("CONNECT %d", code)
				return
			}
			body := bytes.Repeat([]byte{byte(i + 1)}, 300*1024)
			if e = writeAll(c, body); e != nil {
				errs <- e
				return
			}
			if e = c.CloseWrite(); e != nil {
				errs <- e
				return
			}
			got, e := io.ReadAll(c)
			if e != nil {
				errs <- e
				return
			}
			sum := sha256.Sum256(body)
			if !bytes.Equal(got, append(sum[:], body...)) {
				errs <- fmt.Errorf("stream %d body mismatch", i)
				return
			}
			errs <- nil
		}(i)
	}
	for i := 0; i < n; i++ {
		if e := <-errs; e != nil {
			t.Fatal(e)
		}
	}
	targetWG.Wait()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if l.Stats().ActiveStreams == 0 && d.Stats().ActiveStreams == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("streams not reclaimed: l=%d d=%d", l.Stats().ActiveStreams, d.Stats().ActiveStreams)
}

func TestSOCKSRefusalAndIdleCancelDoNotKillSession(t *testing.T) {
	proxy, _, _ := proxyPair(t, 150*time.Millisecond)
	ln, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	refused := ln.Addr().String()
	ln.Close()
	c, code, e := connectSOCKS(proxy, refused, false)
	if e != nil {
		t.Fatal(e)
	}
	c.Close()
	if code != connectionRefused {
		t.Fatalf("want refusal, got %d", code)
	}
	idle, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer idle.Close()
	targetDone := make(chan struct{})
	go func() {
		defer close(targetDone)
		c, e := idle.Accept()
		if e == nil {
			defer c.Close()
			io.Copy(io.Discard, c)
		}
	}()
	c, code, e = connectSOCKS(proxy, idle.Addr().String(), false)
	if e != nil || code != 0 {
		t.Fatalf("connect idle: %d %v", code, e)
	}
	c.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, e = c.Read(make([]byte, 1))
	c.Close()
	if e != io.EOF {
		t.Fatalf("idle connection should close: %v", e)
	}
	<-targetDone
	// A refused connection still returns a real CONNECT reply after the reset.
	c, code, e = connectSOCKS(proxy, refused, false)
	if e != nil {
		t.Fatal(e)
	}
	c.Close()
	if code != connectionRefused {
		t.Fatalf("session broken after idle: %d", code)
	}
}

func TestProtocolRejectionAndIPv6(t *testing.T) {
	var rw bytes.Buffer
	rw.Write([]byte{5, 1, 2})
	if e := negotiate(&rw); e == nil {
		t.Fatal("unsupported authentication accepted")
	}
	if got := rw.Bytes(); !bytes.Equal(got, []byte{5, 255}) {
		t.Fatalf("auth reply: %v", got)
	}
	for _, tc := range []struct {
		wire []byte
		code byte
	}{
		{[]byte{5, 2, 0}, commandUnsupported},
		{[]byte{5, 1, 0, 9}, addressUnsupported},
		{[]byte{5, 1, 0, 3, 0}, addressUnsupported},
	} {
		_, e := readRequest(bytes.NewReader(tc.wire))
		if e == nil || errorCode(e) != tc.code {
			t.Fatalf("request %v: %v", tc.wire, e)
		}
	}
	wire := append([]byte{5, 1, 0, 4}, net.ParseIP("::1").To16()...)
	wire = binary.BigEndian.AppendUint16(wire, 443)
	req, e := readRequest(bytes.NewReader(wire))
	if e != nil || req.address != "[::1]:443" {
		t.Fatalf("IPv6: %+v %v", req, e)
	}
}

func TestSOCKSIPv6Target(t *testing.T) {
	ln, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 unavailable: %v", err)
	}
	defer ln.Close()
	proxy, _, _ := proxyPair(t, time.Second)
	done := make(chan struct{})
	go func() {
		defer close(done)
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		io.Copy(c, c)
	}()
	c, code, err := connectSOCKS(proxy, ln.Addr().String(), false)
	if err != nil || code != 0 {
		t.Fatalf("IPv6 CONNECT: %d %v", code, err)
	}
	defer c.Close()
	c.Write([]byte("ipv6"))
	c.CloseWrite()
	b, err := io.ReadAll(c)
	if err != nil || string(b) != "ipv6" {
		t.Fatalf("IPv6 echo: %q %v", b, err)
	}
	<-done
}
