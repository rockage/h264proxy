# Transport window refactor

This change makes the suspected bottlenecks observable and independently
configurable. Stable defaults from `f5929c6` remain unchanged. No production
gateway is deployed/restarted by the build or tests.

## Parameters

| CLI flag | Environment / client.env | Default | Valid range |
|---|---|---:|---|
| `-dc-high-water` | `H264_DC_HIGH_WATER` | 256 KiB | 32 KiB..8 MiB |
| `-dc-low-water` | `H264_DC_LOW_WATER` | 64 KiB | 0..high-1 |
| `-stream-receive-window` | `H264_STREAM_RECEIVE_WINDOW` | 256 KiB | 256 KiB..4 MiB |

All values are bytes. High/low both zero select the old pair. An explicit high
with low zero means drain to zero. An explicit CLI flag overrides its environment
variable. Invalid settings fail before signaling or listener allocation; client
launchers validate them before sudo/SSH. They are passed as explicit arguments
through sudo, rather than depending on preservation of the user's environment.

The receive window is receiver-owned. Larger credit is advertised after the
first unique DATA via the existing monotonic cumulative WINDOW message. The
first DATA ensures the peer knows the stream; prioritized WINDOW must not race
ahead of OPEN. The wire format and initial 256 KiB grant do not change. Peers
with the old default can consume larger advertised grants and advertise their
own old capacity. Expansion is allowed only for reliable transports.

The receiver counts unique bytes including out-of-order frames and rejects a
peer exceeding its cumulative grant. Duplicate retransmissions do not consume
credit again. The limit bounds unconsumed bytes per stream, not total process
RSS: buffers, retransmission bookkeeping and other streams consume additional
memory. Normal consumption replenishes credit in half-window increments.

## Direction and trial order

For **downloads**:

- The **gateway's** DataChannel high/low settings control the sending buffer.
- The **client's** receive window controls per-stream credit.
- The gateway's receive window and client's sending buffer govern the reverse
  direction. Changing only those is not a download-window experiment.

Suggested trials, on the same origin/path and with background applications
controlled:

1. Both new binaries with defaults: collect baseline waiting counters.
2. Only gateway high/low -> 1048576 / 524288; client receive window unchanged.
3. Restore baseline sending buffer; only client receive window -> 1048576.
4. Combine the two only after evaluating each independently.

A client receive-window-only trial can use the existing gateway's cumulative
WINDOW support; sender-buffer trials and sender-side waiting metrics require
the new gateway binary. Do not interpret old gateway logs as new instrumentation.

Example client receive-only trial:

```bash
H264_STREAM_RECEIVE_WINDOW=1048576 H264_TUN_SERVICE=Wi-Fi \
  bash scripts/start_proxy.sh --sing-box
```

To configure an **upgraded** gateway, its existing systemd EnvironmentFile
`/etc/h264proxy/gateway.env` can contain these values, followed by a controlled
service restart. No ExecStart change is needed:

```text
H264_DC_HIGH_WATER=1048576
H264_DC_LOW_WATER=524288
```

The TUN comparison script forwards all three client settings and records them
in `transport_parameters`. Resume refuses a parameter change. That script still
compares TUN frontends; it does not automate gateway parameter trials.

## Counters in mux_stats

`datachannel`:

- `HighWater`, `LowWater`: effective sending thresholds.
- `BackpressureWaits`: number of writes encountering a full send buffer.
- `BackpressureNanos`: completed time waiting for drain, including canceled waits.
- `BackpressureWaiters`: currently blocked writers, normally 0 or 1.
- `MaxBufferedAmount`: maximum observed amount sampled before writes.

`stats`:

- `ReceiveWindowBytes`: configured local per-stream receive capacity.
- `CreditWaits`, `CreditWaitNanos`, `CreditWaiters`: per-stream credit blocking.
- `QueueWaits`, `QueueWaitNanos`, `QueueWaiters`: blocking when the mux writer
  queue is full, including currently blocked producers.
- `SinkWriteNanos`: time inside transport writes, including DataChannel waits.

Counters are cumulative. Compare deltas over a measurement interval, not just
final totals. Credit/queue time may accumulate across concurrent producers and
exceed wall time. Sink time overlaps DataChannel backpressure time; do not add
them. Active waiters help identify waits that have not completed yet. Keep the
server/client direction explicit when reading SCTP cwnd and these counters.

## Validation and scope

- Mixed default/1 MiB capacities in both directions, payload integrity and FIN.
- Unread receive buffers stop at their credit; reset wakes blocked writers.
- A slow stream does not prevent a second stream delivering a small message.
- Over-credit rejection; duplicate/reordered frames charged only once.
- Custom send thresholds, drain/cancel behavior, wait counters and queue shutdown.
- Real local ICE-TCP + DTLS + SCTP transfer with expanded client receive credit.
- Root and embedded Apple-engine race tests and vet.

The `BenchmarkReliableWindowLatency` benchmark isolates mux flow control over
an unlimited-rate in-memory link with 80 ms simulated RTT. A three-iteration
run on this machine measured 2.64 MB/s with 256 KiB and 7.61 MB/s with 1 MiB;
credit waiting dropped from 710 to 191 ms per 2 MiB transfer. Raw output:
`build/logs/window-refactor-benchmark.txt`. This is **not** a WebRTC or WAN
throughput claim and does not predict 4K playback performance.

This refactor does not implement a new stream scheduler, separate control
DataChannel, SCTP congestion tuning, CPU profiling or multi-connection pool.
Those should follow measured contention rather than being mixed into the
first window experiment. Existing FIFO and feedback priority are preserved.
