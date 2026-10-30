package mux

import (
	"testing"
	"time"
)

func TestRTTInitialRTO(t *testing.T) {
	e := newRTTEstimator()
	if e.RTO() != initialRTO {
		t.Fatalf("initial RTO = %v, want %v", e.RTO(), initialRTO)
	}
}

func TestRTTLeavesRoomForFeedbackScheduling(t *testing.T) {
	e := newRTTEstimator()
	for i := 0; i < 100; i++ {
		e.sample(80 * time.Millisecond)
	}
	if got := e.RTO(); got < 80*time.Millisecond+maintenanceTick {
		t.Fatalf("RTO=%v leaves no ACK scheduling margin", got)
	}
}

func TestRecoveryRTOMarginAndBounds(t *testing.T) {
	for _, c := range []struct{ srtt, estimate, want time.Duration }{
		{0, initialRTO, initialRTO},
		{10 * time.Millisecond, minRTO, minRTO},
		{80 * time.Millisecond, 130 * time.Millisecond, 210 * time.Millisecond},
		{80 * time.Millisecond, 500 * time.Millisecond, 500 * time.Millisecond},
		{2 * time.Second, maxRTO, maxRTO},
	} {
		if got := recoveryRTO(c.srtt, c.estimate); got != c.want {
			t.Fatalf("recoveryRTO(%v,%v)=%v want=%v", c.srtt, c.estimate, got, c.want)
		}
	}
}

func TestRTTFirstSample(t *testing.T) {
	e := newRTTEstimator()
	e.sample(40 * time.Millisecond)
	srtt, rttvar, rto := e.snapshot()
	if srtt != 40*time.Millisecond {
		t.Fatalf("srtt = %v, want 40ms", srtt)
	}
	if rttvar != 20*time.Millisecond {
		t.Fatalf("rttvar = %v, want 20ms", rttvar)
	}
	// RTO = srtt + 4*rttvar = 40 + 80 = 120ms.
	if rto != 120*time.Millisecond {
		t.Fatalf("rto = %v, want 120ms", rto)
	}
}

func TestRTTConvergesToStableRTT(t *testing.T) {
	e := newRTTEstimator()
	for i := 0; i < 100; i++ {
		e.sample(10 * time.Millisecond)
	}
	srtt, rttvar, rto := e.snapshot()
	if srtt < 9*time.Millisecond || srtt > 11*time.Millisecond {
		t.Fatalf("srtt did not converge near 10ms: %v", srtt)
	}
	if rttvar > 2*time.Millisecond {
		t.Fatalf("rttvar should shrink for stable RTT, got %v", rttvar)
	}
	// With tiny variance, RTO is floored by the clock-granularity term but still
	// clamped to at least minRTO.
	if rto < minRTO {
		t.Fatalf("rto below floor: %v", rto)
	}
}

func TestRTTClampLow(t *testing.T) {
	e := newRTTEstimator()
	for i := 0; i < 50; i++ {
		e.sample(100 * time.Microsecond) // sub-ms (loopback)
	}
	if got := e.RTO(); got < minRTO {
		t.Fatalf("RTO %v below floor %v", got, minRTO)
	}
}

func TestRTTClampHigh(t *testing.T) {
	e := newRTTEstimator()
	e.sample(10 * time.Second) // absurdly long
	if got := e.RTO(); got > maxRTO {
		t.Fatalf("RTO %v above ceiling %v", got, maxRTO)
	}
}

func TestRTTIgnoresNonPositive(t *testing.T) {
	e := newRTTEstimator()
	e.sample(0)
	e.sample(-5 * time.Millisecond)
	if e.RTO() != initialRTO {
		t.Fatalf("non-positive samples should be ignored")
	}
}

func TestBackoffRTO(t *testing.T) {
	base := 100 * time.Millisecond
	cases := map[int]time.Duration{
		1: 100 * time.Millisecond,  // shift 0
		2: 200 * time.Millisecond,  // shift 1
		3: 400 * time.Millisecond,  // shift 2
		5: 1600 * time.Millisecond, // shift 4
		9: 1600 * time.Millisecond, // capped at shift 4
	}
	for tries, want := range cases {
		if got := backoffRTO(base, tries); got != want {
			t.Errorf("backoffRTO(base, %d) = %v, want %v", tries, got, want)
		}
	}
}
