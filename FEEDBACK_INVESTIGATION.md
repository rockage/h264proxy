# Feedback and transport investigation — 2026-10-30

This is an **investigation branch**, not a production performance release. Main
and the running proxy were not replaced during this investigation. A local
checkpoint branch `checkpoint-before-feedback-fix-20261030` retains the starting
state (`756ae7d`). The earlier usable checkpoint tag also remains available.

## Concrete corrections under review

- ACK/WINDOW use a separate prioritized queue. After eight feedback frames,
  queued stream traffic gets a turn; OPEN/DATA/CLOSE/RESET retain their order.
  AEAD sealing and fresh retransmission nonces remain in the single writer.
- Newly received data requests prompt ACKs; idle, still-open streams use the
  slower heartbeat instead of requesting an ACK on every 50ms maintenance tick.
  Periodic ACK/WINDOW recovery for lost feedback is retained.
- Repeated copies of the same selective ACK cannot repeatedly trigger fast
  retransmit. A repeated transmission also gets at least an estimated RTT to
  return feedback before another fast retransmit attempt.
- Retransmission timers and RTT samples begin when the writer attempts the
  actual packet send, rather than when a frame enters its queue. Queued initial
  sends/retransmits cannot be scheduled for retransmission again. Frames ACKed
  or canceled while queued are discarded before sending. This does not measure
  time spent inside kernel/network buffers.
- The proxy logs `mux_stats` every five seconds (`-stats-interval 0` disables
  it). Statistics include queued stream/feedback frames, in-flight bytes,
  pacing ceiling, RTT/RTO, retransmissions and congestion losses. The stream
  queue also contains ordered OPEN/CLOSE/RESET frames.

An experimental cwnd/RTT pacing controller exposed a retransmission-queue
avalanche: a 1024-frame queue filled while queued packets were repeatedly
classified as network loss. The timing correction removes this mechanism in
regression tests. Adaptive pacing itself **did not improve the WAN result** and
was removed; the original rate ceiling and AIMD controller remain.

## Observations, not playback guarantees

The Mac route during these tests traversed `utun75`. These results describe that
current route; they are not measurements of a TUN-free physical UDP path. No
system routing, DNS or third-party proxy settings were changed. Signaling for
isolated tests used SSH forwarding; media/data used UDP 19301. Only a temporary
nftables rule was added and it was removed after each experiment.

A raw UDP echo probe used 900-byte packets and a random per-run marker. After
warming up the UDP association, each rate sent 1600 packets:

| Send rate (each direction) | Unique received on VPS | Unique echoes at Mac |
| --- | ---: | ---: |
| 360 KB/s | 1600 | 1600 |
| 720 KB/s | 1600 | 1600 |
| 1.44 MB/s | 1548 | 1548 |

These are short probes, not sustained bandwidth tests. The un-warmed probe
lost initial packets, so it was not used for the steady-flow comparison.

The four-stream bidirectional mux test sent 262144 payload bytes per stream
in each direction, with hash verification:

| Variant | Workload time | Client retransmits | Server retransmits |
| --- | ---: | ---: | ---: |
| Starting version | 2.414 s | 117 | 291 |
| Feedback changes, before writer-clock correction | 5.681 s | 89 | 10 |

Fewer retransmissions did **not** mean faster completion. This candidate was
not deployed based on that result.

HTTPS downloads from the same Cloudflare endpoint, through SOCKS:

| Variant | Bytes | Time | Average |
| --- | ---: | ---: | ---: |
| Existing production proxy | 5,000,000 | 22.678 s | 220,478 B/s |
| Feedback-only experiment | 5,000,000 | 15.760 s | 317,266 B/s |
| Adaptive pacing + writer-clock correction | 5,000,000 | 25.656 s | 194,885 B/s |
| Writer-clock correction, fixed pacing | 5,000,000 | 19.684 s | 254,014 B/s |

These small sequential samples do not establish stable 720P/1080P playback or
justify another automatic production rollout.

## Independent transport controls

| Transport | Bytes | Time | Average |
| --- | ---: | ---: | ---: |
| SSH/TCP random-data download | 20,000,000 | 4.825 s | 4.15 MB/s |
| Reliable WebRTC DataChannel | 5,000,000 | 10.193 s | 0.49 MB/s |
| Reliable WebRTC DataChannel | 20,000,000 | 19.640 s | 1.02 MB/s |

DataChannel payloads passed SHA-256 verification. Its timing excludes ICE
setup and begins at the first received payload; SSH timing includes setup.
The transports, transfer sizes and timing boundaries differ, so this is a
control experiment, not an apples-to-apples protocol ranking or guaranteed
speed multiplier. A DataChannel uses SCTP/DTLS, **not H.264 RTP**. It is not a
replacement implementation of the project's media-carrier requirement.

`cmd/dcbench` preserves the isolated control experiment. It is a benchmark,
not a SOCKS proxy. On both machines, build it from this module:

```bash
go build -o dcbench ./cmd/dcbench
```

On the test VPS (allow the chosen UDP test port temporarily yourself):

```bash
./dcbench -role server -http 127.0.0.1:19300 \
  -public-ip YOUR_PUBLIC_IPV4 -udp-port 19301 -size 20000000
```

On the Mac, keep signaling forwarding open in a separate terminal:

```bash
ssh -N -L 127.0.0.1:19300:127.0.0.1:19300 root@YOUR_SERVER
./dcbench -role client -http 127.0.0.1:19300
```

The second command runs in another terminal. Stop forwarding and remove your
temporary test firewall rule afterward. Do not expose the benchmark signaling
service publicly. It exchanges no project shared key and does not modify an
existing gateway session.

## Validation and next decision

Race tests (`go test -race ./... -count=1`), vet, builds and whitespace checks
passed. New regression tests cover feedback bypass of a full stream queue,
feedback fairness/order, idle ACK behavior, stale SACKs, RTT waiting, queued
frame timing and stale writer entries. Existing loss/RESET/half-close and real
two-process WebRTC tests passed. Linux-native mux tests also passed during the
isolated experiments. Raw logs/reports are local ignored files under
`build/diagnostics2/`.

The next product decision is whether H.264 RTP carriage is mandatory. Keeping
it requires continued work on the reliable media transport. Prioritizing speed
allows a separate DataChannel proxy backend, but that changes the carrier and
must not be described as H.264-video payload transport. No carrier switch has
been made here.


## Decision and follow-up

The user selected mandatory H.264/RTP carriage. No DataChannel proxy backend
will be substituted. Continued RTP recovery work and subsequent test results
are documented in [RTP_REORDER_FIX.md](RTP_REORDER_FIX.md).
