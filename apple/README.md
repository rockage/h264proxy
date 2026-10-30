# Native macOS Packet Tunnel — unsigned development build

This frontend keeps the existing WebRTC / ICE-TCP gateway protocol. It replaces
CLI-managed virtual-interface configuration with Apple's Network Extension APIs.
The current deliverable is a compiled **unsigned** App and Packet Tunnel App
Extension. It cannot install or start a system VPN until it has appropriate
signatures, provisioning profiles, entitlements and user approval.

## Build and verify

On macOS with Go and Apple Command Line Tools:

```bash
bash scripts/build_ne.sh
bash scripts/verify_ne.sh
```

Output: `build/apple/H264Proxy.app`. The bundle contains a native SwiftUI host,
`Contents/PlugIns/H264PacketTunnel.appex`, and the statically linked Go engine.
The archive and build metadata are in `build/apple/`. The build uses the native
host architecture and a macOS 13 deployment target. It does not need Xcode to
produce the unsigned bundle; the Xcode project is supplied for later development
signing. Opening the unsigned App displays configuration and a disabled Connect
button. The build does not register or activate the extension.

```bash
open build/apple/H264Proxy.app
```

The production CLI entrypoints remain available:

```bash
bash scripts/start_proxy.sh --bypass
H264_TUN_SERVICE=Wi-Fi bash scripts/start_proxy.sh --tun
```

`--tun` still selects the existing script-managed frontend. It is not silently
redirected to an unsigned Network Extension.

## Data path

```text
NEPacketTunnelProvider.packetFlow
  -> public readPackets API -> C ABI -> bounded gVisor channel endpoint
  -> user-space TCP termination -> existing mux CONNECT protocol
  -> SCTP / DTLS / active-only ICE-TCP -> existing gateway
  -> returned IP packets -> public writePackets API

system DNS 198.18.0.2 (UDP or TCP)
  -> packet stack -> existing DoH resolver/cache/IPv4 policy
  -> the same encrypted gateway tunnel
```

There are no subprocesses, local SOCKS/DNS listeners, private utun descriptor
lookups, `sudo`, `networksetup`, `route` commands or shell recovery journals in
the extension frontend. The packet stack uses pinned tun2socks core/gVisor
components, not the standalone tun2socks executable. Its dependencies live in
an isolated Go module under `apple/engine/`; the root CLI module's dependencies
are unchanged. Dependency license texts are bundled in `Contents/Resources`.

The engine first connects to the gateway and checks tunneled DoH. The provider
then applies `NEPacketTunnelNetworkSettings` and reports success only after the
system completes that operation. The settings capture IPv4 and IPv6 default
routes, exclude the literal IPv4 gateway endpoint, set MTU 1500 and configure
in-tunnel DNS. Shared key bytes stay in the shared Keychain; preferences contain
an opaque persistent reference, endpoint and schema version only. The key is
not bundled into the application.

The C ABI returns a cancellable handle before network setup completes. Stop
cancels startup, unblocks packet reads, drains workers and closes owned resources.
Provider generations prevent delayed completions from restarting a stopped
session. Terminal transport errors cancel the system tunnel. `NWPathMonitor`
records changes; this first version requires explicit reconnect after terminal
failure or sleep. It does not implement WireGuard-style automatic roaming or
on-demand reconnection.

Current data-plane scope matches the working CLI frontend: TCP and tunneled
DNS; generic UDP/QUIC is unsupported. IPv4 DNS policy suppresses AAAA and
HTTPS/SVCB hints, and IPv6 capture prevents accidental physical-path bypass.
An IPv6-only target is not guaranteed reachable from an IPv4-only gateway.

## Future signed development build

Open `apple/macos/H264Proxy.xcodeproj` in full Xcode. Its two targets are
`H264Proxy` and `H264PacketTunnel`. The extension's build phase builds the Go
archive via `scripts/build_ne.sh --engine-only`.

1. Copy `macos/Config/Local.xcconfig.example` to `Local.xcconfig` and select a
   development team. Create matching App IDs and provisioning profiles.
2. Enable Network Extension / Packet Tunnel and matching shared Keychain access
   for both targets. The supplied bundle identifiers start with
   `com.roca.h264proxy.mac`; change them consistently if registering your own IDs.
3. Build with development signatures and profiles. Import the existing 32-byte
   gateway key through the App's file picker, then connect and accept the system
   VPN configuration prompt.
4. Verify DNS/routing during connection, stop, startup cancellation, network
   changes, sleep/wake, provider crash and restart on a signed installation.

This project currently packages an **App Extension** for development signing.
Developer ID direct distribution may require the **System Extension** deployment
form, different entitlements and activation flow. Do not treat the unsigned
bundle or an ad-hoc signature as a runnable VPN. Use Apple's deployment matrix
before choosing a distribution/signing path:

- [TN3134: Network Extension provider deployment](https://developer.apple.com/documentation/technotes/tn3134-network-extension-provider-deployment)
- [NEPacketTunnelProvider](https://developer.apple.com/documentation/networkextension/nepackettunnelprovider)
- [Network Extension entitlement](https://developer.apple.com/documentation/bundleresources/entitlements/com.apple.developer.networking.networkextension)
- [TN3120: Expected packet tunnel use cases](https://developer.apple.com/documentation/technotes/tn3120-expected-use-cases-for-network-extension-packet-tunnel-providers)

## Validation boundaries

Automated checks cover the listener-free gateway dialer, context lifetime,
active-only ICE TCP, an in-memory IP-packet/DNS/TCP round trip with payload and
FIN verification, DNS under exhausted TCP capacity, engine startup cancellation,
C ABI validation/stale handles/repeated stop, Swift compilation, linkage,
bundle metadata and the unsigned UI gate. No signed system VPN has been launched
on this machine. The Xcode project's property-list syntax has been checked;
full `xcodebuild`, provisioning, OS approval and physical-interface takeover are
pending an Apple signing environment.

The project file is generated deterministically by:

```bash
python3 scripts/generate_ne_project.py
```
