package mux

import (
	"encoding/binary"
	"errors"
	"io"
	"sort"
	"sync"
	"time"

	"github.com/roca/h264-tunnel-lab/internal/muxproto"
)

// Flow-control, sizing, and reliability constants.
const (
	// InitialWindow is the bytes a receiver implicitly grants a new stream
	// before any WINDOW update. Both peers assume the same value.
	InitialWindow int64 = 256 * 1024
	// windowUpdateThreshold is how much additional credit must accrue before
	// the receiver proactively sends a cumulative WINDOW update.
	windowUpdateThreshold = InitialWindow / 2
	// MaxChunk is the max plaintext bytes per DATA frame.
	MaxChunk = 900

	// minRetxInterval throttles ACK-driven fast retransmit of a given frame.
	minRetxInterval = 40 * time.Millisecond
	// maxRetries is the per-frame retransmit cap before a stream is failed.
	maxRetries = 60
	// ackEveryN sends a cumulative ACK at least this often (delivered frames).
	// Kept small so acknowledgements stay ahead of the RTO even when the
	// congestion window keeps only a few frames in flight.
	ackEveryN = 8
)

// ErrStreamClosed is returned by operations on a closed stream.
var ErrStreamClosed = errors.New("mux: stream closed")

// ErrPeerUnreachable is set when retransmits are exhausted.
var ErrPeerUnreachable = errors.New("mux: peer unreachable (retransmits exhausted)")

var ErrStreamReset = errors.New("mux: stream reset")

// retxEntry is one unacknowledged outbound frame kept for possible resend.
type retxEntry struct {
	typ       muxproto.FrameType
	seq       uint64
	pt        []byte
	dataLen   int // DATA payload bytes counted against the congestion window
	firstSent time.Time
	lastSent  time.Time
	tries     int
}

// Stream is a bidirectional, flow-controlled, reliable logical stream
// multiplexed over a Session. It implements io.ReadWriteCloser. Lost frames are
// recovered by a selective-repeat ARQ; flow control uses an idempotent
// cumulative credit so a lost WINDOW update cannot deadlock the sender.
type Stream struct {
	id        uint32
	sess      *Session
	wmu       sync.Mutex // serialize writes and graceful half-close
	resetOnce sync.Once
	abortOnce sync.Once
	aborted   chan struct{}
	resetting bool // protected by txmu

	// receive side
	rmu            sync.Mutex
	rcond          *sync.Cond
	recvBuf        []byte
	reorder        map[uint64][]byte
	nextRecvSeq    uint64
	recvClosed     bool
	closeSeq       uint64
	recvErr        error
	grantedTotal   int64 // cumulative credit granted to the peer
	lastWindowSent int64 // last cumulative credit we transmitted
	deliveredAck   int
	dupCount       uint64
	reorderCount   uint64

	// send side (flow control)
	smu         sync.Mutex
	scond       *sync.Cond
	sentBytes   int64 // cumulative original DATA bytes sent (excl. retransmits)
	maxGrant    int64 // highest cumulative credit observed from the peer
	nextSendSeq uint64
	sendClosed  bool

	// send side (reliability)
	txmu        sync.Mutex
	retx        map[uint64]*retxEntry
	sendCumAck  uint64
	retransmits uint64
	txErr       error
}

func newStream(id uint32, sess *Session) *Stream {
	s := &Stream{
		id:             id,
		aborted:        make(chan struct{}),
		sess:           sess,
		reorder:        make(map[uint64][]byte),
		retx:           make(map[uint64]*retxEntry),
		grantedTotal:   InitialWindow,
		lastWindowSent: InitialWindow,
		maxGrant:       InitialWindow,
	}
	s.rcond = sync.NewCond(&s.rmu)
	s.scond = sync.NewCond(&s.smu)
	return s
}

// ID returns the stream identifier.
func (s *Stream) ID() uint32 { return s.id }

// Aborted closes on RESET or transport failure, allowing pending external
// operations such as TCP dialing to be canceled too. Half-close leaves it open.
func (s *Stream) Aborted() <-chan struct{} { return s.aborted }

// Read returns reassembled in-order bytes. It blocks until data is available,
// the stream is cleanly closed and fully delivered (io.EOF), or the session
// aborts (error). A gap no longer errors: the missing frame is retransmitted.
func (s *Stream) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	s.rmu.Lock()
	for {
		if s.recvErr != nil {
			err := s.recvErr
			s.rmu.Unlock()
			return 0, err
		}
		if len(s.recvBuf) > 0 {
			n := copy(p, s.recvBuf)
			s.recvBuf = s.recvBuf[n:]
			s.grantedTotal += int64(n)
			var grant int64
			if s.grantedTotal-s.lastWindowSent >= windowUpdateThreshold {
				grant = s.grantedTotal
				s.lastWindowSent = s.grantedTotal
			}
			s.rmu.Unlock()
			if grant > 0 {
				s.sess.sendWindow(s.id, uint64(grant))
			}
			return n, nil
		}
		if s.recvClosed && s.nextRecvSeq >= s.closeSeq {
			s.rmu.Unlock()
			return 0, io.EOF
		}
		s.rcond.Wait()
	}
}

// Write fragments p into DATA frames, blocking until cumulative flow credit
// permits, and registers each frame for retransmission.
func (s *Stream) Write(p []byte) (int, error) {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	total := 0
	for len(p) > 0 {
		n := len(p)
		if n > MaxChunk {
			n = MaxChunk
		}
		chunk := p[:n]
		p = p[n:]

		s.smu.Lock()
		for s.sentBytes+int64(n) > s.maxGrant && !s.sendClosed && !s.sess.isClosed() && s.txError() == nil {
			s.scond.Wait()
		}
		if s.sendClosed || s.sess.isClosed() {
			s.smu.Unlock()
			return total, ErrStreamClosed
		}
		if err := s.txError(); err != nil {
			s.smu.Unlock()
			return total, err
		}
		s.sentBytes += int64(n)
		seq := s.nextSendSeq
		s.nextSendSeq++
		s.smu.Unlock()

		// Congestion control: block new data until the window admits it.
		if err := s.sess.cong.grant(int64(n), s.sendAbort); err != nil {
			return total, err
		}
		s.trackRetx(muxproto.FrameData, seq, chunk)
		if err := s.sess.sendFrame(muxproto.FrameData, s.id, seq, chunk); err != nil {
			return total, err
		}
		total += n
	}
	return total, nil
}

// Close sends a (reliable) CLOSE frame announcing no further DATA.
func (s *Stream) Close() error {
	return s.CloseWrite()
}

// CloseWrite reliably sends EOF while preserving the readable direction.
func (s *Stream) CloseWrite() error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	s.smu.Lock()
	if s.sendClosed {
		s.smu.Unlock()
		return nil
	}
	s.sendClosed = true
	closeSeq := s.nextSendSeq
	s.trackRetx(muxproto.FrameClose, closeSeq, nil)
	s.scond.Broadcast()
	s.smu.Unlock()

	return s.sess.sendFrame(muxproto.FrameClose, s.id, closeSeq, nil)
}

// Reset cancels both directions and reliably notifies the peer. Unlike Close,
// this unblocks pending reads/writes immediately and discards buffered data.
func (s *Stream) Reset() error {
	var err error
	s.resetOnce.Do(func() {
		s.txmu.Lock()
		s.resetting = true
		s.txmu.Unlock()
		s.abort(ErrStreamReset)
		s.wmu.Lock()
		defer s.wmu.Unlock()
		// A Write may have reserved credit just before the first abort.
		s.abort(ErrStreamReset)
		s.smu.Lock()
		seq := s.nextSendSeq
		s.smu.Unlock()
		s.trackRetx(muxproto.FrameReset, seq, nil)
		err = s.sess.sendFrame(muxproto.FrameReset, s.id, seq, nil)
		s.txmu.Lock()
		s.resetting = false
		s.txmu.Unlock()
	})
	return err
}

// --- reliability: sender side ---

func (s *Stream) trackRetx(typ muxproto.FrameType, seq uint64, pt []byte) {
	now := time.Now()
	e := &retxEntry{typ: typ, seq: seq, firstSent: now, lastSent: now, tries: 1}
	if len(pt) > 0 {
		e.pt = append([]byte(nil), pt...)
	}
	if typ == muxproto.FrameData {
		e.dataLen = len(pt)
	}
	s.txmu.Lock()
	s.retx[seq] = e
	s.txmu.Unlock()
}

// sendAbort is the abort predicate for a blocked congestion grant.
func (s *Stream) sendAbort() error {
	if err := s.txError(); err != nil {
		return err
	}
	if s.sess.isClosed() {
		return ErrSessionClosed
	}
	return nil
}

func (s *Stream) txError() error {
	s.txmu.Lock()
	defer s.txmu.Unlock()
	return s.txErr
}

// onAck removes acknowledged frames and fast-retransmits still-missing ones.
func (s *Stream) onAck(a *muxproto.Ack) {
	now := time.Now()
	var resend []*retxEntry
	var newestSample time.Time
	failed := false
	s.txmu.Lock()
	if a.CumAck > s.sendCumAck {
		s.sendCumAck = a.CumAck
	}
	maxReported := a.CumAck
	for _, r := range a.Ranges {
		if r.End > maxReported {
			maxReported = r.End
		}
	}
	var ackedBytes int64
	for seq, e := range s.retx {
		if seq < a.CumAck || inRanges(seq, a.Ranges) {
			// Karn's algorithm: only unretransmitted frames yield a clean RTT.
			if e.tries == 1 {
				if e.firstSent.After(newestSample) {
					newestSample = e.firstSent
				}
			}
			ackedBytes += int64(e.dataLen)
			delete(s.retx, seq)
			continue
		}
		// Require three later sequence numbers as loss evidence. One later
		// packet can simply be reordered; it must not halve the whole session's
		// congestion window. Tail losses remain covered by the RTO timer.
		if maxReported > seq && maxReported-seq > 3 && now.Sub(e.lastSent) >= minRetxInterval {
			if e.tries >= maxRetries {
				s.txErr = ErrPeerUnreachable
				failed = true
				continue
			}
			e.lastSent = now
			e.tries++
			s.retransmits++
			resend = append(resend, cloneEntry(e))
		}
	}
	s.txmu.Unlock()

	// One ACK is one timing observation, regardless of how many frames it
	// covers. Sampling every frame in random map order artificially erases
	// RTT variation and can cause premature RTOs on real networks.
	if !newestSample.IsZero() {
		s.sess.rtt.sample(now.Sub(newestSample))
	}
	s.sess.cong.onAck(ackedBytes)
	if len(resend) > 0 {
		s.sess.cong.onLoss(false) // fast retransmit: mild congestion signal
	}
	for _, e := range resend {
		_ = s.sess.sendFrame(e.typ, s.id, e.seq, e.pt)
	}
	if failed {
		s.abort(ErrPeerUnreachable)
		return
	}
	s.smu.Lock()
	s.scond.Broadcast()
	s.smu.Unlock()
}

// retransmitDue resends frames whose RTO elapsed. Called by the session timer.
func (s *Stream) retransmitDue(now time.Time) {
	base := s.sess.rtt.RTO()
	var resend []*retxEntry
	failed := false
	s.txmu.Lock()
	for _, e := range s.retx {
		if now.Sub(e.lastSent) < backoffRTO(base, e.tries) {
			continue
		}
		if e.tries >= maxRetries {
			s.txErr = ErrPeerUnreachable
			failed = true
			continue
		}
		e.lastSent = now
		e.tries++
		s.retransmits++
		resend = append(resend, cloneEntry(e))
	}
	s.txmu.Unlock()

	if len(resend) > 0 {
		s.sess.cong.onLoss(true) // RTO timeout: severe congestion signal
	}
	for _, e := range resend {
		_ = s.sess.sendFrame(e.typ, s.id, e.seq, e.pt)
	}
	if failed {
		s.abort(ErrPeerUnreachable)
	}
}

func cloneEntry(e *retxEntry) *retxEntry {
	c := *e
	if e.pt != nil {
		c.pt = append([]byte(nil), e.pt...)
	}
	return &c
}

func inRanges(seq uint64, ranges []muxproto.SeqRange) bool {
	for _, r := range ranges {
		if seq >= r.Start && seq < r.End {
			return true
		}
	}
	return false
}

// --- reliability: receiver side ---

func (s *Stream) onData(seq uint64, data []byte) {
	s.rmu.Lock()
	if s.recvErr != nil {
		s.rmu.Unlock()
		return
	}
	ackNow := false
	switch {
	case seq < s.nextRecvSeq:
		s.dupCount++
		ackNow = true // sender likely missed our ACK; re-ack immediately
	case seq == s.nextRecvSeq:
		s.recvBuf = append(s.recvBuf, data...)
		s.nextRecvSeq++
		for {
			next, ok := s.reorder[s.nextRecvSeq]
			if !ok {
				break
			}
			s.recvBuf = append(s.recvBuf, next...)
			delete(s.reorder, s.nextRecvSeq)
			s.nextRecvSeq++
		}
		s.deliveredAck++
		if s.deliveredAck >= ackEveryN {
			ackNow = true
			s.deliveredAck = 0
		}
		s.rcond.Signal()
	default: // out of order
		if _, exists := s.reorder[seq]; exists {
			s.dupCount++
		} else {
			cp := make([]byte, len(data))
			copy(cp, data)
			s.reorder[seq] = cp
			s.reorderCount++
			ackNow = true // gap: tell the sender immediately
		}
	}
	ack := s.buildAckLocked()
	s.rmu.Unlock()

	if ackNow {
		s.sess.sendAck(s.id, ack)
	}
}

func (s *Stream) onClose(closeSeq uint64) {
	s.rmu.Lock()
	if s.recvErr != nil {
		s.rmu.Unlock()
		return
	}
	if !s.recvClosed {
		s.recvClosed = true
		s.closeSeq = closeSeq
	}
	s.rcond.Broadcast()
	ack := s.buildAckLocked()
	s.rmu.Unlock()
	s.sess.sendAck(s.id, ack) // CLOSE is acked immediately
}

// buildAckLocked constructs the current ACK. Caller holds rmu.
//
// DATA frames occupy seq 0..N-1, CLOSE occupies seq N. CumAck is the lowest seq
// not yet received in that combined space.
func (s *Stream) buildAckLocked() *muxproto.Ack {
	cum := s.nextRecvSeq
	seqs := make([]uint64, 0, len(s.reorder)+1)
	for seq := range s.reorder {
		seqs = append(seqs, seq)
	}
	if s.recvClosed {
		if s.nextRecvSeq == s.closeSeq {
			cum = s.closeSeq + 1
		} else {
			seqs = append(seqs, s.closeSeq)
		}
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	ranges := coalesce(seqs)
	if len(ranges) > muxproto.MaxAckRanges {
		ranges = ranges[:muxproto.MaxAckRanges]
	}
	return &muxproto.Ack{CumAck: cum, CloseAck: s.recvClosed, Ranges: ranges}
}

func coalesce(sorted []uint64) []muxproto.SeqRange {
	if len(sorted) == 0 {
		return nil
	}
	var out []muxproto.SeqRange
	start := sorted[0]
	prev := sorted[0]
	for _, v := range sorted[1:] {
		if v == prev+1 {
			prev = v
			continue
		}
		out = append(out, muxproto.SeqRange{Start: start, End: prev + 1})
		start, prev = v, v
	}
	out = append(out, muxproto.SeqRange{Start: start, End: prev + 1})
	return out
}

// receiverState reports whether the receiver should resend an ACK now
// (holes/close pending) and whether it is still active (and thus should keep
// re-advertising its cumulative window). It also returns the current grant.
func (s *Stream) receiverState() (ackNow, active bool, grant uint64, ack *muxproto.Ack) {
	s.rmu.Lock()
	defer s.rmu.Unlock()
	holes := len(s.reorder) > 0 || (s.recvClosed && s.nextRecvSeq < s.closeSeq)
	fullyDone := s.recvClosed && s.nextRecvSeq >= s.closeSeq
	started := s.nextRecvSeq > 0 || len(s.reorder) > 0 || s.recvClosed
	active = started && !fullyDone
	return holes, active, uint64(s.grantedTotal), s.buildAckLocked()
}

func (s *Stream) onWindow(grant uint64) {
	s.smu.Lock()
	if int64(grant) > s.maxGrant {
		s.maxGrant = int64(grant)
		s.scond.Broadcast()
	}
	s.smu.Unlock()
}

// abort marks both directions failed (session shutdown or peer unreachable).
func (s *Stream) abort(err error) {
	s.rmu.Lock()
	if s.recvErr == nil {
		s.recvErr = err
	}
	s.rcond.Broadcast()
	s.rmu.Unlock()

	s.txmu.Lock()
	var released int64
	for _, e := range s.retx {
		released += int64(e.dataLen)
	}
	clear(s.retx)
	if s.txErr == nil {
		s.txErr = err
	}
	s.txmu.Unlock()
	s.sess.cong.release(released)

	s.smu.Lock()
	s.sendClosed = true
	s.scond.Broadcast()
	s.smu.Unlock()
	s.abortOnce.Do(func() { close(s.aborted) })
}

// finished permits reclamation only after both directions ended and every
// local DATA/CLOSE/RESET has been acknowledged.
func (s *Stream) finished() bool {
	s.rmu.Lock()
	rdone := s.recvErr != nil || (s.recvClosed && s.nextRecvSeq >= s.closeSeq && len(s.recvBuf) == 0)
	s.rmu.Unlock()
	s.smu.Lock()
	sdone := s.sendClosed
	s.smu.Unlock()
	s.txmu.Lock()
	tdone := !s.resetting && len(s.retx) == 0
	s.txmu.Unlock()
	return rdone && sdone && tdone
}

func (s *Stream) stats() (dup, reorder, retransmits uint64) {
	s.rmu.Lock()
	dup, reorder = s.dupCount, s.reorderCount
	s.rmu.Unlock()
	s.txmu.Lock()
	retransmits = s.retransmits
	s.txmu.Unlock()
	return
}

func encodeWindow(v uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, v)
	return b
}

func decodeWindow(b []byte) (uint64, bool) {
	if len(b) != 8 {
		return 0, false
	}
	return binary.BigEndian.Uint64(b), true
}
