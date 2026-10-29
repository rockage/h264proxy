package mux

import (
	"sync"
	"time"
)

// Congestion-control sizing.
const (
	// mss is the approximate on-wire size budget of one DATA frame
	// (42B header + 900B payload + 16B AEAD tag ≈ 958), rounded up.
	mss = 1000
	// initCwnd is the initial congestion window (RFC 6928: 10 segments).
	initCwnd int64 = 10 * mss
	// minCwnd is the floor the window may be reduced to.
	minCwnd int64 = 2 * mss
	// initSsthresh starts effectively unbounded so the connection begins in
	// slow start.
	initSsthresh int64 = 1 << 40
)

// congestion is a TCP-Reno-style AIMD controller limiting the bytes in flight
// (sent but not yet acknowledged) across the whole session. New DATA is gated
// by grant(); ACKs and losses move the window. ACK/WINDOW/retransmit frames are
// never gated, so feedback always flows.
type congestion struct {
	mu       sync.Mutex
	cond     *sync.Cond
	enabled  bool
	cwnd     int64
	ssthresh int64
	inFlight int64
	rtt      *rttEstimator
	closedCh <-chan struct{}

	lastReduce time.Time

	// metrics
	maxCwnd    int64
	minCwndSet int64
	lossEvents uint64
}

func newCongestion(enabled bool, rtt *rttEstimator, closedCh <-chan struct{}) *congestion {
	c := &congestion{
		enabled:    enabled,
		cwnd:       initCwnd,
		ssthresh:   initSsthresh,
		rtt:        rtt,
		closedCh:   closedCh,
		maxCwnd:    initCwnd,
		minCwndSet: initCwnd,
	}
	c.cond = sync.NewCond(&c.mu)
	return c
}

// grant blocks until the window admits size bytes of new data, then reserves
// them as in-flight. A frame larger than the whole window is allowed only when
// nothing else is in flight (prevents deadlock). It returns the abort error if
// the stream/session is torn down while waiting.
func (c *congestion) grant(size int64, abort func() error) error {
	if !c.enabled {
		return abort()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	for {
		if err := abort(); err != nil {
			return err
		}
		select {
		case <-c.closedCh:
			return ErrSessionClosed
		default:
		}
		if c.inFlight == 0 || c.inFlight+size <= c.cwnd {
			c.inFlight += size
			return nil
		}
		c.cond.Wait()
	}
}

// onAck releases ackedBytes of in-flight data and grows the window. ackedBytes
// is the DATA payload acknowledged for the first time by this ACK.
func (c *congestion) onAck(ackedBytes int64) {
	if !c.enabled || ackedBytes <= 0 {
		return
	}
	c.mu.Lock()
	c.inFlight -= ackedBytes
	if c.inFlight < 0 {
		c.inFlight = 0
	}
	if c.cwnd < c.ssthresh {
		// Slow start: grow by bytes acked (window doubles per RTT).
		c.cwnd += ackedBytes
	} else {
		// Congestion avoidance: ~1 MSS per RTT (appropriate byte counting).
		inc := int64(mss) * ackedBytes / c.cwnd
		if inc < 1 {
			inc = 1
		}
		c.cwnd += inc
	}
	if c.cwnd > c.maxCwnd {
		c.maxCwnd = c.cwnd
	}
	c.cond.Broadcast()
	c.mu.Unlock()
}

// release cancels outstanding reservations without treating them as delivery.
func (c *congestion) release(n int64) {
	c.mu.Lock()
	c.inFlight -= n
	if c.inFlight < 0 {
		c.inFlight = 0
	}
	c.cond.Broadcast()
	c.mu.Unlock()
}

// onLoss reduces the window at most once per RTT. severe (RTO timeout) restarts
// slow start; a mild loss (fast retransmit) drops to the new ssthresh.
func (c *congestion) onLoss(severe bool) {
	if !c.enabled {
		return
	}
	c.mu.Lock()
	now := time.Now()
	srtt, _, _ := c.rtt.snapshot()
	if srtt <= 0 {
		srtt = 100 * time.Millisecond
	}
	if now.Sub(c.lastReduce) >= srtt {
		c.ssthresh = c.cwnd / 2
		if c.ssthresh < minCwnd {
			c.ssthresh = minCwnd
		}
		if severe {
			c.cwnd = initCwnd
		} else {
			c.cwnd = c.ssthresh
		}
		if c.cwnd < minCwnd {
			c.cwnd = minCwnd
		}
		if c.cwnd < c.minCwndSet {
			c.minCwndSet = c.cwnd
		}
		c.lastReduce = now
		c.lossEvents++
	}
	c.cond.Broadcast()
	c.mu.Unlock()
}

// wake re-evaluates blocked grants (called periodically and on shutdown so a
// torn-down stream/session does not hang in grant).
func (c *congestion) wake() {
	c.mu.Lock()
	c.cond.Broadcast()
	c.mu.Unlock()
}

// snapshot returns a metrics view.
func (c *congestion) snapshot() (cwnd, ssthresh, inFlight, maxCwnd, minCwnd int64, lossEvents uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cwnd, c.ssthresh, c.inFlight, c.maxCwnd, c.minCwndSet, c.lossEvents
}
