package mux

import (
	"testing"
	"time"

	"github.com/roca/h264-tunnel-lab/internal/muxproto"
)

func TestFeedbackBypassesFullDataQueueWithoutStarvingData(t *testing.T) {
	s := &Session{closedCh: make(chan struct{}), outCh: make(chan *outFrame, 2), feedbackCh: make(chan *outFrame, 16)}
	s.outCh <- &outFrame{typ: muxproto.FrameOpen}
	s.outCh <- &outFrame{typ: muxproto.FrameData}
	// A full DATA queue must not block the receive goroutine sending ACKs.
	for i := 0; i < 10; i++ {
		if err := s.sendFrame(muxproto.FrameAck, 1, 0, nil); err != nil {
			t.Fatal(err)
		}
	}
	burst := 0
	for i := 0; i < 8; i++ {
		if f := s.nextFrame(&burst); f.typ != muxproto.FrameAck {
			t.Fatalf("feedback not prioritized: %v", f.typ)
		}
	}
	if f := s.nextFrame(&burst); f.typ != muxproto.FrameOpen {
		t.Fatal("DATA queue starved or OPEN order changed")
	}
	for i := 0; i < 2; i++ {
		s.nextFrame(&burst)
	}
	if f := s.nextFrame(&burst); f.typ != muxproto.FrameData {
		t.Fatal("DATA order changed")
	}
	close(s.closedCh)
	if s.nextFrame(&burst) != nil {
		t.Fatal("closed writer did not exit")
	}
}

func TestIdleStreamDoesNotRequestACKOnEveryTick(t *testing.T) {
	st := ackTestStream()
	st.onData(0, []byte("request"))
	ackNow, active, _, ack := st.receiverState()
	if !ackNow || !active || ack.CumAck != 1 {
		t.Fatal("fresh data not promptly acknowledged")
	}
	for i := 0; i < 10; i++ {
		ackNow, active, _, _ = st.receiverState()
		if ackNow || !active {
			t.Fatal("idle stream requested a repeated immediate ACK")
		}
	}
	st.onData(2, []byte("gap"))
	ackNow, _, _, _ = st.receiverState()
	if !ackNow {
		t.Fatal("missing gap feedback")
	}
}

func TestRepeatedSelectiveACKDoesNotRetransmitAgain(t *testing.T) {
	st := ackTestStream()
	st.retx[0] = &retxEntry{typ: muxproto.FrameData, tries: 1, lastSent: time.Now().Add(-time.Second)}
	a := &muxproto.Ack{Ranges: []muxproto.SeqRange{{Start: 1, End: 4}}}
	st.onAck(a)
	if st.retransmits != 1 {
		t.Fatal("initial loss not retransmitted")
	}
	st.retx[0].lastSent = time.Now().Add(-time.Second)
	st.retx[0].queued = false
	st.onAck(a)
	if st.retransmits != 1 {
		t.Fatal("stale SACK caused a repeated retransmit and congestion loss")
	}
	st.sess.rtt.sample(200 * time.Millisecond)
	st.retx[0].lastSent = time.Now().Add(-100 * time.Millisecond)
	a.Ranges[0].End = 5
	st.onAck(a)
	if st.retransmits != 1 {
		t.Fatal("retransmitted again before its ACK could return")
	}
	st.retx[0].lastSent = time.Now().Add(-time.Second)
	st.retx[0].queued = false
	st.onAck(a)
	if st.retransmits != 2 {
		t.Fatal("new evidence failed to recover a lost retransmission")
	}
}

func TestQueuedFramesDoNotStartNetworkTimers(t *testing.T) {
	st := ackTestStream()
	st.trackRetx(muxproto.FrameData, 0, []byte("payload"))
	st.retransmitDue(time.Now().Add(time.Hour))
	st.onAck(&muxproto.Ack{Ranges: []muxproto.SeqRange{{Start: 1, End: 4}}})
	if st.retransmits != 0 || !st.retx[0].firstSent.IsZero() {
		t.Fatal("unsent queued frame counted as network loss")
	}
	sent := time.Now()
	if !st.markSent(muxproto.FrameData, 0, sent) {
		t.Fatal("initial writer send rejected")
	}
	st.retransmitDue(sent.Add(time.Second))
	if st.retransmits != 1 || !st.retx[0].queued {
		t.Fatal("actual wire loss not scheduled for retransmission")
	}
	st.retransmitDue(sent.Add(time.Hour))
	if st.retransmits != 1 {
		t.Fatal("queued retransmit duplicated before writer could send it")
	}
	if !st.markSent(muxproto.FrameData, 0, sent.Add(time.Second)) {
		t.Fatal("retransmit writer send rejected")
	}
	if st.retx[0].firstSent != sent || st.retx[0].lastSent != sent.Add(time.Second) {
		t.Fatal("wire timestamps incorrect")
	}
	st.onAck(&muxproto.Ack{CumAck: 1})
	if st.markSent(muxproto.FrameData, 0, time.Now()) {
		t.Fatal("already ACKed queue entry was sent again")
	}
}
