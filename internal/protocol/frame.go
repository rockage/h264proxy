// Package protocol defines the private, versioned binary frame format carried
// inside each RTP payload. A frame is a fixed-size header followed by a
// payload. The payload is produced by a Cipher: for the encrypted path it is
// AEAD ciphertext (plaintext + auth tag); for the raw path it is the plaintext
// unchanged.
//
// The header is authenticated as AEAD Associated Data, so any tampering with
// the header (type, session id, chunk index, nonce fields, length) is detected
// on decryption.
//
// Wire layout (all integers big-endian):
//
//	offset size field
//	0      4    Magic          "H2TL"
//	4      1    ProtocolVersion
//	5      1    MessageType    (START=1, DATA=2, END=3)
//	6      8    SessionID
//	14     8    ChunkIndex
//	22     4    NoncePrefix    (random per session)
//	26     8    NonceCounter   (strictly monotonic per session)
//	34     4    PayloadLength  (bytes of payload following the header)
//	38     ...  Payload
package protocol

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// HeaderSize is the fixed size in bytes of a frame header.
const HeaderSize = 38

// ProtocolVersion is the current wire version.
const ProtocolVersion uint8 = 1

// Magic identifies a frame of this protocol.
var Magic = [4]byte{'H', '2', 'T', 'L'}

// MessageType enumerates frame kinds.
type MessageType uint8

const (
	// MsgStart carries file metadata (name, size, chunk count, sha256).
	MsgStart MessageType = 1
	// MsgData carries one file chunk.
	MsgData MessageType = 2
	// MsgEnd signals end of transmission and carries final integrity info.
	MsgEnd MessageType = 3
)

func (t MessageType) String() string {
	switch t {
	case MsgStart:
		return "START"
	case MsgData:
		return "DATA"
	case MsgEnd:
		return "END"
	default:
		return fmt.Sprintf("UNKNOWN(%d)", uint8(t))
	}
}

// Errors returned by frame decoding.
var (
	ErrShortBuffer   = errors.New("protocol: buffer shorter than header")
	ErrBadMagic      = errors.New("protocol: bad magic")
	ErrBadVersion    = errors.New("protocol: unsupported protocol version")
	ErrBadLength     = errors.New("protocol: payload length does not match buffer")
	ErrUnknownType   = errors.New("protocol: unknown message type")
	ErrPayloadTooBig = errors.New("protocol: payload length exceeds maximum")
)

// MaxPayloadLength bounds a single frame's payload to guard against absurd
// length fields in corrupted packets.
const MaxPayloadLength = 1 << 16

// Header is the parsed, in-memory form of a frame header.
type Header struct {
	Version      uint8
	Type         MessageType
	SessionID    uint64
	ChunkIndex   uint64
	NoncePrefix  [4]byte
	NonceCounter uint64
	PayloadLen   uint32
}

// Cipher transforms a frame payload. The header bytes are passed as aad so the
// implementation can authenticate them. NoncePrefix/NonceCounter provide the
// per-packet nonce for AEAD implementations; raw implementations ignore them.
type Cipher interface {
	// Seal turns plaintext into the on-wire payload.
	Seal(noncePrefix [4]byte, nonceCounter uint64, aad, plaintext []byte) ([]byte, error)
	// Open turns the on-wire payload back into plaintext, verifying aad.
	Open(noncePrefix [4]byte, nonceCounter uint64, aad, payload []byte) ([]byte, error)
	// Overhead is the number of bytes Seal adds to the plaintext length.
	Overhead() int
}

// marshalHeader writes h into dst (which must be at least HeaderSize long).
func marshalHeader(dst []byte, h *Header) {
	copy(dst[0:4], Magic[:])
	dst[4] = h.Version
	dst[5] = byte(h.Type)
	binary.BigEndian.PutUint64(dst[6:14], h.SessionID)
	binary.BigEndian.PutUint64(dst[14:22], h.ChunkIndex)
	copy(dst[22:26], h.NoncePrefix[:])
	binary.BigEndian.PutUint64(dst[26:34], h.NonceCounter)
	binary.BigEndian.PutUint32(dst[34:38], h.PayloadLen)
}

// Encode builds a complete frame (header + sealed payload) from plaintext.
//
// The PayloadLength field is computed from the plaintext length plus the
// cipher overhead and written into the header before sealing, so the exact
// header bytes that go on the wire are the AAD that authenticates the payload.
func Encode(c Cipher, typ MessageType, sessionID, chunkIndex uint64, noncePrefix [4]byte, nonceCounter uint64, plaintext []byte) ([]byte, error) {
	payloadLen := len(plaintext) + c.Overhead()
	if payloadLen > MaxPayloadLength {
		return nil, ErrPayloadTooBig
	}
	h := Header{
		Version:      ProtocolVersion,
		Type:         typ,
		SessionID:    sessionID,
		ChunkIndex:   chunkIndex,
		NoncePrefix:  noncePrefix,
		NonceCounter: nonceCounter,
		PayloadLen:   uint32(payloadLen),
	}
	frame := make([]byte, HeaderSize+payloadLen)
	marshalHeader(frame[:HeaderSize], &h)

	sealed, err := c.Seal(noncePrefix, nonceCounter, frame[:HeaderSize], plaintext)
	if err != nil {
		return nil, err
	}
	if len(sealed) != payloadLen {
		return nil, fmt.Errorf("protocol: cipher produced %d bytes, expected %d", len(sealed), payloadLen)
	}
	copy(frame[HeaderSize:], sealed)
	return frame, nil
}

// DecodeHeader parses and validates a header from the front of buf. It does not
// touch the payload beyond length validation.
func DecodeHeader(buf []byte) (*Header, error) {
	if len(buf) < HeaderSize {
		return nil, ErrShortBuffer
	}
	if [4]byte{buf[0], buf[1], buf[2], buf[3]} != Magic {
		return nil, ErrBadMagic
	}
	h := &Header{
		Version:    buf[4],
		Type:       MessageType(buf[5]),
		SessionID:  binary.BigEndian.Uint64(buf[6:14]),
		ChunkIndex: binary.BigEndian.Uint64(buf[14:22]),
	}
	copy(h.NoncePrefix[:], buf[22:26])
	h.NonceCounter = binary.BigEndian.Uint64(buf[26:34])
	h.PayloadLen = binary.BigEndian.Uint32(buf[34:38])

	if h.Version != ProtocolVersion {
		return nil, ErrBadVersion
	}
	switch h.Type {
	case MsgStart, MsgData, MsgEnd:
	default:
		return nil, ErrUnknownType
	}
	if h.PayloadLen > MaxPayloadLength {
		return nil, ErrPayloadTooBig
	}
	return h, nil
}

// Decode parses a complete frame from buf, returning the header and the
// decrypted plaintext. buf must contain exactly one frame.
func Decode(c Cipher, buf []byte) (*Header, []byte, error) {
	h, err := DecodeHeader(buf)
	if err != nil {
		return nil, nil, err
	}
	if len(buf) != HeaderSize+int(h.PayloadLen) {
		return nil, nil, ErrBadLength
	}
	aad := buf[:HeaderSize]
	payload := buf[HeaderSize : HeaderSize+int(h.PayloadLen)]
	plaintext, err := c.Open(h.NoncePrefix, h.NonceCounter, aad, payload)
	if err != nil {
		return h, nil, err
	}
	return h, plaintext, nil
}
