package muxproto

import (
	"encoding/binary"
	"errors"
)

// ErrBadAck indicates a malformed ACK payload.
var ErrBadAck = errors.New("muxproto: malformed ACK payload")

// MaxAckRanges caps the number of selective ranges carried in one ACK. If a
// receiver has more holes than this, it sends only the cumulative ack for that
// ACK (the sender still recovers via the RTO timer).
const MaxAckRanges = 32

// SeqRange is an inclusive-exclusive run [Start, End) of received sequence
// numbers above the cumulative ack.
type SeqRange struct {
	Start uint64
	End   uint64
}

// Ack is the decoded payload of a FrameAck. CumAck is the next contiguous
// sequence number the receiver still needs (everything below it is delivered).
// Ranges lists received-but-not-yet-contiguous runs above CumAck. CloseAck is
// set once the CLOSE frame has been received.
type Ack struct {
	CumAck   uint64
	CloseAck bool
	Ranges   []SeqRange
}

// Marshal serializes an Ack:
//
//	8  CumAck
//	1  flags (bit0 = CloseAck)
//	1  nRanges
//	.. nRanges * (8 Start, 8 End)
func (a *Ack) Marshal() []byte {
	n := len(a.Ranges)
	if n > MaxAckRanges {
		n = MaxAckRanges
	}
	buf := make([]byte, 10+n*16)
	binary.BigEndian.PutUint64(buf[0:8], a.CumAck)
	if a.CloseAck {
		buf[8] = 1
	}
	buf[9] = byte(n)
	off := 10
	for i := 0; i < n; i++ {
		binary.BigEndian.PutUint64(buf[off:off+8], a.Ranges[i].Start)
		binary.BigEndian.PutUint64(buf[off+8:off+16], a.Ranges[i].End)
		off += 16
	}
	return buf
}

// UnmarshalAck parses an Ack payload.
func UnmarshalAck(b []byte) (*Ack, error) {
	if len(b) < 10 {
		return nil, ErrBadAck
	}
	a := &Ack{
		CumAck:   binary.BigEndian.Uint64(b[0:8]),
		CloseAck: b[8]&1 == 1,
	}
	n := int(b[9])
	if n > MaxAckRanges || len(b) != 10+n*16 {
		return nil, ErrBadAck
	}
	a.Ranges = make([]SeqRange, n)
	off := 10
	for i := 0; i < n; i++ {
		a.Ranges[i] = SeqRange{
			Start: binary.BigEndian.Uint64(b[off : off+8]),
			End:   binary.BigEndian.Uint64(b[off+8 : off+16]),
		}
		off += 16
	}
	return a, nil
}
