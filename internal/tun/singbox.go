package tun

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

const SingBoxVersion = "1.14.4"

// Sing-box owns the packet stack, DNS and automatic routes. The parent journals
// service DNS and transport exceptions, which also protect the separate WebRTC
// process. There is deliberately no direct Internet outbound or DNS fallback.
func singBoxConfig(c Config) ([]byte, error) {
	host, port, err := net.SplitHostPort(c.SOCKS)
	if err != nil {
		return nil, err
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 1 || p > 65535 {
		return nil, fmt.Errorf("invalid SOCKS port")
	}
	excluded := []string{"127.0.0.0/8", "::1/128"}
	for _, raw := range c.DirectIPs {
		ip := net.ParseIP(raw)
		if ip == nil || ip.To4() == nil {
			return nil, fmt.Errorf("invalid transport IPv4: %s", raw)
		}
		excluded = append(excluded, ip.String()+"/32")
	}
	dnsRules := []any{}
	strategy := "ipv4_only"
	if c.IPv6 {
		strategy = "prefer_ipv4"
	} else {
		dnsRules = append(dnsRules, map[string]any{"query_type": []string{"AAAA", "HTTPS", "SVCB"}, "action": "predefined", "rcode": "NOERROR"})
	}
	config := map[string]any{
		"log": map[string]any{"level": "info", "timestamp": true},
		"dns": map[string]any{
			"servers": []any{map[string]any{"type": "https", "tag": "tunnel-dns", "server": "1.1.1.1", "server_port": 443,
				"path": "/dns-query", "tls": map[string]any{"enabled": true, "server_name": "cloudflare-dns.com"}, "detour": "h264"}},
			"rules": dnsRules, "final": "tunnel-dns", "strategy": strategy,
		},
		"inbounds": []any{
			map[string]any{"type": "tun", "tag": "tun", "interface_name": c.Device, "address": []string{"198.18.0.1/30", "fdfe:dc:264::1/126"},
				"mtu": 1500, "auto_route": true, "dns_mode": "disabled", "route_exclude_address": excluded, "stack": "gvisor"},
			map[string]any{"type": "direct", "tag": "dns-in", "listen": "127.0.0.1", "listen_port": 53},
		},
		"outbounds": []any{map[string]any{"type": "socks", "tag": "h264", "server": host, "server_port": p, "version": "5", "bind_interface": "lo0"}},
		"route": map[string]any{"auto_detect_interface": true, "final": "h264", "rules": []any{
			map[string]any{"inbound": []string{"dns-in"}, "action": "hijack-dns"},
			map[string]any{"port": 53, "action": "hijack-dns"},
			// Fail fast so QUIC can fall back to TCP. The backend has no UDP relay.
			map[string]any{"network": "udp", "action": "reject"},
		}},
	}
	return json.MarshalIndent(config, "", "  ")
}

func (m *Manager) prepareSingBox() error {
	out, err := exec.Command(m.cfg.Binary, "version").Output()
	if err != nil {
		return err
	}
	// Schema changes are intentional upgrades, not surprises from PATH.
	var version string
	if _, err = fmt.Sscanf(string(out), "sing-box version %s", &version); err != nil || version != SingBoxVersion {
		return fmt.Errorf("sing-box %s required; run scripts/build_singbox.sh", SingBoxVersion)
	}
	b, err := singBoxConfig(m.cfg)
	if err != nil {
		return err
	}
	dir, err := os.MkdirTemp(filepath.Dir(m.cfg.StateFile), ".sing-box-*")
	if err != nil {
		return err
	}
	m.childConfig = dir
	path := filepath.Join(dir, "config.json")
	if err = os.WriteFile(path, b, 0600); err != nil {
		return err
	}
	if out, err := exec.Command(m.cfg.Binary, "check", "-c", path).CombinedOutput(); err != nil {
		return fmt.Errorf("sing-box config: %w: %s", err, out)
	}
	m.child = exec.Command(m.cfg.Binary, "run", "-c", path)
	return nil
}

func checkSingBoxDNS(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	r := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, "127.0.0.1:53")
	}}
	ips, err := r.LookupIP(ctx, "ip4", "cloudflare.com")
	if err != nil {
		return fmt.Errorf("sing-box DNS through tunnel: %w", err)
	}
	if len(ips) == 0 {
		return fmt.Errorf("sing-box DNS returned no IPv4 addresses")
	}
	return nil
}
