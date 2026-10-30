package datawire

import (
	"context"
	"io"
	"sync/atomic"
	"testing"
	"time"
)

type fakeWire struct {
	amount   atomic.Uint64
	writes   atomic.Int64
	callback func()
}

func (w *fakeWire) Read([]byte) (int, error)             { return 0, io.EOF }
func (w *fakeWire) Write(b []byte) (int, error)          { w.writes.Add(1); return len(b), nil }
func (w *fakeWire) Close() error                         { return nil }
func (w *fakeWire) BufferedAmount() uint64               { return w.amount.Load() }
func (w *fakeWire) SetBufferedAmountLowThreshold(uint64) {}
func (w *fakeWire) OnBufferedAmountLow(f func())         { w.callback = f }

func TestBackpressureDrainAndClose(t *testing.T) {
	for _, abort := range []bool{false, true} {
		c := New()
		w := &fakeWire{}
		w.amount.Store(highWater + 1)
		c.Attach(w)
		done := make(chan error, 1)
		go func() { done <- c.WritePayload([]byte("packet"), false) }()
		select {
		case err := <-done:
			t.Fatalf("write bypassed backpressure: %v", err)
		case <-time.After(20 * time.Millisecond):
		}
		if abort {
			c.Close()
		} else {
			w.amount.Store(0)
			w.callback()
		}
		select {
		case err := <-done:
			if (err != nil) != abort {
				t.Fatalf("abort=%v err=%v", abort, err)
			}
		case <-time.After(time.Second):
			t.Fatal("blocked writer leaked")
		}
		c.Close()
	}
}
func TestSetupCancellationAndMessageLimit(t *testing.T) {
	c := New()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if c.Wait(ctx) != context.Canceled {
		t.Fatal("setup did not cancel")
	}
	c.Close()
	if _, _, err := c.ReadPayload(); err == nil {
		t.Fatal("read after close")
	}
	if err := c.WritePayload(make([]byte, MaxMessage+1), false); err == nil {
		t.Fatal("oversized message accepted")
	}
}
