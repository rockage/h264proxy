package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/roca/h264-tunnel-lab/internal/logging"
	"github.com/roca/h264-tunnel-lab/internal/mux"
	"github.com/roca/h264-tunnel-lab/internal/rtc"
	"github.com/roca/h264-tunnel-lab/internal/socks"
	"github.com/roca/h264-tunnel-lab/internal/tunnel"
	"github.com/xjasonlyu/tun2socks/v2/core"
	"golang.org/x/net/dns/dnsmessage"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
)

type fakePacketDNS struct{}

func (fakePacketDNS) Close() {}
func (fakePacketDNS) Resolve(_ context.Context, q []byte, _ bool) []byte {
	var m dnsmessage.Message
	if m.Unpack(q) != nil || len(m.Questions) != 1 {
		return nil
	}
	m.Header.Response = true
	m.Answers = []dnsmessage.Resource{{Header: dnsmessage.ResourceHeader{Name: m.Questions[0].Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 30}, Body: &dnsmessage.AResource{A: [4]byte{203, 0, 113, 7}}}}
	b, _ := m.Pack()
	return b
}
func TestStartupCancellation(t *testing.T) {
	e, err := startEngine([]byte(`{"endpoint":"127.0.0.1:1"}`), make([]byte, 32))
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { e.close(); e.close(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("startup cancellation blocked")
	}
}
func TestPacketFlowTCPDNSAndShutdown(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	reserved, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := reserved.Addr().String()
	reserved.Close()
	key := bytes.Repeat([]byte{9}, 32)
	log := logging.NewTo("packet-test", io.Discard)
	gatewayReady := make(chan *tunnel.Peer, 1)
	gatewayErrors := make(chan error, 1)
	go func() {
		p, e := tunnel.Open(ctx, tunnel.Config{Role: mux.RoleListener, HTTP: address, Key: key, RTC: rtc.Options{Transport: "tcp"}, Log: log})
		if e != nil {
			gatewayErrors <- e
			return
		}
		gatewayReady <- p
	}()
	peer, err := tunnel.Open(ctx, tunnel.Config{Role: mux.RoleDialer, HTTP: address, Key: key, RTC: rtc.Options{Transport: "tcp", ActiveTCPOnly: true}, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	var gateway *tunnel.Peer
	select {
	case gateway = <-gatewayReady:
	case err := <-gatewayErrors:
		t.Fatal(err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	defer gateway.Close()
	if peer.ICEProtocol() != "tcp" {
		t.Fatal("packet engine did not retain ICE TCP")
	}
	go func() {
		st, err := gateway.Session.Accept()
		if err != nil {
			return
		}
		defer st.CloseWrite()
		var request [10]byte
		if _, err = io.ReadFull(st, request[:]); err != nil {
			return
		}
		if request[0] != 5 || request[3] != 1 {
			return
		}
		if _, err = st.Write([]byte{5, 0, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
			return
		}
		body, err := io.ReadAll(st)
		if err != nil {
			return
		}
		sum := sha256.Sum256(body)
		st.Write(append(sum[:], body...))
	}()
	engineCtx, engineCancel := context.WithCancel(ctx)
	e := &engine{ctx: engineCtx, cancel: engineCancel, peer: peer, dns: fakePacketDNS{}, slots: make(chan struct{}, 128), dnsSlots: make(chan struct{}, 64), ready: make(chan struct{})}
	e.dialer = socks.Dialer{Context: engineCtx, Session: peer.Session}
	e.endpoint = channel.New(256, mtu, "")
	e.stack, err = core.CreateStack(&core.Config{LinkEndpoint: e.endpoint, TransportHandler: e})
	if err != nil {
		t.Fatal(err)
	}
	close(e.ready)
	defer e.close()
	clientLink := channel.New(256, mtu, "")
	client := stack.New(stack.Options{NetworkProtocols: []stack.NetworkProtocolFactory{ipv4.NewProtocol}, TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol}})
	if err := client.CreateNIC(1, clientLink); err != nil {
		t.Fatal(err)
	}
	if err := client.AddProtocolAddress(1, tcpip.ProtocolAddress{Protocol: header.IPv4ProtocolNumber, AddressWithPrefix: tcpip.AddressWithPrefix{Address: tcpip.AddrFrom4([4]byte{198, 18, 0, 1}), PrefixLen: 24}}, stack.AddressProperties{}); err != nil {
		t.Fatal(err)
	}
	client.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: 1}})
	defer func() { client.Close(); clientLink.Close(); client.Wait() }()
	var pumps sync.WaitGroup
	pumps.Add(2)
	go func() {
		defer pumps.Done()
		for {
			pkt := clientLink.ReadContext(ctx)
			if pkt == nil {
				return
			}
			v := pkt.ToView()
			data := append([]byte(nil), v.AsSlice()...)
			v.Release()
			pkt.DecRef()
			_ = e.inject(data)
		}
	}()
	go func() {
		defer pumps.Done()
		for {
			if ctx.Err() != nil {
				return
			}
			data := e.read(10 * time.Millisecond)
			if len(data) == 0 {
				continue
			}
			pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(data)})
			clientLink.InjectInbound(header.IPv4ProtocolNumber, pkt)
			pkt.DecRef()
		}
	}()
	defer func() { cancel(); pumps.Wait() }()
	// TCP capacity exhaustion must not starve packet DNS.
	for i := 0; i < cap(e.slots); i++ {
		e.slots <- struct{}{}
	}
	dns, err := gonet.DialUDP(client, nil, &tcpip.FullAddress{Addr: tcpip.AddrFrom4([4]byte{198, 18, 0, 2}), Port: 53}, header.IPv4ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	dns.SetDeadline(time.Now().Add(3 * time.Second))
	query, _ := (&dnsmessage.Message{Header: dnsmessage.Header{ID: 7}, Questions: []dnsmessage.Question{{Name: dnsmessage.MustNewName("example.com."), Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}}}).Pack()
	if _, err := dns.Write(query); err != nil {
		t.Fatal(err)
	}
	answer := make([]byte, 4096)
	n, err := dns.Read(answer)
	dns.Close()
	if err != nil {
		t.Fatal(err)
	}
	var message dnsmessage.Message
	if message.Unpack(answer[:n]) != nil || message.Header.ID != 7 || len(message.Answers) != 1 {
		t.Fatal("packet DNS response mismatch")
	}
	for i := 0; i < cap(e.slots); i++ {
		<-e.slots
	}
	conn, err := gonet.DialContextTCP(ctx, client, tcpip.FullAddress{Addr: tcpip.AddrFrom4([4]byte{198, 51, 100, 7}), Port: 443}, header.IPv4ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(8 * time.Second))
	payload := bytes.Repeat([]byte("packet bridge"), 50000)
	if _, err := conn.Write(payload); err != nil {
		t.Fatal(err)
	}
	conn.CloseWrite()
	received, err := io.ReadAll(conn)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(payload)
	if !bytes.Equal(received, append(sum[:], payload...)) {
		t.Fatal("packet TCP byte/FIN integrity failed")
	}
	if e.input.Load() == 0 || e.output.Load() == 0 {
		t.Fatal(fmt.Sprint("packet counters not advanced"))
	}
}
