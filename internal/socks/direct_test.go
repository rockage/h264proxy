package socks

import (
	"bytes"
	"context"
	"io"
	"net"
	"testing"
	"time"
)

func TestDirectDialerKeepsConnectionAfterCallContextEnds(t *testing.T) {
	_, _, sess := proxyPair(t, 0)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, e := ln.Accept()
		if e == nil {
			defer c.Close()
			io.Copy(c, c)
		}
	}()
	call, cancel := context.WithCancel(t.Context())
	d := Dialer{Context: t.Context(), Session: sess}
	c, err := d.DialContext(call, "tcp", ln.Addr().String())
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	defer c.Close()
	cancel()
	c.SetDeadline(time.Now().Add(5 * time.Second))
	payload := bytes.Repeat([]byte("direct gateway"), 30000)
	written := make(chan error, 1)
	go func() { _, e := c.Write(payload); written <- e }()
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatal(err)
	}
	if err := <-written; err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatal("direct transport payload mismatch")
	}
	if _, err := d.DialContext(t.Context(), "udp", ln.Addr().String()); err == nil {
		t.Fatal("unsupported UDP accepted")
	}
}
