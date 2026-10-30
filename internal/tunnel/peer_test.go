package tunnel

import (
	"bytes"
	"context"
	"fmt"
	"github.com/roca/h264-tunnel-lab/internal/datawire"
	"github.com/roca/h264-tunnel-lab/internal/logging"
	"github.com/roca/h264-tunnel-lab/internal/mux"
	"github.com/roca/h264-tunnel-lab/internal/rtc"
	"io"
	"net"
	"testing"
	"time"
)

func TestTCPMuxReleasedWhenSignalingSetupFails(t *testing.T) {
	occupied, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	reserve, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := reserve.Addr().(*net.TCPAddr).Port
	reserve.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	_, err = Open(ctx, Config{Role: mux.RoleListener, HTTP: occupied.Addr().String(), RTC: rtc.Options{Transport: "tcp", TCPPort: port}, Log: logging.NewTo("tcp-release-test", io.Discard)})
	if err == nil {
		t.Fatal("occupied signaling address accepted")
	}
	ln, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", fmt.Sprint(port)))
	if err != nil {
		t.Fatalf("ICE TCP listener leaked after setup failure: %v", err)
	}
	ln.Close()
}

func TestExpandedWindowOverLocalICETCP(t *testing.T) {
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	base := Config{HTTP: addr, Key: bytes.Repeat([]byte{0x39}, 32), RTC: rtc.Options{Transport: "tcp"}, Log: logging.NewTo("window-ice-tcp", io.Discard)}
	listenerResult := make(chan *Peer, 1)
	listenerError := make(chan error, 1)
	go func() {
		p, e := Open(ctx, base)
		if e != nil {
			listenerError <- e
		} else {
			listenerResult <- p
		}
	}()
	// Probe TCP readiness without fetching/consuming the single-peer offer.
	for {
		c, e := net.DialTimeout("tcp4", addr, 20*time.Millisecond)
		if e == nil {
			c.Close()
			break
		}
		select {
		case e := <-listenerError:
			t.Fatal(e)
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(5 * time.Millisecond):
		}
	}
	clientConfig := base
	clientConfig.Role = mux.RoleDialer
	clientConfig.RTC.ActiveTCPOnly = true
	clientConfig.StreamReceiveWindow = 1 << 20
	clientConfig.DataChannel = datawire.Config{HighWater: 1 << 20, LowWater: 512 << 10}
	client, err := Open(ctx, clientConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	var server *Peer
	select {
	case server = <-listenerResult:
	case e := <-listenerError:
		t.Fatal(e)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	defer server.Close()
	st, err := client.Session.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	remote, err := server.Session.Accept()
	if err != nil {
		t.Fatal(err)
	}
	payload := bytes.Repeat([]byte("ICE-TCP-expanded-window-integrity"), 65536)
	done := make(chan error, 1)
	go func() {
		_, e := remote.Write(payload)
		if e == nil {
			e = remote.CloseWrite()
		}
		done <- e
	}()
	got, err := io.ReadAll(st)
	if err != nil || !bytes.Equal(got, payload) {
		t.Fatal("large TCP transfer failed", err)
	}
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if client.ICEProtocol() != "tcp" || client.Session.Stats().ReceiveWindowBytes != 1<<20 || client.DataChannelStats().HighWater != 1<<20 {
		t.Fatal("requested transport parameters not applied")
	}
	if err = st.CloseWrite(); err != nil {
		t.Fatal(err)
	}
}
