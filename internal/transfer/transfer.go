// Package transfer implements the file sender and receiver on top of the frame
// protocol. It is transport-agnostic: the sender writes frame payloads to a
// PacketSink and the receiver reads them from a PacketSource. In production
// those are backed by the WebRTC RTP track; in tests they can be backed by an
// in-memory pipe, which lets the framing/crypto logic be tested without a
// network.
//
// Design guarantees:
//   - The sender streams the input file; it never holds the whole file in
//     memory (two sequential passes: one to hash, one to send).
//   - The receiver writes each chunk directly to its offset in a temp file via
//     WriteAt and only renames it to the final path after a full SHA-256 match,
//     so a partial or corrupted transfer never produces an output file.
package transfer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	cryptox "github.com/roca/h264-tunnel-lab/internal/crypto"
	"github.com/roca/h264-tunnel-lab/internal/logging"
	"github.com/roca/h264-tunnel-lab/internal/metrics"
	"github.com/roca/h264-tunnel-lab/internal/protocol"
)

// PacketSink accepts one frame payload to be carried as a single RTP packet.
// marker indicates the RTP marker bit should be set (used for the END frame).
type PacketSink interface {
	WritePayload(payload []byte, marker bool) error
}

// PacketSource yields frame payloads carried by RTP packets, in arrival order.
// It returns io.EOF when the channel is closed.
type PacketSource interface {
	ReadPayload() (payload []byte, marker bool, err error)
}

// BufferedPacketSource optionally reads into caller-owned storage. The caller
// consumes the packet before reusing dst; the source must not retain dst.
type BufferedPacketSource interface {
	ReadPayloadInto(dst []byte) (n int, marker bool, err error)
}

// SendOptions configures a send.
type SendOptions struct {
	SessionID uint64
	InPath    string
	ChunkSize int
	// RateBytesPerSec limits the payload send rate; 0 means unlimited.
	RateBytesPerSec int
}

// hashFile streams a file through SHA-256 without loading it into memory.
func hashFile(path string) ([32]byte, uint64, error) {
	var sum [32]byte
	f, err := os.Open(path)
	if err != nil {
		return sum, 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return sum, 0, err
	}
	copy(sum[:], h.Sum(nil))
	return sum, uint64(n), nil
}

// Send transmits the file at opts.InPath as START + DATA* + END frames through
// sink, sealing each with sealer. It fills in the send-side metrics on rep.
func Send(ctx context.Context, log *logging.Logger, sink PacketSink, sealer *cryptox.Sealer, opts SendOptions, rep *metrics.Report) error {
	if opts.ChunkSize <= 0 {
		return errors.New("transfer: chunk size must be positive")
	}
	sha, size, err := hashFile(opts.InPath)
	if err != nil {
		return fmt.Errorf("hash input: %w", err)
	}
	totalChunks := size / uint64(opts.ChunkSize)
	if size%uint64(opts.ChunkSize) != 0 || size == 0 {
		totalChunks++
	}
	rep.FileBytes = size
	rep.TotalChunks = totalChunks
	rep.SHA256Expected = hex.EncodeToString(sha[:])

	var encTimer metrics.Timer
	start := time.Now()

	// START
	startMeta := &protocol.StartMeta{
		FileSize:    size,
		TotalChunks: totalChunks,
		ChunkSize:   uint32(opts.ChunkSize),
		SHA256:      sha,
		Name:        filepath.Base(opts.InPath),
	}
	if err := sendFrame(log, sink, sealer, &encTimer, protocol.MsgStart, opts.SessionID, 0, startMeta.Marshal(), false, rep); err != nil {
		return err
	}
	log.Info("start_sent", logging.KeyBytesSent, size, "total_chunks", totalChunks)

	// DATA
	f, err := os.Open(opts.InPath)
	if err != nil {
		return err
	}
	defer f.Close()

	buf := make([]byte, opts.ChunkSize)
	var idx uint64
	pacer := newPacer(opts.RateBytesPerSec)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, rerr := io.ReadFull(f, buf)
		if n > 0 {
			if err := sendFrame(log, sink, sealer, &encTimer, protocol.MsgData, opts.SessionID, idx, buf[:n], false, rep); err != nil {
				return err
			}
			pacer.wait(protocol.HeaderSize + n + sealer.Cipher().Overhead())
			idx++
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			break
		}
		if rerr != nil {
			return fmt.Errorf("read input: %w", rerr)
		}
	}

	// END
	endMeta := &protocol.EndMeta{FileSize: size, TotalChunks: totalChunks, SHA256: sha}
	if err := sendFrame(log, sink, sealer, &encTimer, protocol.MsgEnd, opts.SessionID, idx, endMeta.Marshal(), true, rep); err != nil {
		return err
	}

	rep.TransferSeconds = time.Since(start).Seconds()
	rep.EncryptSeconds = encTimer.Seconds()
	if rep.TransferSeconds > 0 {
		rep.PayloadThroughput = float64(rep.BytesSent) / rep.TransferSeconds
	}
	rep.SendComplete = true
	rep.PeakMemoryBytes = metrics.PeakMemoryBytes()
	log.Info("send_complete", logging.KeyStatus, "ok", "rtp_packets_sent", rep.RTPPacketsSent, logging.KeyBytesSent, rep.BytesSent)
	return nil
}

func sendFrame(log *logging.Logger, sink PacketSink, sealer *cryptox.Sealer, enc *metrics.Timer, typ protocol.MessageType, sessionID, chunkIndex uint64, plaintext []byte, marker bool, rep *metrics.Report) error {
	var frame []byte
	var err error
	enc.Track(func() {
		frame, err = sealer.EncodeFrame(typ, sessionID, chunkIndex, plaintext)
	})
	if err != nil {
		return fmt.Errorf("encode %s frame: %w", typ, err)
	}
	if err := sink.WritePayload(frame, marker); err != nil {
		return fmt.Errorf("write %s frame: %w", typ, err)
	}
	rep.RTPPacketsSent++
	rep.BytesSent += uint64(len(frame))
	log.Debug("frame_sent",
		"type", typ.String(),
		logging.KeyChunkIndex, chunkIndex,
		logging.KeyPacketSize, len(frame),
	)
	return nil
}

// pacer provides a simple byte-rate limiter.
type pacer struct {
	rate int
	next time.Time
}

func newPacer(rate int) *pacer { return &pacer{rate: rate, next: time.Now()} }

func (p *pacer) wait(bytes int) {
	if p.rate <= 0 {
		return
	}
	delay := time.Duration(float64(bytes) / float64(p.rate) * float64(time.Second))
	p.next = p.next.Add(delay)
	now := time.Now()
	if p.next.After(now) {
		time.Sleep(p.next.Sub(now))
	} else {
		// Fell behind; reset baseline to avoid unbounded catch-up bursts.
		p.next = now
	}
}
