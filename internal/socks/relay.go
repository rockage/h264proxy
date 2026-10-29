package socks

import (
	"context"
	"io"
	"net"
	"sync"
	"time"

	"github.com/roca/h264-tunnel-lab/internal/mux"
)

type activeReader struct {
	io.Reader
	touch func()
}

func (r activeReader) Read(b []byte) (int, error) {
	n, err := r.Reader.Read(b)
	if n > 0 {
		r.touch()
	}
	return n, err
}

// Relay preserves TCP FIN in each direction. A request can finish writing and
// still receive a response until the target sends EOF. Errors/cancellation
// reset only this stream, never the shared transport.
func relay(ctx context.Context, conn net.Conn, st *mux.Stream, idle time.Duration) error {
	defer conn.Close()
	var once sync.Once
	stop := func() { once.Do(func() { conn.Close(); st.Reset() }) }
	cancelHook := context.AfterFunc(ctx, stop)
	defer cancelHook()
	touch := func() {}
	if idle > 0 {
		timer := time.AfterFunc(idle, stop)
		defer timer.Stop()
		touch = func() { timer.Reset(idle) }
	}
	done := make(chan error, 2)
	go func() {
		_, err := io.Copy(st, activeReader{conn, touch})
		if err == nil {
			err = st.CloseWrite()
		}
		if err != nil {
			stop()
		}
		done <- err
	}()
	go func() {
		_, err := io.Copy(conn, activeReader{st, touch})
		if err == nil {
			if cw, ok := conn.(interface{ CloseWrite() error }); ok {
				err = cw.CloseWrite()
			} else {
				err = conn.Close()
			}
		}
		if err != nil {
			stop()
		}
		done <- err
	}()
	first, second := <-done, <-done
	if first != nil {
		return first
	}
	return second
}
