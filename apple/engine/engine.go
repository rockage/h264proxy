package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/roca/h264-tunnel-lab/internal/logging"
	"github.com/roca/h264-tunnel-lab/internal/mux"
	"github.com/roca/h264-tunnel-lab/internal/rtc"
	"github.com/roca/h264-tunnel-lab/internal/socks"
	resolver "github.com/roca/h264-tunnel-lab/internal/tun"
	"github.com/roca/h264-tunnel-lab/internal/tunnel"
	"github.com/xjasonlyu/tun2socks/v2/core"
	"github.com/xjasonlyu/tun2socks/v2/core/adapter"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

const dnsAddress = "198.18.0.2"
const mtu = 1500

type configuration struct {
	Endpoint string `json:"endpoint"`
}

func parseConfiguration(raw []byte, key []byte) (configuration, error) {
	var c configuration
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, fmt.Errorf("invalid engine configuration")
	}
	host, port, err := net.SplitHostPort(c.Endpoint)
	if err != nil || net.ParseIP(host) == nil || net.ParseIP(host).To4() == nil {
		return c, fmt.Errorf("endpoint must be a literal IPv4 address and port")
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 1 || p > 65535 {
		return c, fmt.Errorf("invalid endpoint port")
	}
	if len(key) != 32 {
		return c, fmt.Errorf("shared key must be 32 bytes")
	}
	return c, nil
}

type packetDNS interface {
	Resolve(context.Context, []byte, bool) []byte
	Close()
}

type engine struct {
	ready    chan struct{}
	initErr  error
	ctx      context.Context
	cancel   context.CancelFunc
	peer     *tunnel.Peer
	endpoint *channel.Endpoint
	stack    *stack.Stack
	dns      packetDNS
	dialer   socks.Dialer
	mu       sync.Mutex
	packetMu sync.RWMutex
	closed   bool
	wg       sync.WaitGroup
	once     sync.Once
	slots    chan struct{}
	dnsSlots chan struct{}
	input    atomic.Uint64
	output   atomic.Uint64
	rejected atomic.Uint64
}

func startEngine(raw, key []byte) (*engine, error) {
	c, err := parseConfiguration(raw, key)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	e := &engine{ctx: ctx, cancel: cancel, slots: make(chan struct{}, 128), dnsSlots: make(chan struct{}, 64), ready: make(chan struct{})}
	go func() {
		e.initErr = e.initialize(c, key)
		close(e.ready)
		if e.initErr != nil {
			e.close()
		}
	}()
	return e, nil
}

func (e *engine) initialize(c configuration, key []byte) (err error) {
	ctx, cancel := e.ctx, e.cancel
	log := logging.NewTo("ne-engine", os.Stderr)
	timer := time.AfterFunc(30*time.Second, cancel)
	e.peer, err = tunnel.Open(ctx, tunnel.Config{Role: mux.RoleDialer, HTTP: c.Endpoint, Key: key, RTC: rtc.Options{Network: true, Transport: "tcp", ActiveTCPOnly: true}, Log: log})
	timer.Stop()
	if err != nil {
		return err
	}
	e.dialer = socks.Dialer{Context: ctx, Session: e.peer.Session, Config: socks.Config{HandshakeTimeout: 10 * time.Second, DialTimeout: 10 * time.Second, IdleTimeout: 5 * time.Minute, Log: log}}
	dns := resolver.NewPacketResolver(ctx, e.dialer.DialContext, false, log)
	e.dns = dns
	health, cancelHealth := context.WithTimeout(ctx, 15*time.Second)
	err = dns.Check(health)
	cancelHealth()
	if err != nil {
		return err
	}
	e.endpoint = channel.New(256, mtu, "")
	e.stack, err = core.CreateStack(&core.Config{LinkEndpoint: e.endpoint, TransportHandler: e})
	if err != nil {
		return err
	}

	if err := ctx.Err(); err != nil {
		return err
	}
	go func() {
		select {
		case <-e.peer.Session.Closed():
			e.cancel()
		case <-ctx.Done():
		}
	}()
	return nil
}

func (e *engine) launch(conn io.Closer, control bool, work func()) {
	slots := e.slots
	if control {
		slots = e.dnsSlots
	}
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		conn.Close()
		return
	}
	select {
	case slots <- struct{}{}:
	default:
		e.mu.Unlock()
		e.rejected.Add(1)
		conn.Close()
		return
	}
	e.wg.Add(1)
	e.mu.Unlock()
	go func() { defer e.wg.Done(); defer func() { <-slots }(); defer conn.Close(); work() }()
}
func isDNS(address string, port uint16) bool { return address == dnsAddress && port == 53 }
func (e *engine) HandleTCP(c adapter.TCPConn) {
	id := c.ID()
	host := id.LocalAddress.String()
	e.launch(c, isDNS(host, id.LocalPort), func() {
		if isDNS(host, id.LocalPort) {
			e.serveDNSStream(c)
			return
		}
		_ = e.dialer.ForwardTCP(e.ctx, c, net.JoinHostPort(host, fmt.Sprint(id.LocalPort)))
	})
}
func (e *engine) HandleUDP(c adapter.UDPConn) {
	id := c.ID()
	if !isDNS(id.LocalAddress.String(), id.LocalPort) {
		e.rejected.Add(1)
		c.Close()
		return
	}
	e.launch(c, true, func() {
		b := make([]byte, 65535)
		for {
			c.SetReadDeadline(time.Now().Add(15 * time.Second))
			n, err := c.Read(b)
			if err != nil {
				return
			}
			reply := e.dns.Resolve(e.ctx, b[:n], true)
			if len(reply) > 0 {
				if _, err = c.Write(reply); err != nil {
					return
				}
			}
		}
	})
}
func (e *engine) serveDNSStream(c net.Conn) {
	for {
		c.SetDeadline(time.Now().Add(20 * time.Second))
		var size [2]byte
		if _, err := io.ReadFull(c, size[:]); err != nil {
			return
		}
		n := int(binary.BigEndian.Uint16(size[:]))
		if n == 0 {
			return
		}
		query := make([]byte, n)
		if _, err := io.ReadFull(c, query); err != nil {
			return
		}
		answer := e.dns.Resolve(e.ctx, query, false)
		if len(answer) == 0 {
			return
		}
		binary.BigEndian.PutUint16(size[:], uint16(len(answer)))
		if _, err := io.Copy(c, io.MultiReader(bytes.NewReader(size[:]), bytes.NewReader(answer))); err != nil {
			return
		}
	}
}
func (e *engine) inject(packet []byte) error {
	select {
	case <-e.ready:
		if e.initErr != nil {
			return e.initErr
		}
	default:
		return fmt.Errorf("engine not ready")
	}
	e.packetMu.RLock()
	defer e.packetMu.RUnlock()
	e.mu.Lock()
	closed := e.closed
	e.mu.Unlock()
	if closed {
		return fmt.Errorf("engine stopped")
	}
	if len(packet) < 20 || len(packet) > mtu {
		return fmt.Errorf("invalid packet length")
	}
	proto := header.IPv4ProtocolNumber
	switch packet[0] >> 4 {
	case 4:
	case 6:
		if len(packet) < 40 {
			return fmt.Errorf("short IPv6 packet")
		}
		proto = header.IPv6ProtocolNumber
	default:
		return fmt.Errorf("invalid IP version")
	}
	pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(packet)})
	e.endpoint.InjectInbound(proto, pkt)
	pkt.DecRef()
	e.input.Add(1)
	return nil
}
func (e *engine) read(timeout time.Duration) []byte {
	select {
	case <-e.ready:
		if e.initErr != nil {
			return nil
		}
	default:
		return nil
	}
	ctx, cancel := context.WithTimeout(e.ctx, timeout)
	defer cancel()
	pkt := e.endpoint.ReadContext(ctx)
	if pkt == nil {
		return nil
	}
	defer pkt.DecRef()
	view := pkt.ToView()
	defer view.Release()
	data := append([]byte(nil), view.AsSlice()...)
	e.output.Add(1)
	return data
}
func (e *engine) close() {
	e.once.Do(func() {
		e.cancel()
		<-e.ready
		e.mu.Lock()
		e.closed = true
		e.mu.Unlock()
		e.cancel()
		e.packetMu.Lock()
		if e.stack != nil {
			e.stack.Close()
		}
		if e.endpoint != nil {
			e.endpoint.Close()
		}
		e.packetMu.Unlock()
		if e.peer != nil {
			e.peer.Close()
		}
		if e.dns != nil {
			e.dns.Close()
		}
		e.wg.Wait()
		if e.stack != nil {
			e.stack.Wait()
		}
	})
}
func (e *engine) status() []byte {
	state := "starting"
	select {
	case <-e.ready:
		state = "stopped"
	default:
		b, _ := json.Marshal(map[string]any{"state": state})
		return b
	}
	if e.peer != nil {
		state = e.peer.ConnectionState()
	}
	b, _ := json.Marshal(map[string]any{"state": state, "transport": "tcp", "input_packets": e.input.Load(), "output_packets": e.output.Load(), "unsupported_or_busy": e.rejected.Load()})
	return b
}
