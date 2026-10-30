# TUN / DataChannel bandwidth investigation — 2026-10-30

**Historical Nuremberg-node results:** every experiment below targeted
`152.53.13.153` because the old launcher silently selected that VPS. The user
has now selected `207.148.110.180`; these bandwidth/loss measurements do not
describe that current VPS or its path. The explicit target is stored in
`build/client.env`, and its exit IP has been verified separately.

Status: stable 720P playback has **not** been achieved or verified. This round
identified a transport limitation; it did not produce a proven production
throughput fix. Production gateway settings and its active session were not
changed during these experiments.

## Observations

The production gateway's recent five-second mux egress samples were mostly
0.35–1.24 Mbps, with around 50–96 active streams and a persistently populated
DataChannel send buffer. These are aggregate traffic samples, not link capacity
measurements. Client proxy/tun2socks CPU usage was below 1% at the observation.
The VPS route was a static /32 through the physical Wi-Fi interface.

An independent gateway session on the same VPS isolated one TCP download from
an HTTP server bound to the VPS loopback. Signalling was forwarded over SSH;
WebRTC media/data packets travelled directly to the VPS. This removed YouTube
CDN/DNS and the TUN packet conversion from the measured transfer. Existing
production traffic still shared the physical WAN link, and the sequential
experiments were not a controlled, randomized capacity study.

Actual SCTP stats showed roughly 175–285 ms RTT in the UDP trials. The peer's
receive window typically had around 0.95–1 MiB available, while SCTP congestion
windows repeatedly fell into the 1–30 KiB range. The old mux `CwndBytes` value
is an application estimator, not the SCTP congestion window. The source now
logs true transport metrics under `sctp` and the selected `ice_protocol`.

## Transfer experiments

Each successful result below transferred and SHA-256-verified 2,097,152 bytes.
Time includes SOCKS setup and first response. Defaults retained a 16 KiB DATA
message size and Pion's default congestion-avoidance step.

| Trial | Seconds | Payload Mbps | Result |
| --- | ---: | ---: | --- |
| Default, first run | 27.708 | 0.61 | Full hash verified |
| Default, second run | 17.453 | 0.96 | Full hash verified |
| CA step 4,800 bytes | 16.498 | 1.02 | Full hash verified |
| CA step 9,600 bytes | 13.881 | 1.21 | Full hash verified |
| 1 KiB messages, application pace 500 KB/s | 16.876 | 0.99 | Full hash verified |
| ICE-TCP | 35.004 timeout | 0.46 | Partial body; not an accepted result |
| Actual UDP packet pacing, 250 KB/s | 35.007 timeout | 0.47 | Partial body; not an accepted result |

Variation between default runs is substantial. These samples do not establish
that the CA-step trials have a durable advantage, and none demonstrates stable
720P. ICE-TCP also increased observed SCTP RTT to around 1–2 seconds. The
experimental transport/tuning/pacing changes were removed from production
source. No new default or congestion-window floor was applied.

## Direct UDP loss probe

A separate sequence-numbered downlink probe sent 1,192-byte datagrams from the
VPS to this Mac, outside SCTP and TUN, on the temporary test port:

| Offered rate | Sent packets | Received packets | Missing |
| --- | ---: | ---: | ---: |
| 0.95 Mbps | 300 | 298 | 0.67% |
| 3.81 Mbps | 600 | 594 | 1.00% |

These are short samples, not a long-term loss guarantee. They establish that
loss occurred outside the mux/SCTP implementation during this test. The
combination of this loss and repeated SCTP window reductions explains why
adding receive buffers or changing DNS cannot by itself establish the required
sustained throughput.

A 2 MiB SSH transfer measured 2.15 Mbps including SSH setup; a separate larger
TCP/SSH probe was partial at a 45-second cutoff while other tests/production
traffic were active. Neither should be presented as a clean TCP link-capacity
benchmark or evidence that switching transport alone will meet the video goal.

## Retained work and next direction

Only actual SCTP stats / selected ICE protocol diagnostics are retained from
this bandwidth round. Previous DNS caching, idle-feedback reduction, route
checks and recoverable ICE-disconnection fixes remain intact.

For the next throughput experiment, loss recovery below SCTP (for example FEC)
or a different transport congestion controller is more directly targeted at
the observed limitation than more TUN/DNS tuning. Either needs separate tests;
this investigation does not claim a future speed gain.

All temporary gateway/HTTP processes, test firewall ports and staging binaries
were removed. The production gateway was left running. Detailed test logs and
an archive of the rejected experiments are local ignored artifacts under
`build/logs/perf-*` and `build/bandwidth-experiments-source.tar.gz`.
