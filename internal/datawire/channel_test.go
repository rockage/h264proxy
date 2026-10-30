package datawire

import (
	"context"
	"fmt"
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

type payloadWire struct {
	fakeWire
	payload []byte
}

func (w *payloadWire) Read(dst []byte) (int, error) { return copy(dst, w.payload), nil }

func TestBufferedReadOwnershipAndBoundaries(t *testing.T) {
	c := New()
	w := &payloadWire{payload: make([]byte, MaxMessage)}
	for i := range w.payload {
		w.payload[i] = byte(i)
	}
	c.Attach(w)
	defer c.Close()
	if _, _, err := c.ReadPayloadInto(make([]byte, MaxMessage-1)); err != io.ErrShortBuffer {
		t.Fatalf("short buffer: %v", err)
	}
	owned, _, err := c.ReadPayload()
	if err != nil || len(owned) != MaxMessage {
		t.Fatalf("full message: %d %v", len(owned), err)
	}
	dst := make([]byte, MaxMessage*2)
	w.payload = []byte("next packet")
	n, _, err := c.ReadPayloadInto(dst)
	if err != nil || string(dst[:n]) != "next packet" {
		t.Fatalf("buffered message: %q %v", dst[:n], err)
	}
	for i, b := range owned {
		if b != byte(i) {
			t.Fatal("caller-owned prior packet was overwritten")
		}
	}
	if allocations := testing.AllocsPerRun(100, func() {
		if _, _, err := c.ReadPayloadInto(dst); err != nil {
			panic(err)
		}
	}); allocations != 0 {
		t.Fatalf("buffered reads allocate: %v", allocations)
	}
}

var benchmarkPayload []byte

func BenchmarkReadPayloadBuffers(b *testing.B) {
	for _, size := range []int{70, 16 * 1024} {
		for _, reuse := range []bool{false, true} {
			b.Run(fmt.Sprintf("size_%d/reuse_%v", size, reuse), func(b *testing.B) {
				c := New()
				c.Attach(&payloadWire{payload: make([]byte, size)})
				defer c.Close()
				dst := make([]byte, MaxMessage)
				b.ReportAllocs()
				b.SetBytes(int64(size))
				for b.Loop() {
					if reuse {
						n, _, err := c.ReadPayloadInto(dst)
						if err != nil {
							b.Fatal(err)
						}
						benchmarkPayload = dst[:n]
					} else {
						p, _, err := c.ReadPayload()
						if err != nil {
							b.Fatal(err)
						}
						benchmarkPayload = p
					}
				}
			})
		}
	}
}
