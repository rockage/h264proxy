// Package metrics defines the machine-readable report written by the server and
// client, plus helpers for timing and peak-memory sampling.
package metrics

import (
	"encoding/json"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"
)

// Report is the JSON document each side writes at the end of a run. Not every
// field is meaningful on both sides; unused fields stay at their zero value.
type Report struct {
	Role              string  `json:"role"` // "server" or "client"
	SessionID         string  `json:"session_id"`
	Mode              string  `json:"mode"` // "aead" or "raw"
	NegotiatedCodec   string  `json:"negotiated_codec"`
	PayloadType       uint8   `json:"payload_type"`
	SSRC              uint32  `json:"ssrc"`
	ConnectSeconds    float64 `json:"connect_seconds"`
	FileBytes         uint64  `json:"file_bytes"`
	TotalChunks       uint64  `json:"total_chunks"`
	RTPPacketsSent    uint64  `json:"rtp_packets_sent"`
	RTPPacketsRecv    uint64  `json:"rtp_packets_received"`
	BytesSent         uint64  `json:"bytes_sent"`     // RTP payload bytes
	BytesReceived     uint64  `json:"bytes_received"` // RTP payload bytes
	TransferSeconds   float64 `json:"transfer_seconds"`
	PayloadThroughput float64 `json:"payload_throughput_bytes_per_sec"`
	EncryptSeconds    float64 `json:"encrypt_seconds"`
	DecryptSeconds    float64 `json:"decrypt_seconds"`
	MissingChunks     uint64  `json:"missing_chunks"`
	DuplicateChunks   uint64  `json:"duplicate_chunks"`
	ReorderedChunks   uint64  `json:"reordered_chunks"`
	SHA256Expected    string  `json:"sha256_expected"`
	SHA256Actual      string  `json:"sha256_actual"`
	SHA256Match       bool    `json:"sha256_match"`
	SendComplete      bool    `json:"send_complete"`
	ReceiveComplete   bool    `json:"receive_complete"`
	PeakMemoryBytes   uint64  `json:"peak_memory_bytes"`
	Error             string  `json:"error,omitempty"`
}

// Write serializes the report as indented JSON to path.
func (r *Report) Write(path string) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(path, b, 0o644)
}

// PeakMemoryBytes returns the process peak resident set size. On Linux it reads
// VmHWM from /proc/self/status (the true high-water mark). Elsewhere it falls
// back to the Go runtime's cumulative max heap-in-use figure, which is an
// approximation; callers should treat a zero as "unavailable".
func PeakMemoryBytes() uint64 {
	if v, ok := linuxVmHWM(); ok {
		return v
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return ms.Sys
}

func linuxVmHWM() (uint64, bool) {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(line, "VmHWM:") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				kb, err := strconv.ParseUint(fields[1], 10, 64)
				if err == nil {
					return kb * 1024, true
				}
			}
		}
	}
	return 0, false
}

// Timer is a tiny stopwatch that accumulates elapsed durations, used to total
// up encrypt/decrypt time across many chunks.
type Timer struct {
	total time.Duration
}

// Track runs f, adding its wall-clock duration to the running total.
func (t *Timer) Track(f func()) {
	start := time.Now()
	f()
	t.total += time.Since(start)
}

// Seconds returns the accumulated time in seconds.
func (t *Timer) Seconds() float64 { return t.total.Seconds() }
