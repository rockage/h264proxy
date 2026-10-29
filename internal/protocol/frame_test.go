package protocol

import (
	"bytes"
	"testing"
)

// rawCipher is a minimal pass-through cipher for protocol tests.
type rawCipher struct{}

func (rawCipher) Overhead() int { return 0 }
func (rawCipher) Seal(_ [4]byte, _ uint64, _, pt []byte) ([]byte, error) {
	return append([]byte(nil), pt...), nil
}
func (rawCipher) Open(_ [4]byte, _ uint64, _, ct []byte) ([]byte, error) {
	return append([]byte(nil), ct...), nil
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	c := rawCipher{}
	prefix := [4]byte{1, 2, 3, 4}
	plaintext := []byte("hello \x00 world 世界")
	frame, err := Encode(c, MsgData, 0xDEADBEEF, 42, prefix, 7, plaintext)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	h, got, err := Decode(c, frame)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if h.Type != MsgData || h.SessionID != 0xDEADBEEF || h.ChunkIndex != 42 || h.NonceCounter != 7 {
		t.Fatalf("header mismatch: %+v", h)
	}
	if h.NoncePrefix != prefix {
		t.Fatalf("nonce prefix mismatch: %v", h.NoncePrefix)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("plaintext mismatch: %q", got)
	}
}

func TestDecodeBadMagic(t *testing.T) {
	c := rawCipher{}
	frame, _ := Encode(c, MsgStart, 1, 0, [4]byte{}, 0, []byte("x"))
	frame[0] = 'X'
	if _, _, err := Decode(c, frame); err != ErrBadMagic {
		t.Fatalf("want ErrBadMagic, got %v", err)
	}
}

func TestDecodeBadVersion(t *testing.T) {
	c := rawCipher{}
	frame, _ := Encode(c, MsgStart, 1, 0, [4]byte{}, 0, []byte("x"))
	frame[4] = 99 // version byte
	if _, _, err := Decode(c, frame); err != ErrBadVersion {
		t.Fatalf("want ErrBadVersion, got %v", err)
	}
}

func TestDecodeUnknownType(t *testing.T) {
	c := rawCipher{}
	frame, _ := Encode(c, MsgStart, 1, 0, [4]byte{}, 0, []byte("x"))
	frame[5] = 200 // type byte
	if _, _, err := Decode(c, frame); err != ErrUnknownType {
		t.Fatalf("want ErrUnknownType, got %v", err)
	}
}

func TestDecodeBadLength(t *testing.T) {
	c := rawCipher{}
	frame, _ := Encode(c, MsgData, 1, 0, [4]byte{}, 0, []byte("abcd"))
	// Truncate the payload; header still claims 4 bytes.
	short := frame[:len(frame)-1]
	if _, _, err := Decode(c, short); err != ErrBadLength {
		t.Fatalf("want ErrBadLength, got %v", err)
	}
}

func TestDecodeShortBuffer(t *testing.T) {
	c := rawCipher{}
	if _, _, err := Decode(c, make([]byte, HeaderSize-1)); err != ErrShortBuffer {
		t.Fatalf("want ErrShortBuffer, got %v", err)
	}
}

func TestStartMetaRoundTrip(t *testing.T) {
	m := &StartMeta{FileSize: 123456, TotalChunks: 139, ChunkSize: 900, Name: "file 名.bin"}
	for i := range m.SHA256 {
		m.SHA256[i] = byte(i)
	}
	got, err := UnmarshalStart(m.Marshal())
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.FileSize != m.FileSize || got.TotalChunks != m.TotalChunks || got.ChunkSize != m.ChunkSize || got.Name != m.Name || got.SHA256 != m.SHA256 {
		t.Fatalf("start meta mismatch: %+v vs %+v", got, m)
	}
}

func TestEndMetaRoundTrip(t *testing.T) {
	m := &EndMeta{FileSize: 999, TotalChunks: 2}
	for i := range m.SHA256 {
		m.SHA256[i] = byte(255 - i)
	}
	got, err := UnmarshalEnd(m.Marshal())
	if err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.FileSize != m.FileSize || got.TotalChunks != m.TotalChunks || got.SHA256 != m.SHA256 {
		t.Fatalf("end meta mismatch")
	}
}

func TestStartMetaTruncated(t *testing.T) {
	if _, err := UnmarshalStart([]byte{1, 2, 3}); err != ErrBadMeta {
		t.Fatalf("want ErrBadMeta, got %v", err)
	}
}
