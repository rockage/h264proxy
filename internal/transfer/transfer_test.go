package transfer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"testing"

	cryptox "github.com/roca/h264-tunnel-lab/internal/crypto"
	"github.com/roca/h264-tunnel-lab/internal/logging"
	"github.com/roca/h264-tunnel-lab/internal/metrics"
	"github.com/roca/h264-tunnel-lab/internal/protocol"
)

// capturingSink records every frame payload written, in order.
type capturingSink struct {
	frames  [][]byte
	markers []bool
}

func (c *capturingSink) WritePayload(p []byte, marker bool) error {
	cp := make([]byte, len(p))
	copy(cp, p)
	c.frames = append(c.frames, cp)
	c.markers = append(c.markers, marker)
	return nil
}

// sliceSource yields pre-captured frames in a given order.
type sliceSource struct {
	frames [][]byte
	i      int
}

func (s *sliceSource) ReadPayload() ([]byte, bool, error) {
	if s.i >= len(s.frames) {
		return nil, false, io.EOF
	}
	f := s.frames[s.i]
	s.i++
	return f, false, nil
}

func nopLogger() *logging.Logger { return logging.NewTo("test", io.Discard) }

func makeFile(t *testing.T, size int) (string, [32]byte) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "in.bin")
	data := make([]byte, size)
	rng := rand.New(rand.NewSource(int64(size) + 1))
	rng.Read(data)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path, sha256.Sum256(data)
}

// capture runs a Send over an AEAD sealer and returns the captured frames plus
// the key used, so tests can replay them through a receiver.
func capture(t *testing.T, inPath string, chunk int) ([][]byte, []byte) {
	t.Helper()
	key, _ := cryptox.GenerateKey()
	c, err := cryptox.NewAEADCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	sealer, _ := cryptox.NewSealer(c)
	sink := &capturingSink{}
	rep := &metrics.Report{}
	err = Send(context.Background(), nopLogger(), sink, sealer,
		SendOptions{SessionID: 1, InPath: inPath, ChunkSize: chunk}, rep)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if !rep.SendComplete {
		t.Fatal("send not complete")
	}
	return sink.frames, key
}

func receive(t *testing.T, frames [][]byte, key []byte) (*metrics.Report, string, error) {
	t.Helper()
	c, _ := cryptox.NewAEADCipher(key)
	outDir := t.TempDir()
	out := filepath.Join(outDir, "out.bin")
	rep := &metrics.Report{}
	err := Receive(context.Background(), nopLogger(), &sliceSource{frames: frames}, c,
		ReceiveOptions{OutPath: out, TmpDir: outDir}, rep)
	return rep, out, err
}

func TestHappyPathReassembly(t *testing.T) {
	in, want := makeFile(t, 10_000)
	frames, key := capture(t, in, 900)
	rep, out, err := receive(t, frames, key)
	if err != nil {
		t.Fatalf("receive: %v", err)
	}
	if !rep.SHA256Match || !rep.ReceiveComplete {
		t.Fatalf("not complete: %+v", rep)
	}
	got, _ := os.ReadFile(out)
	if sha256.Sum256(got) != want {
		t.Fatal("output hash mismatch")
	}
	if rep.SHA256Expected != hex.EncodeToString(want[:]) {
		t.Fatal("expected hash not recorded")
	}
}

func TestReorderTolerated(t *testing.T) {
	in, _ := makeFile(t, 20_000)
	frames, key := capture(t, in, 900)
	// Shuffle DATA frames (indices 1..len-2); keep START first and END last.
	data := frames[1 : len(frames)-1]
	rng := rand.New(rand.NewSource(99))
	rng.Shuffle(len(data), func(i, j int) { data[i], data[j] = data[j], data[i] })
	rep, _, err := receive(t, frames, key)
	if err != nil {
		t.Fatalf("receive: %v", err)
	}
	if !rep.SHA256Match {
		t.Fatal("hash mismatch after reorder")
	}
	if rep.ReorderedChunks == 0 {
		t.Fatal("expected reordered chunks to be counted")
	}
}

func TestDuplicateDetected(t *testing.T) {
	in, _ := makeFile(t, 8_000)
	frames, key := capture(t, in, 900)
	// Duplicate the 2nd and 3rd DATA frames.
	dup := [][]byte{}
	dup = append(dup, frames[0])            // START
	dup = append(dup, frames[1], frames[1]) // DATA0 twice
	dup = append(dup, frames[2], frames[2]) // DATA1 twice
	dup = append(dup, frames[3:]...)        // rest + END
	rep, _, err := receive(t, dup, key)
	if err != nil {
		t.Fatalf("receive: %v", err)
	}
	if !rep.SHA256Match {
		t.Fatal("hash mismatch")
	}
	if rep.DuplicateChunks != 2 {
		t.Fatalf("want 2 duplicates, got %d", rep.DuplicateChunks)
	}
}

func TestMissingChunkFails(t *testing.T) {
	in, _ := makeFile(t, 9_000)
	frames, key := capture(t, in, 900)
	// Drop one DATA frame (index 2).
	dropped := append([][]byte{}, frames[:2]...)
	dropped = append(dropped, frames[3:]...)
	rep, out, err := receive(t, dropped, key)
	if err != ErrIncomplete {
		t.Fatalf("want ErrIncomplete, got %v", err)
	}
	if rep.MissingChunks == 0 {
		t.Fatal("expected missing chunk count > 0")
	}
	if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
		t.Fatal("output file must not exist on failed transfer")
	}
}

func TestWrongKeyFails(t *testing.T) {
	in, _ := makeFile(t, 5_000)
	frames, _ := capture(t, in, 900)
	wrongKey, _ := cryptox.GenerateKey()
	rep, out, err := receive(t, frames, wrongKey)
	if err != ErrIncomplete {
		t.Fatalf("want ErrIncomplete, got %v", err)
	}
	if rep.ReceiveComplete {
		t.Fatal("must not complete with wrong key")
	}
	if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
		t.Fatal("output file must not exist with wrong key")
	}
}

func TestTamperFails(t *testing.T) {
	in, _ := makeFile(t, 7_000)
	frames, key := capture(t, in, 900)
	// Flip a byte in the payload region of a DATA frame.
	victim := frames[2]
	victim[protocol.HeaderSize+1] ^= 0xFF
	rep, out, err := receive(t, frames, key)
	if err != ErrIncomplete {
		t.Fatalf("want ErrIncomplete, got %v", err)
	}
	if rep.MissingChunks == 0 {
		t.Fatal("tampered chunk should become missing")
	}
	if _, statErr := os.Stat(out); !os.IsNotExist(statErr) {
		t.Fatal("output file must not exist after tamper")
	}
}

// TestRawVectorsByteExact exercises the M2 raw (NullCipher) path with the
// required test vectors and checks byte-exact recovery through the framing
// layer (no network).
func TestRawVectorsByteExact(t *testing.T) {
	vectors := map[string][]byte{
		"hello":   []byte("Hello World"),
		"chinese": []byte("你好，世界！UTF-8 中文测试"),
		"zeros":   {0x00, 0x01, 0x00, 0x02, 0x00, 0x00, 0xFF, 0x00},
		"random":  randomBytes(4096),
		"1kb":     randomBytes(1024),
	}
	for name, payload := range vectors {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			in := filepath.Join(dir, "in")
			if err := os.WriteFile(in, payload, 0o644); err != nil {
				t.Fatal(err)
			}
			sealer, _ := cryptox.NewSealer(cryptox.NullCipher{})
			sink := &capturingSink{}
			if err := Send(context.Background(), nopLogger(), sink, sealer,
				SendOptions{SessionID: 7, InPath: in, ChunkSize: 900}, &metrics.Report{}); err != nil {
				t.Fatal(err)
			}
			out := filepath.Join(dir, "out")
			rep := &metrics.Report{}
			if err := Receive(context.Background(), nopLogger(), &sliceSource{frames: sink.frames}, cryptox.NullCipher{},
				ReceiveOptions{OutPath: out, TmpDir: dir}, rep); err != nil {
				t.Fatalf("receive: %v", err)
			}
			got, _ := os.ReadFile(out)
			if sha256.Sum256(got) != sha256.Sum256(payload) {
				t.Fatalf("byte mismatch for %s", name)
			}
		})
	}
}

func randomBytes(n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(int64(n))).Read(b)
	return b
}
