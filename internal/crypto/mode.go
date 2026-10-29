package cryptox

import (
	"fmt"

	"github.com/roca/h264-tunnel-lab/internal/protocol"
)

// Mode selects the payload protection scheme.
const (
	// ModeAEAD uses AES-256-GCM (the production path, M3/M4).
	ModeAEAD = "aead"
	// ModeRaw uses no encryption (the M2 raw-transport experiment).
	ModeRaw = "raw"
)

// CipherForMode builds the protocol.Cipher for the given mode. In raw mode the
// key is ignored and may be nil.
func CipherForMode(mode string, key []byte) (protocol.Cipher, error) {
	switch mode {
	case ModeAEAD:
		return NewAEADCipher(key)
	case ModeRaw:
		return NullCipher{}, nil
	default:
		return nil, fmt.Errorf("unknown mode %q (want %q or %q)", mode, ModeAEAD, ModeRaw)
	}
}
