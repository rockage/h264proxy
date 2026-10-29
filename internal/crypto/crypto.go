// Package cryptox implements the AEAD payload protection for the tunnel and a
// null (pass-through) cipher used for the pre-encryption raw transport
// experiments. It also handles loading the 32-byte key from a file and
// building per-packet nonces.
//
// Nonce construction (AES-256-GCM, 12-byte nonce):
//
//	nonce = NoncePrefix[4] || NonceCounter[8]
//
// NoncePrefix is random per session; NonceCounter is strictly monotonic within
// a session. A Sealer refuses to reuse a counter value, which prevents nonce
// reuse under a fixed key within a session.
package cryptox

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/roca/h264-tunnel-lab/internal/protocol"
)

// KeySize is the required key length for AES-256.
const KeySize = 32

// NonceSize is the AES-GCM nonce length in bytes.
const NonceSize = 12

// gcmTagSize is the AES-GCM authentication tag length.
const gcmTagSize = 16

// Errors returned by this package.
var (
	ErrKeySize        = fmt.Errorf("key must be exactly %d bytes", KeySize)
	ErrNonceReuse     = errors.New("cryptox: nonce counter reuse detected")
	ErrNonceExhausted = errors.New("cryptox: nonce counter space exhausted")
)

// GenerateKey returns a fresh random 32-byte key.
func GenerateKey() ([]byte, error) {
	k := make([]byte, KeySize)
	if _, err := rand.Read(k); err != nil {
		return nil, err
	}
	return k, nil
}

// LoadKey reads a 32-byte key from path and validates its length.
func LoadKey(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(b) != KeySize {
		return nil, fmt.Errorf("%w: got %d bytes from %s", ErrKeySize, len(b), path)
	}
	return b, nil
}

// KeyFingerprint returns a short, non-reversible identifier for a key, safe to
// log. It is the first 8 hex chars of SHA-256(key).
func KeyFingerprint(key []byte) string {
	sum := sha256.Sum256(key)
	return hex.EncodeToString(sum[:])[:8]
}

// RandomNoncePrefix returns a fresh random 4-byte nonce prefix for a session.
func RandomNoncePrefix() ([4]byte, error) {
	var p [4]byte
	_, err := rand.Read(p[:])
	return p, err
}

func buildNonce(prefix [4]byte, counter uint64) []byte {
	n := make([]byte, NonceSize)
	copy(n[0:4], prefix[:])
	binary.BigEndian.PutUint64(n[4:12], counter)
	return n
}

// AEADCipher implements protocol.Cipher using AES-256-GCM.
type AEADCipher struct {
	aead cipher.AEAD
}

// NewAEADCipher builds an AES-256-GCM cipher from a 32-byte key.
func NewAEADCipher(key []byte) (*AEADCipher, error) {
	if len(key) != KeySize {
		return nil, ErrKeySize
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &AEADCipher{aead: aead}, nil
}

// Overhead returns the GCM tag size.
func (c *AEADCipher) Overhead() int { return gcmTagSize }

// Seal encrypts plaintext with AAD and the nonce built from prefix||counter.
func (c *AEADCipher) Seal(prefix [4]byte, counter uint64, aad, plaintext []byte) ([]byte, error) {
	nonce := buildNonce(prefix, counter)
	return c.aead.Seal(nil, nonce, plaintext, aad), nil
}

// Open decrypts payload with AAD and the nonce built from prefix||counter.
func (c *AEADCipher) Open(prefix [4]byte, counter uint64, aad, payload []byte) ([]byte, error) {
	nonce := buildNonce(prefix, counter)
	return c.aead.Open(nil, nonce, payload, aad)
}

// NullCipher implements protocol.Cipher with no encryption. It is used for the
// M2 raw-transport experiments: the payload equals the plaintext. Integrity is
// still verified at the application layer via SHA-256.
type NullCipher struct{}

// Overhead is zero for the null cipher.
func (NullCipher) Overhead() int { return 0 }

// Seal returns a copy of plaintext.
func (NullCipher) Seal(_ [4]byte, _ uint64, _, plaintext []byte) ([]byte, error) {
	out := make([]byte, len(plaintext))
	copy(out, plaintext)
	return out, nil
}

// Open returns a copy of payload.
func (NullCipher) Open(_ [4]byte, _ uint64, _, payload []byte) ([]byte, error) {
	out := make([]byte, len(payload))
	copy(out, payload)
	return out, nil
}

// Sealer wraps a protocol.Cipher with a per-session nonce prefix and a
// monotonic counter, and guards against nonce-counter reuse. It is safe for
// concurrent use.
type Sealer struct {
	cipher  protocol.Cipher
	prefix  [4]byte
	mu      sync.Mutex
	counter uint64
	started bool
	last    uint64
}

// NewSealer creates a Sealer with a random nonce prefix.
func NewSealer(c protocol.Cipher) (*Sealer, error) {
	prefix, err := RandomNoncePrefix()
	if err != nil {
		return nil, err
	}
	return &Sealer{cipher: c, prefix: prefix}, nil
}

// Prefix returns the session nonce prefix.
func (s *Sealer) Prefix() [4]byte { return s.prefix }

// Cipher exposes the underlying cipher (e.g. for Overhead calculations).
func (s *Sealer) Cipher() protocol.Cipher { return s.cipher }

// Next allocates the next monotonic nonce counter, verifying it has not been
// used before. It returns ErrNonceReuse if a reuse is somehow attempted and
// ErrNonceExhausted if the counter space is exhausted.
func (s *Sealer) Next() (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started && s.counter <= s.last {
		return 0, ErrNonceReuse
	}
	if s.started && s.counter == ^uint64(0) {
		return 0, ErrNonceExhausted
	}
	c := s.counter
	s.last = c
	s.started = true
	if s.counter != ^uint64(0) {
		s.counter++
	}
	return c, nil
}

// EncodeFrame seals plaintext into a complete frame using the next nonce
// counter and the session prefix.
func (s *Sealer) EncodeFrame(typ protocol.MessageType, sessionID, chunkIndex uint64, plaintext []byte) ([]byte, error) {
	counter, err := s.Next()
	if err != nil {
		return nil, err
	}
	return protocol.Encode(s.cipher, typ, sessionID, chunkIndex, s.prefix, counter, plaintext)
}
