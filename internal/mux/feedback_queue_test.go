package mux

import (
	"github.com/roca/h264-tunnel-lab/internal/muxproto"
	"testing"
	"time"
)

func TestReliableFeedbackCoalescesWithoutBlockingProducers(t *testing.T) {
	s := &Session{closedCh: make(chan struct{}), outCh: make(chan *outFrame, 2), reliableFeedback: newFeedbackQueue()}
	s.outCh <- &outFrame{typ: muxproto.FrameOpen}
	s.outCh <- &outFrame{typ: muxproto.FrameData}
	done := make(chan error, 1)
	go func() {
		for id := uint32(1); id <= 600; id++ {
			for n := uint64(1); n <= 3; n++ {
				if err := s.sendFrame(muxproto.FrameAck, id, 0, (&muxproto.Ack{CumAck: n}).Marshal()); err != nil {
					done <- err
					return
				}
				if err := s.sendFrame(muxproto.FrameWindow, id, 0, encodeWindow(n*1024)); err != nil {
					done <- err
					return
				}
			}
		}
		done <- nil
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("feedback producer blocked behind transport writer")
	}
	if s.reliableFeedback.len() != 1200 || s.feedbackCoalesced.Load() != 2400 {
		t.Fatalf("pending=%d coalesced=%d", s.reliableFeedback.len(), s.feedbackCoalesced.Load())
	}
	burst := 0
	for i := 0; i < 8; i++ {
		f := s.nextFrame(&burst)
		if f.typ != muxproto.FrameAck && f.typ != muxproto.FrameWindow {
			t.Fatal("feedback not prioritized")
		}
	}
	if f := s.nextFrame(&burst); f.typ != muxproto.FrameOpen {
		t.Fatal("DATA queue starved or ordering changed")
	}
	close(s.closedCh)
	if s.nextFrame(&burst) != nil {
		t.Fatal("closed writer did not stop")
	}
}

func TestReliableFeedbackPreservesNewestCreditAndFinalACK(t *testing.T) {
	q := newFeedbackQueue()
	ack := func(a muxproto.Ack) {
		t.Helper()
		if _, err := q.put(&outFrame{typ: muxproto.FrameAck, streamID: 1, pt: a.Marshal()}); err != nil {
			t.Fatal(err)
		}
	}
	ack(muxproto.Ack{CumAck: 3, CloseAck: true, Ranges: []muxproto.SeqRange{{Start: 8, End: 10}}})
	ack(muxproto.Ack{CumAck: 5, Ranges: []muxproto.SeqRange{{Start: 6, End: 9}}})
	ack(muxproto.Ack{CumAck: 2}) // An older snapshot races with a newer callback.
	a, err := muxproto.UnmarshalAck(q.pop().pt)
	if err != nil || a.CumAck != 5 || !a.CloseAck || len(a.Ranges) != 1 || a.Ranges[0] != (muxproto.SeqRange{Start: 6, End: 10}) {
		t.Fatalf("lost cumulative/final/selective ACK: %+v %v", a, err)
	}
	for _, v := range []uint64{4096, 8192, 1024} {
		if _, err := q.put(&outFrame{typ: muxproto.FrameWindow, streamID: 1, pt: encodeWindow(v)}); err != nil {
			t.Fatal(err)
		}
	}
	if v, ok := decodeWindow(q.pop().pt); !ok || v != 8192 {
		t.Fatalf("credit regressed to %d", v)
	}
	if q.len() != 0 {
		t.Fatal("drained feedback retained")
	}
}

func TestReliableFeedbackMemoryBound(t *testing.T) {
	q := newFeedbackQueue()
	for i := uint32(0); i < maxPendingFeedback; i++ {
		if _, err := q.put(&outFrame{typ: muxproto.FrameWindow, streamID: i, pt: encodeWindow(1)}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := q.put(&outFrame{typ: muxproto.FrameWindow, streamID: maxPendingFeedback, pt: encodeWindow(1)}); err == nil {
		t.Fatal("distinct feedback entries escaped memory bound")
	}
	if merged, err := q.put(&outFrame{typ: muxproto.FrameWindow, streamID: 0, pt: encodeWindow(2)}); !merged || err != nil {
		t.Fatal("full queue refused an existing cumulative state update")
	}
}
