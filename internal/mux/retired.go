package mux

// Retired IDs are coalesced separately by parity. Dense sequential traffic
// needs one range instead of one tombstone per connection. Live or never-seen
// IDs can leave gaps between ranges.
type idRange struct{ lo, hi uint32 }

// Caller holds streamsMu.
func (s *Session) retiredContains(id uint32) bool {
	v := id / 2
	for _, r := range s.retired[id%2] {
		if v < r.lo {
			return false
		}
		if v <= r.hi {
			return true
		}
	}
	return false
}

func (s *Session) retire(st *Stream) {
	s.streamsMu.Lock()
	defer s.streamsMu.Unlock()
	if s.streams[st.id] != st || !st.finished() {
		return
	}
	d, r, rt := st.stats()
	s.retiredDup += d
	s.retiredReorder += r
	s.retiredRetx += rt
	delete(s.streams, st.id)
	v, parity := st.id/2, st.id%2
	ranges := s.retired[parity]
	next := idRange{v, v}
	out := make([]idRange, 0, len(ranges)+1)
	inserted := false
	for _, r := range ranges {
		switch {
		case r.hi+1 < next.lo:
			out = append(out, r)
		case next.hi+1 < r.lo:
			if !inserted {
				out = append(out, next)
				inserted = true
			}
			out = append(out, r)
		default:
			if r.lo < next.lo {
				next.lo = r.lo
			}
			if r.hi > next.hi {
				next.hi = r.hi
			}
		}
	}
	if !inserted {
		out = append(out, next)
	}
	s.retired[parity] = out
}
