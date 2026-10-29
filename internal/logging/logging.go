// Package logging provides a thin wrapper around log/slog that emits
// structured JSON log records with a stable set of fields. The records are
// intended to be machine-readable so they can be post-processed by other
// tools.
//
// The logger never records raw key material or full nonces. Callers should
// log a key fingerprint (see crypto.KeyFingerprint) instead of a key.
package logging

import (
	"io"
	"log/slog"
	"os"
	"sync"
)

// Field keys used across the project. Keeping them centralized avoids typos
// and keeps the emitted JSON schema stable for downstream analysis.
const (
	KeyComponent  = "component"
	KeySessionID  = "session_id"
	KeyEvent      = "event"
	KeyPCState    = "peer_connection_state"
	KeyICEState   = "ice_state"
	KeyCodec      = "codec"
	KeyRTPSeq     = "rtp_sequence"
	KeyRTPTS      = "rtp_timestamp"
	KeyPacketSize = "packet_size"
	KeyChunkIndex = "chunk_index"
	KeyBytesSent  = "bytes_sent"
	KeyBytesRecv  = "bytes_received"
	KeyStatus     = "transfer_status"
	KeyError      = "error"
	KeyPayloadT   = "payload_type"
	KeySSRC       = "ssrc"
)

// Logger wraps slog.Logger and pins a component name onto every record.
type Logger struct {
	inner     *slog.Logger
	component string
}

var (
	fileMu  sync.Mutex
	logFile *os.File
)

// New builds a Logger that writes JSON records to stderr and, if logPath is
// non-empty, also to the given file. The component name is attached to every
// record emitted through the returned Logger.
func New(component, logPath string) (*Logger, error) {
	var w io.Writer = os.Stderr
	if logPath != "" {
		fileMu.Lock()
		f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		fileMu.Unlock()
		if err != nil {
			return nil, err
		}
		logFile = f
		w = io.MultiWriter(os.Stderr, f)
	}
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug})
	return &Logger{inner: slog.New(h), component: component}, nil
}

// NewTo builds a Logger writing JSON records to an arbitrary writer. It is used
// by tests (e.g. with io.Discard) and whenever the default stderr+file sinks
// are not wanted.
func NewTo(component string, w io.Writer) *Logger {
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug})
	return &Logger{inner: slog.New(h), component: component}
}

// With returns a child logger carrying an additional stable attribute, e.g.
// the session id, on every subsequent record.
func (l *Logger) With(key string, value any) *Logger {
	return &Logger{inner: l.inner.With(key, value), component: l.component}
}

func (l *Logger) args(event string, kv []any) []any {
	out := make([]any, 0, len(kv)+4)
	out = append(out, KeyComponent, l.component, KeyEvent, event)
	out = append(out, kv...)
	return out
}

// Info logs an informational event with optional key/value pairs.
func (l *Logger) Info(event string, kv ...any) { l.inner.Info(event, l.args(event, kv)...) }

// Debug logs a debug event with optional key/value pairs.
func (l *Logger) Debug(event string, kv ...any) { l.inner.Debug(event, l.args(event, kv)...) }

// Warn logs a warning event with optional key/value pairs.
func (l *Logger) Warn(event string, kv ...any) { l.inner.Warn(event, l.args(event, kv)...) }

// Error logs an error event with optional key/value pairs.
func (l *Logger) Error(event string, kv ...any) { l.inner.Error(event, l.args(event, kv)...) }

// Close closes the underlying log file if one was opened.
func Close() {
	fileMu.Lock()
	defer fileMu.Unlock()
	if logFile != nil {
		_ = logFile.Close()
		logFile = nil
	}
}
