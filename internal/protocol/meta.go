package protocol

import (
	"encoding/binary"
	"errors"
)

// ErrBadMeta indicates a malformed START/END payload.
var ErrBadMeta = errors.New("protocol: malformed metadata payload")

// StartMeta is the decrypted plaintext of a START frame. It describes the file
// being transferred so the receiver can allocate storage and verify the result.
type StartMeta struct {
	FileSize    uint64
	TotalChunks uint64
	ChunkSize   uint32
	SHA256      [32]byte
	Name        string
}

// MarshalStart serializes StartMeta.
//
//	8  FileSize
//	8  TotalChunks
//	4  ChunkSize
//	32 SHA256
//	2  NameLen
//	.. Name
func (m *StartMeta) Marshal() []byte {
	name := []byte(m.Name)
	buf := make([]byte, 8+8+4+32+2+len(name))
	binary.BigEndian.PutUint64(buf[0:8], m.FileSize)
	binary.BigEndian.PutUint64(buf[8:16], m.TotalChunks)
	binary.BigEndian.PutUint32(buf[16:20], m.ChunkSize)
	copy(buf[20:52], m.SHA256[:])
	binary.BigEndian.PutUint16(buf[52:54], uint16(len(name)))
	copy(buf[54:], name)
	return buf
}

// UnmarshalStart parses a StartMeta payload.
func UnmarshalStart(b []byte) (*StartMeta, error) {
	if len(b) < 54 {
		return nil, ErrBadMeta
	}
	m := &StartMeta{
		FileSize:    binary.BigEndian.Uint64(b[0:8]),
		TotalChunks: binary.BigEndian.Uint64(b[8:16]),
		ChunkSize:   binary.BigEndian.Uint32(b[16:20]),
	}
	copy(m.SHA256[:], b[20:52])
	nameLen := int(binary.BigEndian.Uint16(b[52:54]))
	if len(b) != 54+nameLen {
		return nil, ErrBadMeta
	}
	m.Name = string(b[54 : 54+nameLen])
	return m, nil
}

// EndMeta is the decrypted plaintext of an END frame. It echoes the integrity
// information so the receiver can cross-check against the START frame and its
// own accounting.
type EndMeta struct {
	FileSize    uint64
	TotalChunks uint64
	SHA256      [32]byte
}

// Marshal serializes EndMeta.
func (m *EndMeta) Marshal() []byte {
	buf := make([]byte, 8+8+32)
	binary.BigEndian.PutUint64(buf[0:8], m.FileSize)
	binary.BigEndian.PutUint64(buf[8:16], m.TotalChunks)
	copy(buf[16:48], m.SHA256[:])
	return buf
}

// UnmarshalEnd parses an EndMeta payload.
func UnmarshalEnd(b []byte) (*EndMeta, error) {
	if len(b) != 48 {
		return nil, ErrBadMeta
	}
	m := &EndMeta{
		FileSize:    binary.BigEndian.Uint64(b[0:8]),
		TotalChunks: binary.BigEndian.Uint64(b[8:16]),
	}
	copy(m.SHA256[:], b[16:48])
	return m, nil
}
