package media

import (
	"io"
	"sync"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
)

// RTPWriter adapts a TrackLocalStaticRTP to the transfer.PacketSink interface.
// It maintains the RTP sequence number, timestamp and marker bit. Note that
// Pion overwrites SSRC and PayloadType with the negotiated values inside
// WriteRTP, so those fields set here are placeholders; the sequence number,
// timestamp, marker and payload are preserved and carried on the wire.
type RTPWriter struct {
	track  *webrtc.TrackLocalStaticRTP
	mu     sync.Mutex
	seq    uint16
	ts     uint32
	tsStep uint32
}

// NewRTPWriter builds an RTPWriter. The sequence number and timestamp start at
// the given values (callers typically randomize them). tsStep is added to the
// timestamp per packet to keep it monotonic.
func NewRTPWriter(track *webrtc.TrackLocalStaticRTP, startSeq uint16, startTS, tsStep uint32) *RTPWriter {
	return &RTPWriter{track: track, seq: startSeq, ts: startTS, tsStep: tsStep}
}

// WritePayload sends one payload as a single RTP packet.
func (w *RTPWriter) WritePayload(payload []byte, marker bool) error {
	w.mu.Lock()
	seq := w.seq
	ts := w.ts
	w.seq++
	w.ts += w.tsStep
	w.mu.Unlock()

	pkt := &rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			Marker:         marker,
			PayloadType:    DefaultPayloadType, // overwritten by Pion
			SequenceNumber: seq,
			Timestamp:      ts,
			SSRC:           0, // overwritten by Pion
		},
		Payload: payload,
	}
	return w.track.WriteRTP(pkt)
}

// RTPReader adapts a TrackRemote to the transfer.PacketSource interface. Each
// payload is copied out of Pion's internal buffer so the caller may retain it.
type RTPReader struct {
	track *webrtc.TrackRemote
}

// NewRTPReader builds an RTPReader around a received track.
func NewRTPReader(track *webrtc.TrackRemote) *RTPReader { return &RTPReader{track: track} }

// ReadPayload reads the next RTP packet and returns a copy of its payload and
// marker bit. It returns the underlying error (e.g. io.EOF) when the track
// ends.
func (r *RTPReader) ReadPayload() ([]byte, bool, error) {
	pkt, _, err := r.track.ReadRTP()
	if err != nil {
		return nil, false, err
	}
	out := make([]byte, len(pkt.Payload))
	copy(out, pkt.Payload)
	return out, pkt.Header.Marker, nil
}

// DeferredRTPReader is a PacketSource whose underlying remote track is not
// known at construction time. In a full-duplex session each peer must be able
// to construct its mux Session (which needs a source) before the remote track
// exists, because the remote track only materializes via OnTrack once the peer
// sends its first RTP packet. ReadPayload blocks until SetTrack is called, then
// delegates to a normal RTPReader. Abort unblocks pending/future reads with EOF
// (used on connection teardown).
type DeferredRTPReader struct {
	ready   chan struct{}
	aborted chan struct{}
	setOnce sync.Once
	abtOnce sync.Once
	r       *RTPReader
}

// NewDeferredRTPReader builds an unattached reader.
func NewDeferredRTPReader() *DeferredRTPReader {
	return &DeferredRTPReader{ready: make(chan struct{}), aborted: make(chan struct{})}
}

// SetTrack attaches the remote track; the first call wins.
func (d *DeferredRTPReader) SetTrack(track *webrtc.TrackRemote) {
	d.setOnce.Do(func() {
		d.r = NewRTPReader(track)
		close(d.ready)
	})
}

// Abort causes pending and future ReadPayload calls to return io.EOF.
func (d *DeferredRTPReader) Abort() {
	d.abtOnce.Do(func() { close(d.aborted) })
}

// ReadPayload blocks until a track is attached (or abort), then reads.
func (d *DeferredRTPReader) ReadPayload() ([]byte, bool, error) {
	select {
	case <-d.ready:
		return d.r.ReadPayload()
	case <-d.aborted:
		return nil, false, io.EOF
	}
}
