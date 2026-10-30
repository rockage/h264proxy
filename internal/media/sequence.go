package media

import "sync"

// ReceiveStats describes RTP sequence observations after SRTP authentication.
// Gaps can be filled by reordered packets; they are not proof of final loss.
// MissingBeforeFirstPacket is unknowable. Late recovery is tracked in a bounded
// 4096-slot history; older untracked gaps remain in UnrecoveredGaps.
type ReceiveStats struct {
	Packets             uint64
	ForwardGapPackets   uint64
	OutOfOrderPackets   uint64
	RecoveredGapPackets uint64
	UnrecoveredGaps     uint64
}

type sequenceTracker struct {
	mu      sync.Mutex
	started bool
	high    uint64
	missing [4096]uint64 // extended sequence + 1; zero is an empty slot
	stats   ReceiveStats
}

func (s *sequenceTracker) observe(seq uint16) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stats.Packets++
	if !s.started {
		s.started = true
		s.high = uint64(seq)
		return
	}
	delta := int16(seq - uint16(s.high))
	if delta > 0 {
		next := s.high + uint64(delta)
		gap := uint64(delta - 1)
		s.stats.ForwardGapPackets += gap
		s.stats.UnrecoveredGaps += gap
		first := s.high + 1
		if next-first > uint64(len(s.missing)) {
			first = next - uint64(len(s.missing))
		}
		for v := first; v < next; v++ {
			s.missing[v%uint64(len(s.missing))] = v + 1
		}
		s.high = next
		return
	}
	s.stats.OutOfOrderPackets++
	if int64(s.high)+int64(delta) < 0 {
		return
	}
	v := uint64(int64(s.high) + int64(delta))
	slot := v % uint64(len(s.missing))
	if s.missing[slot] == v+1 {
		s.missing[slot] = 0
		s.stats.UnrecoveredGaps--
		s.stats.RecoveredGapPackets++
	}
}

func (s *sequenceTracker) snapshot() ReceiveStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stats
}
