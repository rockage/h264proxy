// Package datawire adapts a detached reliable, ordered DataChannel to mux packets.
package datawire

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"time"
)

const MaxMessage = 32 * 1024
const highWater = 256 * 1024
const lowWater = 64 * 1024

type Config struct{ HighWater, LowWater uint64 }

func (cfg Config) Validate() error {
	if cfg.HighWater == 0 && cfg.LowWater == 0 {
		return nil
	}
	if cfg.HighWater < MaxMessage || cfg.HighWater > 8*1024*1024 || cfg.LowWater >= cfg.HighWater {
		return errors.New("datawire: require 32 KiB <= high water <= 8 MiB and low water < high water")
	}
	return nil
}

type Stats struct {
	HighWater, LowWater                                     uint64
	BackpressureWaits, BackpressureNanos, MaxBufferedAmount uint64
	BackpressureWaiters                                     int64
}

// Wire is implemented by Pion's detached DataChannel. Reads preserve messages.
type Wire interface {
	io.ReadWriteCloser
	BufferedAmount() uint64
	SetBufferedAmountLowThreshold(uint64)
	OnBufferedAmountLow(func())
}

type Channel struct {
	mu                            sync.Mutex
	wire                          Wire
	ready                         chan struct{}
	closed                        chan struct{}
	low                           chan struct{}
	once                          sync.Once
	cfg                           Config
	waits, waitNanos, maxBuffered atomic.Uint64
	waiters                       atomic.Int64
}

func New() *Channel {
	c, _ := NewWithConfig(Config{})
	return c
}
func NewWithConfig(cfg Config) (*Channel, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if cfg.HighWater == 0 {
		cfg = Config{highWater, lowWater}
	}
	return &Channel{ready: make(chan struct{}), closed: make(chan struct{}), low: make(chan struct{}, 1), cfg: cfg}, nil
}
func (c *Channel) Stats() Stats {
	return Stats{HighWater: c.cfg.HighWater, LowWater: c.cfg.LowWater, BackpressureWaits: c.waits.Load(), BackpressureNanos: c.waitNanos.Load(), MaxBufferedAmount: c.maxBuffered.Load(), BackpressureWaiters: c.waiters.Load()}
}
func (c *Channel) Attach(w Wire) {
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case <-c.closed:
		w.Close()
		return
	default:
	}
	if c.wire != nil {
		w.Close()
		return
	}
	w.SetBufferedAmountLowThreshold(c.cfg.LowWater)
	w.OnBufferedAmountLow(func() {
		select {
		case c.low <- struct{}{}:
		default:
		}
	})
	c.wire = w
	close(c.ready)
}
func (c *Channel) Wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.closed:
		return io.ErrClosedPipe
	case <-c.ready:
		return nil
	}
}
func (c *Channel) get() (Wire, error) {
	if err := c.Wait(context.Background()); err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.wire, nil
}

// WritePayload is called by the mux's sole writer; queue growth is bounded.
func (c *Channel) WritePayload(b []byte, _ bool) error {
	if len(b) > MaxMessage {
		return errors.New("datawire: message too large")
	}
	w, err := c.get()
	if err != nil {
		return err
	}
	if err = c.waitForCapacity(w); err != nil {
		return err
	}
	n, err := w.Write(b)
	if err == nil && n != len(b) {
		return io.ErrShortWrite
	}
	return err
}

func (c *Channel) waitForCapacity(w Wire) error {
	amount := w.BufferedAmount()
	for old := c.maxBuffered.Load(); amount > old && !c.maxBuffered.CompareAndSwap(old, amount); old = c.maxBuffered.Load() {
	}
	if amount > c.cfg.HighWater {
		started := time.Now()
		c.waits.Add(1)
		c.waiters.Add(1)
		defer func() { c.waitNanos.Add(uint64(time.Since(started))); c.waiters.Add(-1) }()
	}
	for w.BufferedAmount() > c.cfg.HighWater {
		select {
		case <-c.closed:
			return io.ErrClosedPipe
		case <-c.low:
		}
	}
	return nil
}
func (c *Channel) ReadPayload() ([]byte, bool, error) {
	b := make([]byte, MaxMessage)
	n, marker, err := c.ReadPayloadInto(b)
	return b[:n], marker, err
}

// ReadPayloadInto preserves message boundaries without allocating a buffer for
// every small control frame. Existing ReadPayload callers still own their data.
func (c *Channel) ReadPayloadInto(dst []byte) (int, bool, error) {
	if len(dst) < MaxMessage {
		return 0, false, io.ErrShortBuffer
	}
	w, err := c.get()
	if err != nil {
		return 0, false, err
	}
	n, err := w.Read(dst[:MaxMessage])
	return n, false, err
}
func (c *Channel) BufferedAmount() uint64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.wire == nil {
		return 0
	}
	return c.wire.BufferedAmount()
}
func (c *Channel) Close() error {
	c.once.Do(func() {
		close(c.closed)
		c.mu.Lock()
		w := c.wire
		c.mu.Unlock()
		if w != nil {
			w.Close()
		}
	})
	return nil
}
