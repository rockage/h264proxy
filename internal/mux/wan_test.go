package mux

import (
	"bytes"
	"io"
	"sync"
	"testing"
	"time"

	cryptox "github.com/roca/h264-tunnel-lab/internal/crypto"
	"github.com/roca/h264-tunnel-lab/internal/logging"
	"github.com/roca/h264-tunnel-lab/internal/muxproto"
)

type timedPacket struct {
	at   time.Time
	data []byte
	late bool
}

type lockedLogBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *lockedLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}
func (b *lockedLogBuffer) String() string { b.mu.Lock(); defer b.mu.Unlock(); return b.b.String() }

// A pipelined link: delay is per path, not a sleep serializing every packet.
type wanLink struct {
	mu         sync.Mutex
	queue      chan timedPacket
	out        chan []byte
	done       chan struct{}
	next       time.Time
	delay      time.Duration
	rate       int
	jitter     bool
	dataFrames int
}

func newWANLink(delay time.Duration, rate int, jitter ...bool) *wanLink {
	l := &wanLink{queue: make(chan timedPacket, 8192), out: make(chan []byte, 8192), done: make(chan struct{}), delay: delay, rate: rate}
	if len(jitter) > 0 {
		l.jitter = jitter[0]
	}
	go func() {
		for {
			select {
			case <-l.done:
				return
			case p := <-l.queue:
				if p.late {
					go func(p timedPacket) { l.deliver(p) }(p)
					continue
				}
				if !l.deliver(p) {
					return
				}
			}
		}
	}()
	return l
}
func (l *wanLink) deliver(p timedPacket) bool {
	if wait := time.Until(p.at); wait > 0 {
		timer := time.NewTimer(wait)
		select {
		case <-timer.C:
		case <-l.done:
			timer.Stop()
			return false
		}
	}
	select {
	case l.out <- p.data:
		return true
	case <-l.done:
		return false
	}
}
func (l *wanLink) WritePayload(b []byte, _ bool) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if l.next.Before(now) {
		l.next = now
	}
	l.next = l.next.Add(time.Duration(int64(len(b)) * int64(time.Second) / int64(l.rate)))
	p := timedPacket{at: l.next.Add(l.delay), data: bytes.Clone(b)}
	if l.jitter && len(b) > 5 && b[5] == byte(muxproto.FrameData) {
		l.dataFrames++
		if l.dataFrames%29 == 0 {
			p.late = true
			p.at = p.at.Add(20 * time.Millisecond)
		}
	}
	select {
	case l.queue <- p:
		return nil
	case <-l.done:
		return io.ErrClosedPipe
	}
}
func (l *wanLink) ReadPayload() ([]byte, bool, error) {
	select {
	case b := <-l.out:
		return b, false, nil
	case <-l.done:
		return nil, false, io.EOF
	}
}

func TestLossFreeWANDoesNotManufactureLoss(t *testing.T) {
	for _, path := range []struct {
		name    string
		delay   time.Duration
		rate    int
		reorder bool
	}{{"20ms-fast", 10 * time.Millisecond, 16_000_000, false}, {"80ms-fast", 40 * time.Millisecond, 16_000_000, false}, {"80ms-900KBps", 40 * time.Millisecond, 900_000, false}, {"80ms-reorder20ms", 40 * time.Millisecond, 900_000, true}} {
		t.Run(path.name, func(t *testing.T) {
			a, b := newWANLink(path.delay, path.rate, path.reorder), newWANLink(path.delay, path.rate)
			key, _ := cryptox.GenerateKey()
			var logs lockedLogBuffer
			log := logging.NewTo("wan-test", &logs)
			l, err := New(Config{SessionID: 987, Role: RoleListener, Mode: cryptox.ModeAEAD, Master: key, Sink: a, Source: b, Log: log, RateBytesPerSec: DefaultRateBytesPerSec})
			if err != nil {
				t.Fatal(err)
			}
			d, err := New(Config{SessionID: 987, Role: RoleDialer, Mode: cryptox.ModeAEAD, Master: key, Sink: b, Source: a, Log: log, RateBytesPerSec: DefaultRateBytesPerSec})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { l.Close(); d.Close(); close(a.done); close(b.done) }()
			st, err := l.OpenStream()
			if err != nil {
				t.Fatal(err)
			}
			remote, err := d.Accept()
			if err != nil {
				t.Fatal(err)
			}
			blob := randBlob(991, 1024*1024)
			sent := make(chan error, 1)
			received := make(chan error, 1)
			go func() {
				_, err := st.Write(blob)
				if err == nil {
					err = st.CloseWrite()
				}
				sent <- err
			}()
			start := time.Now()
			go func() {
				got, err := io.ReadAll(remote)
				if err == nil && !bytes.Equal(got, blob) {
					err = io.ErrUnexpectedEOF
				}
				received <- err
			}()
			select {
			case err := <-received:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(12 * time.Second):
				t.Fatal("loss-free WAN stalled")
			}
			if err := <-sent; err != nil {
				t.Fatal(err)
			}
			stats := l.Stats()
			t.Logf("duration=%v stats=%+v", time.Since(start), stats)
			if stats.TimeoutRetransmits != 0 || stats.FastRetransmits != 0 {
				t.Log(logs.String())
				t.Fatalf("ordered loss-free path manufactured loss: %+v", stats)
			}
		})
	}
}
