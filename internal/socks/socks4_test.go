package socks

import (
	"bytes"
	"encoding/binary"
	"io"
	"net"
	"strconv"
	"testing"
	"time"
)

func socks4ConnectRecord(target string, domain bool) []byte {
	host, port, _ := net.SplitHostPort(target)
	p, _ := strconv.Atoi(port)
	wire := []byte{4, 1, byte(p >> 8), byte(p)}
	if domain {
		wire = append(wire, 0, 0, 0, 1)
	} else {
		wire = append(wire, net.ParseIP(host).To4()...)
	}
	wire = append(wire, 0) // empty USERID
	if domain {
		wire = append(wire, host...)
		wire = append(wire, 0)
	}
	return wire
}

func TestSOCKS4And4aPipelinedHalfClose(t *testing.T) {
	proxy, _, _ := proxyPair(t, 3*time.Second)
	for _, domain := range []bool{false, true} {
		name := "SOCKS4"
		if domain {
			name = "SOCKS4a"
		}
		t.Run(name, func(t *testing.T) {
			target, e := net.Listen("tcp", "127.0.0.1:0")
			if e != nil {
				t.Fatal(e)
			}
			defer target.Close()
			done := make(chan struct{})
			go func() {
				defer close(done)
				c, e := target.Accept()
				if e != nil {
					return
				}
				defer c.Close()
				b, e := io.ReadAll(c)
				if e == nil {
					writeAll(c, append([]byte("after-fin:"), b...))
				}
			}()
			addr := target.Addr().String()
			if domain {
				_, p, _ := net.SplitHostPort(addr)
				addr = net.JoinHostPort("localhost", p)
			}
			c, e := net.DialTimeout("tcp", proxy, time.Second)
			if e != nil {
				t.Fatal(e)
			}
			defer c.Close()
			c.SetDeadline(time.Now().Add(10 * time.Second))
			payload := bytes.Repeat([]byte("payload"), 512)
			// Coalesce CONNECT and application bytes in one TCP write. Sniffing must
			// not discard or replay any of the already-arrived application payload.
			if e = writeAll(c, append(socks4ConnectRecord(addr, domain), payload...)); e != nil {
				t.Fatal(e)
			}
			var status [8]byte
			if _, e = io.ReadFull(c, status[:]); e != nil {
				t.Fatal(e)
			}
			if status[0] != 0 || status[1] != 90 {
				t.Fatalf("CONNECT reply: %v", status)
			}
			c.(*net.TCPConn).CloseWrite()
			got, e := io.ReadAll(c)
			if e != nil || !bytes.Equal(got, append([]byte("after-fin:"), payload...)) {
				t.Fatalf("response len=%d err=%v", len(got), e)
			}
			<-done
		})
	}
	// A target failure must use SOCKS4's eight-byte rejection, not SOCKS5 status.
	refused, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	addr := refused.Addr().String()
	refused.Close()
	c, e := net.DialTimeout("tcp", proxy, time.Second)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	writeAll(c, socks4ConnectRecord(addr, false))
	var status [8]byte
	if _, e = io.ReadFull(c, status[:]); e != nil {
		t.Fatal(e)
	}
	if status[0] != 0 || status[1] != 91 {
		t.Fatalf("failure reply: %v", status)
	}
}

func TestSOCKS4Validation(t *testing.T) {
	valid := []byte{4, 1, 0, 80, 1, 2, 3, 4, 0}
	req, e := readSOCKS4Request(bytes.NewReader(valid))
	if e != nil || req.address != "1.2.3.4:80" {
		t.Fatalf("parse: %+v %v", req, e)
	}
	if !bytes.Equal(req.wire, []byte{5, 1, 0, 1, 1, 2, 3, 4, 0, 80}) {
		t.Fatalf("tunnel request: %v", req.wire)
	}
	bind := bytes.Clone(valid)
	bind[1] = 2
	if _, e = readSOCKS4Request(bytes.NewReader(bind)); e == nil || errorCode(e) != commandUnsupported {
		t.Fatalf("BIND accepted: %v", e)
	}
	for _, wire := range [][]byte{
		valid[:7], valid[:8],
		append(valid[:8:8], bytes.Repeat([]byte{'x'}, 256)...),
		{4, 1, 0, 80, 0, 0, 0, 1, 0, 0},
		append([]byte{4, 1, 0, 80, 0, 0, 0, 1, 0}, bytes.Repeat([]byte{'x'}, 256)...),
	} {
		if _, e = readSOCKS4Request(bytes.NewReader(wire)); e == nil {
			t.Fatalf("invalid request accepted, len=%d", len(wire))
		}
	}
	b := clientReply(4, 0, reply(0, &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 1234}))
	if len(b) != 8 || b[1] != 90 || binary.BigEndian.Uint16(b[2:4]) != 1234 || !bytes.Equal(b[4:], []byte{127, 0, 0, 1}) {
		t.Fatalf("bound address: %v", b)
	}
}
