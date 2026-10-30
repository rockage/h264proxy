package mux

import (
	"bytes"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	cryptox "github.com/roca/h264-tunnel-lab/internal/crypto"
	"github.com/roca/h264-tunnel-lab/internal/logging"
)

type timedFrame struct {
	data     []byte
	delivery time.Time
}
type latencyPipe struct {
	frames chan timedFrame
	closed chan struct{}
	once   sync.Once
	delay  time.Duration
}

func newLatencyPipe(delay time.Duration) *latencyPipe {
	return &latencyPipe{frames: make(chan timedFrame, 8192), closed: make(chan struct{}), delay: delay}
}
func (p *latencyPipe) WritePayload(b []byte, _ bool) error {
	f := timedFrame{append([]byte(nil), b...), time.Now().Add(p.delay)}
	select {
	case p.frames <- f:
		return nil
	case <-p.closed:
		return io.ErrClosedPipe
	}
}
func (p *latencyPipe) ReadPayload() ([]byte, bool, error) {
	select {
	case f := <-p.frames:
		if delay := time.Until(f.delivery); delay > 0 {
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-timer.C:
			case <-p.closed:
				return nil, false, io.EOF
			}
		}
		return f.data, false, nil
	case <-p.closed:
		return nil, false, io.EOF
	}
}
func (p *latencyPipe) close() { p.once.Do(func() { close(p.closed) }) }

// This isolates mux flow control under simulated RTT and unlimited link rate.
// It is not a WebRTC, CPU-throughput or WAN speed benchmark.
func BenchmarkReliableWindowLatency(b *testing.B) {
	for _, window := range []int64{InitialWindow, 1 << 20} {
		b.Run(fmt.Sprintf("RTT80ms/window%dKiB", window/1024), func(b *testing.B) {
			ab, ba := newLatencyPipe(40*time.Millisecond), newLatencyPipe(40*time.Millisecond)
			key, _ := cryptox.GenerateKey()
			log := logging.NewTo("window-bench", io.Discard)
			l, e := New(Config{SessionID: 456, Role: RoleListener, Mode: cryptox.ModeAEAD, Master: key, ReliableTransport: true, ReceiveWindow: window, Sink: ab, Source: ba, Log: log})
			if e != nil {
				b.Fatal(e)
			}
			r, e := New(Config{SessionID: 456, Role: RoleDialer, Mode: cryptox.ModeAEAD, Master: key, ReliableTransport: true, Sink: ba, Source: ab, Log: log})
			if e != nil {
				b.Fatal(e)
			}
			defer func() { l.Close(); r.Close(); ab.close(); ba.close() }()
			payload := bytes.Repeat([]byte{0x56}, 2<<20)
			b.SetBytes(int64(len(payload)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				st, e := r.OpenStream()
				if e != nil {
					b.Fatal(e)
				}
				done := make(chan error, 1)
				go func() {
					_, e := st.Write(payload)
					if e == nil {
						e = st.CloseWrite()
					}
					done <- e
				}()
				remote, e := l.Accept()
				if e != nil {
					b.Fatal(e)
				}
				got, e := io.ReadAll(remote)
				if e != nil || !bytes.Equal(got, payload) {
					b.Fatal("payload mismatch", e)
				}
				if e = <-done; e != nil {
					b.Fatal(e)
				}
				if e = remote.CloseWrite(); e != nil {
					b.Fatal(e)
				}
				if _, e = io.Copy(io.Discard, st); e != nil {
					b.Fatal(e)
				}
			}
			b.StopTimer()
			s := r.Stats()
			b.ReportMetric(float64(s.CreditWaitNanos)/float64(time.Millisecond)/float64(b.N), "credit-wait-ms/op")
		})
	}
}
