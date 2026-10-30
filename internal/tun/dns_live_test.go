package tun

import (
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
)

// Opt-in probe through an already connected client. Does not alter DNS/routes
// or restart the gateway: H264_TUN_LIVE_SOCKS=127.0.0.1:1080 go test -v
// ./internal/tun -run TestLiveDNS -count=1
func TestLiveDNS(t *testing.T) {
	socks := os.Getenv("H264_TUN_LIVE_SOCKS")
	if socks == "" {
		t.Skip("set H264_TUN_LIVE_SOCKS to probe an existing tunnel")
	}
	p, err := startDNS(t.Context(), "127.0.0.1:0", socks, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	p.client.Transport = roundTripper(func(r *http.Request) (*http.Response, error) {
		resp, err := p.transport.RoundTrip(r)
		if err == nil {
			t.Logf("DoH protocol: %s", resp.Proto)
		}
		return resp, err
	})
	if err := p.check(t.Context()); err != nil {
		t.Fatal(err)
	}
	// Test the actual UDP listener and IPv4-only policy, not only direct DoH.
	for _, typ := range []dnsmessage.Type{dnsmessage.TypeA, dnsmessage.TypeAAAA, 64, 65} {
		q := dnsmessage.Message{Header: dnsmessage.Header{ID: uint16(typ), RecursionDesired: true}, Questions: []dnsmessage.Question{{Name: dnsmessage.MustNewName("www.youtube.com."), Type: typ, Class: dnsmessage.ClassINET}}}
		wire, _ := q.Pack()
		c, err := net.Dial("udp4", p.udp.LocalAddr().String())
		if err != nil {
			t.Fatal(err)
		}
		c.SetDeadline(time.Now().Add(16 * time.Second))
		_, err = c.Write(wire)
		if err != nil {
			c.Close()
			t.Fatal(err)
		}
		buf := make([]byte, 4096)
		n, err := c.Read(buf)
		c.Close()
		if err != nil {
			t.Fatal(err)
		}
		var a dnsmessage.Message
		if err := a.Unpack(buf[:n]); err != nil || a.Header.ID != q.Header.ID || a.Header.RCode != dnsmessage.RCodeSuccess {
			t.Fatalf("UDP query type %d: %+v %v", typ, a, err)
		}
		if (len(a.Answers) > 0) != (typ == dnsmessage.TypeA) {
			t.Fatalf("unexpected UDP answers for type %d: %+v", typ, a)
		}
		t.Logf("UDP DNS type %d: answers=%d", typ, len(a.Answers))
	}
	for _, host := range []string{"www.youtube.com.", "i.ytimg.com."} {
		for i := 0; i < 3; i++ {
			q := dnsmessage.Message{Header: dnsmessage.Header{ID: uint16(i + 1), RecursionDesired: true}, Questions: []dnsmessage.Question{{Name: dnsmessage.MustNewName(host), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}}}
			wire, _ := q.Pack()
			start := time.Now()
			a, err := p.exchange(t.Context(), wire)
			if err != nil {
				t.Fatal(err)
			}
			var answer dnsmessage.Message
			if err = answer.Unpack(a); err != nil || answer.Header.RCode != dnsmessage.RCodeSuccess || len(answer.Answers) == 0 {
				t.Fatalf("invalid DNS response: %v %+v", err, answer)
			}
			t.Logf("%s lookup %d: %v", host, i+1, time.Since(start))
		}
	}
}
