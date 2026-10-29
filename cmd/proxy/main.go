// Command proxy exposes SOCKS5 CONNECT through a WebRTC H.264 mux tunnel.
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
	rate := flag.Int("rate", mux.DefaultRateBytesPerSec, "aggregate sender rate ceiling in bytes/sec (0=unlimited)")
	setup := flag.Duration("connect-timeout", 45*time.Second, "tunnel setup timeout")
	handshake := flag.Duration("handshake-timeout", 15*time.Second, "SOCKS/CONNECT handshake timeout")
	dial := flag.Duration("dial-timeout", 10*time.Second, "remote TCP connect timeout")
	idle := flag.Duration("idle-timeout", 5*time.Minute, "per-connection inactivity timeout (0=disabled)")
	max := flag.Int("max-connections", 128, "maximum concurrent proxied connections")
	logPath := flag.String("log", "", "optional structured log file")
	report := flag.String("report", "", "write final mux statistics on exit")
	flag.Parse()
	if *role != "gateway" && *role != "client" {
		fmt.Fprintln(os.Stderr, "-role must be gateway or client")
		os.Exit(2)
	}
	if *rate < 0 || *max <= 0 || *setup <= 0 || *handshake <= 0 || *dial <= 0 || *idle < 0 {
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
	}, socks.Config{RequireDomain: *remoteDNS, HandshakeTimeout: *handshake, DialTimeout: *dial, IdleTimeout: *idle, MaxConnections: *max, Log: log})
	stop()
	if err != nil {
		log.Error("fatal", "error", err.Error())
	}
	logging.Close()
	if err != nil {
		os.Exit(1)
	}
}

func run(ctx context.Context, role, socksAddr, keyPath, report string, setup time.Duration, cfg tunnel.Config, sc socks.Config) error {
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
		err = socks.Serve(ctx, ln, peer.Session, sc)
	}
	if socks.NormalStop(err) && parent.Err() != nil {
		return nil
	}
	if ctx.Err() != nil {
		return fmt.Errorf("tunnel closed: %w", err)
	}
	return err
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
