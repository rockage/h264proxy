# sing-box TUN frontend and comparison

This is an optional macOS frontend, independent of the unsigned Apple Network
Extension App. It needs administrator authorization, not an Apple Developer
membership. It does not change the gateway protocol or deploy a new server.

```text
applications -> macOS utun -> sing-box 1.14.4 / gVisor
            -> loopback H264Proxy SOCKS5 -> mux / WebRTC / ICE-TCP -> existing VPS
DNS -> sing-box -> Cloudflare DoH over the same H264Proxy SOCKS5 tunnel
```

## Build and select

```bash
bash scripts/build_tun.sh
bash scripts/build_singbox.sh

# Existing frontend
H264_TUN_SERVICE=Wi-Fi bash scripts/start_proxy.sh --tun

# New frontend; stop the previous client first
H264_TUN_SERVICE=Wi-Fi bash scripts/start_proxy.sh --sing-box
```

Both modes read `build/client.env`; environment variables take precedence.
Use the actual active network service (Wi-Fi or Ethernet). For the current
207.148.110.180 gateway, keep `H264_ICE_TRANSPORT=tcp`. The build pins the upstream
1.14.4 release with architecture-specific SHA256 values from GitHub release
metadata. No new Go dependency is added to the root module. The downloaded
sing-box GPL license is retained at `build/tools/sing-box.LICENSE`.

The CLI also accepts `-tun -tun-backend sing-box -sing-box /absolute/path`.
The original `-tun` default remains tun2socks. Both log `tun_ready` with an
explicit backend. Launcher logs are `build/logs/tun-BACKEND-client.log`.

## Routing, DNS and lifecycle

- sing-box owns TUN configuration and automatic routes; MTU is 1500, matching
  the existing frontend. gVisor is selected to avoid changing multiple variables
  in the first comparison. This is not a test of newer/custom sing-tun stacks.
- The parent pins and verifies signaling/ICE transport exceptions. The separate
  WebRTC process therefore stays on the physical network. The SOCKS outbound is
  explicitly bound to `lo0`, not the physical interface.
- sing-box listens for UDP/TCP DNS on `127.0.0.1:53` and hijacks TUN port-53
  traffic, including hard-coded DNS destinations. Its only upstream is
  `1.1.1.1:443`, with verified TLS name `cloudflare-dns.com`, through H264Proxy.
  There is no direct Internet outbound or system-DNS fallback.
- The IPv4 DNS policy matches the existing frontend: AAAA, HTTPS and SVCB get
  NOERROR/NODATA. General UDP/QUIC is rejected promptly; the existing backend
  still cannot relay it. IPv6 routes are captured, not a promise of IPv6 egress.
- `dns_mode=disabled` prevents sing-box from independently changing system DNS.
  Instead the parent journals the active service's original DNS, sets it to
  `127.0.0.1` only after a successful sing-box DNS query, and restores it on exit.
- Startup waits for the interface **and** an effective public route into it,
  verifies transport exceptions, then checks DNS before reporting ready.
- Both frontends share `/Library/Application Support/H264Proxy/tun-state.json`
  and cannot run together. Normal stop interrupts the child for graceful route
  cleanup. Recovery verifies the orphan's complete command, interrupts it and
  then restores the journal. A forced child kill may interrupt automatic cleanup;
  verify actual routes after any abnormal termination.
- Other VPNs/TUNs must be stopped. Read-only launcher checks reject conflicting
  routes before gateway restart. Switching Wi-Fi/Ethernet, sleep or permanent
  transport failure currently requires restart; no automatic roaming is added.

Manual recovery remains:

```bash
sudo ./build/proxy -tun-recover
```

## Full-system comparison

Run as your normal user, after stopping the existing H264 client and other
VPNs (including v2rayN). The script prompts for sudo; do not run the entire
script as root, because SSH needs your normal user's key. Once started it runs
independently; the chat does not need to remain connected.

```bash
bash scripts/compare_tun.sh
```

If a phase fails after earlier phases completed, resume from the same directory:

```bash
bash scripts/compare_tun.sh --resume /absolute/path/to/build/tun-compare-TIMESTAMP
```

The original JSON and failed-phase console are retained. The report records the
resume time; the resulting ABBA sequence has a pause and is not a continuous
trial. Isolated DNS failures are counted and retained while throughput testing
continues; a domain failing all three A lookups stops the phase.

The script checks the physical route, starts a fresh session on the configured
gateway, and runs ABBA (tun2socks, sing-box, sing-box, tun2socks). Each phase uses:

- The same client binary, endpoint, key, ICE-TCP, SOCKS address, device and MTU.
- No explicit HTTP/SOCKS proxy; curl also ignores proxy environment variables.
- A public-route check plus an egress-IP check against the configured VPS.
- First and repeated DNS A lookups, IPv4-policy checks, and an additional
  hard-coded DNS hijack check for sing-box.
- Two serial and two concurrent 4 MiB HTTPS downloads from Cloudflare's speed
  endpoint, recording TTFB, DNS/TLS time, transfer time and throughput.
- Length verification and recorded SHA256 per response. Cloudflare generates
  responses; their hashes are **not** expected equal. This is not a fixed-file
  end-to-end checksum benchmark.
- Stop/recovery and comparison of service DNS and transport route with the
  initial state. Startup/routing failure, a domain failing all three A queries,
  or failed restoration stops later stages. Individual DNS/download failures
  remain in the report while the other measurements continue.

Raw logs, measurements and summary are under `build/tun-compare-TIMESTAMP/`.
The public CDN is an additional source of variation. Short ABBA trials can
identify large differences and functional failures; they do not establish
YouTube playback performance or a guaranteed throughput advantage. Baseline
and new mode must both succeed before drawing a performance conclusion.

## Automated validation and current limits

```bash
H264_SINGBOX_TEST_BINARY="$PWD/build/tools/sing-box" go test -race ./internal/tun
go test -race ./...
go vet ./...
```

The real-executable integration validates production config syntax, DNS over
UDP/TCP, filtered record types and a SHA256-verified TCP payload through
sing-box -> SOCKS -> local fixtures. It substitutes a mixed ingress for TUN and
uses an unprivileged DNS port; it does **not** validate macOS route takeover.
The test-only local DoH fixture disables certificate verification; the generated
production config does not.

The full-system ABBA comparison was completed on 2026-10-31 via the user's
interactive terminal, with v2rayN stopped and physical Wi-Fi-to-VPS ICE-TCP.
Both frontends took over the public route, exited through 207.148.110.180, and
restored service DNS and transport routing after each measured phase. This
validates normal lifecycle on this machine, not forced-crash or network-roaming
recovery. See [measured results and limitations](SINGBOX_COMPARISON.md). No
general performance advantage is established; the old default is retained.
