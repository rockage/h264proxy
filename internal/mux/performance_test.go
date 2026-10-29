package mux

import (
	"testing"
	"time"

	"github.com/roca/h264-tunnel-lab/internal/muxproto"
)

func TestPacerRetainsOversleepCreditAndBoundsIdleBurst(t *testing.T) {
	now := time.Unix(1000, 0)
	p := &pacer{rate: 1_000_000, next: now}
	if got := p.schedule(1000, now); got != time.Millisecond {
		t.Fatalf("first packet delay: %v", got)
	}
	// The scheduler woke 3ms late. Credit should allow three packets to
	// catch up, instead of restarting a fresh 1ms sleep after each packet.
	wake := now.Add(4 * time.Millisecond)
	for i := 0; i < 3; i++ {
		if got := p.schedule(1000, wake); got != 0 {
			t.Fatalf("lost wakeup credit: %v", got)
		}
	}
	if got := p.schedule(1000, wake); got != time.Millisecond {
		t.Fatalf("rate not enforced: %v", got)
	}
	// Long idle periods may allow only a small bounded burst.
	wake = now.Add(time.Hour)
	burst := 0
	for p.schedule(1000, wake) == 0 {
		burst++
		if burst > 9 {
			t.Fatal("unbounded idle burst")
		}
	}
	if burst < 1 {
		t.Fatal("missing bounded burst")
	}
	if (&pacer{}).schedule(1000, now) != 0 {
		t.Fatal("unlimited pacer delayed")
	}
}

func ackTestStream() *Stream {
	s := &Session{rtt: newRTTEstimator(), closedCh: make(chan struct{}), outCh: make(chan *outFrame, 16)}
	s.cong = newCongestion(true, s.rtt, s.closedCh)
	return newStream(1, s)
}

func TestFastRetransmitToleratesReordering(t *testing.T) {
	for _, tc := range []struct {
		end  uint64
		want uint64
	}{{2, 0}, {3, 0}, {4, 1}} {
		st := ackTestStream()
		st.retx[0] = &retxEntry{typ: muxproto.FrameData, tries: 1, lastSent: time.Now().Add(-time.Second)}
		st.onAck(&muxproto.Ack{Ranges: []muxproto.SeqRange{{Start: 1, End: tc.end}}})
		if st.retransmits != tc.want {
			t.Fatalf("end=%d retransmits=%d want=%d", tc.end, st.retransmits, tc.want)
		}
		_, _, _, _, _, loss := st.sess.cong.snapshot()
		if loss != tc.want {
			t.Fatalf("reordering reduced congestion window: %d", loss)
		}
		// Immediate repeated selective ACKs must not resend again.
		st.onAck(&muxproto.Ack{Ranges: []muxproto.SeqRange{{Start: 1, End: tc.end}}})
		if st.retransmits != tc.want {
			t.Fatal("duplicate ACK caused another retransmit")
		}
	}
}

func TestBatchAckUsesOneRTTObservationAndKarn(t *testing.T) {
	st := ackTestStream()
	now := time.Now()
	for i := uint64(0); i < 100; i++ {
		st.retx[i] = &retxEntry{tries: 1, firstSent: now.Add(-time.Duration(129-i) * time.Millisecond)}
	}
	// A retransmitted packet with a newer timestamp is not a valid sample.
	st.retx[100] = &retxEntry{tries: 2, firstSent: now.Add(-time.Millisecond)}
	st.onAck(&muxproto.Ack{CumAck: 101})
	srtt, variance, _ := st.sess.rtt.snapshot()
	if srtt < 30*time.Millisecond || srtt > 60*time.Millisecond || variance != srtt/2 {
		t.Fatalf("batch ACK counted as multiple RTT observations or violated Karn: srtt=%v variance=%v", srtt, variance)
	}
	st.onAck(&muxproto.Ack{CumAck: 101})
	srtt2, variance2, _ := st.sess.rtt.snapshot()
	if srtt2 != srtt || variance2 != variance {
		t.Fatal("duplicate ACK changed RTT estimator")
	}
}
