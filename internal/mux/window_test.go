package mux

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"testing"
	"time"

	cryptox "github.com/roca/h264-tunnel-lab/internal/crypto"
	"github.com/roca/h264-tunnel-lab/internal/logging"
	"github.com/roca/h264-tunnel-lab/internal/muxproto"
)

func windowPair(t testing.TB, left, right int64) (*Session, *Session) {
	t.Helper()
	a, b := newPipe(), newPipe()
	key, _ := cryptox.GenerateKey()
	log := logging.NewTo("window-test", io.Discard)
	l, e := New(Config{SessionID: 123, Role: RoleListener, Mode: cryptox.ModeAEAD, Master: key, ReliableTransport: true, ReceiveWindow: left, Sink: a, Source: b, Log: log})
	if e != nil {
		t.Fatal(e)
	}
	r, e := New(Config{SessionID: 123, Role: RoleDialer, Mode: cryptox.ModeAEAD, Master: key, ReliableTransport: true, ReceiveWindow: right, Sink: b, Source: a, Log: log})
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { l.Close(); r.Close(); a.close(); b.close() })
	return l, r
}

func TestWindowExpansionMixedDefaultsAndFIN(t *testing.T) {
	for _, windows := range [][2]int64{{0, 0}, {1 << 20, 0}, {0, 1 << 20}, {1 << 20, 1 << 20}} {
		t.Run(fmt.Sprint(windows), func(t *testing.T) {
			l, r := windowPair(t, windows[0], windows[1])
			st, e := r.OpenStream()
			if e != nil {
				t.Fatal(e)
			}
			remote, e := l.Accept()
			if e != nil {
				t.Fatal(e)
			}
			payload := bytes.Repeat([]byte("asymmetric-window-and-half-close"), 45000)
			done := make(chan error, 2)
			go func() {
				_, e := st.Write(payload)
				if e == nil {
					e = st.CloseWrite()
				}
				done <- e
			}()
			go func() {
				_, e := remote.Write(payload)
				if e == nil {
					e = remote.CloseWrite()
				}
				done <- e
			}()
			read := func(s *Stream) error {
				b, e := io.ReadAll(s)
				if e == nil && sha256.Sum256(b) != sha256.Sum256(payload) {
					return fmt.Errorf("payload mismatch")
				}
				return e
			}
			reads := make(chan error, 2)
			go func() { reads <- read(st) }()
			go func() { reads <- read(remote) }()
			for i := 0; i < 4; i++ {
				select {
				case e := <-done:
					if e != nil {
						t.Fatal(e)
					}
				case e := <-reads:
					if e != nil {
						t.Fatal(e)
					}
				case <-time.After(5 * time.Second):
					t.Fatal("credit/FIN deadlock")
				}
			}
		})
	}
}

func TestExpandedWindowBoundAndBlockedWriteCancellation(t *testing.T) {
	for _, capacity := range []int64{InitialWindow, 1 << 20} {
		l, r := windowPair(t, capacity, 0)
		st, e := r.OpenStream()
		if e != nil {
			t.Fatal(e)
		}
		remote, e := l.Accept()
		if e != nil {
			t.Fatal(e)
		}
		done := make(chan error, 1)
		go func() { _, e := st.Write(make([]byte, capacity*2)); done <- e }()
		await(t, func() bool {
			remote.rmu.Lock()
			defer remote.rmu.Unlock()
			return int64(len(remote.recvBuf)) == capacity
		})
		await(t, func() bool { return r.Stats().CreditWaiters > 0 })
		// A slow reader exhausts only its own credit. New connections must
		// still open and deliver while the large stream remains blocked.
		small, e := r.OpenStream()
		if e != nil {
			t.Fatal(e)
		}
		smallRemote, e := l.Accept()
		if e != nil {
			t.Fatal(e)
		}
		if _, e = small.Write([]byte("control")); e != nil {
			t.Fatal(e)
		}
		control := make(chan error, 1)
		go func() {
			b := make([]byte, 7)
			_, e := io.ReadFull(smallRemote, b)
			if e == nil && string(b) != "control" {
				e = fmt.Errorf("small stream corrupted")
			}
			control <- e
		}()
		select {
		case e := <-control:
			if e != nil {
				t.Fatal(e)
			}
		case <-time.After(time.Second):
			t.Fatal("slow stream blocked another connection")
		}
		remote.rmu.Lock()
		if remote.receivedTotal != capacity || int64(len(remote.recvBuf)) > capacity {
			t.Fatal("receive memory exceeded capacity")
		}
		remote.rmu.Unlock()
		if e = st.Reset(); e != nil {
			t.Fatal(e)
		}
		select {
		case e = <-done:
			if e == nil {
				t.Fatal("blocked write succeeded after reset")
			}
		case <-time.After(time.Second):
			t.Fatal("credit waiter leaked")
		}
		if stats := r.Stats(); stats.CreditWaiters != 0 || stats.CreditWaitNanos == 0 {
			t.Fatalf("wait metrics: %+v", stats)
		}
	}
}

func TestReceiveCreditViolationClosesSession(t *testing.T) {
	l, r := windowPair(t, 0, 0)
	st, e := r.OpenStream()
	if e != nil {
		t.Fatal(e)
	}
	remote, e := l.Accept()
	if e != nil {
		t.Fatal(e)
	}
	// Bypass sender flow control to simulate a peer violating its grant.
	remote.onData(0, make([]byte, InitialWindow+1))
	select {
	case <-l.Closed():
	case <-time.After(time.Second):
		t.Fatal("over-credit peer accepted")
	}
	_ = st
}

func TestReceiveCreditCountsUniqueReorderedBytes(t *testing.T) {
	l, r := windowPair(t, 0, 0)
	if _, e := r.OpenStream(); e != nil {
		t.Fatal(e)
	}
	remote, e := l.Accept()
	if e != nil {
		t.Fatal(e)
	}
	half := make([]byte, InitialWindow/2)
	remote.onData(1, half)
	remote.onData(1, half) // a duplicate must not consume credit again
	remote.onData(0, half)
	remote.rmu.Lock()
	total, n := remote.receivedTotal, len(remote.recvBuf)
	remote.rmu.Unlock()
	if l.isClosed() || total != InitialWindow || int64(n) != InitialWindow {
		t.Fatal("duplicate/reorder charged twice", total, n)
	}
}

func TestReceiveWindowValidation(t *testing.T) {
	for _, v := range []int64{-1, InitialWindow - 1, 4*1024*1024 + 1} {
		if ValidateReceiveWindow(v, true) == nil {
			t.Fatal("invalid window accepted", v)
		}
	}
	if ValidateReceiveWindow(1<<20, false) == nil {
		t.Fatal("legacy expansion accepted")
	}
}

func TestFullWriterQueueCancellationMetrics(t *testing.T) {
	sink, source := newPipe(), newPipe()
	sink.ch = make(chan []byte, 1)
	s, e := New(Config{SessionID: 789, Role: RoleDialer, Mode: cryptox.ModeRaw, ReliableTransport: true, Sink: sink, Source: source, Log: logging.NewTo("queue-test", io.Discard)})
	if e != nil {
		t.Fatal(e)
	}
	defer func() { s.Close(); sink.close(); source.close() }()
	done := make(chan error, 1)
	go func() {
		for i := 0; i < 2048; i++ {
			if e := s.sendFrame(muxproto.FrameOpen, 1, 0, nil); e != nil {
				done <- e
				return
			}
		}
		done <- nil
	}()
	await(t, func() bool { return s.Stats().QueueWaiters > 0 && len(s.outCh) == cap(s.outCh) })
	s.Close()
	select {
	case e := <-done:
		if e == nil {
			t.Fatal("full writer queue ignored shutdown")
		}
	case <-time.After(time.Second):
		t.Fatal("enqueue waiter leaked")
	}
	if s.Stats().QueueWaitNanos == 0 || s.Stats().QueueWaiters != 0 {
		t.Fatal("queue wait duration missing")
	}
}
