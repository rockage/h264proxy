package cryptox

import (
	"bytes"
	"os"
	"testing"

	"github.com/roca/h264-tunnel-lab/internal/protocol"
)

func testKey(t *testing.T) []byte {
	t.Helper()
	k, err := GenerateKey()
	if err != nil {
		t.Fatalf("genkey: %v", err)
	}
	if len(k) != KeySize {
		t.Fatalf("key size %d", len(k))
	}
	return k
}

func TestAEADRoundTrip(t *testing.T) {
	c, err := NewAEADCipher(testKey(t))
	if err != nil {
		t.Fatalf("new aead: %v", err)
	}
	prefix := [4]byte{9, 8, 7, 6}
	aad := []byte("header-bytes")
	pt := []byte("secret payload \x00\x01\x02 世界")
	ct, err := c.Seal(prefix, 1, aad, pt)
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if bytes.Equal(ct, pt) {
		t.Fatal("ciphertext equals plaintext")
	}
	if len(ct) != len(pt)+c.Overhead() {
		t.Fatalf("overhead wrong: %d vs %d", len(ct), len(pt)+c.Overhead())
	}
	got, err := c.Open(prefix, 1, aad, ct)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if !bytes.Equal(got, pt) {
		t.Fatalf("roundtrip mismatch")
	}
}

func TestAEADWrongKeyFails(t *testing.T) {
	c1, _ := NewAEADCipher(testKey(t))
	c2, _ := NewAEADCipher(testKey(t))
	prefix := [4]byte{1, 1, 1, 1}
	aad := []byte("aad")
	ct, _ := c1.Seal(prefix, 5, aad, []byte("data"))
	if _, err := c2.Open(prefix, 5, aad, ct); err == nil {
		t.Fatal("expected auth failure with wrong key")
	}
}

func TestAEADTamperFails(t *testing.T) {
	c, _ := NewAEADCipher(testKey(t))
	prefix := [4]byte{2, 2, 2, 2}
	aad := []byte("aad")
	ct, _ := c.Seal(prefix, 3, aad, []byte("the quick brown fox"))
	ct[0] ^= 0xFF // flip a ciphertext bit
	if _, err := c.Open(prefix, 3, aad, ct); err == nil {
		t.Fatal("expected auth failure after tampering ciphertext")
	}
}

func TestAEADTamperAADFails(t *testing.T) {
	c, _ := NewAEADCipher(testKey(t))
	prefix := [4]byte{3, 3, 3, 3}
	ct, _ := c.Seal(prefix, 4, []byte("good-aad"), []byte("payload"))
	if _, err := c.Open(prefix, 4, []byte("bad!-aad"), ct); err == nil {
		t.Fatal("expected auth failure with altered AAD")
	}
}

func TestAEADWrongNonceFails(t *testing.T) {
	c, _ := NewAEADCipher(testKey(t))
	prefix := [4]byte{4, 4, 4, 4}
	aad := []byte("aad")
	ct, _ := c.Seal(prefix, 10, aad, []byte("payload"))
	if _, err := c.Open(prefix, 11, aad, ct); err == nil {
		t.Fatal("expected failure decrypting with wrong nonce counter")
	}
}

func TestLoadKeyWrongSize(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/short.key"
	if err := os.WriteFile(path, make([]byte, 16), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadKey(path); err == nil {
		t.Fatal("expected error for 16-byte key")
	}
}

func TestSealerMonotonicNonce(t *testing.T) {
	s, err := NewSealer(NullCipher{})
	if err != nil {
		t.Fatalf("new sealer: %v", err)
	}
	seen := map[uint64]bool{}
	for i := 0; i < 1000; i++ {
		n, err := s.Next()
		if err != nil {
			t.Fatalf("next: %v", err)
		}
		if seen[n] {
			t.Fatalf("nonce counter %d reused", n)
		}
		seen[n] = true
	}
}

// TestSealerNonceReuseGuard verifies the guard rejects a counter that is not
// strictly greater than the last one issued. We simulate this by constructing a
// sealer whose internal counter has been forced backwards.
func TestSealerNonceReuseGuard(t *testing.T) {
	s, _ := NewSealer(NullCipher{})
	if _, err := s.Next(); err != nil { // issues 0, last=0, counter=1
		t.Fatal(err)
	}
	// Force the next counter back to a used value.
	s.counter = 0
	if _, err := s.Next(); err != ErrNonceReuse {
		t.Fatalf("want ErrNonceReuse, got %v", err)
	}
}

func TestKeyFingerprintStable(t *testing.T) {
	k := make([]byte, KeySize)
	if KeyFingerprint(k) != KeyFingerprint(k) {
		t.Fatal("fingerprint not stable")
	}
	if len(KeyFingerprint(k)) != 8 {
		t.Fatal("fingerprint length")
	}
}

func TestCipherForMode(t *testing.T) {
	if _, err := CipherForMode(ModeRaw, nil); err != nil {
		t.Fatalf("raw: %v", err)
	}
	if _, err := CipherForMode(ModeAEAD, testKey(t)); err != nil {
		t.Fatalf("aead: %v", err)
	}
	if _, err := CipherForMode("bogus", nil); err == nil {
		t.Fatal("expected error for unknown mode")
	}
}

var _ protocol.Cipher = (*AEADCipher)(nil)
var _ protocol.Cipher = NullCipher{}
