package transfer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/roca/h264-tunnel-lab/internal/logging"
	"github.com/roca/h264-tunnel-lab/internal/metrics"
	"github.com/roca/h264-tunnel-lab/internal/protocol"
)

// ReceiveOptions configures a receive.
type ReceiveOptions struct {
	OutPath string
	TmpDir  string
	// ReadDeadline bounds how long the receiver waits for the next packet once
	// started; zero means rely on ctx only.
	IdleTimeout time.Duration
}

// ErrIncomplete is returned when the transfer ended without a byte-exact,
// hash-verified file.
var ErrIncomplete = errors.New("transfer: incomplete or failed transfer")

type receiver struct {
	log    *logging.Logger
	cipher protocol.Cipher
	opts   ReceiveOptions
	rep    *metrics.Report

	tmp         *os.File
	tmpName     string
	meta        *protocol.StartMeta
	received    []bool
	recvCount   uint64
	lastChunk   int64
	decTimer    metrics.Timer
	started     bool
	endSeen     bool
	endMeta     *protocol.EndMeta
	decodeFails uint64
}

// Receive consumes frames from src, reconstructs the file into a temp file, and
// atomically commits it to opts.OutPath only if every chunk arrives and the
// SHA-256 matches. It fills in the receive-side metrics on rep.
func Receive(ctx context.Context, log *logging.Logger, src PacketSource, cipher protocol.Cipher, opts ReceiveOptions, rep *metrics.Report) error {
	r := &receiver{log: log, cipher: cipher, opts: opts, rep: rep, lastChunk: -1}
	defer r.cleanup()

	start := time.Now()
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		payload, _, err := src.ReadPayload()
		if err != nil {
			if err == io.EOF {
				break
			}
			return fmt.Errorf("read packet: %w", err)
		}
		rep.RTPPacketsRecv++
		rep.BytesReceived += uint64(len(payload))

		if done, herr := r.handle(payload); herr != nil {
			return herr
		} else if done {
			break
		}
	}

	rep.TransferSeconds = time.Since(start).Seconds()
	rep.DecryptSeconds = r.decTimer.Seconds()
	rep.PeakMemoryBytes = metrics.PeakMemoryBytes()

	return r.finalize()
}

// handle processes one received RTP payload. It returns done=true when the END
// frame has been processed.
func (r *receiver) handle(payload []byte) (bool, error) {
	var (
		hdr *protocol.Header
		pt  []byte
		err error
	)
	r.decTimer.Track(func() {
		hdr, pt, err = protocol.Decode(r.cipher, payload)
	})
	if err != nil {
		// Could not authenticate/parse this packet (corruption, tampering, or
		// wrong key). Count it; a resulting missing chunk will fail the hash
		// check at the end. We do not abort mid-stream on a single bad packet.
		r.decodeFails++
		r.log.Warn("decode_failed", logging.KeyError, err.Error(), logging.KeyPacketSize, len(payload))
		return false, nil
	}

	switch hdr.Type {
	case protocol.MsgStart:
		return false, r.onStart(hdr, pt)
	case protocol.MsgData:
		return false, r.onData(hdr, pt)
	case protocol.MsgEnd:
		return r.onEnd(hdr, pt)
	default:
		return false, nil
	}
}

func (r *receiver) onStart(hdr *protocol.Header, pt []byte) error {
	if r.started {
		// Duplicate START; ignore but log.
		r.log.Warn("duplicate_start", logging.KeySessionID, hdr.SessionID)
		return nil
	}
	meta, err := protocol.UnmarshalStart(pt)
	if err != nil {
		return fmt.Errorf("bad START meta: %w", err)
	}
	tmp, err := os.CreateTemp(r.opts.TmpDir, "h264tun-*.part")
	if err != nil {
		return fmt.Errorf("create temp: %w", err)
	}
	r.tmp = tmp
	r.tmpName = tmp.Name()
	r.meta = meta
	r.received = make([]bool, meta.TotalChunks)
	r.started = true
	r.rep.FileBytes = meta.FileSize
	r.rep.TotalChunks = meta.TotalChunks
	r.rep.SHA256Expected = hex.EncodeToString(meta.SHA256[:])
	r.log.Info("start_received",
		logging.KeySessionID, hdr.SessionID,
		"file_name", meta.Name,
		logging.KeyBytesRecv, meta.FileSize,
		"total_chunks", meta.TotalChunks,
		"chunk_size", meta.ChunkSize,
	)
	return nil
}

func (r *receiver) onData(hdr *protocol.Header, pt []byte) error {
	if !r.started {
		// DATA before START: cannot place it. Count as reorder; it will likely
		// become a missing chunk and fail the final check.
		r.rep.ReorderedChunks++
		r.log.Warn("data_before_start", logging.KeyChunkIndex, hdr.ChunkIndex)
		return nil
	}
	idx := hdr.ChunkIndex
	if idx >= r.meta.TotalChunks {
		r.log.Warn("chunk_out_of_range", logging.KeyChunkIndex, idx)
		return nil
	}
	// Reorder detection (informational): a chunk index lower than the highest
	// seen so far indicates out-of-order arrival.
	if int64(idx) < r.lastChunk {
		r.rep.ReorderedChunks++
	}
	if int64(idx) > r.lastChunk {
		r.lastChunk = int64(idx)
	}
	if r.received[idx] {
		r.rep.DuplicateChunks++
		r.log.Warn("duplicate_chunk", logging.KeyChunkIndex, idx)
		return nil
	}
	offset := int64(idx) * int64(r.meta.ChunkSize)
	if _, err := r.tmp.WriteAt(pt, offset); err != nil {
		return fmt.Errorf("write chunk %d: %w", idx, err)
	}
	r.received[idx] = true
	r.recvCount++
	r.log.Debug("chunk_written", logging.KeyChunkIndex, idx, logging.KeyPacketSize, len(pt))
	return nil
}

func (r *receiver) onEnd(hdr *protocol.Header, pt []byte) (bool, error) {
	meta, err := protocol.UnmarshalEnd(pt)
	if err != nil {
		return false, fmt.Errorf("bad END meta: %w", err)
	}
	r.endMeta = meta
	r.endSeen = true
	r.log.Info("end_received", logging.KeySessionID, hdr.SessionID, "total_chunks", meta.TotalChunks)
	// END is our signal to stop reading. Any chunk not yet received is missing.
	return true, nil
}

// finalize checks completeness and integrity, and commits the output on
// success.
func (r *receiver) finalize() error {
	if !r.started {
		r.rep.Error = "no START frame decoded (wrong key or no data received)"
		r.log.Error("finalize_failed", logging.KeyError, r.rep.Error)
		return ErrIncomplete
	}

	missing := r.meta.TotalChunks - r.recvCount
	r.rep.MissingChunks = missing
	r.rep.ReceiveComplete = false

	if !r.endSeen {
		r.rep.Error = "no END frame received"
	}
	if missing > 0 {
		if r.rep.Error == "" {
			r.rep.Error = fmt.Sprintf("%d missing chunk(s)", missing)
		}
	}
	// Cross-check END metadata against START.
	if r.endMeta != nil {
		if r.endMeta.FileSize != r.meta.FileSize || r.endMeta.TotalChunks != r.meta.TotalChunks || r.endMeta.SHA256 != r.meta.SHA256 {
			r.rep.Error = "END metadata does not match START metadata"
		}
	}
	if r.rep.Error != "" {
		r.log.Error("finalize_failed", logging.KeyError, r.rep.Error, "missing", missing, "duplicates", r.rep.DuplicateChunks)
		return ErrIncomplete
	}

	// Ensure exact file size, flush, then stream-hash the temp file.
	if err := r.tmp.Truncate(int64(r.meta.FileSize)); err != nil {
		return fmt.Errorf("truncate temp: %w", err)
	}
	if err := r.tmp.Sync(); err != nil {
		return fmt.Errorf("sync temp: %w", err)
	}
	actual, err := r.hashTemp()
	if err != nil {
		return err
	}
	r.rep.SHA256Actual = hex.EncodeToString(actual[:])
	r.rep.SHA256Match = actual == r.meta.SHA256
	if !r.rep.SHA256Match {
		r.rep.Error = "SHA-256 mismatch after reassembly"
		r.log.Error("sha256_mismatch", "expected", r.rep.SHA256Expected, "actual", r.rep.SHA256Actual)
		return ErrIncomplete
	}

	// Atomic commit: close temp, rename into place.
	name := r.tmpName
	if err := r.tmp.Close(); err != nil {
		return fmt.Errorf("close temp: %w", err)
	}
	r.tmp = nil
	if err := os.Rename(name, r.opts.OutPath); err != nil {
		return fmt.Errorf("commit output: %w", err)
	}
	r.tmpName = ""
	r.rep.ReceiveComplete = true
	r.log.Info("receive_complete",
		logging.KeyStatus, "ok",
		"sha256", r.rep.SHA256Actual,
		"rtp_packets_received", r.rep.RTPPacketsRecv,
		logging.KeyBytesRecv, r.rep.BytesReceived,
		"duplicates", r.rep.DuplicateChunks,
		"reordered", r.rep.ReorderedChunks,
	)
	return nil
}

func (r *receiver) hashTemp() ([32]byte, error) {
	var sum [32]byte
	if _, err := r.tmp.Seek(0, io.SeekStart); err != nil {
		return sum, err
	}
	h := sha256.New()
	if _, err := io.Copy(h, r.tmp); err != nil {
		return sum, err
	}
	copy(sum[:], h.Sum(nil))
	return sum, nil
}

func (r *receiver) cleanup() {
	if r.tmp != nil {
		_ = r.tmp.Close()
	}
	// Remove the temp file unless it was committed (tmpName cleared on success).
	if r.tmpName != "" {
		_ = os.Remove(r.tmpName)
	}
}
