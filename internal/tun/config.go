// Package tun manages an optional macOS TUN frontend for the existing SOCKS proxy.
// Packet processing runs in a separately installed tun2socks process.
package tun

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"

	"github.com/roca/h264-tunnel-lab/internal/logging"
)

type Config struct {
	Enabled   bool
	IPv6      bool
	Log       *logging.Logger
	Device    string
	Service   string
	Binary    string
	StateFile string
	SOCKS     string
	DirectIPs []string
}

func (c Config) Validate() error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("TUN automatic routing currently supports macOS only")
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("TUN requires root; run the client with sudo")
	}
	if !regexp.MustCompile(`^utun[0-9]{1,4}$`).MatchString(c.Device) {
		return fmt.Errorf("TUN device must be an explicit utun number, e.g. utun123")
	}
	if c.Service == "" {
		return fmt.Errorf("-tun-service is required (active macOS network service, e.g. Wi-Fi or Ethernet)")
	}
	if !filepath.IsAbs(c.StateFile) {
		return fmt.Errorf("-tun-state must be an absolute path")
	}
	if _, err := exec.LookPath(c.Binary); err != nil {
		return fmt.Errorf("find tun2socks: %w; run scripts/build_tun.sh first", err)
	}
	host, _, err := net.SplitHostPort(c.SOCKS)
	if err != nil || net.ParseIP(host) == nil || !net.ParseIP(host).IsLoopback() {
		return fmt.Errorf("TUN requires a loopback SOCKS listener with an explicit IP address")
	}
	return nil
}

// ResolveBypass is called before changing DNS or routes. ICE uses UDP/IPv4 in
// this project. Pin signalling, every known remote candidate and STUN endpoint.
func ResolveBypass(ctx context.Context, endpoint string, stuns, candidates []string) ([]string, error) {
	host, _, err := net.SplitHostPort(endpoint)
	if err != nil {
		return nil, fmt.Errorf("signalling endpoint: %w", err)
	}
	hosts := []string{host}
	hosts = append(hosts, candidates...)
	for _, raw := range stuns {
		s := strings.TrimPrefix(strings.TrimPrefix(raw, "stun:"), "stuns:")
		s = strings.SplitN(s, "?", 2)[0]
		if h, _, e := net.SplitHostPort(s); e == nil {
			s = h
		}
		if s != "" {
			hosts = append(hosts, s)
		}
	}
	seen := map[string]bool{}
	for _, h := range hosts {
		ips, e := net.DefaultResolver.LookupIP(ctx, "ip4", h)
		if e != nil {
			return nil, fmt.Errorf("resolve bypass host %s: %w", h, e)
		}
		for _, ip := range ips {
			if !ip.IsLoopback() && !ip.IsUnspecified() {
				seen[ip.String()] = true
			}
		}
	}
	result := make([]string, 0, len(seen))
	for ip := range seen {
		result = append(result, ip)
	}
	sort.Strings(result)
	return result, nil
}
