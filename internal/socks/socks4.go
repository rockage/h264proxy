package socks

import (
	"bytes"
	"fmt"
	"io"
)

// readClientRequest sniffs only one byte, without buffering application bytes.
// Both ingress protocols become the same reliable SOCKS5 CONNECT record on the
// tunnel, so existing gateways require no wire-protocol update.
func readClientRequest(conn io.ReadWriter) (request, byte, error) {
	var version [1]byte
	if _, err := io.ReadFull(conn, version[:]); err != nil {
		return request{}, 0, err
	}
	rw := struct {
		io.Reader
		io.Writer
	}{io.MultiReader(bytes.NewReader(version[:]), conn), conn}
	switch version[0] {
	case 5:
		if err := negotiate(rw); err != nil {
			return request{}, 5, err
		}
		req, err := readRequest(rw)
		return req, 5, err
	case 4:
		req, err := readSOCKS4Request(rw)
		return req, 4, err
	default:
		return request{}, version[0], fmt.Errorf("unsupported SOCKS version 0x%02x; configure SOCKS5 or SOCKS4, not HTTP", version[0])
	}
}

func readSOCKS4Request(r io.Reader) (request, error) {
	var hdr [8]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return request{}, err
	}
	if hdr[0] != 4 {
		return request{}, fmt.Errorf("expected SOCKS4")
	}
	if hdr[1] != 1 {
		return request{}, &protocolError{commandUnsupported, "SOCKS4 supports CONNECT only"}
	}
	// SOCKS4 USERID is not an authentication mechanism in this no-auth listener.
	if _, err := readCString(r, 255); err != nil {
		return request{}, err
	}
	wire := []byte{5, 1, 0}
	if hdr[4] == 0 && hdr[5] == 0 && hdr[6] == 0 && hdr[7] != 0 {
		host, err := readCString(r, 255)
		if err != nil {
			return request{}, err
		}
		if len(host) == 0 {
			return request{}, &protocolError{addressUnsupported, "empty SOCKS4a domain"}
		}
		wire = append(wire, 3, byte(len(host)))
		wire = append(wire, host...)
	} else {
		wire = append(wire, 1)
		wire = append(wire, hdr[4:8]...)
	}
	wire = append(wire, hdr[2:4]...)
	return readRequest(bytes.NewReader(wire))
}

func readCString(r io.Reader, max int) ([]byte, error) {
	b := make([]byte, 0, 32)
	for i := 0; i <= max; i++ {
		var c [1]byte
		if _, err := io.ReadFull(r, c[:]); err != nil {
			return nil, err
		}
		if c[0] == 0 {
			return b, nil
		}
		if i == max {
			return nil, fmt.Errorf("SOCKS4 field exceeds %d bytes", max)
		}
		b = append(b, c[0])
	}
	panic("unreachable")
}

func clientReply(version, code byte, wire []byte) []byte {
	if version != 4 {
		if wire != nil {
			return wire
		}
		return reply(code, nil)
	}
	out := []byte{0, 91, 0, 0, 0, 0, 0, 0}
	if code == succeeded {
		out[1] = 90
	}
	if len(wire) == 10 && wire[3] == 1 {
		copy(out[2:4], wire[8:10])
		copy(out[4:8], wire[4:8])
	}
	return out
}
