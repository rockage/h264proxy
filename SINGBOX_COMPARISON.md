# macOS TUN comparison — 2026-10-31

sing-box 1.14.4 has been integrated as an optional TUN/DNS/automatic-route
frontend for the existing H264Proxy transport. The first real-system comparison
does **not** establish an overall speed or reliability advantage. Keep the
existing tun2socks frontend as the default; sing-box is available for evaluation.

## Conditions and evidence

- Same local client build, shared key, 207.148.110.180 gateway and ICE-TCP
  endpoint (TCP/9301). No gateway binary/protocol replacement for this work.
- macOS physical Wi-Fi (`en1`), v2rayN stopped, `utun123`, MTU 1500, gVisor.
- ABBA: tun2socks, sing-box, sing-box, tun2socks. The trial was resumed after
  failures, so there are pauses between phases; this is not a continuous trial.
- Each phase: two serial and two concurrent 4 MiB Cloudflare HTTPS downloads;
  no explicit proxy; egress verified as 207.148.110.180.
- All 15 successful downloads have the same observed SHA256:
  `7563843dc61195c0c2991aa93be05ca667d1c819ce6745746668795895be8f1c`.
  The test endpoint generates responses; this matching hash is an observation,
  not a guarantee about future responses.
- Raw report: `build/tun-compare-20261031-010010/comparison.json`; per-phase
  console logs and snapshots before resuming are in that directory.
- DNS and transport route restoration checks passed after every measured phase.

## Aggregate results

| Metric | Existing tun2socks | sing-box |
|---|---:|---:|
| Successful serial download median | 2.35 Mbps | 2.95 Mbps |
| Mean aggregate concurrent throughput, successful pairs | 3.21 Mbps | 2.56 Mbps |
| Failed downloads / attempts | 0 / 8 | 1 / 8 |
| First DNS A lookup median | 257.9 ms | 179.1 ms |
| Repeated DNS A lookup median | 11.2 ms | 10.3 ms |
| DNS A failures in retained measured phases | 0 / 18 | 0 / 18 |
| Mean startup time | 2.87 s | 2.86 s |

The serial statistic includes only successful transfers. Read it alongside
the failure count; excluding the failed transfer is not evidence of better
overall performance. sing-box's successful serial median was 25.7% higher,
but concurrent aggregate throughput was 20.2% lower.

## Phase variation

| Phase | Frontend | Serial Mbps (two attempts) | Concurrent aggregate Mbps |
|---|---|---|---:|
| A1 | tun2socks | 1.13, 0.64 | 1.38 |
| B1 | sing-box | incomplete, 4.67 | 2.79 |
| B2 | sing-box | 2.95, 2.57 | 2.34 |
| A2 | tun2socks | 4.55, 3.57 | 5.04 |

The old frontend's concurrent throughput varied from 1.38 to 5.04 Mbps—larger
than the aggregate difference between frontends. Background traffic also
varied: live mux samples peaked at 37, 19, 46 and 30 active streams across
the phases. Public-CDN/network variation, background applications and trial
pauses prevent attributing the aggregate difference to frontend code alone.

## Functional results and unresolved failures

- Both frontends correctly routed test traffic through TUN and the expected VPS.
- IPv4 DNS policy passed: AAAA/HTTPS/SVCB returned NOERROR with no answers.
- sing-box successfully hijacked DNS addressed directly to 8.8.8.8. This is a
  functional addition over the old frontend's local-resolver-only setup.
- A prior sing-box attempt had one 10-second UDP DNS timeout for i.ytimg.com;
  the next two queries succeeded. It remains in `prior_attempts` and the
  preserved console log. The final 18/18 figure does not erase this failure.
- One sing-box download closed with 4,148,569 bytes still expected, after about
  45 KiB. curl reported error 18. A local SOCKS write reported broken pipe
  around that time. These logs locate a premature close in the forwarding
  chain, but do not isolate sing-box, the backend relay or the upstream as its
  cause. The other seven downloads completed.
- There was no sustained transport-session failure during that download;
  subsequent transfers continued on the same client session.
- Earlier test-harness failures (TTY-scoped sudo authorization and old macOS
  dig not recognizing HTTPS/SVCB names) were corrected and are not frontend
  performance failures. The test now uses TYPE65/TYPE64 and retains the TTY.

## Decision

Keep `--tun` as the default and `--sing-box` as an explicit alternative. The
new frontend adds integrated DNS hijacking and automatic routing, but this
round does not justify switching the default on speed grounds. Before doing
so, reproduce and isolate the early close, then repeat under controlled
background load with a fixed test origin. YouTube playback itself was not
measured in this trial. Generic UDP/QUIC remains unsupported by our backend.
