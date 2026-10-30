package media

import "testing"

func TestRTPSequenceWrapGapsReorderingAndDuplicates(t *testing.T) {
	var s sequenceTracker
	for _, seq := range []uint16{65534, 65535, 0, 2, 1, 3, 3} {
		s.observe(seq)
	}
	st := s.snapshot()
	if st.Packets != 7 || st.ForwardGapPackets != 1 || st.OutOfOrderPackets != 2 || st.RecoveredGapPackets != 1 || st.UnrecoveredGaps != 0 {
		t.Fatalf("stats=%+v", st)
	}
	s.observe(6)
	if st = s.snapshot(); st.UnrecoveredGaps != 2 {
		t.Fatalf("missing=%+v", st)
	}
	s.observe(4)
	s.observe(4)
	s.observe(5)
	if st = s.snapshot(); st.UnrecoveredGaps != 0 || st.RecoveredGapPackets != 3 {
		t.Fatalf("duplicate repaired a gap twice: %+v", st)
	}
}

func TestRTPSequenceBoundsLargeGapHistory(t *testing.T) {
	var s sequenceTracker
	s.observe(0)
	s.observe(10000)
	s.observe(9999)
	st := s.snapshot()
	if st.ForwardGapPackets != 9999 || st.UnrecoveredGaps != 9998 || st.RecoveredGapPackets != 1 {
		t.Fatalf("stats=%+v", st)
	}
}
