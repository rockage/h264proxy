package tun

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"
)

type Manager struct {
	cfg              Config
	tx               *transaction
	dns              *dnsProxy
	child            *exec.Cmd
	done             chan struct{}
	childErr         error
	once             sync.Once
	closeErr         error
	bypassInterfaces map[string]string
}

func (m *Manager) Done() <-chan struct{} { return m.done }
func (m *Manager) Err() error {
	<-m.done
	if m.childErr == nil {
		return fmt.Errorf("tun2socks exited unexpectedly")
	}
	return fmt.Errorf("tun2socks: %w", m.childErr)
}

func cmd(name string, args ...string) command { return command{Name: name, Args: args} }

func routeField(out, key string) string {
	for _, line := range strings.Split(out, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), ":")
		if ok && k == key {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func serviceDevice(out, service string) string {
	selected := false
	header := regexp.MustCompile(`^\([0-9]+\) (.*)$`)
	device := regexp.MustCompile(`Device: ([^)]+)\)`)
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if match := header.FindStringSubmatch(line); match != nil {
			selected = match[1] == service
			continue
		}
		if selected {
			if match := device.FindStringSubmatch(line); match != nil {
				return strings.TrimSpace(match[1])
			}
		}
	}
	return ""
}

// Start only runs after the tunnel and SOCKS listener are ready. All mutations
// are additive routes or journaled DNS changes; the existing default stays intact.
func Start(ctx context.Context, c Config) (_ *Manager, err error) {
	if err = c.Validate(); err != nil {
		return nil, err
	}
	if _, err = net.InterfaceByName(c.Device); err == nil {
		return nil, fmt.Errorf("TUN interface %s already exists; choose an unused -tun-device", c.Device)
	}
	m := &Manager{cfg: c, done: make(chan struct{})}
	defer func() {
		if err != nil {
			if restoreErr := m.Close(); restoreErr != nil {
				// Do not let context cancellation hide a failed rollback upstream.
				err = fmt.Errorf("TUN setup failed: %v; restore failed: %v (recovery file: %s)", err, restoreErr, c.StateFile)
			}
		}
	}()
	out, err := execute(cmd("/sbin/route", "-n", "get", "default"))
	if err != nil {
		return nil, err
	}
	iface := routeField(out, "interface")
	if iface == "" || strings.HasPrefix(iface, "utun") {
		return nil, fmt.Errorf("physical default interface required; stop other VPNs first")
	}
	order, err := execute(cmd("/usr/sbin/networksetup", "-listnetworkserviceorder"))
	if err != nil {
		return nil, err
	}
	if serviceDevice(order, c.Service) != iface {
		return nil, fmt.Errorf("network service %q does not match default interface %s", c.Service, iface)
	}
	dns, err := execute(cmd("/usr/sbin/networksetup", "-getdnsservers", c.Service))
	if err != nil {
		return nil, err
	}
	oldDNS, err := parseDNS(dns)
	if err != nil {
		return nil, err
	}
	m.tx, err = newTransaction(c.StateFile, execute)
	if err != nil {
		return nil, err
	}
	m.dns, err = startDNS(ctx, "127.0.0.1:53", c.SOCKS, c.IPv6, c.Log)
	if err != nil {
		return nil, err
	}
	if err = m.dns.check(ctx); err != nil {
		return nil, err
	}
	// Resolve the executable before starting, not via a shell. Do not bind
	// tun2socks' dialer to the physical NIC: its proxy lives on loopback.
	m.child = exec.Command(c.Binary, "-device", c.Device, "-proxy", "socks5://"+c.SOCKS, "-mtu", "1500", "-loglevel", "warn")
	m.child.Stdout, m.child.Stderr = os.Stderr, os.Stderr
	if err = m.child.Start(); err != nil {
		m.child = nil
		return nil, err
	}
	go func() { m.childErr = m.child.Wait(); close(m.done) }()
	m.tx.state.ChildPID = m.child.Process.Pid
	m.tx.state.ChildCommand = strings.Join(m.child.Args, " ")
	if err = m.tx.save(); err != nil {
		return nil, err
	}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	ready := false
	for !ready {
		if _, e := net.InterfaceByName(c.Device); e == nil {
			ready = true
			break
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-m.done:
			return nil, m.Err()
		case <-timer.C:
			return nil, fmt.Errorf("timed out waiting for %s", c.Device)
		case <-ticker.C:
		}
	}
	if _, err = execute(cmd("/sbin/ifconfig", c.Device, "inet", "198.18.0.1", "198.18.0.1", "netmask", "255.255.255.255", "up")); err != nil {
		return nil, err
	}
	if _, err = execute(cmd("/sbin/ifconfig", c.Device, "inet6", "fdfe:dc:264::1", "prefixlen", "128")); err != nil {
		return nil, err
	}
	// Inspect each endpoint's route before adding split defaults. Preserve the
	// actual interface/gateway (including directly attached LAN candidates).
	for _, ip := range c.DirectIPs {
		if err = m.pin(ip); err != nil {
			return nil, err
		}
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if err = m.installRoutes(); err != nil {
		return nil, err
	}
	if err = m.verifyBypass(); err != nil {
		return nil, err
	}
	if err = m.dns.check(ctx); err != nil {
		return nil, fmt.Errorf("tunnel health after TUN route takeover: %w", err)
	}
	if err = m.tx.apply(cmd("/usr/sbin/networksetup", "-setdnsservers", c.Service, "127.0.0.1"), cmd("/usr/sbin/networksetup", append([]string{"-setdnsservers", c.Service}, oldDNS...)...)); err != nil {
		return nil, err
	}
	// DNS caches may contain locally resolved destinations; new lookups must use
	// the tunnel. Cache flush failure does not invalidate installed configuration.
	_, _ = execute(cmd("/usr/bin/dscacheutil", "-flushcache"))
	_, _ = execute(cmd("/usr/bin/killall", "-HUP", "mDNSResponder"))
	return m, nil
}

func parseDNS(out string) ([]string, error) {
	if strings.Contains(out, "There aren't any DNS Servers set on") {
		return []string{"Empty"}, nil
	}
	var ips []string
	for _, s := range strings.Fields(out) {
		if net.ParseIP(strings.SplitN(s, "%", 2)[0]) == nil {
			return nil, fmt.Errorf("unrecognized macOS DNS configuration: %q", out)
		}
		ips = append(ips, s)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("empty macOS DNS configuration")
	}
	return ips, nil
}

func (m *Manager) pin(ip string) error {
	addr := net.ParseIP(ip)
	if addr == nil || addr.To4() == nil {
		return fmt.Errorf("bypass address must be IPv4: %s", ip)
	}
	if addr.IsLoopback() {
		return nil
	}
	out, err := m.tx.run(cmd("/sbin/route", "-n", "get", ip))
	if err != nil {
		return err
	}
	gw, iface := routeField(out, "gateway"), routeField(out, "interface")
	if iface == "" || strings.HasPrefix(iface, "utun") {
		return fmt.Errorf("bypass IP %s uses interface %q; disable other VPN/TUN connections first", ip, iface)
	}
	if m.bypassInterfaces == nil {
		m.bypassInterfaces = make(map[string]string)
	}
	m.bypassInterfaces[ip] = iface
	// Traffic itself creates HOST routes. Only a static, unscoped /32 is a
	// durable exception to the split defaults we are about to install.
	flags := routeFlags(out)
	if routeField(out, "destination") == ip && flags["HOST"] && flags["STATIC"] && !flags["IFSCOPE"] && !flags["WASCLONED"] {
		return nil
	}
	args := []string{"-n", "add", "-host", ip}
	if net.ParseIP(gw) != nil {
		args = append(args, gw)
	} else {
		args = append(args, "-interface", iface)
	}
	return m.tx.apply(cmd("/sbin/route", args...), cmd("/sbin/route", "-n", "delete", "-host", ip))
}

func routeFlags(out string) map[string]bool {
	flags := make(map[string]bool)
	for _, flag := range strings.Split(strings.Trim(routeField(out, "flags"), "<>"), ",") {
		flags[strings.TrimSpace(flag)] = true
	}
	return flags
}

// Check the effective routes after takeover, not just before adding defaults.
// A stale/scoped host route can look direct before the network route changes.
func (m *Manager) verifyBypass() error {
	for ip, want := range m.bypassInterfaces {
		out, err := m.tx.run(cmd("/sbin/route", "-n", "get", ip))
		if err != nil {
			return err
		}
		if got := routeField(out, "interface"); got != want {
			return fmt.Errorf("TUN would route transport endpoint %s through %q instead of %q; rolling back", ip, got, want)
		}
	}
	return nil
}

func (m *Manager) installRoutes() error {
	// More-specific LAN/loopback routes stay direct. IPv6 also enters the stack,
	// preventing an IPv6 default route from silently bypassing the IPv4 tunnel.
	for _, r := range []struct{ family, network string }{{"-inet", "0.0.0.0/1"}, {"-inet", "128.0.0.0/1"}, {"-inet6", "::/1"}, {"-inet6", "8000::/1"}} {
		if err := m.tx.apply(cmd("/sbin/route", "-n", "add", r.family, "-net", r.network, "-interface", m.cfg.Device), cmd("/sbin/route", "-n", "delete", r.family, "-net", r.network, "-interface", m.cfg.Device)); err != nil {
			return err
		}
	}
	return nil
}

func (m *Manager) Close() error {
	m.once.Do(func() {
		// Restore DNS/routes while the forwarding process and DNS listener still live.
		if m.tx != nil {
			m.closeErr = m.tx.rollback()
		}
		if m.dns != nil {
			m.dns.Close()
		}
		if m.child != nil && m.child.Process != nil {
			_ = m.child.Process.Signal(os.Interrupt)
			select {
			case <-m.done:
			case <-time.After(2 * time.Second):
				_ = m.child.Process.Kill()
				<-m.done
			}
		}
	})
	return m.closeErr
}
