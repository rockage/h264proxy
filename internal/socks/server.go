package socks

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/roca/h264-tunnel-lab/internal/logging"
	"github.com/roca/h264-tunnel-lab/internal/mux"
)

type Config struct {
	// RequireDomain rejects IP-only ingress requests: their original hostname
	// is unavailable and cannot safely be recovered after local DNS resolution.
	RequireDomain    bool
	HandshakeTimeout time.Duration
	DialTimeout      time.Duration
	IdleTimeout      time.Duration
	MaxConnections   int
	Log              *logging.Logger
}

func (c Config) defaults() Config {
	if c.HandshakeTimeout <= 0 {
		c.HandshakeTimeout = 15 * time.Second
	}
	if c.DialTimeout <= 0 {
		c.DialTimeout = 10 * time.Second
	}
	if c.MaxConnections <= 0 {
		c.MaxConnections = 128
	}
	return c
}

// Serve runs a no-authentication SOCKS5/SOCKS4(a) listener. The caller chooses the bind
// address; the proxy command defaults to loopback.
func Serve(ctx context.Context, ln net.Listener, sess *mux.Session, cfg Config) error {
	cfg = cfg.defaults()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	closeHook := context.AfterFunc(ctx, func() { ln.Close() })
	defer closeHook()
	go func() {
		select {
		case <-sess.Closed():
			cancel()
		case <-ctx.Done():
		}
	}()
	var wg sync.WaitGroup
	defer wg.Wait()
	// Cancellation must precede waiting for workers on an accept failure.
	defer cancel()
	slots := make(chan struct{}, cfg.MaxConnections)
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		select {
		case slots <- struct{}{}:
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { <-slots }()
				if e := serveClient(ctx, conn, sess, cfg); e != nil && cfg.Log != nil {
					cfg.Log.Debug("socks_connection_ended", "error", e.Error())
				}
			}()
		default:
			conn.Close()
		}
	}
}

func serveClient(ctx context.Context, conn net.Conn, sess *mux.Session, cfg Config) error {
	defer conn.Close()
	connHook := context.AfterFunc(ctx, func() { conn.Close() })
	defer connHook()
	conn.SetDeadline(time.Now().Add(cfg.HandshakeTimeout))
	req, version, err := readClientRequest(conn)
	if err != nil {
		if version == 4 {
			_ = writeAll(conn, clientReply(version, errorCode(err), nil))
		}
		// A SOCKS5 negotiation failure already sent its method response.
		var pe *protocolError
		if version == 5 && errors.As(err, &pe) {
			_ = writeAll(conn, reply(pe.code, nil))
		}
		return err
	}
	if cfg.RequireDomain {
		if err := requireRemoteDNS(req); err != nil {
			_ = writeAll(conn, clientReply(version, addressUnsupported, nil))
			if cfg.Log != nil {
				cfg.Log.Warn("local_dns_rejected", "protocol", version, "error", err.Error())
			}
			return err
		}
	}
	st, err := sess.OpenStream()
	if err != nil {
		_ = writeAll(conn, clientReply(version, generalFailure, nil))
		return err
	}
	complete := false
	defer func() {
		if !complete {
			st.Reset()
		}
	}()
	handshake, cancel := context.WithTimeout(ctx, cfg.HandshakeTimeout)
	hook := context.AfterFunc(handshake, func() { conn.Close(); st.Reset() })
	defer func() { hook(); cancel() }()
	if err = writeAll(st, req.wire); err != nil {
		return err
	}
	code, wire, err := readReply(st)
	if err != nil {
		_ = writeAll(conn, clientReply(version, generalFailure, nil))
		return err
	}
	if err = writeAll(conn, clientReply(version, code, wire)); err != nil {
		return err
	}
	if code != succeeded {
		err = st.CloseWrite()
		complete = err == nil
		return err
	}
	if !hook() || handshake.Err() != nil {
		return context.DeadlineExceeded
	}
	cancel()
	conn.SetDeadline(time.Time{})
	complete = true // relay owns cancellation and stream shutdown from here
	return relay(ctx, conn, st, cfg.IdleTimeout)
}

// ServeGateway resolves and dials targets on the remote side of the tunnel.
func ServeGateway(ctx context.Context, sess *mux.Session, cfg Config) error {
	cfg = cfg.defaults()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	hook := context.AfterFunc(ctx, func() { sess.Close() })
	defer hook()
	var wg sync.WaitGroup
	defer wg.Wait()
	defer cancel()
	slots := make(chan struct{}, cfg.MaxConnections)
	for {
		st, err := sess.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		select {
		case slots <- struct{}{}:
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer func() { <-slots }()
				if e := serveTarget(ctx, st, cfg); e != nil && cfg.Log != nil {
					cfg.Log.Debug("target_connection_ended", "stream_id", st.ID(), "error", e.Error())
				}
			}()
		default:
			st.Reset()
		}
	}
}

func serveTarget(ctx context.Context, st *mux.Stream, cfg Config) error {
	handshake, cancel := context.WithTimeout(ctx, cfg.HandshakeTimeout+cfg.DialTimeout)
	go func() {
		select {
		case <-st.Aborted():
			cancel()
		case <-handshake.Done():
		}
	}()
	hook := context.AfterFunc(handshake, func() { st.Reset() })
	defer func() { hook(); cancel() }()
	complete := false
	defer func() {
		if !complete {
			st.Reset()
		}
	}()
	req, err := readRequest(st)
	if err != nil {
		return err
	}
	dialCtx, dialCancel := context.WithTimeout(handshake, cfg.DialTimeout)
	conn, err := (&net.Dialer{}).DialContext(dialCtx, "tcp", req.address)
	dialCancel()
	if err != nil {
		if e := writeAll(st, reply(errorCode(err), nil)); e != nil {
			return e
		}
		if e := st.CloseWrite(); e != nil {
			return e
		}
		complete = true
		return fmt.Errorf("dial target: %w", err)
	}
	defer conn.Close()
	if err = writeAll(st, reply(succeeded, conn.LocalAddr())); err != nil {
		return err
	}
	if !hook() || handshake.Err() != nil {
		return context.DeadlineExceeded
	}
	cancel()
	complete = true
	return relay(ctx, conn, st, cfg.IdleTimeout)
}

// NormalStop distinguishes expected service teardown for command exit status.
func NormalStop(err error) bool {
	return err == nil || errors.Is(err, context.Canceled) || errors.Is(err, net.ErrClosed)
}
