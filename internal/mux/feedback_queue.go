package mux

import (
	"container/list"
	"fmt"
	"sort"
	"sync"

	"github.com/roca/h264-tunnel-lab/internal/muxproto"
)

// Reliable feedback is cumulative state, not a FIFO of historical snapshots.
// Keep one pending ACK and WINDOW per stream. Producers never wait for the
// transport writer, which may itself be waiting for the remote receiver.
// The explicit cap bounds memory even for feedback on retired stream IDs.
const maxPendingFeedback = 4096

type feedbackKey struct {
	id  uint32
	typ muxproto.FrameType
}

type feedbackQueue struct {
	mu      sync.Mutex
	order   list.List
	entries map[feedbackKey]*list.Element
	wake    chan struct{}
}

func newFeedbackQueue() *feedbackQueue {
	return &feedbackQueue{entries: make(map[feedbackKey]*list.Element), wake: make(chan struct{}, 1)}
}

func (q *feedbackQueue) put(f *outFrame) (bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	key := feedbackKey{f.streamID, f.typ}
	merged := false
	if e := q.entries[key]; e != nil {
		old := e.Value.(*outFrame)
		switch f.typ {
		case muxproto.FrameWindow:
			a, okA := decodeWindow(old.pt)
			b, okB := decodeWindow(f.pt)
			if !okA || !okB {
				return false, fmt.Errorf("invalid cumulative WINDOW")
			}
			if a > b {
				f.pt = old.pt
			}
		case muxproto.FrameAck:
			a, err := muxproto.UnmarshalAck(old.pt)
			if err != nil {
				return false, err
			}
			b, err := muxproto.UnmarshalAck(f.pt)
			if err != nil {
				return false, err
			}
			b.CumAck = max(a.CumAck, b.CumAck)
			b.CloseAck = a.CloseAck || b.CloseAck
			ranges := append(a.Ranges, b.Ranges...)
			sort.Slice(ranges, func(i, j int) bool { return ranges[i].Start < ranges[j].Start })
			b.Ranges = nil
			for _, r := range ranges {
				if r.End <= b.CumAck {
					continue
				}
				r.Start = max(r.Start, b.CumAck)
				n := len(b.Ranges)
				if n > 0 && r.Start <= b.Ranges[n-1].End {
					b.Ranges[n-1].End = max(b.Ranges[n-1].End, r.End)
				} else {
					b.Ranges = append(b.Ranges, r)
				}
			}
			f.pt = b.Marshal()
		}
		e.Value = f
		merged = true
	} else {
		if len(q.entries) >= maxPendingFeedback {
			return false, fmt.Errorf("reliable feedback queue exceeded %d distinct entries", maxPendingFeedback)
		}
		q.entries[key] = q.order.PushBack(f)
	}
	select {
	case q.wake <- struct{}{}:
	default:
	}
	return merged, nil
}

func (q *feedbackQueue) pop() *outFrame {
	q.mu.Lock()
	defer q.mu.Unlock()
	e := q.order.Front()
	if e == nil {
		return nil
	}
	f := e.Value.(*outFrame)
	delete(q.entries, feedbackKey{f.streamID, f.typ})
	q.order.Remove(e)
	return f
}

func (q *feedbackQueue) len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.entries)
}

func (s *Session) nextReliableFrame(burst *int) *outFrame {
	for {
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
		if f := s.reliableFeedback.pop(); f != nil {
			*burst++
			return f
		}
		select {
		case <-s.closedCh:
			return nil
		case <-s.reliableFeedback.wake:
		case f := <-s.outCh:
			*burst = 0
			return f
		}
	}
}
