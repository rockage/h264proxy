package mux

import (
	"bytes"
	"fmt"
	cryptox "github.com/roca/h264-tunnel-lab/internal/crypto"
	"github.com/roca/h264-tunnel-lab/internal/logging"
	"github.com/roca/h264-tunnel-lab/internal/muxproto"
	"io"
	"testing"
	"time"
)

func TestReliableTransportNeverRetransmits(t *testing.T) {
	st := ackTestStream()
	st.sess.cfg.ReliableTransport = true
	st.trackRetx(muxproto.FrameData, 0, []byte("data"))
	if len(st.retx[0].pt) != 0 {
		t.Fatal("SCTP payload retained for application retransmission")
	}
	st.markSent(muxproto.FrameData, 0, time.Now().Add(-time.Hour))
	st.onAck(&muxproto.Ack{Ranges: []muxproto.SeqRange{{Start: 1, End: 8}}})
	st.retransmitDue(time.Now())
	if st.retransmits != 0 || st.txErr != nil {
		t.Fatal("SCTP delay caused application recovery")
	}
	st.onAck(&muxproto.Ack{CumAck: 1})
	if len(st.retx) != 0 {
		t.Fatal("delivery ACK did not retire metadata")
	}
}

// A set of idle TCP connections should not consume the writer with periodic
// reliability heartbeats when the underlying transport already guarantees delivery.
func TestReliableIdleFeedbackAndWindowRefill(t *testing.T) {
	l2d, d2l := newPipe(), newPipe()
	log := logging.NewTo("reliable-test", io.Discard)
	master, _ := cryptox.GenerateKey()
	l, err := New(Config{SessionID: 77, Role: RoleListener, Mode: cryptox.ModeAEAD, Master: master, Sink: l2d, Source: d2l, Log: log, ReliableTransport: true})
	if err != nil {
		t.Fatal(err)
	}
	d, err := New(Config{SessionID: 77, Role: RoleDialer, Mode: cryptox.ModeAEAD, Master: master, Sink: d2l, Source: l2d, Log: log, ReliableTransport: true})
	if err != nil {
		l.Close()
		t.Fatal(err)
	}
	defer func() { l.Close(); d.Close(); l2d.close(); d2l.close() }()
	st, err := d.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	remote, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	if _, err = st.Write([]byte("ping")); err != nil {
		t.Fatal(err)
	}
	b := make([]byte, 4)
	if _, err = io.ReadFull(remote, b); err != nil {
		t.Fatal(err)
	}
	await(t, func() bool { st.txmu.Lock(); defer st.txmu.Unlock(); return len(st.retx) == 0 })
	before := l.Stats().FramesSent + d.Stats().FramesSent
	time.Sleep(650 * time.Millisecond)
	after := l.Stats().FramesSent + d.Stats().FramesSent
	if after != before {
		t.Fatalf("idle reliable streams generated %d extra control frames", after-before)
	}
	t.Log("idle feedback: 0 frames across three former heartbeat intervals")
	// Removing the loss-recovery heartbeat must still refill flow control well
	// beyond the initial 256 KiB credit and preserve half-close/reclamation.
	payload := randBlob(99, 2*1024*1024)
	sent := make(chan error, 1)
	go func() {
		_, e := st.Write(payload)
		if e == nil {
			e = st.CloseWrite()
		}
		sent <- e
	}()
	received := make(chan error, 1)
	go func() {
		got, e := io.ReadAll(remote)
		if e == nil && !bytes.Equal(got, payload) {
			e = fmt.Errorf("payload mismatch")
		}
		if e == nil {
			e = remote.CloseWrite()
		}
		received <- e
	}()
	for _, done := range []chan error{sent, received} {
		select {
		case e := <-done:
			if e != nil {
				t.Fatal(e)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("window refill stalled")
		}
	}
	if _, err = io.Copy(io.Discard, st); err != nil {
		t.Fatal(err)
	}
	await(t, func() bool { return l.Stats().ActiveStreams == 0 && d.Stats().ActiveStreams == 0 })
}
