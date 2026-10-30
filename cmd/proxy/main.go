// Command proxy exposes SOCKS5 CONNECT through a WebRTC DataChannel mux tunnel.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	cryptox "github.com/roca/h264-tunnel-lab/internal/crypto"
	"github.com/roca/h264-tunnel-lab/internal/logging"
	"github.com/roca/h264-tunnel-lab/internal/mux"
	"github.com/roca/h264-tunnel-lab/internal/rtc"
	"github.com/roca/h264-tunnel-lab/internal/socks"
	"github.com/roca/h264-tunnel-lab/internal/tun"
	"github.com/roca/h264-tunnel-lab/internal/tunnel"
)

func main() {
	role := flag.String("role", "", "gateway or client (required)")
	httpAddr := flag.String("http", "127.0.0.1:9300", "gateway signaling bind / client remote address")
	socksAddr := flag.String("socks", "127.0.0.1:1080", "client SOCKS5/SOCKS4(a) listening address (no authentication)")
	remoteDNS := flag.Bool("remote-dns-only", true, "client: require hostnames resolved at the gateway; reject IP-only/SOCKS4 requests")
	keyPath := flag.String("key", "build/key.bin", "32-byte shared key file")
	netMode := flag.Bool("net", false, "use real-network ICE instead of loopback")
	publicIP := flag.String("public-ip", "", "gateway public IP candidate")
	udpPort := flag.Int("udp-port", 0, "fixed local ICE UDP port")
	stun := flag.String("stun", "", "comma-separated STUN URLs")
	rate := flag.Int("rate", 0, "aggregate sender rate ceiling in bytes/sec (0=unlimited)")
	setup := flag.Duration("connect-timeout", 45*time.Second, "tunnel setup timeout")
	handshake := flag.Duration("handshake-timeout", 15*time.Second, "SOCKS/CONNECT handshake timeout")
	dial := flag.Duration("dial-timeout", 10*time.Second, "remote TCP connect timeout")
	idle := flag.Duration("idle-timeout", 5*time.Minute, "per-connection inactivity timeout (0=disabled)")
	max := flag.Int("max-connections", 128, "maximum concurrent proxied connections")
	logPath := flag.String("log", "", "optional structured log file")
	report := flag.String("report", "", "write final mux statistics on exit")
	statsInterval := flag.Duration("stats-interval", 5*time.Second, "log live transport counters at this interval (0=disabled)")
	tunEnabled := flag.Bool("tun", false, "client: enable macOS TUN TCP proxy and DNS over the tunnel (requires root)")
	tunIPv6 := flag.Bool("tun-ipv6", false, "allow AAAA/HTTPS/SVCB DNS records in TUN mode (requires working gateway IPv6)")
	tunDevice := flag.String("tun-device", "utun123", "unused macOS utun device")
	tunService := flag.String("tun-service", "", "active macOS network service, e.g. Wi-Fi or Ethernet (required with -tun)")
	tunBinary := flag.String("tun2socks", "tun2socks", "path to tun2socks executable")
	tunState := flag.String("tun-state", "/Library/Application Support/H264Proxy/tun-state.json", "absolute recovery journal path")
	tunRecover := flag.Bool("tun-recover", false, "restore DNS/routes from -tun-state after an abnormal exit, then exit")
	flag.Parse()

	if *tunRecover {
		if err := tun.Recover(*tunState); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Println("TUN DNS/routes restored")
		return
	}
	tc := tun.Config{IPv6: *tunIPv6, Enabled: *tunEnabled, Device: *tunDevice, Service: *tunService, Binary: *tunBinary, StateFile: *tunState, SOCKS: *socksAddr}
	if tc.Enabled {
		if *role != "client" || !*netMode {
			fmt.Fprintln(os.Stderr, "-tun requires -role client -net")
			os.Exit(2)
		}
		if err := tc.Validate(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(2)
		}
		// TUN carries destination IPs, not SOCKS hostnames. DNS is intercepted
		// separately and sent through the tunnel by the TUN frontend.
		*remoteDNS = false
	}
	if *role != "gateway" && *role != "client" {
		fmt.Fprintln(os.Stderr, "-role must be gateway or client")
		os.Exit(2)
	}
	if *rate < 0 || *max <= 0 || *setup <= 0 || *handshake <= 0 || *dial <= 0 || *idle < 0 || *statsInterval < 0 {
		fmt.Fprintln(os.Stderr, "invalid rate, connection limit or timeout")
		os.Exit(2)
	}
	log, err := logging.New("proxy-"+*role, *logPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err = run(ctx, *role, *socksAddr, *keyPath, *report, *setup, tunnel.Config{
		HTTP: *httpAddr, Rate: *rate, Log: log,
		RTC: rtc.Options{Network: *netMode, PublicIP: *publicIP, UDPPort: *udpPort, STUNServers: splitCSV(*stun)},
	}, socks.Config{RequireDomain: *remoteDNS, HandshakeTimeout: *handshake, DialTimeout: *dial, IdleTimeout: *idle, MaxConnections: *max, Log: log}, *statsInterval, tc)
	stop()
	if err != nil {
		log.Error("fatal", "error", err.Error())
	}
	logging.Close()
	if err != nil {
		os.Exit(1)
	}
}

func run(ctx context.Context, role, socksAddr, keyPath, report string, setup time.Duration, cfg tunnel.Config, sc socks.Config, statsInterval time.Duration, tc tun.Config) error {
	parent := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	key, err := cryptox.LoadKey(keyPath)
	if err != nil {
		return err
	}
	cfg.Key = key
	cfg.Role = mux.RoleListener
	if role == "client" {
		cfg.Role = mux.RoleDialer
	}
	// Setup is bounded independently of the long-lived service context.
	timer := time.AfterFunc(setup, cancel)
	peer, err := tunnel.Open(ctx, cfg)
	timer.Stop()
	if err != nil {
		return err
	}
	defer peer.Close()
	if statsInterval > 0 {
		go logTransportStats(ctx, peer, cfg.Log, statsInterval)
	}
	if report != "" {
		defer func() {
			b, e := json.MarshalIndent(peer.Session.Stats(), "", "  ")
			if e == nil {
				e = os.WriteFile(report, append(b, '\n'), 0600)
			}
			if e != nil {
				cfg.Log.Error("report_write_failed", "error", e.Error())
			}
		}()
	}
	go func() {
		select {
		case <-peer.Session.Closed():
			cancel()
		case <-ctx.Done():
		}
	}()
	if role == "gateway" {
		err = socks.ServeGateway(ctx, peer.Session, sc)
	} else {
		ln, e := net.Listen("tcp", socksAddr)
		if e != nil {
			return e
		}
		defer ln.Close()
		cfg.Log.Info("socks_listening", "addr", ln.Addr().String())
		cfg.Log.Info("dns_policy", "remote_dns_only", sc.RequireDomain, "resolver", "gateway")
		err = serveClient(ctx, ln, peer, cfg, sc, tc)
	}
	if socks.NormalStop(err) && parent.Err() != nil {
		return nil
	}
	if ctx.Err() != nil {
		return fmt.Errorf("tunnel closed: %w", err)
	}
	return err
}

func serveClient(ctx context.Context, ln net.Listener, peer *tunnel.Peer, cfg tunnel.Config, sc socks.Config, tc tun.Config) (result error) {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	var serveErr error
	go func() {
		serveErr = socks.Serve(ctx, ln, peer.Session, sc)
		close(done)
	}()
	defer func() { cancel(); ln.Close(); <-done }()
	if !tc.Enabled {
		<-done
		return serveErr
	}
	var err error
	tc.DirectIPs, err = tun.ResolveBypass(ctx, cfg.HTTP, cfg.RTC.STUNServers, peer.RemoteIPs())
	if err != nil {
		return err
	}
	tc.Log = cfg.Log
	m, err := tun.Start(ctx, tc)
	if err != nil {
		return err
	}
	defer func() {
		if err := m.Close(); err != nil {
			cfg.Log.Error("tun_restore_failed", "error", err.Error(), "recovery_file", tc.StateFile)
			// Preserve cleanup failures even when the triggering error is cancellation.
			result = fmt.Errorf("TUN restore failed: %v (client result: %v)", err, result)
		}
	}()
	cfg.Log.Info("tun_ready", "device", tc.Device, "network_service", tc.Service, "dns", "DoH through SOCKS", "dns_ipv6", tc.IPv6, "udp", "unsupported", "bypass_ips", tc.DirectIPs)
	select {
	case <-done:
		return serveErr
	case <-ctx.Done():
		return ctx.Err()
	case <-m.Done():
		return fmt.Errorf("TUN forwarding stopped: %w", m.Err())
	}
}

func logTransportStats(ctx context.Context, peer *tunnel.Peer, log *logging.Logger, interval time.Duration) {
	sess := peer.Session
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-sess.Closed():
			return
		case <-ticker.C:
			log.Info("mux_stats", "stats", sess.Stats(), "transport", "datachannel-v1", "ice_protocol", peer.ICEProtocol(), "buffered_amount", peer.BufferedAmount(), "sctp", peer.SCTPStats())
		}
	}
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
