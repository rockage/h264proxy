// Package socks implements SOCKS5 TCP CONNECT over one reliable mux stream,
// with SOCKS4/4a ingress compatibility.
package socks

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"syscall"
)

const (
	succeeded          byte = 0
	generalFailure     byte = 1
	networkUnreachable byte = 3
	hostUnreachable    byte = 4
	connectionRefused  byte = 5
	ttlExpired         byte = 6
	commandUnsupported byte = 7
	addressUnsupported byte = 8
)

type request struct {
	address string
	wire    []byte
}

func requireRemoteDNS(req request) error {
	host, _, err := net.SplitHostPort(req.address)
	if err != nil {
		return err
	}
	_, ipErr := netip.ParseAddr(strings.TrimSuffix(host, "."))
	if len(req.wire) < 4 || req.wire[3] != 3 || ipErr == nil {
		return &protocolError{addressUnsupported, "remote DNS required: send a hostname using SOCKS5 or SOCKS4a; IP-only/SOCKS4 requests cannot recover the original hostname (use -remote-dns-only=false only for intentional IP connections)"}
	}
	return nil
}

type protocolError struct {
	code byte
	msg  string
}

func (e *protocolError) Error() string { return e.msg }

func negotiate(rw io.ReadWriter) error {
	var hdr [2]byte
	if _, err := io.ReadFull(rw, hdr[:]); err != nil {
		return err
	}
	if hdr[0] != 5 {
		return fmt.Errorf("expected SOCKS5 version 0x05, received 0x%02x", hdr[0])
	}
	methods := make([]byte, int(hdr[1]))
	if _, err := io.ReadFull(rw, methods); err != nil {
		return err
	}
	for _, m := range methods {
		if m == 0 {
			return writeAll(rw, []byte{5, 0})
		}
	}
	_ = writeAll(rw, []byte{5, 255})
	return fmt.Errorf("no supported authentication method")
}

func readRequest(r io.Reader) (request, error) {
	var hdr [3]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return request{}, err
	}
	if hdr[0] != 5 || hdr[2] != 0 {
		return request{}, &protocolError{generalFailure, "invalid request header"}
	}
	if hdr[1] != 1 {
		return request{}, &protocolError{commandUnsupported, "only CONNECT is supported"}
	}
	address, wire, err := readAddress(r)
	if err != nil {
		return request{}, err
	}
	return request{address, append(hdr[:], wire...)}, nil
}

// Address encoding is shared by requests and replies (RFC 1928).
func readAddress(r io.Reader) (string, []byte, error) {
	var typ [1]byte
	if _, err := io.ReadFull(r, typ[:]); err != nil {
		return "", nil, err
	}
	wire := []byte{typ[0]}
	var n int
	switch typ[0] {
	case 1:
		n = 4
	case 4:
		n = 16
	case 3:
		var size [1]byte
		if _, err := io.ReadFull(r, size[:]); err != nil {
			return "", nil, err
		}
		if size[0] == 0 {
			return "", nil, &protocolError{addressUnsupported, "empty domain"}
		}
		wire = append(wire, size[0])
		n = int(size[0])
	default:
		return "", nil, &protocolError{addressUnsupported, "unsupported address type"}
	}
	b := make([]byte, n+2)
	if _, err := io.ReadFull(r, b); err != nil {
		return "", nil, err
	}
	host := string(b[:n])
	if typ[0] != 3 {
		host = net.IP(b[:n]).String()
	}
	port := binary.BigEndian.Uint16(b[n:])
	if typ[0] == 3 {
		for _, c := range b[:n] {
			if c == 0 || c < 33 || c == 127 {
				return "", nil, &protocolError{addressUnsupported, "invalid domain"}
			}
		}
	}
	return net.JoinHostPort(host, strconv.Itoa(int(port))), append(wire, b...), nil
}

func reply(code byte, addr net.Addr) []byte {
	out := []byte{5, code, 0, 1, 0, 0, 0, 0, 0, 0}
	if tcp, ok := addr.(*net.TCPAddr); ok {
		ip := tcp.IP.To4()
		typ := byte(1)
		if ip == nil {
			ip = tcp.IP.To16()
			typ = 4
		}
		if ip != nil {
			out = append([]byte{5, code, 0, typ}, ip...)
			out = binary.BigEndian.AppendUint16(out, uint16(tcp.Port))
		}
	}
	return out
}

func readReply(r io.Reader) (byte, []byte, error) {
	var hdr [3]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return 0, nil, err
	}
	if hdr[0] != 5 || hdr[2] != 0 {
		return 0, nil, fmt.Errorf("invalid CONNECT response")
	}
	_, wire, err := readAddress(r)
	return hdr[1], append(hdr[:], wire...), err
}

func errorCode(err error) byte {
	var p *protocolError
	switch {
	case errors.As(err, &p):
		return p.code
	case errors.Is(err, syscall.ECONNREFUSED):
		return connectionRefused
	case errors.Is(err, syscall.ENETUNREACH):
		return networkUnreachable
	case errors.Is(err, syscall.EHOSTUNREACH):
		return hostUnreachable
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return hostUnreachable
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return ttlExpired
	}
	return generalFailure
}

func writeAll(w io.Writer, b []byte) error {
	for len(b) > 0 {
		n, err := w.Write(b)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}
