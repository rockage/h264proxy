package socks

import (
	"bytes"
	"context"
	"io"
	"net"
	"testing"
	"time"
)

// A nil session is intentional: rejected requests must never open a stream or
// reach the gateway.
func TestRemoteDNSRejectsResolvedRequestsBeforeOpeningStream(t *testing.T) {
	for _, tc := range []struct {
		name string
		wire []byte
		v4   bool
	}{
		{"SOCKS5 IPv4", []byte{5, 1, 0, 1, 127, 0, 0, 1, 0, 80}, false},
		{"SOCKS5 IPv6", append(append([]byte{5, 1, 0, 4}, net.ParseIP("::1").To16()...), 0, 80), false},
		{"SOCKS4 IPv4", socks4ConnectRecord("127.0.0.1:80", false), true},
		{"SOCKS4a numeric domain", socks4ConnectRecord("127.0.0.1:80", true), true},
		{"SOCKS5 numeric domain", append(append([]byte{5, 1, 0, 3, 9}, []byte("127.0.0.1")...), 0, 80), false},
		{"SOCKS5 numeric domain with root dot", append(append([]byte{5, 1, 0, 3, 10}, []byte("127.0.0.1.")...), 0, 80), false},
		{"SOCKS5 IPv6 zone in domain", append(append([]byte{5, 1, 0, 3, 12}, []byte("fe80::1%eth0")...), 0, 80), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server, client := net.Pipe()
			defer client.Close()
			client.SetDeadline(time.Now().Add(2 * time.Second))
			done := make(chan error, 1)
			go func() {
				done <- serveClient(context.Background(), server, nil, Config{RequireDomain: true, HandshakeTimeout: time.Second})
			}()
			if !tc.v4 {
				if _, err := client.Write([]byte{5, 1, 0}); err != nil {
					t.Fatal(err)
				}
				var auth [2]byte
				if _, err := io.ReadFull(client, auth[:]); err != nil || auth != [2]byte{5, 0} {
					t.Fatalf("auth %v: %v", auth, err)
				}
			}
			if _, err := client.Write(tc.wire); err != nil {
				t.Fatal(err)
			}
			if tc.v4 {
				var status [8]byte
				if _, err := io.ReadFull(client, status[:]); err != nil || status[1] != 91 {
					t.Fatalf("reply %v: %v", status, err)
				}
			} else {
				code, _, err := readReply(client)
				if err != nil || code != addressUnsupported {
					t.Fatalf("reply %d: %v", code, err)
				}
			}
			if err := <-done; err == nil || errorCode(err) != addressUnsupported {
				t.Fatalf("policy result: %v", err)
			}
		})
	}
}

func TestRemoteDNSDomainRelay(t *testing.T) {
	proxy, _, _ := proxyPair(t, 3*time.Second, func(c *Config) { c.RequireDomain = true })
	for _, socks4a := range []bool{false, true} {
		target, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { target.Close() })
		done := make(chan struct{})
		go func() {
			defer close(done)
			c, err := target.Accept()
			if err != nil {
				return
			}
			defer c.Close()
			b, err := io.ReadAll(c)
			if err == nil {
				writeAll(c, append([]byte("remote:"), b...))
			}
		}()
		_, port, _ := net.SplitHostPort(target.Addr().String())
		address := net.JoinHostPort("localhost", port)
		var c *net.TCPConn
		if socks4a {
			raw, err := net.DialTimeout("tcp", proxy, time.Second)
			if err != nil {
				t.Fatal(err)
			}
			c = raw.(*net.TCPConn)
			c.SetDeadline(time.Now().Add(5 * time.Second))
			if err := writeAll(c, socks4ConnectRecord(address, true)); err != nil {
				t.Fatal(err)
			}
			var status [8]byte
			if _, err := io.ReadFull(c, status[:]); err != nil || status[1] != 90 {
				t.Fatalf("SOCKS4a %v: %v", status, err)
			}
		} else {
			var code byte
			c, code, err = connectSOCKS(proxy, address, true)
			if err != nil || code != succeeded {
				t.Fatalf("SOCKS5 %d: %v", code, err)
			}
		}
		defer c.Close()
		if err := writeAll(c, []byte("payload")); err != nil {
			t.Fatal(err)
		}
		c.CloseWrite()
		got, err := io.ReadAll(c)
		if err != nil || !bytes.Equal(got, []byte("remote:payload")) {
			t.Fatalf("relay %q: %v", got, err)
		}
		<-done
	}
}
