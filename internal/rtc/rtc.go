// Package rtc builds the Pion WebRTC API for H.264-only sessions and provides
// small signaling helpers. It supports two modes:
//
//   - Loopback (phase 1 / local tests): ICE runs over a 127.0.0.1 UDP mux with
//     the loopback candidate included, mDNS disabled, UDP/IPv4 only, no
//     STUN/TURN.
//   - Network (real deployment): ICE binds all interfaces (optionally a fixed
//     UDP port for firewalling), STUN servers are configured for NAT traversal,
//     and a public IP can be advertised as a 1:1-NAT host candidate (for cloud
//     VPSes whose socket sees only a private IP).
//
// Both modes register ONLY H.264 and use an empty interceptor registry (no
// NACK): reliability/congestion live in the mux layer, not here.
package rtc

import (
	"fmt"
	"net"

	"github.com/pion/ice/v4"
	"github.com/pion/interceptor"
	plog "github.com/pion/logging"
	"github.com/pion/webrtc/v4"

	"github.com/roca/h264-tunnel-lab/internal/media"
)

// RecvBufferBytes is the UDP socket receive buffer requested for the ICE mux. A
// large buffer absorbs RTP bursts before the application drains them. The OS
// may clamp it to rmem_max.
const RecvBufferBytes = 16 * 1024 * 1024

// Options configures NewAPI/Config.
type Options struct {
	// Network selects real-network mode. When false, loopback mode is used.
	Network bool
	// STUNServers are STUN URLs (e.g. "stun:stun.l.google.com:19302") used in
	// network mode to discover server-reflexive candidates behind NAT.
	STUNServers []string
	// PublicIP, when set in network mode, is advertised as a host candidate via
	// 1:1 NAT mapping (the server's public IP on a cloud VPS).
	PublicIP string
	// UDPPort, when > 0 in network mode, binds ICE to this fixed UDP port so a
	// single port can be opened in the firewall. 0 uses ephemeral ports.
	UDPPort int
}

// LoopbackOptions returns the local-only configuration (phase-1 default).
func LoopbackOptions() Options { return Options{Network: false} }

// NewAPI builds a configured *webrtc.API for the given options.
func NewAPI(opts Options) (*webrtc.API, error) {
	m := &webrtc.MediaEngine{}
	if err := media.RegisterH264Only(m); err != nil {
		return nil, err
	}
	se := webrtc.SettingEngine{}
	se.SetICEMulticastDNSMode(ice.MulticastDNSModeDisabled)
	se.SetNetworkTypes([]webrtc.NetworkType{webrtc.NetworkTypeUDP4})

	if opts.Network {
		if err := configureNetwork(&se, opts); err != nil {
			return nil, err
		}
	} else {
		if err := configureLoopback(&se); err != nil {
			return nil, err
		}
	}

	ir := &interceptor.Registry{} // intentionally empty
	return webrtc.NewAPI(
		webrtc.WithMediaEngine(m),
		webrtc.WithSettingEngine(se),
		webrtc.WithInterceptorRegistry(ir),
	), nil
}

func configureLoopback(se *webrtc.SettingEngine) error {
	se.SetIncludeLoopbackCandidate(true)
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		return err
	}
	_ = conn.SetReadBuffer(RecvBufferBytes)
	se.SetICEUDPMux(ice.NewUDPMuxDefault(ice.UDPMuxParams{
		UDPConn: conn,
		Logger:  plog.NewDefaultLoggerFactory().NewLogger("udpmux"),
	}))
	return nil
}

func configureNetwork(se *webrtc.SettingEngine, opts Options) error {
	if opts.PublicIP != "" {
		se.SetNAT1To1IPs([]string{opts.PublicIP}, webrtc.ICECandidateTypeHost)
	}
	if opts.UDPPort > 0 {
		conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4zero, Port: opts.UDPPort})
		if err != nil {
			return fmt.Errorf("bind udp :%d: %w", opts.UDPPort, err)
		}
		_ = conn.SetReadBuffer(RecvBufferBytes)
		se.SetICEUDPMux(ice.NewUDPMuxDefault(ice.UDPMuxParams{
			UDPConn: conn,
			Logger:  plog.NewDefaultLoggerFactory().NewLogger("udpmux"),
		}))
	}
	// With UDPPort == 0, Pion gathers ephemeral host candidates on all
	// interfaces; STUN (below, via Config) adds server-reflexive candidates.
	return nil
}

// Config returns the PeerConnection configuration. In network mode it includes
// the configured STUN servers; loopback mode uses none.
func Config(opts Options) webrtc.Configuration {
	cfg := webrtc.Configuration{ICEServers: []webrtc.ICEServer{}}
	if opts.Network {
		for _, u := range opts.STUNServers {
			if u != "" {
				cfg.ICEServers = append(cfg.ICEServers, webrtc.ICEServer{URLs: []string{u}})
			}
		}
	}
	return cfg
}

// Signal is the JSON payload exchanged over HTTP during signaling.
type Signal struct {
	SessionID uint64                    `json:"session_id"`
	SDP       webrtc.SessionDescription `json:"sdp"`
}

// GatherAndFreeze sets the local description and blocks until ICE gathering
// completes, returning the final (non-trickle) description so a single complete
// SDP can be exchanged over HTTP. In network mode this also waits for STUN
// server-reflexive candidates.
func GatherAndFreeze(pc *webrtc.PeerConnection, desc webrtc.SessionDescription) (*webrtc.SessionDescription, error) {
	done := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(desc); err != nil {
		return nil, err
	}
	<-done
	return pc.LocalDescription(), nil
}
