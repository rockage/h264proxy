package muxproto

import (
	"crypto/hkdf"
	"crypto/sha256"
	"encoding/binary"
)

// Direction labels for the two independent AEAD streams. Each direction uses
// its own HKDF-derived subkey, so even if the per-direction random nonce
// prefixes were to collide, the keys differ and no nonce is ever reused under a
// single key across directions.
const (
	// DirDialerToListener is traffic from the dialer (client) to the listener.
	DirDialerToListener = "h264tunnel/v2/dialer->listener"
	// DirListenerToDialer is traffic from the listener (server) to the dialer.
	DirListenerToDialer = "h264tunnel/v2/listener->dialer"
)

// DeriveKey derives a 32-byte per-direction AES-256 subkey from the shared
// master key, bound to the session id and the direction label via HKDF-SHA256.
func DeriveKey(master []byte, sessionID uint64, direction string) ([]byte, error) {
	var salt [8]byte
	binary.BigEndian.PutUint64(salt[:], sessionID)
	return hkdf.Key(sha256.New, master, salt[:], direction, 32)
}
