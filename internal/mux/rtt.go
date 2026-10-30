package mux

import (
	"sync"
	"time"
)

// RTO estimation parameters (RFC 6298, with a lower floor suited to loopback
// and LAN paths so recovery stays responsive in this lab).
const (
	initialRTO = 250 * time.Millisecond
	// minRTO is kept above the receiver's ACK cadence (one ACK per maintenance
	// tick, 50 ms) with margin, so a timely ACK is never mistaken for loss.
	minRTO = 100 * time.Millisecond
	maxRTO = 2 * time.Second
	// ACK and retransmission maintenance runs on this tick. RTO and fast
	// loss detection must leave room for that feedback scheduling delay.
	maintenanceTick = 50 * time.Millisecond
	clockGran       = maintenanceTick
	// maxBackoffShift caps per-frame exponential backoff at 2^4 = 16x base RTO.
	maxBackoffShift = 4
)

// rttEstimator maintains a smoothed RTT (SRTT) and its variation (RTTVAR) and
// derives the retransmission timeout, following Jacobson/Karels (alpha=1/8,
// beta=1/4, K=4). It is shared per Session (one network path) and safe for
// concurrent use.
type rttEstimator struct {
	mu        sync.Mutex
	hasSample bool
	srtt      time.Duration
	rttvar    time.Duration
	rto       time.Duration
}

func newRTTEstimator() *rttEstimator {
	return &rttEstimator{rto: initialRTO}
}

// sample feeds one unambiguous RTT measurement (Karn's algorithm: callers must
// not sample retransmitted frames).
func (e *rttEstimator) sample(r time.Duration) {
	if r <= 0 {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.hasSample {
		e.srtt = r
		e.rttvar = r / 2
		e.hasSample = true
	} else {
		diff := e.srtt - r
		if diff < 0 {
			diff = -diff
		}
		// RTTVAR = 3/4*RTTVAR + 1/4*|SRTT-R|
		e.rttvar = (3*e.rttvar)/4 + diff/4
		// SRTT = 7/8*SRTT + 1/8*R
		e.srtt = (7*e.srtt)/8 + r/8
	}
	v := 4 * e.rttvar
	if v < clockGran {
		v = clockGran
	}
	e.rto = clampRTO(e.srtt + v)
}

// RTO returns the current base retransmission timeout.
func (e *rttEstimator) RTO() time.Duration {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.rto
}

// snapshot returns srtt, rttvar and rto for diagnostics.
func (e *rttEstimator) snapshot() (srtt, rttvar, rto time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.srtt, e.rttvar, e.rto
}

func clampRTO(d time.Duration) time.Duration {
	if d < minRTO {
		return minRTO
	}
	if d > maxRTO {
		return maxRTO
	}
	return d
}

// Allow a compressed/reordered ACK path one extra RTT and the delayed-ACK
// tick before loss recovery collapses the session window.
func recoveryRTO(srtt, estimate time.Duration) time.Duration {
	if guard := 2*srtt + maintenanceTick; guard > estimate {
		estimate = guard
	}
	return clampRTO(estimate)
}

// backoffRTO applies per-frame exponential backoff: a frame retried `tries`
// times waits base<<min(tries-1, maxBackoffShift) before the next timer-driven
// retransmit.
func backoffRTO(base time.Duration, tries int) time.Duration {
	shift := tries - 1
	if shift < 0 {
		shift = 0
	}
	if shift > maxBackoffShift {
		shift = maxBackoffShift
	}
	return base << uint(shift)
}
