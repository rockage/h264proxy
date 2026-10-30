package socks

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"strconv"

	"github.com/roca/h264-tunnel-lab/internal/mux"
)

// Dialer exposes the existing gateway CONNECT protocol without opening a local
// SOCKS listener. Context owns established connections; call contexts bound
// handshakes only, matching net.Dialer's lifetime semantics.
type Dialer struct {
	Context context.Context
	Session *mux.Session
	Config  Config
}

func (d Dialer) open(ctx context.Context, address string) (*mux.Stream, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return nil, err
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return nil, fmt.Errorf("invalid target port")
	}
	wire := []byte{5, 1, 0}
	if ip := net.ParseIP(host); ip != nil {
		if v4 := ip.To4(); v4 != nil {
			wire = append(wire, 1)
			wire = append(wire, v4...)
		} else {
			wire = append(wire, 4)
			wire = append(wire, ip.To16()...)
		}
	} else {
		if len(host) == 0 || len(host) > 255 {
			return nil, fmt.Errorf("invalid target hostname")
		}
		wire = append(wire, 3, byte(len(host)))
		wire = append(wire, host...)
	}
	wire = binary.BigEndian.AppendUint16(wire, uint16(port))
	cfg := d.Config.defaults()
	handshake, cancel := context.WithTimeout(ctx, cfg.HandshakeTimeout+cfg.DialTimeout)
	defer cancel()
	st, err := d.Session.OpenStream()
	if err != nil {
		return nil, err
	}
	hook := context.AfterFunc(handshake, func() { st.Reset() })
	complete := false
	defer func() {
		hook()
		if !complete {
			st.Reset()
		}
	}()
	if err = writeAll(st, wire); err != nil {
		return nil, err
	}
	code, _, err := readReply(st)
	if err != nil {
		return nil, err
	}
	if code != succeeded {
		return nil, &protocolError{code, "gateway CONNECT failed"}
	}
	if !hook() || handshake.Err() != nil {
		return nil, context.DeadlineExceeded
	}
	complete = true
	return st, nil
}

func (d Dialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, fmt.Errorf("gateway supports TCP only")
	}
	st, err := d.open(ctx, address)
	if err != nil {
		return nil, err
	}
	local, remote := net.Pipe()
	parent := d.Context
	if parent == nil {
		parent = context.Background()
	}
	go relay(parent, remote, st, d.Config.defaults().IdleTimeout)
	return local, nil
}

// ForwardTCP terminates the packet stack's TCP connection and preserves FIN in
// both directions while carrying bytes through the unchanged gateway protocol.
func (d Dialer) ForwardTCP(ctx context.Context, conn net.Conn, address string) error {
	st, err := d.open(ctx, address)
	if err != nil {
		conn.Close()
		return err
	}
	return relay(ctx, conn, st, d.Config.defaults().IdleTimeout)
}
