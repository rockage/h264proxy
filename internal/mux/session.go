// Package mux implements a full-duplex, multi-stream layer over a single
// bidirectional WebRTC H.264 media channel. Many logical streams
// (io.ReadWriteCloser) are multiplexed in each direction over one Session,
// each with independent in-order reassembly, duplicate/reorder detection, and
// credit-based flow control. Each direction is protected by its own
// HKDF-derived AES-256-GCM subkey.
//
// A Session is transport-agnostic: it writes/reads frame payloads through the
// transfer.PacketSink / PacketSource interfaces, which in production are backed
// by the WebRTC RTP track (one send track, one received track).
package mux

import (
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	cryptox "github.com/roca/h264-tunnel-lab/internal/crypto"
	"github.com/roca/h264-tunnel-lab/internal/logging"
	"github.com/roca/h264-tunnel-lab/internal/muxproto"
	"github.com/roca/h264-tunnel-lab/internal/protocol"
	"github.com/roca/h264-tunnel-lab/internal/transfer"
)

// Role determines stream-id parity (to avoid collisions) and key direction.
type Role int

const (
	// RoleListener is the signaling offerer (even stream ids).
	RoleListener Role = iota
	// RoleDialer is the signaling answerer (odd stream ids).
	RoleDialer
)

func (r Role) String() string {
	if r == RoleListener {
		return "listener"
	}
	return "dialer"
}

// ErrSessionClosed is returned once the session is shut down.
var ErrSessionClosed = errors.New("mux: session closed")

// Config configures a Session.
type Config struct {
	SessionID uint64
	Role      Role
	Mode      string // cryptox.ModeAEAD or cryptox.ModeRaw
	Master    []byte // master key (aead mode)
	Sink      transfer.PacketSink
	Source    transfer.PacketSource
	Log       *logging.Logger
	// RateBytesPerSec paces the aggregate send side to avoid overflowing the
	// media receive path (which, unlike a reliable transport, silently drops
	// packets under burst). 0 means unlimited.
	RateBytesPerSec int
	// NoCongestion disables the AIMD congestion controller, leaving only
	// per-stream flow control and the fixed pacer.
	NoCongestion bool
}

// DefaultRateBytesPerSec is a conservative send pace that avoided loss in
// testing on loopback. Callers may override via Config.RateBytesPerSec.
const DefaultRateBytesPerSec = 16_000_000

// outFrame is a queued, not-yet-sealed frame for the single writer goroutine.
type outFrame struct {
	typ      muxproto.FrameType
	streamID uint32
	seq      uint64
	pt       []byte
}

// Session multiplexes streams over one media channel direction-pair.
type Session struct {
	cfg        Config
	log        *logging.Logger
	sealer     *cryptox.Sealer // send side (own nonce prefix + counter)
	recvCipher protocol.Cipher // receive side

	rtt  *rttEstimator // adaptive RTO estimator (shared per path)
	cong *congestion   // AIMD congestion controller (shared per path)

	oddLocal bool // true if this side allocates odd stream ids

	streamsMu                               sync.Mutex
	streams                                 map[uint32]*Stream
	nextLocalID                             uint32
	retired                                 [2][]idRange
	retiredDup, retiredReorder, retiredRetx uint64

	acceptMu   sync.Mutex
	acceptCond *sync.Cond
	acceptQ    []*Stream

	closed     atomic.Bool
	closedCh   chan struct{}
	done       chan struct{} // closed when demux exits
	writerUp   chan struct{} // closed when writer goroutine exits
	outCh      chan *outFrame
	feedbackCh chan *outFrame // ACK/WINDOW must not wait behind queued DATA

	// metrics (atomic)
	framesSent atomic.Uint64
	framesRecv atomic.Uint64
	bytesSent  atomic.Uint64
	bytesRecv  atomic.Uint64
	decodeFail atomic.Uint64
	pacingRate atomic.Int64
}

// New builds a Session and starts its demux goroutine.
func New(cfg Config) (*Session, error) {
	if cfg.Log == nil {
		return nil, errors.New("mux: nil logger")
	}
	sendCipher, recvCipher, err := buildCiphers(cfg)
	if err != nil {
		return nil, err
	}
	sealer, err := cryptox.NewSealer(sendCipher)
	if err != nil {
		return nil, err
	}
	s := &Session{
		cfg:        cfg,
		log:        cfg.Log.With(logging.KeySessionID, fmt.Sprintf("%016x", cfg.SessionID)),
		sealer:     sealer,
		recvCipher: recvCipher,
		rtt:        newRTTEstimator(),
		streams:    make(map[uint32]*Stream),
		closedCh:   make(chan struct{}),
		done:       make(chan struct{}),
		writerUp:   make(chan struct{}),
		outCh:      make(chan *outFrame, 1024),
		feedbackCh: make(chan *outFrame, 256),
	}
	s.cong = newCongestion(!cfg.NoCongestion, s.rtt, s.closedCh)
	if cfg.Role == RoleDialer {
		s.oddLocal = true
		s.nextLocalID = 1
	} else {
		s.oddLocal = false
		s.nextLocalID = 2
	}
	s.acceptCond = sync.NewCond(&s.acceptMu)
	go s.writeLoop()
	go s.demux()
	go s.maintain()
	return s, nil
}

// writeLoop is the single goroutine that seals, paces, and writes every
// outbound frame. Centralizing the send keeps the nonce counter monotonic,
// serializes RTP writes, and applies the send-rate pace that prevents media
// receive-buffer overflow.
func (s *Session) writeLoop() {
	defer close(s.writerUp)
	rate := s.cfg.RateBytesPerSec
	p := newPacer(rate)
	feedbackBurst := 0
	for {
		f := s.nextFrame(&feedbackBurst)
		if f == nil {
			return
		}
		if f.typ == muxproto.FrameData || f.typ == muxproto.FrameClose || f.typ == muxproto.FrameReset {
			s.streamsMu.Lock()
			st := s.streams[f.streamID]
			s.streamsMu.Unlock()
			if st == nil || !st.markSent(f.typ, f.seq, time.Now()) {
				continue
			}
		}
		s.pacingRate.Store(int64(p.rate))
		counter, err := s.sealer.Next()
		if err != nil {
			s.log.Error("nonce_exhausted", logging.KeyError, err.Error())
			s.shutdown(err)
			return
		}
		frame, err := muxproto.Encode(s.sealer.Cipher(), f.typ, s.cfg.SessionID, f.streamID, f.seq, s.sealer.Prefix(), counter, f.pt)
		if err != nil {
			s.log.Error("encode_failed", logging.KeyError, err.Error())
			continue
		}
		if err := s.cfg.Sink.WritePayload(frame, false); err != nil {
			s.log.Debug("sink_write_error", logging.KeyError, err.Error())
			s.shutdown(err)
			return
		}
		s.framesSent.Add(1)
		s.bytesSent.Add(uint64(len(frame)))
		p.wait(len(frame))
	}
}

// Prioritize feedback, but admit queued DATA after eight feedback frames so
// a busy receiver cannot starve the opposite direction. OPEN/DATA/CLOSE/RESET
// retain their original queue ordering; sealing/nonces stay in writeLoop.
func (s *Session) nextFrame(burst *int) *outFrame {
	select {
	case <-s.closedCh:
		return nil
	default:
	}
	if *burst >= 8 {
		select {
		case f := <-s.outCh:
			*burst = 0
			return f
		default:
		}
	}
	select {
	case f := <-s.feedbackCh:
		*burst++
		return f
	default:
	}
	select {
	case <-s.closedCh:
		return nil
	case f := <-s.feedbackCh:
		*burst++
		return f
	case f := <-s.outCh:
		*burst = 0
		return f
	}
}

func buildCiphers(cfg Config) (send, recv protocol.Cipher, err error) {
	if cfg.Mode == cryptox.ModeRaw {
		return cryptox.NullCipher{}, cryptox.NullCipher{}, nil
	}
	var sendDir, recvDir string
	if cfg.Role == RoleListener {
		sendDir, recvDir = muxproto.DirListenerToDialer, muxproto.DirDialerToListener
	} else {
		sendDir, recvDir = muxproto.DirDialerToListener, muxproto.DirListenerToDialer
	}
	sendKey, err := muxproto.DeriveKey(cfg.Master, cfg.SessionID, sendDir)
	if err != nil {
		return nil, nil, err
	}
	recvKey, err := muxproto.DeriveKey(cfg.Master, cfg.SessionID, recvDir)
	if err != nil {
		return nil, nil, err
	}
	sc, err := cryptox.NewAEADCipher(sendKey)
	if err != nil {
		return nil, nil, err
	}
	rc, err := cryptox.NewAEADCipher(recvKey)
	if err != nil {
		return nil, nil, err
	}
	return sc, rc, nil
}

func (s *Session) isClosed() bool { return s.closed.Load() }

func (s *Session) isPeerID(id uint32) bool {
	return (id%2 == 1) != s.oddLocal
}

// OpenStream allocates a new local stream and announces it with an OPEN frame.
func (s *Session) OpenStream() (*Stream, error) {
	if s.isClosed() {
		return nil, ErrSessionClosed
	}
	s.streamsMu.Lock()
	if s.isClosed() || s.nextLocalID == 0 || s.nextLocalID == ^uint32(0) {
		s.streamsMu.Unlock()
		return nil, ErrSessionClosed
	}
	id := s.nextLocalID
	s.nextLocalID += 2
	st := newStream(id, s)
	s.streams[id] = st
	s.streamsMu.Unlock()

	if err := s.sendFrame(muxproto.FrameOpen, id, 0, nil); err != nil {
		return nil, err
	}
	s.log.Debug("stream_opened", "stream_id", id)
	return st, nil
}

// Accept blocks until the peer opens a new stream, or the session closes.
func (s *Session) Accept() (*Stream, error) {
	s.acceptMu.Lock()
	defer s.acceptMu.Unlock()
	for len(s.acceptQ) == 0 {
		if s.isClosed() {
			return nil, ErrSessionClosed
		}
		s.acceptCond.Wait()
	}
	st := s.acceptQ[0]
	s.acceptQ = s.acceptQ[1:]
	return st, nil
}

// getStream returns the stream for id, creating it if the id is peer-allocated
// and createIfPeer is set (handles OPEN, or DATA arriving before OPEN).
func (s *Session) getStream(id uint32, createIfPeer bool) *Stream {
	s.streamsMu.Lock()
	st := s.streams[id]
	if st != nil {
		s.streamsMu.Unlock()
		return st
	}
	if createIfPeer && s.isPeerID(id) {
		if id == 0 || s.retiredContains(id) || s.isClosed() {
			s.streamsMu.Unlock()
			return nil
		}
		st = newStream(id, s)
		s.streams[id] = st
		s.streamsMu.Unlock()
		s.acceptMu.Lock()
		s.acceptQ = append(s.acceptQ, st)
		s.acceptCond.Signal()
		s.acceptMu.Unlock()
		s.log.Debug("stream_accepted", "stream_id", id)
		return st
	}
	s.streamsMu.Unlock()
	return nil
}

// sendFrame enqueues one frame for the writer goroutine. The plaintext is
// copied so the caller may reuse its buffer. It blocks (providing backpressure)
// when the outbound queue is full, and returns once the session closes.
func (s *Session) sendFrame(typ muxproto.FrameType, streamID uint32, seq uint64, plaintext []byte) error {
	if s.isClosed() {
		return ErrSessionClosed
	}
	f := &outFrame{typ: typ, streamID: streamID, seq: seq}
	if len(plaintext) > 0 {
		f.pt = append([]byte(nil), plaintext...)
	}
	queue := s.outCh
	if (typ == muxproto.FrameAck || typ == muxproto.FrameWindow) && s.feedbackCh != nil {
		queue = s.feedbackCh
	}
	select {
	case queue <- f:
		return nil
	case <-s.closedCh:
		return ErrSessionClosed
	}
}

// sendWindow advertises the cumulative flow-control credit for a stream. The
// value is absolute (monotonic), so a lost WINDOW frame is corrected by the
// next one rather than causing a stall.
func (s *Session) sendWindow(streamID uint32, grant uint64) {
	if err := s.sendFrame(muxproto.FrameWindow, streamID, 0, encodeWindow(grant)); err != nil {
		s.log.Debug("window_update_failed", "stream_id", streamID, logging.KeyError, err.Error())
	}
}

// demux reads frames from the source and dispatches them until the source ends.
func (s *Session) demux() {
	defer close(s.done)
	for {
		payload, _, err := s.cfg.Source.ReadPayload()
		if err != nil {
			if err != io.EOF {
				s.log.Debug("source_read_error", logging.KeyError, err.Error())
			}
			s.shutdown(err)
			return
		}
		s.framesRecv.Add(1)
		s.bytesRecv.Add(uint64(len(payload)))

		h, pt, derr := muxproto.Decode(s.recvCipher, payload)
		if derr != nil {
			s.decodeFail.Add(1)
			s.log.Warn("frame_decode_failed", logging.KeyError, derr.Error())
			continue
		}
		if h.SessionID != s.cfg.SessionID || h.StreamID == 0 {
			continue
		}
		s.streamsMu.Lock()
		retired := s.retiredContains(h.StreamID)
		s.streamsMu.Unlock()
		if retired {
			// A final ACK can be lost after reclamation. Re-ack duplicates
			// without recreating a stream or retaining its buffers forever.
			if h.Type == muxproto.FrameData || h.Type == muxproto.FrameClose || h.Type == muxproto.FrameReset {
				s.sendAck(h.StreamID, &muxproto.Ack{CumAck: h.Seq + 1, CloseAck: true})
			}
			continue
		}
		switch h.Type {
		case muxproto.FrameOpen:
			s.getStream(h.StreamID, true)
		case muxproto.FrameData:
			if st := s.getStream(h.StreamID, true); st != nil {
				st.onData(h.Seq, pt)
			}
		case muxproto.FrameClose:
			if st := s.getStream(h.StreamID, true); st != nil {
				st.onClose(h.Seq)
			}
		case muxproto.FrameWindow:
			if st := s.getStream(h.StreamID, false); st != nil {
				if delta, ok := decodeWindow(pt); ok {
					st.onWindow(delta)
				}
			}
		case muxproto.FrameAck:
			if st := s.getStream(h.StreamID, false); st != nil {
				if ack, err := muxproto.UnmarshalAck(pt); err == nil {
					st.onAck(ack)
				}
			}
		case muxproto.FrameReset:
			if st := s.getStream(h.StreamID, true); st != nil {
				st.abort(ErrStreamReset)
				s.sendAck(st.id, &muxproto.Ack{CumAck: h.Seq + 1, CloseAck: true})
			}
		}
	}
}

// sendAck transmits a receiver ACK for a stream (best effort).
func (s *Session) sendAck(streamID uint32, ack *muxproto.Ack) {
	_ = s.sendFrame(muxproto.FrameAck, streamID, 0, ack.Marshal())
}

// maintain drives timer-based retransmission and periodic ACKs. One goroutine
// per session scans every tick until the session closes.
func (s *Session) maintain() {
	const tick = 50 * time.Millisecond
	t := time.NewTicker(tick)
	defer t.Stop()
	ticks := 0
	for {
		select {
		case <-s.closedCh:
			return
		case now := <-t.C:
			ticks++
			s.cong.wake() // let blocked grants re-check abort/close
			s.streamsMu.Lock()
			streams := make([]*Stream, 0, len(s.streams))
			for _, st := range s.streams {
				streams = append(streams, st)
			}
			s.streamsMu.Unlock()

			// Resend the receiver's window/ACK heartbeat every ~200ms so a lost
			// WINDOW or ACK cannot stall a live transfer.
			heartbeat := ticks%4 == 0
			for _, st := range streams {
				st.retransmitDue(now) // sender: RTO retransmit
				ackNow, active, grant, ack := st.receiverState()
				// Fresh data/holes need prompt feedback. Idle streams only need
				// the slower heartbeat to recover a lost ACK or WINDOW.
				if ackNow || (active && heartbeat) {
					s.sendAck(st.id, ack)
				}
				if active && heartbeat {
					s.sendWindow(st.id, grant)
				}
				if st.finished() {
					s.retire(st)
				}
			}
		}
	}
}

// shutdown aborts all streams and wakes any blocked Accept. It is idempotent.
func (s *Session) shutdown(cause error) {
	if s.closed.Swap(true) {
		return
	}
	close(s.closedCh)
	s.cong.wake() // release any grant blocked in Write
	s.streamsMu.Lock()
	for _, st := range s.streams {
		st.abort(ErrSessionClosed)
	}
	s.streamsMu.Unlock()
	s.acceptMu.Lock()
	s.acceptCond.Broadcast()
	s.acceptMu.Unlock()
}

// Close shuts down the session. The demux goroutine exits once the underlying
// source is closed by the caller (e.g. closing the PeerConnection).
func (s *Session) Close() error {
	s.shutdown(nil)
	return nil
}

// Done returns a channel closed when the demux goroutine has exited.
func (s *Session) Done() <-chan struct{} { return s.done }

// Closed is closed immediately on shutdown, even if the source is blocked.
func (s *Session) Closed() <-chan struct{} { return s.closedCh }

// Stats is a snapshot of session counters.
type Stats struct {
	ActiveStreams              int
	QueuedDataFrames           int
	QueuedFeedbackFrames       int
	InFlightBytes              int64
	PacingBytesPerSec          int64
	FramesSent, FramesReceived uint64
	BytesSent, BytesReceived   uint64
	DecodeFailures             uint64
	DupChunks, ReorderChunks   uint64
	Retransmits                uint64
	// RTO estimator snapshot (milliseconds).
	SRTTMillis float64
	RTOMillis  float64
	// Congestion controller snapshot.
	CwndBytes      int64
	MaxCwndBytes   int64
	MinCwndBytes   int64
	SsthreshBytes  int64
	CongLossEvents uint64
}

// Stats returns a snapshot including aggregated per-stream counters.
func (s *Session) Stats() Stats {
	srtt, _, rto := s.rtt.snapshot()
	cwnd, ssthresh, inFlight, maxCwnd, minCwnd, lossEvents := s.cong.snapshot()
	st := Stats{
		QueuedDataFrames:     len(s.outCh),
		QueuedFeedbackFrames: len(s.feedbackCh),
		InFlightBytes:        inFlight,
		PacingBytesPerSec:    s.pacingRate.Load(),
		FramesSent:           s.framesSent.Load(),
		FramesReceived:       s.framesRecv.Load(),
		BytesSent:            s.bytesSent.Load(),
		BytesReceived:        s.bytesRecv.Load(),
		DecodeFailures:       s.decodeFail.Load(),
		SRTTMillis:           float64(srtt) / float64(time.Millisecond),
		RTOMillis:            float64(rto) / float64(time.Millisecond),
		CwndBytes:            cwnd,
		MaxCwndBytes:         maxCwnd,
		MinCwndBytes:         minCwnd,
		SsthreshBytes:        ssthresh,
		CongLossEvents:       lossEvents,
	}
	s.streamsMu.Lock()
	st.ActiveStreams = len(s.streams)
	st.DupChunks = s.retiredDup
	st.ReorderChunks = s.retiredReorder
	st.Retransmits = s.retiredRetx
	for _, stream := range s.streams {
		d, r, rt := stream.stats()
		st.DupChunks += d
		st.ReorderChunks += r
		st.Retransmits += rt
	}
	s.streamsMu.Unlock()
	return st
}

// pacer is a simple byte-rate limiter used by the writer goroutine.
type pacer struct {
	rate int
	next time.Time
}

func newPacer(rate int) *pacer { return &pacer{rate: rate, next: time.Now()} }

func (p *pacer) wait(n int) {
	if p.rate <= 0 {
		return
	}
	if delay := p.schedule(n, time.Now()); delay > 0 {
		time.Sleep(delay)
	}
}

// Keep timer oversleep as pacing credit, bounded to eight DATA frames. Resetting
// the deadline after every late wake discards that credit and can turn a
// microsecond packet interval into one OS timer tick per packet. The bound
// prevents an idle sender from accumulating an unbounded catch-up burst.
func (p *pacer) schedule(n int, now time.Time) time.Duration {
	if p.rate <= 0 {
		return 0
	}
	burst := time.Duration(float64(8*(MaxChunk+128)) / float64(p.rate) * float64(time.Second))
	if earliest := now.Add(-burst); p.next.Before(earliest) {
		p.next = earliest
	}
	delay := time.Duration(float64(n) / float64(p.rate) * float64(time.Second))
	p.next = p.next.Add(delay)
	if p.next.After(now) {
		return p.next.Sub(now)
	}
	return 0
}
