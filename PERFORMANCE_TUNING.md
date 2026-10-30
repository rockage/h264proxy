# Performance tuning — 2026-10-30

Baseline saved before edits: local commit `ae44556`, tag `stable-pre-performance-20261030`.

Retained changes: bounded pacing credit after timer oversleep; one unambiguous RTT sample per ACK (Karn preserved); fast retransmit requires a gap of at least three sequence positions. Wire format, encryption, congestion control and rate ceiling are unchanged.

## WAN comparison

Mac to Linux amd64 VPS 207.148.110.180, TUN disabled. Independent WebRTC session on UDP 19301; signaling forwarded over SSH, media remains direct UDP. One stream in each direction, 2,097,152 payload bytes per stream, AEAD, 16,000,000 bytes/s pacing ceiling. Production port 9300 was not restarted during these tests.

| Variant | Workload seconds (client) | Client retransmits | Server retransmits |
| --- | ---: | ---: | ---: |
| Baseline A | 25.358 | 255 | 688 |
| Retained changes A | 11.458 | 82 | 445 |
| Baseline B | 25.465 | 187 | 581 |
| Retained changes B | 16.333 | 147 | 265 |
| Rejected: RTO floor 200 ms | 32.164 | 33 | 632 |
| Rejected: 500 KB/s rate ceiling | 23.246 | 66 | 593 |

All workloads completed with byte/hash verification in both directions. The retained version improved these particular WAN runs; this is a small sequential sample, not a guaranteed multiplier or a controlled link-capacity measurement. Workload time includes both simultaneous directions; it must not be presented as single-direction HTTP download speed. Retransmission remains substantial, especially downlink. No stable 1080P/4K playback claim is made.

Increasing the RTO floor to 200ms and lowering the fixed rate ceiling did not improve the overall workload and were not retained. Existing 100ms–2s adaptive RTO limits remain unchanged.

## Verification

`go test -race ./...`, `go vet ./...`, `go build ./...`, `git diff --check` passed. Added deterministic tests for timer credit/rate bounds, mild reordering vs fast retransmit, duplicate ACKs, ACK batching and Karn sampling. Existing induced-loss, half-close and two-process WebRTC proxy tests passed. Raw reports are local ignored artifacts under `build/perf-tuning/`.

These changes require updating both endpoints to affect both directions. Do not disable congestion control or ARQ to raise a benchmark score.


## Deployment and HTTP smoke checks

Deployed source commit `0866f63` on both endpoints after the comparisons. The
VPS passed native Go mux tests and native Linux/amd64 compilation. Saved its
old binary as `/opt/h264proxy/bin/proxy.preperf-20261030`, the old Mac binary
as `build/perf-tuning/proxy-before`, and retained the source checkpoint tag.
No key or firewall persistence changes were made; temporary test rules and
remote test files were removed. The gateway and local SOCKS listener are active.

Using the same Cloudflare HTTPS download endpoint through production SOCKS:

| Check | Time | Average payload speed |
| --- | ---: | ---: |
| Before update, 1,000,000 bytes | 6.005 s | 166,529 B/s |
| After update, 1,000,000 bytes | 1.291 s | 774,724 B/s |
| After update, 5,000,000 bytes | 24.428 s | 204,683 B/s |

All returned HTTP 200; an additional SOCKS HTTPS IP check returned the correct
VPS address. These results show improved short-transfer behavior but also show
that sustained throughput is still low. A 4.6x short-transfer observation must
not be represented as a sustained speed multiplier. Stable 1080P playback has
not been verified. The updated client was left running for manual testing.
