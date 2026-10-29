package muxproto

import (
	"bytes"
	"testing"

	cryptox "github.com/roca/h264-tunnel-lab/internal/crypto"
)

func TestV2RoundTrip(t *testing.T) {
	key, _ := cryptox.GenerateKey()
	c, err := cryptox.NewAEADCipher(key)
	if err != nil {
		t.Fatal(err)
	}
	prefix := [4]byte{5, 6, 7, 8}
	pt := []byte("multiplexed \x00 payload 数据")
	frame, err := Encode(c, FrameData, 0xABCD, 7, 42, prefix, 99, pt)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	h, got, err := Decode(c, frame)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if h.Type != FrameData || h.SessionID != 0xABCD || h.StreamID != 7 || h.Seq != 42 || h.NonceCounter != 99 || h.NoncePrefix != prefix {
		t.Fatalf("header mismatch: %+v", h)
	}
	if !bytes.Equal(got, pt) {
		t.Fatalf("payload mismatch: %q", got)
	}
}

func TestV2BadVersion(t *testing.T) {
	c := cryptox.NullCipher{}
	frame, _ := Encode(c, FrameOpen, 1, 1, 0, [4]byte{}, 0, nil)
	frame[4] = 1 // v1
	if _, err := DecodeHeader(frame); err != ErrBadVersion {
		t.Fatalf("want ErrBadVersion, got %v", err)
	}
}

func TestV2BadMagic(t *testing.T) {
	c := cryptox.NullCipher{}
	frame, _ := Encode(c, FrameOpen, 1, 1, 0, [4]byte{}, 0, nil)
	frame[0] = 'Z'
	if _, err := DecodeHeader(frame); err != ErrBadMagic {
		t.Fatalf("want ErrBadMagic, got %v", err)
	}
}

func TestV2UnknownType(t *testing.T) {
	c := cryptox.NullCipher{}
	frame, _ := Encode(c, FrameData, 1, 1, 0, [4]byte{}, 0, []byte("x"))
	frame[5] = 250
	if _, err := DecodeHeader(frame); err != ErrUnknownType {
		t.Fatalf("want ErrUnknownType, got %v", err)
	}
}

func TestV2BadLength(t *testing.T) {
	c := cryptox.NullCipher{}
	frame, _ := Encode(c, FrameData, 1, 1, 0, [4]byte{}, 0, []byte("abcd"))
	if _, _, err := Decode(c, frame[:len(frame)-1]); err != ErrBadLength {
		t.Fatalf("want ErrBadLength, got %v", err)
	}
}

func TestV2TamperDetected(t *testing.T) {
	key, _ := cryptox.GenerateKey()
	c, _ := cryptox.NewAEADCipher(key)
	frame, _ := Encode(c, FrameData, 1, 3, 4, [4]byte{1, 2, 3, 4}, 5, []byte("payload"))
	frame[HeaderSize+2] ^= 0xFF
	if _, _, err := Decode(c, frame); err == nil {
		t.Fatal("expected auth failure after payload tamper")
	}
	// Tamper a header (AAD) field: StreamID.
	frame2, _ := Encode(c, FrameData, 1, 3, 4, [4]byte{1, 2, 3, 4}, 6, []byte("payload"))
	frame2[14] ^= 0xFF
	if _, _, err := Decode(c, frame2); err == nil {
		t.Fatal("expected auth failure after header tamper")
	}
}

func TestDeriveKeyDistinctPerDirection(t *testing.T) {
	master, _ := cryptox.GenerateKey()
	k1, err := DeriveKey(master, 42, DirDialerToListener)
	if err != nil {
		t.Fatal(err)
	}
	k2, _ := DeriveKey(master, 42, DirListenerToDialer)
	k1b, _ := DeriveKey(master, 42, DirDialerToListener)
	if bytes.Equal(k1, k2) {
		t.Fatal("per-direction subkeys must differ")
	}
	if !bytes.Equal(k1, k1b) {
		t.Fatal("derivation must be deterministic")
	}
	if len(k1) != 32 {
		t.Fatalf("subkey length %d", len(k1))
	}
	// Different session id -> different key.
	k3, _ := DeriveKey(master, 43, DirDialerToListener)
	if bytes.Equal(k1, k3) {
		t.Fatal("subkey must depend on session id")
	}
}

func TestAckRoundTrip(t *testing.T) {
	a := &Ack{CumAck: 100, CloseAck: true, Ranges: []SeqRange{{102, 105}, {110, 111}}}
	got, err := UnmarshalAck(a.Marshal())
	if err != nil {
		t.Fatal(err)
	}
	if got.CumAck != 100 || !got.CloseAck || len(got.Ranges) != 2 ||
		got.Ranges[0] != (SeqRange{102, 105}) || got.Ranges[1] != (SeqRange{110, 111}) {
		t.Fatalf("ack mismatch: %+v", got)
	}
}

func TestAckEmpty(t *testing.T) {
	a := &Ack{CumAck: 7}
	got, err := UnmarshalAck(a.Marshal())
	if err != nil || got.CumAck != 7 || got.CloseAck || len(got.Ranges) != 0 {
		t.Fatalf("empty ack mismatch: %+v err=%v", got, err)
	}
}

func TestAckTruncated(t *testing.T) {
	if _, err := UnmarshalAck([]byte{1, 2, 3}); err != ErrBadAck {
		t.Fatalf("want ErrBadAck, got %v", err)
	}
}

func TestAckRangeCap(t *testing.T) {
	a := &Ack{CumAck: 0}
	for i := 0; i < MaxAckRanges+10; i++ {
		a.Ranges = append(a.Ranges, SeqRange{uint64(i * 2), uint64(i*2 + 1)})
	}
	got, err := UnmarshalAck(a.Marshal())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Ranges) != MaxAckRanges {
		t.Fatalf("want %d ranges, got %d", MaxAckRanges, len(got.Ranges))
	}
}

func TestAckFrameTypeAccepted(t *testing.T) {
	c := cryptoxNull()
	frame, _ := Encode(c, FrameAck, 1, 2, 0, [4]byte{}, 0, (&Ack{CumAck: 5}).Marshal())
	h, pt, err := Decode(c, frame)
	if err != nil || h.Type != FrameAck {
		t.Fatalf("ack frame decode: %v type=%v", err, h.Type)
	}
	if _, err := UnmarshalAck(pt); err != nil {
		t.Fatalf("ack payload: %v", err)
	}
}

func cryptoxNull() cryptox.NullCipher { return cryptox.NullCipher{} }
