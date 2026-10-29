package mux

import (
	"testing"
	"time"
)

func newTestCong(enabled bool) (*congestion, chan struct{}) {
	closed := make(chan struct{})
	return newCongestion(enabled, newRTTEstimator(), closed), closed
}

func TestCongDisabledIsNoop(t *testing.T) {
	c, _ := newTestCong(false)
	if err := c.grant(1<<20, func() error { return nil }); err != nil {
		t.Fatalf("disabled grant should not block or error: %v", err)
	}
	c.onAck(1000)
	c.onLoss(true)
	cwnd, _, _, _, _, loss := c.snapshot()
	if cwnd != initCwnd || loss != 0 {
		t.Fatalf("disabled controller should not change: cwnd=%d loss=%d", cwnd, loss)
	}
}

func TestCongSlowStartGrowsByAckedBytes(t *testing.T) {
	c, _ := newTestCong(true)
	if err := c.grant(5000, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	cwnd0, _, inFlight0, _, _, _ := c.snapshot()
	if inFlight0 != 5000 {
		t.Fatalf("inFlight=%d, want 5000", inFlight0)
	}
	c.onAck(3000) // still in slow start (cwnd < ssthresh)
	cwnd1, _, inFlight1, _, _, _ := c.snapshot()
	if cwnd1 != cwnd0+3000 {
		t.Fatalf("slow start: cwnd %d -> %d, want +3000", cwnd0, cwnd1)
	}
	if inFlight1 != 2000 {
		t.Fatalf("inFlight=%d, want 2000", inFlight1)
	}
}

func TestCongAvoidanceGrowsSlowly(t *testing.T) {
	c, _ := newTestCong(true)
	// Force congestion avoidance: ssthresh below cwnd.
	c.mu.Lock()
	c.cwnd = 100000
	c.ssthresh = 1000
	c.mu.Unlock()
	c.onAck(1000)
	cwnd, _, _, _, _, _ := c.snapshot()
	// CA increment = mss*acked/cwnd = 1000*1000/100000 = 10 bytes.
	if cwnd <= 100000 || cwnd > 100000+mss {
		t.Fatalf("CA grew too much or not at all: %d", cwnd)
	}
}

func TestCongMildLossHalvesOncePerRTT(t *testing.T) {
	c, _ := newTestCong(true)
	c.mu.Lock()
	c.cwnd = 80000
	c.mu.Unlock()
	c.onLoss(false)
	cwnd, ssthresh, _, _, _, loss := c.snapshot()
	if ssthresh != 40000 || cwnd != 40000 || loss != 1 {
		t.Fatalf("mild loss: cwnd=%d ssthresh=%d loss=%d, want 40000/40000/1", cwnd, ssthresh, loss)
	}
	// A second loss within one RTT must not reduce again.
	c.onLoss(false)
	cwnd2, _, _, _, _, loss2 := c.snapshot()
	if cwnd2 != 40000 || loss2 != 1 {
		t.Fatalf("second loss within RTT should be ignored: cwnd=%d loss=%d", cwnd2, loss2)
	}
}

func TestCongSevereLossResetsToInit(t *testing.T) {
	c, _ := newTestCong(true)
	c.mu.Lock()
	c.cwnd = 80000
	c.mu.Unlock()
	c.onLoss(true)
	cwnd, ssthresh, _, _, _, _ := c.snapshot()
	if cwnd != initCwnd || ssthresh != 40000 {
		t.Fatalf("severe loss: cwnd=%d ssthresh=%d, want %d/40000", cwnd, ssthresh, initCwnd)
	}
}

func TestCongFloor(t *testing.T) {
	c, _ := newTestCong(true)
	c.mu.Lock()
	c.cwnd = minCwnd
	c.mu.Unlock()
	c.onLoss(false)
	cwnd, _, _, _, _, _ := c.snapshot()
	if cwnd < minCwnd {
		t.Fatalf("cwnd fell below floor: %d", cwnd)
	}
}

func TestCongGrantBlocksUntilAck(t *testing.T) {
	c, _ := newTestCong(true)
	// Fill the window.
	if err := c.grant(initCwnd, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		// This grant must block (inFlight already == cwnd, nonzero).
		_ = c.grant(5000, func() error { return nil })
		close(done)
	}()
	select {
	case <-done:
		t.Fatal("grant should have blocked while window full")
	case <-time.After(100 * time.Millisecond):
	}
	// Acking frees space and grows cwnd, unblocking the grant.
	c.onAck(initCwnd)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("grant did not unblock after ack")
	}
}

func TestCongGrantAbortReleases(t *testing.T) {
	c, closed := newTestCong(true)
	_ = c.grant(initCwnd, func() error { return nil })
	errc := make(chan error, 1)
	go func() { errc <- c.grant(5000, func() error { return nil }) }()
	time.Sleep(50 * time.Millisecond)
	close(closed) // session closed
	c.wake()
	select {
	case err := <-errc:
		if err != ErrSessionClosed {
			t.Fatalf("want ErrSessionClosed, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("grant did not return after close")
	}
}

func TestCongLargeFrameAllowedWhenIdle(t *testing.T) {
	c, _ := newTestCong(true)
	// A frame bigger than cwnd is admitted only because nothing is in flight.
	if err := c.grant(initCwnd*4, func() error { return nil }); err != nil {
		t.Fatalf("idle large grant should succeed: %v", err)
	}
}
