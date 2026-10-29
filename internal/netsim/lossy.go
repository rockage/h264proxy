// Package netsim provides small network impairment helpers for testing and
// demonstrating the mux reliability layer. It is not used on the production
// data path.
package netsim

import (
	"math/rand"
	"sync"
	"sync/atomic"

	"github.com/roca/h264-tunnel-lab/internal/transfer"
)

// LossySink wraps a transfer.PacketSink and silently drops each frame with a
// fixed probability, modelling a lossy UDP path (the write "succeeds" from the
// sender's point of view, but the packet never arrives). It is safe for
// concurrent use.
type LossySink struct {
	inner transfer.PacketSink
	p     float64
	mu    sync.Mutex
	rng   *rand.Rand

	dropped atomic.Uint64
	passed  atomic.Uint64
}

// NewLossySink wraps inner, dropping a fraction p (0..1) of frames. seed makes
// the loss pattern reproducible.
func NewLossySink(inner transfer.PacketSink, p float64, seed int64) *LossySink {
	return &LossySink{inner: inner, p: p, rng: rand.New(rand.NewSource(seed))}
}

// WritePayload drops the frame with probability p, otherwise forwards it.
func (l *LossySink) WritePayload(b []byte, marker bool) error {
	l.mu.Lock()
	drop := l.rng.Float64() < l.p
	l.mu.Unlock()
	if drop {
		l.dropped.Add(1)
		return nil // sender believes the packet was sent; it is lost in transit
	}
	l.passed.Add(1)
	return l.inner.WritePayload(b, marker)
}

// Dropped returns the number of frames dropped so far.
func (l *LossySink) Dropped() uint64 { return l.dropped.Load() }

// Passed returns the number of frames forwarded so far.
func (l *LossySink) Passed() uint64 { return l.passed.Load() }
