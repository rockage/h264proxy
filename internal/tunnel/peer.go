// Package tunnel establishes a long-lived, single-peer WebRTC mux transport.
package tunnel

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/pion/ice/v4"
	"github.com/pion/webrtc/v4"
	cryptox "github.com/roca/h264-tunnel-lab/internal/crypto"
	"github.com/roca/h264-tunnel-lab/internal/datawire"
	"github.com/roca/h264-tunnel-lab/internal/logging"
	"github.com/roca/h264-tunnel-lab/internal/mux"
	"github.com/roca/h264-tunnel-lab/internal/rtc"
)

type Config struct {
	Role mux.Role
	HTTP string
	Key  []byte
	RTC  rtc.Options
	Rate int
	Log  *logging.Logger
}

type Peer struct {
	Session *mux.Session
	pc      *webrtc.PeerConnection
	wire    *datawire.Channel
	tcpMux  ice.TCPMux
	http    *http.Server
	once    sync.Once
}

func (p *Peer) BufferedAmount() uint64 { return p.wire.BufferedAmount() }

// SCTPStats reports the real transport congestion/receive windows and RTT.
// The mux's cwnd/RTT counters describe application feedback, not SCTP.
func (p *Peer) SCTPStats() webrtc.SCTPTransportStats { return p.pc.SCTP().Stats() }

func (p *Peer) ConnectionState() string { return p.pc.ConnectionState().String() }

// ICEPath identifies the actual selected endpoints, not just the signaling IP.
func (p *Peer) ICEPath() map[string]any {
	s := p.pc.SCTP()
	if s == nil || s.Transport() == nil {
		return nil
	}
	ice := s.Transport().ICETransport()
	pair, err := ice.GetSelectedCandidatePair()
	if err != nil || pair == nil {
		return nil
	}
	return map[string]any{"local_ip": pair.Local.Address, "local_port": pair.Local.Port, "local_type": pair.Local.Typ.String(), "remote_ip": pair.Remote.Address, "remote_port": pair.Remote.Port, "remote_type": pair.Remote.Typ.String(), "protocol": pair.Remote.Protocol.String()}
}

func (p *Peer) ICEProtocol() string {
	pair, err := p.pc.SCTP().Transport().ICETransport().GetSelectedCandidatePair()
	if err != nil || pair == nil {
		return ""
	}
	return pair.Remote.Protocol.String()
}

// RemoteIPs returns the known ICE endpoints before a TUN changes system routes.
// Keep all candidates reachable, including alternatives to the selected pair.
func (p *Peer) RemoteIPs() []string {
	seen := make(map[string]bool)
	for _, s := range p.pc.GetStats() {
		if c, ok := s.(webrtc.ICECandidateStats); ok && c.Type == webrtc.StatsTypeRemoteCandidate && net.ParseIP(c.IP) != nil {
			seen[c.IP] = true
		}
	}
	if s := p.pc.SCTP(); s != nil && s.Transport() != nil {
		if pair, err := s.Transport().ICETransport().GetSelectedCandidatePair(); err == nil && pair != nil {
			seen[pair.Remote.Address] = true
		}
	}
	var ips []string
	for ip := range seen {
		ips = append(ips, ip)
	}
	return ips
}

func (p *Peer) Close() error {
	p.once.Do(func() {
		if p.Session != nil {
			p.Session.Close()
		}
		p.wire.Close()
		if p.http != nil {
			p.http.Close()
		}
		p.pc.Close()
		if p.tcpMux != nil {
			p.tcpMux.Close()
		}

	})
	return nil
}

// Open uses ctx for both setup and the lifetime of the returned transport.
func Open(ctx context.Context, cfg Config) (_ *Peer, err error) {
	if cfg.Log == nil {
		return nil, fmt.Errorf("tunnel: logger required")
	}
	var tcpMux ice.TCPMux
	if cfg.RTC.Transport == "tcp" && !cfg.RTC.ActiveTCPOnly {
		host := "0.0.0.0"
		if !cfg.RTC.Network {
			host = "127.0.0.1"
		}
		ln, e := net.Listen("tcp4", net.JoinHostPort(host, fmt.Sprint(cfg.RTC.TCPPort)))
		if e != nil {
			return nil, fmt.Errorf("listen ICE TCP: %w", e)
		}
		// Blocking writes preserve reliable ICE-TCP delivery instead of dropping
		// packets when an additional TCP-mux write queue fills.
		tcpMux = ice.NewTCPMuxDefault(ice.TCPMuxParams{Listener: ln, ReadBufferSize: 64 * 1024, WriteBufferSize: 0})
		cfg.RTC.TCPMux = tcpMux
		defer func() {
			if err != nil {
				tcpMux.Close()
			}
		}()
	}
	api, err := rtc.NewDataAPI(cfg.RTC)
	if err != nil {
		return nil, err
	}
	pc, err := api.NewPeerConnection(rtc.Config(cfg.RTC))
	if err != nil {
		return nil, err
	}
	p := &Peer{pc: pc, wire: datawire.New(), tcpMux: tcpMux}
	defer func() {
		if err != nil {
			p.Close()
		}
	}()
	// One negotiated reliable channel. The mux preserves per-stream byte order.
	// Fixed ID avoids an untrusted
	// peer opening extra channels; both roles configure exactly the same channel.
	negotiated, ordered, id := true, true, uint16(0)
	dc, err := pc.CreateDataChannel("proxy-v1", &webrtc.DataChannelInit{Negotiated: &negotiated, Ordered: &ordered, ID: &id})
	if err != nil {
		return nil, err
	}
	dc.OnOpen(func() {
		rw, e := dc.Detach()
		if e != nil {
			p.wire.Close()
			return
		}
		wire, ok := rw.(datawire.Wire)
		if !ok {
			rw.Close()
			p.wire.Close()
			return
		}
		p.wire.Attach(wire)
	})
	dc.OnClose(func() { p.wire.Close() })
	dc.OnError(func(error) { p.wire.Close() })
	connected, failed := make(chan struct{}), make(chan struct{})
	var connOnce, failOnce sync.Once
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		cfg.Log.Info("pc_state_change", "state", state.String(), "ice_path", p.ICEPath())
		switch state {
		case webrtc.PeerConnectionStateConnected:
			connOnce.Do(func() { close(connected) })
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			p.wire.Close()
			failOnce.Do(func() { close(failed) })
		}
		// Disconnected is recoverable while ICE checks continue. Closing SCTP
		// here turns a brief routing change/lost keepalive into a remote abort.
	})
	var sid uint64
	if cfg.Role == mux.RoleListener {
		var b [8]byte
		if _, err = rand.Read(b[:]); err != nil {
			return nil, err
		}
		sid = binary.BigEndian.Uint64(b[:])
		offer, e := pc.CreateOffer(nil)
		if e != nil {
			return nil, e
		}
		frozen, e := gather(ctx, pc, offer)
		if e != nil {
			return nil, e
		}
		signal := rtc.Signal{Transport: "datachannel-v1", SessionID: sid, SDP: *frozen}
		handler := http.NewServeMux()
		handler.HandleFunc("GET /offer", func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode(signal) })
		var answerMu sync.Mutex
		answered := false
		handler.HandleFunc("POST /answer", func(w http.ResponseWriter, r *http.Request) {
			answerMu.Lock()
			defer answerMu.Unlock()
			if answered {
				http.Error(w, "already answered", http.StatusConflict)
				return
			}
			var answer rtc.Signal
			if e := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&answer); e != nil || answer.Transport != "datachannel-v1" || answer.SessionID != sid || answer.SDP.Type != webrtc.SDPTypeAnswer {
				http.Error(w, "invalid answer", http.StatusBadRequest)
				return
			}
			if e := pc.SetRemoteDescription(answer.SDP); e != nil {
				http.Error(w, e.Error(), http.StatusBadRequest)
				return
			}
			answered = true
			w.WriteHeader(http.StatusOK)
		})
		ln, e := net.Listen("tcp", cfg.HTTP)
		if e != nil {
			return nil, e
		}
		p.http = &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second}
		go p.http.Serve(ln)
		cfg.Log.Info("http_listening", "addr", ln.Addr().String())
	} else {
		signal, e := fetch(ctx, cfg.HTTP)
		if e != nil {
			return nil, e
		}
		if signal.Transport != "datachannel-v1" {
			return nil, fmt.Errorf("tunnel: incompatible carrier; upgrade both peers to DataChannel")
		}
		if signal.SDP.Type != webrtc.SDPTypeOffer {
			return nil, fmt.Errorf("tunnel: expected offer")
		}
		sid = signal.SessionID
		if e = pc.SetRemoteDescription(signal.SDP); e != nil {
			return nil, e
		}
		answer, e := pc.CreateAnswer(nil)
		if e != nil {
			return nil, e
		}
		frozen, e := gather(ctx, pc, answer)
		if e != nil {
			return nil, e
		}
		data, e := json.Marshal(rtc.Signal{Transport: "datachannel-v1", SessionID: sid, SDP: *frozen})
		if e != nil {
			return nil, e
		}
		req, e := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+cfg.HTTP+"/answer", bytes.NewReader(data))
		if e != nil {
			return nil, e
		}
		resp, e := (&http.Client{Timeout: 10 * time.Second}).Do(req)
		if e != nil {
			return nil, e
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("tunnel: answer HTTP %d", resp.StatusCode)
		}
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-failed:
		return nil, fmt.Errorf("tunnel: peer failed during setup")
	case <-connected:
	}
	if err = p.wire.Wait(ctx); err != nil {
		return nil, err
	}
	p.Session, err = mux.New(mux.Config{SessionID: sid, Role: cfg.Role, Mode: cryptox.ModeAEAD, Master: cfg.Key, ReliableTransport: true, Sink: p.wire, Source: p.wire, Log: cfg.Log, RateBytesPerSec: cfg.Rate})
	if err != nil {
		return nil, err
	}
	cfg.Log.Info("tunnel_ready", "transport", "datachannel-v1", "datachannel_ordered", true, "ice_path", p.ICEPath(), "ice_protocol", p.ICEProtocol(), "session_id", fmt.Sprintf("%016x", sid))
	go func() {
		select {
		case <-ctx.Done():
		case <-failed:
		case <-p.Session.Closed():
		}
		p.Close()
	}()
	return p, nil
}

func gather(ctx context.Context, pc *webrtc.PeerConnection, sdp webrtc.SessionDescription) (*webrtc.SessionDescription, error) {
	done := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(sdp); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-done:
		return pc.LocalDescription(), nil
	}
}

func fetch(ctx context.Context, addr string) (*rtc.Signal, error) {
	client := &http.Client{Timeout: 3 * time.Second}
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/offer", nil)
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err == nil {
			var signal rtc.Signal
			// Bound SDP response memory, including for a misconfigured endpoint.
			if resp.StatusCode == http.StatusOK {
				err = json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 64*1024)).Decode(&signal)
			} else {
				err = fmt.Errorf("HTTP %d", resp.StatusCode)
			}
			resp.Body.Close()
			if err == nil {
				return &signal, nil
			}
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("fetch offer: %w", ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
}
