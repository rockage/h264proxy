# DataChannel proxy migration

`cmd/proxy` uses a single negotiated reliable, ordered WebRTC DataChannel
(`proxy-v1`, SCTP stream ID 0). The transport is SCTP → DTLS → ICE/UDP. No video
track, RTP payload or H.264 codec is registered by the proxy. Negotiation must
advertise `transport: datachannel-v1`; old RTP peers are rejected explicitly.
The existing module/repository names remain stable.

The H.264 proxy is frozen at checkpoint `a7d1a5f`, local tag
`h264-rtp-final-20261030`. Historical file-transfer and mux demonstration
commands remain lab tools; they are not a proxy fallback.

## Transport responsibilities

- SCTP provides reliable ordered delivery and congestion control. Application
  selective retransmission and Reno are disabled on this transport, including
  during slow delivery. SCTP retransmissions are not counted by mux counters.
- Mux preserves stream IDs, per-stream flow credit (256 KiB initial window),
  DATA/CLOSE/RESET, half-close, delivery ACKs for metadata reclamation, and
  per-direction HKDF/AES-256-GCM authentication using the existing shared key.
- DATA plaintext chunks grow from 900 bytes to 16 KiB. Each encrypted frame is
  one DataChannel message; the adapter caps messages at 32 KiB.
- Detached DataChannel reads backpressure the receive side. A sole mux writer
  checks SCTP queued bytes, waits above 256 KiB and resumes at 64 KiB. Closing
  the transport wakes blocked writers/readers. Mux queues and stream credit
  additionally bound application buffering. SCTP/kernel buffers are separate.
- `-rate` defaults to 0 (no additional application pace); a positive value is
  still an optional ceiling. `mux_stats` logs transport and buffered bytes,
  rather than RTP gap counters. Mux RTT reflects application delivery feedback;
  its RTO/cwnd fields are legacy application estimators, not SCTP statistics.
- Invalid authenticated framing closes a reliable session instead of silently
  discarding a message that the application will never retransmit.

This remains a single-client SOCKS TCP gateway. Remote DNS, limits, cancellation
and half-close are preserved. There is no automatic reconnect or SOCKS UDP
ASSOCIATE. One ordered channel can cause head-of-line blocking across streams.
Using DataChannel alone does not guarantee a speed increase or video camouflage.

## Upgrade both peers

Build on each platform using its native Go environment:

```bash
go build -o build/proxy ./cmd/proxy
```

Keep the shared key, signaling port and ICE UDP port. The Linux installer now
checks for SCTP/DataChannel SDP instead of H.264. Stop the old Mac client,
install the new gateway binary, restart `h264proxy.service`, then start the
new Mac client. A gateway restart creates a fresh single-client SDP session.

```bash
./build/proxy -role client -net -http YOUR_VPS_IP:9300 \
  -key build/server.key -socks 127.0.0.1:1080 \
  -stun stun:stun.l.google.com:19302
```

Chrome keeps the same SOCKS5 address. Validate with remote DNS:

```bash
curl --noproxy '' --socks5-hostname 127.0.0.1:1080 https://api.ipify.org
```

A ready log must include `"transport":"datachannel-v1"`. `/offer` must include
`m=application` and `webrtc-datachannel`, with no `m=video`. Reverting requires
restoring the H.264 binary on both peers and restarting the gateway/client.

## Verification

`go test -race ./... -count=1`, `go vet ./...`, `go build ./...` and `bash -n
install.sh` cover this migration. The real two-process proxy test checks SCTP
SDP with no video, concurrent HTTP body/hash transfers exceeding flow credit,
remote-DNS policy, TCP FIN before response, clean shutdown, stream reclamation,
zero authentication failures and zero application retransmissions. Additional
adapter tests cover blocked-send drain, cancellation and message bounds. Mux
regression tests check that even hour-old delivery metadata never invokes
application loss recovery in reliable mode.

## WAN check — 2026-10-30

An isolated gateway on the existing VPS and a test SOCKS listener returned the
VPS exit IP. A 20,000,000-byte HTTPS download returned HTTP 200 in 16.712s
(1,196,745 B/s, about 9.57 Mbps). Both endpoints reported zero mux decode errors
and zero application retransmissions. These are not SCTP loss counters. The
current Mac route includes a TUN interface; routing/other proxies were unchanged.
This sequential sample does not establish stable video playback or a guaranteed
speed advantage over RTP. The public `h264-rtp-final-20261030` branch preserves
the old carrier for rollback.
