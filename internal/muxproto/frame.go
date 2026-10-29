// Package muxproto defines the version-2 wire format used by the full-duplex
// multiplexing layer (internal/mux). It is a separate protocol from the
// version-1 file-transfer framing in internal/protocol, which remains
// unchanged; muxproto reuses the same AEAD Cipher and the Sealer's nonce
// discipline (random per-direction prefix + strictly monotonic counter).
//
// A frame is a fixed 42-byte header followed by an AEAD payload. The header is
// authenticated as Associated Data.
//
// Wire layout (all integers big-endian):
//
//	off size field
//	0   4    Magic "H2TL"
//	4   1    Version (=2)
//	5   1    FrameType (OPEN=1, DATA=2, CLOSE=3, WINDOW=4, ACK=5, RESET=6)
//	6   8    SessionID
//	14  4    StreamID
//	18  8    Seq           (per-stream, per-direction, monotonic from 0)
//	26  4    NoncePrefix   (per-direction session prefix)
//	30  8    NonceCounter  (per-direction, monotonic across all streams)
//	38  4    PayloadLength
//	42  ...  Payload
package muxproto

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/roca/h264-tunnel-lab/internal/protocol"
)

// HeaderSize is the fixed size of a v2 frame header.
const HeaderSize = 42

// Version is the v2 protocol version.
const Version uint8 = 2

// Magic identifies frames of this protocol (shared tag with v1; the Version
// byte distinguishes them).
var Magic = [4]byte{'H', '2', 'T', 'L'}

// MaxPayloadLength bounds a single frame payload.
const MaxPayloadLength = 1 << 16

// FrameType enumerates mux frame kinds.
type FrameType uint8

const (
	// FrameOpen opens a new stream. Payload is optional open metadata.
	FrameOpen FrameType = 1
	// FrameData carries stream payload bytes.
	FrameData FrameType = 2
	// FrameClose signals the sender will send no more DATA on the stream.
	FrameClose FrameType = 3
	// FrameWindow advertises absolute cumulative flow credit (payload: uint64).
	FrameWindow FrameType = 4
	// FrameAck acknowledges received DATA/CLOSE for reliability (payload: Ack).
	FrameAck FrameType = 5
	// FrameReset aborts both stream directions; ACK uses Seq+1.
	FrameReset FrameType = 6
)

func (t FrameType) String() string {
	switch t {
	case FrameOpen:
		return "OPEN"
	case FrameData:
		return "DATA"
	case FrameClose:
		return "CLOSE"
	case FrameWindow:
		return "WINDOW"
	case FrameAck:
		return "ACK"
	case FrameReset:
		return "RESET"
	default:
		return fmt.Sprintf("UNKNOWN(%d)", uint8(t))
	}
}

// Errors returned by frame decoding.
var (
	ErrShortBuffer = errors.New("muxproto: buffer shorter than header")
	ErrBadMagic    = errors.New("muxproto: bad magic")
	ErrBadVersion  = errors.New("muxproto: unsupported version")
	ErrUnknownType = errors.New("muxproto: unknown frame type")
	ErrBadLength   = errors.New("muxproto: payload length does not match buffer")
	ErrTooBig      = errors.New("muxproto: payload length exceeds maximum")
)

// Header is the parsed form of a v2 frame header.
type Header struct {
	Type         FrameType
	SessionID    uint64
	StreamID     uint32
	Seq          uint64
	NoncePrefix  [4]byte
	NonceCounter uint64
	PayloadLen   uint32
}

func marshalHeader(dst []byte, h *Header) {
	copy(dst[0:4], Magic[:])
	dst[4] = Version
	dst[5] = byte(h.Type)
	binary.BigEndian.PutUint64(dst[6:14], h.SessionID)
	binary.BigEndian.PutUint32(dst[14:18], h.StreamID)
	binary.BigEndian.PutUint64(dst[18:26], h.Seq)
	copy(dst[26:30], h.NoncePrefix[:])
	binary.BigEndian.PutUint64(dst[30:38], h.NonceCounter)
	binary.BigEndian.PutUint32(dst[38:42], h.PayloadLen)
}

// Encode builds a complete v2 frame (header + sealed payload). The caller
// supplies the per-direction nonce prefix and a fresh monotonic counter (from a
// cryptox.Sealer); the exact header bytes become the AEAD associated data.
func Encode(c protocol.Cipher, typ FrameType, sessionID uint64, streamID uint32, seq uint64, noncePrefix [4]byte, nonceCounter uint64, plaintext []byte) ([]byte, error) {
	payloadLen := len(plaintext) + c.Overhead()
	if payloadLen > MaxPayloadLength {
		return nil, ErrTooBig
	}
	h := Header{
		Type:         typ,
		SessionID:    sessionID,
		StreamID:     streamID,
		Seq:          seq,
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
		return nil, fmt.Errorf("muxproto: cipher produced %d bytes, want %d", len(sealed), payloadLen)
	}
	copy(frame[HeaderSize:], sealed)
	return frame, nil
}

// DecodeHeader parses and validates a v2 header from the front of buf.
func DecodeHeader(buf []byte) (*Header, error) {
	if len(buf) < HeaderSize {
		return nil, ErrShortBuffer
	}
	if [4]byte{buf[0], buf[1], buf[2], buf[3]} != Magic {
		return nil, ErrBadMagic
	}
	if buf[4] != Version {
		return nil, ErrBadVersion
	}
	h := &Header{
		Type:      FrameType(buf[5]),
		SessionID: binary.BigEndian.Uint64(buf[6:14]),
		StreamID:  binary.BigEndian.Uint32(buf[14:18]),
		Seq:       binary.BigEndian.Uint64(buf[18:26]),
	}
	copy(h.NoncePrefix[:], buf[26:30])
	h.NonceCounter = binary.BigEndian.Uint64(buf[30:38])
	h.PayloadLen = binary.BigEndian.Uint32(buf[38:42])

	switch h.Type {
	case FrameOpen, FrameData, FrameClose, FrameWindow, FrameAck, FrameReset:
	default:
		return nil, ErrUnknownType
	}
	if h.PayloadLen > MaxPayloadLength {
		return nil, ErrTooBig
	}
	return h, nil
}

// Decode parses a complete v2 frame and returns the header and decrypted
// plaintext. buf must contain exactly one frame.
func Decode(c protocol.Cipher, buf []byte) (*Header, []byte, error) {
	h, err := DecodeHeader(buf)
	if err != nil {
		return nil, nil, err
	}
	if len(buf) != HeaderSize+int(h.PayloadLen) {
		return nil, nil, ErrBadLength
	}
	aad := buf[:HeaderSize]
	payload := buf[HeaderSize : HeaderSize+int(h.PayloadLen)]
	pt, err := c.Open(h.NoncePrefix, h.NonceCounter, aad, payload)
	if err != nil {
		return h, nil, err
	}
	return h, pt, nil
}
