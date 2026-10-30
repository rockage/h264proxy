// Package datawire adapts a detached reliable, ordered DataChannel to mux packets.
package datawire

import (
	"context"
	"errors"
	"io"
	"sync"
)

const MaxMessage = 32 * 1024
const highWater = 256 * 1024
const lowWater = 64 * 1024

// Wire is implemented by Pion's detached DataChannel. Reads preserve messages.
type Wire interface {
	io.ReadWriteCloser
	BufferedAmount() uint64
	SetBufferedAmountLowThreshold(uint64)
	OnBufferedAmountLow(func())
}

type Channel struct {
	mu     sync.Mutex
	wire   Wire
	ready  chan struct{}
	closed chan struct{}
	low    chan struct{}
	once   sync.Once
}

func New() *Channel {
	return &Channel{ready: make(chan struct{}), closed: make(chan struct{}), low: make(chan struct{}, 1)}
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
	w.SetBufferedAmountLowThreshold(lowWater)
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
	for w.BufferedAmount() > highWater {
		select {
		case <-c.closed:
			return io.ErrClosedPipe
		case <-c.low:
		}
	}
	n, err := w.Write(b)
	if err == nil && n != len(b) {
		return io.ErrShortWrite
	}
	return err
}
func (c *Channel) ReadPayload() ([]byte, bool, error) {
	w, err := c.get()
	if err != nil {
		return nil, false, err
	}
	b := make([]byte, MaxMessage)
	n, err := w.Read(b)
	return b[:n], false, err
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
