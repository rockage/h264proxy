package mux

import (
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	cryptox "github.com/roca/h264-tunnel-lab/internal/crypto"
	"github.com/roca/h264-tunnel-lab/internal/muxproto"
	"github.com/roca/h264-tunnel-lab/internal/transfer"
)

type dropControl struct {
	transfer.PacketSink
	mu      sync.Mutex
	typ     muxproto.FrameType
	dropped bool
}

func (d *dropControl) WritePayload(b []byte, m bool) error {
	h, e := muxproto.DecodeHeader(b)
	d.mu.Lock()
	drop := e == nil && h.Type == d.typ && !d.dropped
	if drop {
		d.dropped = true
	}
	d.mu.Unlock()
	if drop {
		return nil
	}
	return d.PacketSink.WritePayload(b, m)
}

func await(t *testing.T, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition did not converge")
}

func TestResetUnblocksWritersAndSurvivesLoss(t *testing.T) {
	l, d, done := newPair(t, cryptox.ModeAEAD)
	defer done()
	// A lost RESET and a lost ACK must both recover.
	d.cfg.Sink = &dropControl{PacketSink: d.cfg.Sink, typ: muxproto.FrameReset}
	l.cfg.Sink = &dropControl{PacketSink: l.cfg.Sink, typ: muxproto.FrameAck}
	st, e := d.OpenStream()
	if e != nil {
		t.Fatal(e)
	}
	remote, e := l.Accept()
	if e != nil {
		t.Fatal(e)
	}
	written := make(chan error, 1)
	go func() { _, e := st.Write(make([]byte, 2*int(InitialWindow))); written <- e }()
	await(t, func() bool { _, _, f, _, _, _ := d.cong.snapshot(); return f > 0 })
	if e = st.Reset(); e != nil {
		t.Fatal(e)
	}
	select {
	case <-st.Aborted():
	default:
		t.Fatal("reset did not signal cancellation")
	}
	select {
	case e = <-written:
		if !errors.Is(e, ErrStreamReset) {
			t.Fatalf("write: %v", e)
		}
	case <-time.After(time.Second):
		t.Fatal("write stayed blocked")
	}
	read := make(chan error, 1)
	go func() { _, e := io.Copy(io.Discard, remote); read <- e }()
	select {
	case e = <-read:
		if !errors.Is(e, ErrStreamReset) {
			t.Fatalf("read: %v", e)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("peer reset not delivered")
	}
	await(t, func() bool { return l.Stats().ActiveStreams == 0 && d.Stats().ActiveStreams == 0 })
	_, _, flight, _, _, _ := d.cong.snapshot()
	if flight != 0 {
		t.Fatalf("leaked congestion reservation: %d", flight)
	}
	// Reset must not affect a later connection on the same session.
	next, e := d.OpenStream()
	if e != nil {
		t.Fatal(e)
	}
	peer, e := l.Accept()
	if e != nil {
		t.Fatal(e)
	}
	go func() { next.Write([]byte("alive")); next.CloseWrite() }()
	b, e := io.ReadAll(peer)
	if e != nil || string(b) != "alive" {
		t.Fatalf("next stream: %q %v", b, e)
	}
	peer.CloseWrite()
	io.Copy(io.Discard, next)
	select {
	case <-next.Aborted():
		t.Fatal("graceful half-close signaled abort")
	default:
	}
}

func TestFinishedStreamsAreReclaimedAndLateFramesNotReopened(t *testing.T) {
	l, d, done := newPair(t, cryptox.ModeAEAD)
	defer done()
	var last uint32
	for i := 0; i < 40; i++ {
		st, e := d.OpenStream()
		if e != nil {
			t.Fatal(e)
		}
		peer, e := l.Accept()
		if e != nil {
			t.Fatal(e)
		}
		last = st.ID()
		st.CloseWrite()
		peer.CloseWrite()
		if _, e = io.Copy(io.Discard, st); e != nil {
			t.Fatal(e)
		}
		if _, e = io.Copy(io.Discard, peer); e != nil {
			t.Fatal(e)
		}
	}
	await(t, func() bool { return l.Stats().ActiveStreams == 0 && d.Stats().ActiveStreams == 0 })
	l.streamsMu.Lock()
	n := len(l.retired[1])
	l.streamsMu.Unlock()
	if n != 1 {
		t.Fatalf("expected coalesced retired IDs, got %d ranges", n)
	}
	// A delayed duplicate OPEN/CLOSE must not allocate or enqueue another stream.
	d.sendFrame(muxproto.FrameOpen, last, 0, nil)
	d.sendFrame(muxproto.FrameClose, last, 0, nil)
	time.Sleep(100 * time.Millisecond)
	if l.Stats().ActiveStreams != 0 {
		t.Fatal("late frame resurrected stream")
	}
	l.acceptMu.Lock()
	queued := len(l.acceptQ)
	l.acceptMu.Unlock()
	if queued != 0 {
		t.Fatal("late stream was accepted twice")
	}
}
