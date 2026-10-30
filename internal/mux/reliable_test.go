package mux

import (
	"github.com/roca/h264-tunnel-lab/internal/muxproto"
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
