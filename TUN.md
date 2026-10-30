# macOS TUN frontend (first stage)

An additional sing-box frontend is now selectable with `--sing-box`; the
existing `--tun` remains tun2socks. See [sing-box setup and controlled
comparison](SINGBOX_TUN.md). Both reuse the same transport and recovery journal.

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

## Reliable transport queue and receive-buffer optimization

Reliable DataChannel sessions coalesce queued ACK and WINDOW updates by stream
and frame type. Cumulative acknowledgment and credit cannot regress; final
close acknowledgments and selective ranges survive merging. A FIFO of distinct
keys preserves fairness, with DATA admitted after eight feedback frames. The
queue has a 4096-entry memory bound; exceeding it terminates the session rather
than silently losing credit or blocking the receive thread behind network I/O.
Legacy RTP feedback and recovery are unchanged. `FeedbackCoalesced` in live mux
statistics counts updates replaced by newer cumulative state.

The mux uses an optional caller-owned receive-buffer interface for DataChannel
messages. It consumes/decrypts each message before the next read; retained stream
data is copied. Existing `ReadPayload` callers still receive independent storage.
The new receive interface eliminates the former 32 KiB allocation per message;
it does not eliminate decryption, reassembly or other transport allocations.

2026-10-30 staged measurements used a SHA256-verified 4 MiB file served on the
207.148.110.180 gateway's loopback interface, with three serial downloads then
four concurrent downloads. Both client and gateway were switched together.
The client-to-gateway path traversed v2rayN to keep the chat connected, so these
are SOCKS tunnel comparisons under that route, not physical direct-path or
full-system TUN benchmarks. Short sequential trials are sensitive to network
variation and SCTP warm-up; do not interpret their differences as a guaranteed
throughput improvement. Raw results are in the ignored `build/perf-20261030/`.

Transport bypass routes are now pinned and verified before the initial DoH
probe, then verified again after split-default takeover. This protects the
startup probe from later broader route changes and reports conflicting VPN
routes before attributing failure to DNS. On macOS a scoped cloned lookup can
hide an existing unscoped static /32; the routing table is checked so an existing
user route is reused without being claimed for cleanup. `tun_transport_route`
records the route before the probe and again if it fails. Connection events and
live counters include selected ICE endpoints; setup errors also identify a
transport that is no longer connected. These diagnostics do not establish why
a historical UDP path failed or guarantee recovery from every network change.

### ICE fails before DNS: check the actual UDP listener and host firewall

On 2026-10-30 the 207 gateway listened on TCP 9300 and UDP 9300, but its native
nftables input chain had a drop policy and allowed TCP 9300 plus UDP 9301 only.
An explicit UDP 9300 accept rule was added live and to
`/etc/sysconfig/nftables.conf`, with the original configuration backed up and
`nft -c -f` validation performed before saving. Existing rules were preserved;
the full ruleset was not flushed or reloaded. Three probes bound to the client
physical Wi-Fi reached the new rule (counter 0 to 3), and tunneled DoH/HTTPS
checks passed. A historical example using UDP 9301 is not the live service's
configuration. Check the unit/environment, `ss -lunp`, and the active nftables
rules together; successful TCP signaling does not establish UDP reachability.

## Explicit ICE TCP mode for a poor UDP path

`-ice-transport tcp` selects ICE-TCP on both peers. The gateway also sets
`-ice-tcp-port 9301`, separate from TCP 9300 HTTP signaling. `H264_ICE_TRANSPORT=tcp`
in `build/client.env` makes both client launchers select it; the general default
remains UDP. The deployment unit reads `ICE_TRANSPORT` and `ICE_TCP_PORT` from
`gateway.env`, which the install helper now writes. TCP mode does not gather
UDP STUN candidates. The selected path is logged as `ice_protocol: tcp`.

On 2026-10-30 a physical Wi-Fi-to-207 UDP control (one stream, no TUN frontend)
received only 1,667,072 bytes in 60 seconds; unordered SCTP message delivery
received 1,449,984 bytes in 45 seconds and did not solve the problem. Production
therefore retains ordered, reliable delivery. A same-file ICE-TCP control
completed 4,194,304 bytes in 11.552627 seconds (about 2.90 Mbps), SHA256 verified,
with concurrent DoH/IPv4 DNS checks passing. This is a measured alternative for
that path, not a claim that ICE-TCP is faster on every network. Both still use
SCTP/DTLS and mux encryption. UDP mode remains selectable after updating both
peer configurations. Full-system TUN performance still needs user validation.

## Explicit startup switches

`bash scripts/start_proxy.sh --bypass` starts the existing SOCKS-only client;
`H264_TUN_SERVICE=Wi-Fi bash scripts/start_proxy.sh --tun` starts the TUN client.
The launcher requires exactly one switch and rejects mixed/unknown options
before any sudo or gateway restart. The binary also accepts explicit `-bypass`
and `-tun` client flags and rejects enabling both. Startup logs `client_mode`
with `bypass` or `tun`. Stop the old client before switching modes. The bypass
launcher checks for an occupied SOCKS listener before restarting the gateway,
matching the TUN launcher's existing guard.

The client reserves its SOCKS listener before opening a WebRTC session. An
occupied address therefore fails locally without consuming the gateway's
single-peer signaling session. Both launchers test actual socket binding before
their gateway restart step. `socks_reserved` identifies the initial reservation;
`socks_listening` is emitted only after tunnel setup and indicates readiness.
