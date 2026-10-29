// Package media centralizes the H.264 codec definition and helpers for
// building and verifying the video track. We deliberately register ONLY H.264
// so SDP negotiation cannot silently fall back to VP8/VP9/AV1; if the remote
// offers none of our H.264 lines, negotiation fails, which is the desired
// behavior for this experiment.
package media

import (
	"fmt"
	"strings"

	"github.com/pion/webrtc/v4"
)

// H264MimeType is the canonical mime type we negotiate.
const H264MimeType = webrtc.MimeTypeH264

// ClockRate is the RTP clock rate for H.264 (90 kHz).
const ClockRate = 90000

// DefaultPayloadType is the dynamic payload type we propose for H.264. Pion may
// renumber it during negotiation; callers must read the negotiated value back
// rather than assume this constant.
const DefaultPayloadType = 96

// h264FmtpLine is a widely compatible H.264 fmtp line (constrained baseline,
// level 3.1, non-interleaved, with in-band parameter sets).
const h264FmtpLine = "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f"

// H264Codec returns the RTPCodecParameters for our single negotiated codec.
func H264Codec() webrtc.RTPCodecParameters {
	return webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:     H264MimeType,
			ClockRate:    ClockRate,
			Channels:     0,
			SDPFmtpLine:  h264FmtpLine,
			RTCPFeedback: nil, // no NACK/PLI feedback: we measure the raw channel
		},
		PayloadType: DefaultPayloadType,
	}
}

// RegisterH264Only registers exactly one video codec (H.264) on the media
// engine. No audio and no other video codecs are registered.
func RegisterH264Only(m *webrtc.MediaEngine) error {
	return m.RegisterCodec(H264Codec(), webrtc.RTPCodecTypeVideo)
}

// NewVideoTrack creates a sendable static RTP track advertised as H.264.
func NewVideoTrack(id, streamID string) (*webrtc.TrackLocalStaticRTP, error) {
	return webrtc.NewTrackLocalStaticRTP(
		H264Codec().RTPCodecCapability,
		id, streamID,
	)
}

// IsH264 reports whether a negotiated codec mime type is H.264 (case
// insensitive).
func IsH264(mimeType string) bool {
	return strings.EqualFold(mimeType, H264MimeType)
}

// VerifyCodec returns an error if the negotiated mime type is not H.264. This
// is the gate that makes the test fail if the session negotiated VP8/VP9/AV1 or
// anything else.
func VerifyCodec(mimeType string) error {
	if !IsH264(mimeType) {
		return fmt.Errorf("negotiated codec %q is not H.264", mimeType)
	}
	return nil
}
