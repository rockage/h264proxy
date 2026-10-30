# macOS TUN frontend (first stage)

The optional TUN frontend reuses the existing SOCKS5 client and WebRTC
DataChannel transport. The VPS gateway and wire protocol do not change.
The default SOCKS-only mode keeps its existing remote-domain-only policy.

```text
application IP packets
    -> macOS utun
    -> tun2socks v2.6.0 / gVisor
    -> loopback SOCKS5
    -> existing WebRTC DataChannel + mux
    -> existing VPS TCP gateway

system DNS -> 127.0.0.1:53 (UDP or TCP)
    -> TTL cache / identical-query coalescing
    -> HTTP/2 DNS-over-HTTPS through the same SOCKS5 / WebRTC tunnel
    -> 1.1.1.1:443, TLS name cloudflare-dns.com
```

## Scope

- Automatic configuration currently supports **macOS only**, using an explicit
  active network service and an unused `utun` device.
- TCP connections are proxied. IPv4 and IPv6 split-default routes enter the
  stack to prevent direct IPv6 bypass. DNS defaults to IPv4-only: AAAA,
  HTTPS and SVCB queries receive local NOERROR/NODATA, so applications use A
  records instead of unusable IPv6 addresses or service-binding IPv6 hints.
  IPv6 literals and cached IPv6 addresses still require VPS IPv6 connectivity.
  `-tun-ipv6` opts into unfiltered DNS only for a gateway with working IPv6.
- **Generic UDP, QUIC and ICMP are unsupported.** UDP sent into this frontend
  cannot be relayed by the existing TCP-only SOCKS gateway. Applications may
  fall back to TCP; applications requiring UDP will fail.
- System DNS for the chosen network service goes through encrypted DNS over
  the existing tunnel. There is no direct DNS fallback. Hard-coded UDP DNS
  destinations are not transparently rewritten to the local resolver.
- More-specific routes, such as local LAN/link-local routes, stay direct.
  Signalling, known remote ICE candidates and STUN addresses are also direct.
  Other VPNs, scoped DNS resolvers, multiple active network services, existing
  custom routes, and applications explicitly binding a physical interface are
  outside the first-stage coverage. This is **not a strict traffic lockdown**.
- TUN mode accepts IP-only SOCKS requests because IP packets do not retain the
  original hostname. It automatically sets `RequireDomain=false` for its local
  SOCKS listener. Fake-IP mapping is not implemented.
- On failure/normal shutdown, the client restores routes and DNS and returns
  to the machine's ordinary connectivity. There is no persistent kill switch.
- Network changes while running (Wi-Fi/Ethernet switching, sleep/resume,
  DHCP gateway changes) require stopping and restarting the TUN client.

## Build

From the repository root, as the normal user:

```bash
bash scripts/build_tun.sh
```

This builds `build/proxy` and installs **tun2socks v2.6.0** into
`build/tools/tun2socks`. The helper is a separate executable: the project's
`go.mod` and the existing server's dependencies are unchanged. Source:
[xjasonlyu/tun2socks](https://github.com/xjasonlyu/tun2socks/tree/v2.6.0).

## Run

The launcher does not choose a historical test VPS by default. Set the current
endpoint and SSH destination in the local ignored file `build/client.env`,
using `scripts/client.env.example` as a template. For this checkout the selected
endpoint is `207.148.110.180:9300`, SSH is `root@207.148.110.180`, and its matching
key is kept separately at `build/proxy-207.148.110.180.key`. Environment variables
override the file; `H264_CONFIG` selects another configuration file. Values are
literal (no shell execution or variable expansion).

Both TUN and SOCKS launchers read the same file and print the selected target.
An absent endpoint is an error before sudo or SSH. Automatic gateway restart
also requires an explicit SSH destination; use `H264_RESTART_GATEWAY=0` when
restarting is unnecessary. Existing background SOCKS jobs read this same file
when launched again.

Stop the existing local SOCKS client first. Check the active network service:

```bash
networksetup -listnetworkserviceorder
route -n get default
```

For example, if the default route uses the device belonging to `Ethernet`:

```bash
H264_TUN_SERVICE=Ethernet bash scripts/start_tun.sh
```

Or use `scripts/start_tun.command`, which prompts for the service name. The
launcher runs SSH as the normal user and requests sudo only for the client.
It restarts the configured single-client VPS gateway for a fresh session,
following the existing SOCKS launcher. Set `H264_RESTART_GATEWAY=0` if a fresh
gateway is already waiting and no remote service restart is needed.

Existing launcher settings are supported: `H264_SSH`, `H264_ENDPOINT`,
`H264_KEY`, `H264_SOCKS`, `H264_STUN`. `H264_TUN_DEVICE` defaults to `utun123`.
Set `H264_KEY` to the key paired with the selected VPS. Without an explicit key
setting the legacy path remains `build/proxy-vps.key`; it must match that VPS.

To run the client directly against an already prepared gateway:

```bash
sudo ./build/proxy -role client -net \
  -http YOUR_VPS_IPV4:9300 -key ./build/proxy-vps.key \
  -tun -tun-service Ethernet -tun-device utun123 \
  -tun2socks "$PWD/build/tools/tun2socks"
```

The client first establishes WebRTC, starts SOCKS and verifies DNS-over-HTTPS
through the tunnel. It installs durable unscoped static host exceptions (a
cloned/scoped HOST entry is not sufficient), then adds the TUN split routes.
After takeover it rechecks the effective endpoint interfaces and performs
another uncached DoH health check before switching system DNS or declaring
TUN ready. A failed post-takeover check rolls back the changes.
Look for the structured `tun_ready` event. Keep the terminal open.

**Ctrl-C stops the client and restores its network changes.** The existing
`stop_client.sh` controls the separate SOCKS launcher, not this foreground
sudo session. Do not run both launchers simultaneously.

## YouTube startup optimization

The DNS forwarder explicitly enables HTTP/2 over its SOCKS connection and
reuses idle connections for up to 90 seconds. Successful, non-truncated positive
answers are cached for their minimum record TTL, with a five-minute maximum
local retention and a 512-entry limit. Cache hits adjust response IDs and age
record TTLs without changing EDNS flags. Simultaneous identical queries share
one upstream request. Failed/zero-TTL/negative responses are not cached.

The reliable DataChannel mode no longer repeats ACK/WINDOW recovery heartbeats
for idle streams. New data is still acknowledged, consumption still replenishes
the cumulative window, and CLOSE/RESET acknowledgments remain intact. Legacy
RTP mode retains its loss-recovery heartbeats. The wire format is unchanged;
update both client and gateway to remove redundant traffic in both directions.

The old SOCKS Chrome launcher already disables QUIC. For a comparable TUN-only
browser window (no explicit proxy, QUIC disabled), run after starting TUN:

```bash
bash scripts/chrome_tun.command
```

To compare with QUIC enabled, use a second separate profile:

```bash
H264_TUN_QUIC=1 bash scripts/chrome_tun.command
```

These flags apply only to the launched test profiles. The TUN gateway still
cannot forward UDP. QUIC fallback as a cause of playback delay has not been
confirmed by packet capture, and actual YouTube time-to-play remains a manual
browser comparison.

Historical validation on 2026-10-30 against the Nuremberg node (not the current
`207.148.110.180` target): real HTTPS
forwarding exited through `152.53.13.153`; DoH negotiated HTTP/2. A YouTube A
lookup took about 749 ms cold and 10–49 microseconds from this process's cache;
`i.ytimg.com` took about 290 ms cold and 16–20 microseconds cached. These are
spot checks, not controlled measurements of video time-to-play. Sixteen real
idle HTTP connections through SOCKS/WebRTC produced zero additional mux frames
in a one-second observation. Race tests, the real two-process integration
suite, vet and builds passed. Actual YouTube playback remains to be compared
in the browser.

An optional DNS timing probe can use an already connected SOCKS client without
changing routes/DNS or restarting the gateway:

```bash
H264_TUN_LIVE_SOCKS=127.0.0.1:1080 go test -v ./internal/tun -run TestLiveDNS -count=1
```

## Sustained bandwidth status

See [TUN_BANDWIDTH.md](TUN_BANDWIDTH.md) for the historical Nuremberg-node throughput
and direct UDP-loss investigation. Those measurements do not apply to the
current `207.148.110.180` target. Stable 720P on the current target has not yet
been measured; experimental
window, message-size, pacing and ICE-TCP settings were not adopted. Actual SCTP
metrics are available under `sctp` in the updated source's `mux_stats` log.

## Brief ICE disconnections

A `disconnected` PeerConnection state no longer immediately closes SCTP on
either endpoint. ICE may recover the existing session while its connectivity
checks continue; `failed`, `closed`, explicit shutdown and real transport errors
still terminate the session. This is recovery of the current ICE session, not
automatic creation of a new session after a permanent failure.

On 2026-10-30 a controlled real-VPS test blocked the gateway's incoming and
outgoing UDP/9300 for seven seconds. Both peers transitioned from `connected`
to `disconnected` and back to `connected`; HTTPS forwarding resumed under the
same session ID without a gateway restart or client reconnect. The temporary
firewall rules were removed. This validates transient ICE recovery, not a
privileged simulation of macOS route takeover.

Regression tests also cover cloned/scoped host routes, checks after adding
split routes, rollback when an endpoint points back into utun, and health
checks that cannot be satisfied by a cached DNS response. Darwin route flag
meanings are defined in Apple's [XNU route.h](https://github.com/apple-oss-distributions/xnu/blob/main/bsd/net/route.h).

## Recovery

The client records changes in `/Library/Application Support/H264Proxy/tun-state.json` (root-only).
It preserves the existing default route, adds host exceptions and split
routes, and records the original DNS server list, including automatic/DHCP
configuration. Partial setup rolls back completed changes in reverse order.
A failed restore retains the remaining recovery operations.

The launcher now automatically attempts recovery when a previous journal is
present; recovery still refuses to modify a running instance. Routes already
removed with a vanished utun device and replacement cloned routes owned by
another VPN do not keep the journal stuck.

For manual recovery after an abnormal termination such as `kill -9`, run:

```bash
sudo ./build/proxy -tun-recover
```

Recovery refuses to operate while the recorded parent PID is still alive.
It restores routes/DNS and stops an orphaned tun2socks only when its PID and
complete command match the recorded process. The journal persists across reboot, so run the same recovery command if the
client did not shut down cleanly before a restart. As a manual fallback, for a
network that originally used DHCP-provided DNS:

```bash
sudo networksetup -setdnsservers Ethernet Empty
```

Replace `Ethernet` and restore the actual original static servers if applicable.
A custom `-tun-state` path must also be supplied to `-tun-recover`.

The recovery journal covers the installed configuration, not arbitrary
concurrent changes by another network manager. Do not edit routes/DNS or run
another VPN while this client is active.

## Verification

Automated checks (do not require root and do not change system routing):

```bash
go test -race ./...
go vet ./...
go build ./...
bash -n scripts/build_tun.sh scripts/start_tun.sh scripts/start_tun.command
```

New tests cover DNS-over-HTTPS through a real SOCKS handshake and local TLS
server, UDP truncation/TCP retry, repeated framed TCP DNS queries, invalid
answers, DNS failure without direct fallback, route exceptions, partial setup
rollback and retained recovery state. The existing integration suite checks
real two-process SOCKS/WebRTC forwarding and its original DNS policy.

Actual privileged macOS TUN/routing acceptance is a separate manual check:

1. Before starting, record `networksetup -getdnsservers Ethernet` and the
   ordinary public IP. Disable any application/system proxy so the test
   specifically exercises the TUN route.
2. Start the TUN client and wait for `tun_ready`.
3. Check `ifconfig utun123`, `scutil --dns`, and `route -n get 8.8.8.8`.
   The public destination should use utun; `route -n get YOUR_VPS_IPV4`
   should still use the original physical network.
4. Run `dig @127.0.0.1 example.com`, followed by the same query with `+tcp`.
   Run `curl --noproxy '*' --http1.1 -4 https://api.ipify.org`: the result
   should be the VPS egress IP. An ordinary application with no proxy
   configuration should also load a TCP HTTPS site.
5. Ctrl-C. Check that DNS equals the recorded original configuration, split
   routes are gone, the original public IP returns and the journal is gone.
6. In a controlled test, terminate the parent abruptly, run `-tun-recover`
   and verify the same restored state. Also test gateway disconnection.

As implemented, automated tests/builds have passed on macOS arm64. The real
privileged network takeover has **not yet been exercised**: the development
session has no password-free sudo access. No system routes or DNS settings
were changed during development.

DoH startup health checks retry a transient timeout or premature EOF once, on
a fresh connection after 200 ms. Both attempts use the same encrypted SOCKS
path and uncached DNS request; invalid answers and cancellation still fail
immediately. A persistent failure aborts setup and restores network state.

The TUN launcher saves structured client logs to `build/logs/tun-client.log`
and exit statistics to `build/logs/tun-client-report.json`. Runtime DNS failures
are logged as `tun_dns_query_failed` with the query name, duration and error.
After switching DNS, startup flushes both the directory-service DNS cache and
mDNSResponder. Applications with their own DNS caches or secure DNS settings
may require restarting or disabling their custom resolver to follow this policy.
