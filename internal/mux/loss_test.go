package mux

import (
	"crypto/sha256"
	"io"
	"sync"
	"testing"
	"time"

	cryptox "github.com/roca/h264-tunnel-lab/internal/crypto"
	"github.com/roca/h264-tunnel-lab/internal/logging"
	"github.com/roca/h264-tunnel-lab/internal/netsim"
)

// newLossyPair connects two sessions whose send paths drop a fraction p of
// frames in each direction, returning the two lossy sinks for drop stats.
func newLossyPair(t *testing.T, p float64) (l, d *Session, lossL, lossD *netsim.LossySink, closeFn func()) {
	t.Helper()
	master, _ := cryptox.GenerateKey()
	l2d := newPipe()
	d2l := newPipe()
	lossL = netsim.NewLossySink(l2d, p, 1) // listener -> dialer
	lossD = netsim.NewLossySink(d2l, p, 2) // dialer -> listener
	log := logging.NewTo("test", io.Discard)

	var err error
	l, err = New(Config{SessionID: 0x55, Role: RoleListener, Mode: cryptox.ModeAEAD, Master: master, Sink: lossL, Source: d2l, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	d, err = New(Config{SessionID: 0x55, Role: RoleDialer, Mode: cryptox.ModeAEAD, Master: master, Sink: lossD, Source: l2d, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	closeFn = func() { l.Close(); d.Close(); l2d.close(); d2l.close() }
	return
}

// TestMuxReliableUnderLoss transfers data in both directions over a channel
// that drops ~20% of frames, and verifies full byte-exact recovery. It asserts
// that retransmission actually happened and that frames were really dropped.
func TestMuxReliableUnderLoss(t *testing.T) {
	l, d, lossL, lossD, done := newLossyPair(t, 0.20)
	defer done()

	const nEach = 3
	const size = 200 * 1024
	dialerBlobs := make([][]byte, nEach)
	listenerBlobs := make([][]byte, nEach)
	for i := 0; i < nEach; i++ {
		dialerBlobs[i] = randBlob(int64(300+i), size)
		listenerBlobs[i] = randBlob(int64(400+i), size)
	}

	var wg sync.WaitGroup
	recv := func(sess *Session, want [][]byte, label string) {
		defer wg.Done()
		wantSet := map[[32]byte]bool{}
		for _, w := range want {
			wantSet[sha256.Sum256(w)] = true
		}
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
				b, err := io.ReadAll(st)
				if err != nil {
					t.Errorf("%s read: %v", label, err)
					return
				}
				if !wantSet[sha256.Sum256(b)] {
					t.Errorf("%s recovered blob matches nothing expected", label)
				}
			}(st)
		}
		inner.Wait()
	}

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
				if _, err := st.Write(blob); err != nil {
					t.Errorf("write: %v", err)
				}
				_ = st.Close()
			}(st, blobs[i])
		}
		sw.Wait()
	}

	wg.Add(2)
	go recv(l, dialerBlobs, "listener-recv")
	go recv(d, listenerBlobs, "dialer-recv")
	go send(d, dialerBlobs)
	go send(l, listenerBlobs)

	waitTimeout(t, &wg, 60*time.Second)

	ls, ds := l.Stats(), d.Stats()
	if lossL.Dropped() == 0 || lossD.Dropped() == 0 {
		t.Fatalf("expected frames to be dropped: l=%d d=%d", lossL.Dropped(), lossD.Dropped())
	}
	if ls.Retransmits == 0 || ds.Retransmits == 0 {
		t.Fatalf("expected retransmissions on both sides: l=%d d=%d", ls.Retransmits, ds.Retransmits)
	}
	t.Logf("dropped: l->d=%d d->l=%d", lossL.Dropped(), lossD.Dropped())
	t.Logf("retransmits: listener=%d dialer=%d", ls.Retransmits, ds.Retransmits)
}
