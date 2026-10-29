package mux

import (
	"crypto/sha256"
	"io"
	"math/rand"
	"sync"
	"testing"
	"time"

	cryptox "github.com/roca/h264-tunnel-lab/internal/crypto"
	"github.com/roca/h264-tunnel-lab/internal/logging"
)

// pipe is a one-way in-memory byte-frame channel implementing both
// transfer.PacketSink (Write) and transfer.PacketSource (Read).
type pipe struct {
	ch     chan []byte
	closed chan struct{}
	once   sync.Once
}

func newPipe() *pipe { return &pipe{ch: make(chan []byte, 8192), closed: make(chan struct{})} }

func (p *pipe) WritePayload(b []byte, _ bool) error {
	cp := make([]byte, len(b))
	copy(cp, b)
	select {
	case p.ch <- cp:
		return nil
	case <-p.closed:
		return io.ErrClosedPipe
	}
}

func (p *pipe) ReadPayload() ([]byte, bool, error) {
	select {
	case b := <-p.ch:
		return b, false, nil
	case <-p.closed:
		// Drain anything already queued before reporting EOF.
		select {
		case b := <-p.ch:
			return b, false, nil
		default:
			return nil, false, io.EOF
		}
	}
}

func (p *pipe) close() { p.once.Do(func() { close(p.closed) }) }

func newPair(t *testing.T, mode string) (listener, dialer *Session, closeFn func()) {
	t.Helper()
	master, _ := cryptox.GenerateKey()
	l2d := newPipe() // listener -> dialer
	d2l := newPipe() // dialer -> listener
	log := logging.NewTo("test", io.Discard)

	var err error
	listener, err = New(Config{SessionID: 0x1234, Role: RoleListener, Mode: mode, Master: master, Sink: l2d, Source: d2l, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	dialer, err = New(Config{SessionID: 0x1234, Role: RoleDialer, Mode: mode, Master: master, Sink: d2l, Source: l2d, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	closeFn = func() {
		listener.Close()
		dialer.Close()
		l2d.close()
		d2l.close()
	}
	return
}

func randBlob(seed int64, n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(seed)).Read(b)
	return b
}

// sendBlob writes blob to stream then closes it (write side).
func sendBlob(t *testing.T, st *Stream, blob []byte) {
	t.Helper()
	if _, err := st.Write(blob); err != nil {
		t.Errorf("write stream %d: %v", st.ID(), err)
	}
	if err := st.Close(); err != nil {
		t.Errorf("close stream %d: %v", st.ID(), err)
	}
}

// recvBlob reads a stream to EOF and returns the bytes.
func recvBlob(st *Stream) ([]byte, error) {
	return io.ReadAll(st)
}

func TestMuxSingleStream(t *testing.T) {
	l, d, done := newPair(t, cryptox.ModeAEAD)
	defer done()

	blob := randBlob(1, 5000)
	go func() {
		st, err := d.OpenStream()
		if err != nil {
			t.Error(err)
			return
		}
		sendBlob(t, st, blob)
	}()

	st, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	got, err := recvBlob(st)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(got) != sha256.Sum256(blob) {
		t.Fatal("payload mismatch")
	}
}

// TestMuxConcurrentBidirectional opens multiple concurrent streams in BOTH
// directions, each carrying a payload larger than the flow-control window (so
// WINDOW refills are exercised), and verifies every stream byte-for-byte with
// no cross-talk.
func TestMuxConcurrentBidirectional(t *testing.T) {
	l, d, done := newPair(t, cryptox.ModeAEAD)
	defer done()

	const nEach = 4
	const size = 600 * 1024 // > InitialWindow (256 KiB) -> forces WINDOW updates

	// Expected blobs keyed by a label.
	dialerBlobs := make([][]byte, nEach)
	listenerBlobs := make([][]byte, nEach)
	for i := 0; i < nEach; i++ {
		dialerBlobs[i] = randBlob(int64(100+i), size)
		listenerBlobs[i] = randBlob(int64(200+i), size)
	}

	var wg sync.WaitGroup

	// Receivers: each side accepts nEach streams and verifies.
	recv := func(sess *Session, want [][]byte, label string) {
		defer wg.Done()
		got := make([][]byte, 0, nEach)
		var mu sync.Mutex
		var inner sync.WaitGroup
		for i := 0; i < nEach; i++ {
			st, err := sess.Accept()
			if err != nil {
				t.Errorf("%s accept: %v", label, err)
				return
			}
			inner.Add(1)
			go func(st *Stream) {
				defer inner.Done()
				b, err := recvBlob(st)
				if err != nil {
					t.Errorf("%s read stream %d: %v", label, st.ID(), err)
					return
				}
				mu.Lock()
				got = append(got, b)
				mu.Unlock()
			}(st)
		}
		inner.Wait()
		// Each received blob must match exactly one expected blob.
		wantSet := map[[32]byte]bool{}
		for _, w := range want {
			wantSet[sha256.Sum256(w)] = true
		}
		for _, g := range got {
			if !wantSet[sha256.Sum256(g)] {
				t.Errorf("%s got a blob matching no expected payload", label)
			}
		}
		if len(got) != nEach {
			t.Errorf("%s got %d streams, want %d", label, len(got), nEach)
		}
	}

	wg.Add(2)
	go recv(l, dialerBlobs, "listener-recv")
	go recv(d, listenerBlobs, "dialer-recv")

	// Senders: dialer -> listener and listener -> dialer, concurrently.
	send := func(sess *Session, blobs [][]byte) {
		var sw sync.WaitGroup
		for i := 0; i < nEach; i++ {
			st, err := sess.OpenStream()
			if err != nil {
				t.Errorf("open: %v", err)
				continue
			}
			sw.Add(1)
			go func(st *Stream, blob []byte) {
				defer sw.Done()
				sendBlob(t, st, blob)
			}(st, blobs[i])
		}
		sw.Wait()
	}
	go send(d, dialerBlobs)
	go send(l, listenerBlobs)

	waitTimeout(t, &wg, 30*time.Second)

	ls := l.Stats()
	ds := d.Stats()
	if ls.FramesReceived == 0 || ds.FramesReceived == 0 {
		t.Fatal("expected both directions to carry frames (full-duplex)")
	}
	t.Logf("listener stats: %+v", ls)
	t.Logf("dialer stats:   %+v", ds)
}

func TestMuxRawMode(t *testing.T) {
	l, d, done := newPair(t, cryptox.ModeRaw)
	defer done()
	blob := randBlob(7, 200*1024)
	go func() {
		st, _ := d.OpenStream()
		sendBlob(t, st, blob)
	}()
	st, err := l.Accept()
	if err != nil {
		t.Fatal(err)
	}
	got, _ := recvBlob(st)
	if sha256.Sum256(got) != sha256.Sum256(blob) {
		t.Fatal("raw mode payload mismatch")
	}
}

func waitTimeout(t *testing.T, wg *sync.WaitGroup, d time.Duration) {
	t.Helper()
	ch := make(chan struct{})
	go func() { wg.Wait(); close(ch) }()
	select {
	case <-ch:
	case <-time.After(d):
		t.Fatal("timed out waiting for transfers")
	}
}
