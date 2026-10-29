// Command muxnode demonstrates and exercises the full-duplex, multi-stream mux
// layer (internal/mux) over a real WebRTC H.264 SRTP channel. Two muxnode
// processes (one -role listener, one -role dialer) establish a single
// bidirectional H.264 transceiver and then, concurrently in BOTH directions,
// open N logical streams, each carrying a self-describing payload
// (sha256(body) || body). Each side verifies every stream it receives.
//
// Signaling (SDP + a tiny done-barrier) uses local HTTP only; all stream data
// travels on the SRTP media channel.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/pion/webrtc/v4"

	cryptox "github.com/roca/h264-tunnel-lab/internal/crypto"
	"github.com/roca/h264-tunnel-lab/internal/logging"
	"github.com/roca/h264-tunnel-lab/internal/media"
	"github.com/roca/h264-tunnel-lab/internal/mux"
	"github.com/roca/h264-tunnel-lab/internal/netsim"
	"github.com/roca/h264-tunnel-lab/internal/rtc"
	"github.com/roca/h264-tunnel-lab/internal/transfer"
)

type nodeReport struct {
	Role            string    `json:"role"`
	SessionID       string    `json:"session_id"`
	Mode            string    `json:"mode"`
	NegotiatedCodec string    `json:"negotiated_codec"`
	LocalSSRC       uint32    `json:"local_ssrc"`
	RemoteSSRC      uint32    `json:"remote_ssrc"`
	ConnectSeconds  float64   `json:"connect_seconds"`
	WorkloadSeconds float64   `json:"workload_seconds"`
	StreamsOpened   int       `json:"streams_opened"`
	StreamsRecvOK   int       `json:"streams_recv_ok"`
	StreamsRecvFail int       `json:"streams_recv_fail"`
	BytesPerStream  int       `json:"bytes_per_stream"`
	DuplexConfirmed bool      `json:"duplex_confirmed"`
	Retransmits     uint64    `json:"retransmits"`
	FramesDropped   uint64    `json:"frames_dropped_locally"`
	SRTTMillis      float64   `json:"srtt_ms"`
	RTOMillis       float64   `json:"rto_ms"`
	CwndBytes       int64     `json:"cwnd_bytes"`
	MaxCwndBytes    int64     `json:"max_cwnd_bytes"`
	SsthreshBytes   int64     `json:"ssthresh_bytes"`
	CongLossEvents  uint64    `json:"cong_loss_events"`
	Stats           mux.Stats `json:"stats"`
	Error           string    `json:"error,omitempty"`
}

func main() {
	role := flag.String("role", "", "listener or dialer (required)")
	addr := flag.String("http", "127.0.0.1:8080", "HTTP signaling address")
	keyPath := flag.String("key", "testdata/key.bin", "32-byte key file (aead mode)")
	mode := flag.String("mode", cryptox.ModeAEAD, "aead or raw")
	nStreams := flag.Int("streams", 4, "number of streams to open toward the peer")
	size := flag.Int("size", 256*1024, "bytes per stream payload")
	rate := flag.Int("rate", mux.DefaultRateBytesPerSec, "aggregate send pace in bytes/sec (0=unlimited)")
	loss := flag.Float64("loss", 0, "fraction of outbound frames to drop (0..1) to exercise reliability")
	netMode := flag.Bool("net", false, "network mode (real deployment) instead of loopback")
	publicIP := flag.String("public-ip", "", "listener: advertise this public IP as a 1:1-NAT host candidate")
	udpPort := flag.Int("udp-port", 0, "fixed UDP port for ICE media (network mode; open it in the firewall)")
	stun := flag.String("stun", "", "comma-separated STUN URLs (network mode)")
	logPath := flag.String("log", "", "optional JSON log file")
	reportPath := flag.String("report", "", "optional JSON report path")
	timeout := flag.Duration("timeout", 5*time.Minute, "overall timeout")
	connectWait := flag.Duration("connect-wait", 30*time.Second, "offer fetch retry budget (dialer)")
	flag.Parse()

	if *role != "listener" && *role != "dialer" {
		fmt.Fprintln(os.Stderr, "-role must be listener or dialer")
		os.Exit(2)
	}
	log, err := logging.New("muxnode-"+*role, *logPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "logger:", err)
		os.Exit(1)
	}
	defer logging.Close()

	cfg := nodeConfig{
		role: *role, addr: *addr, keyPath: *keyPath, mode: *mode,
		nStreams: *nStreams, size: *size, rate: *rate, loss: *loss, reportPath: *reportPath,
		timeout: *timeout, connectWait: *connectWait,
		rtcOpts: rtc.Options{Network: *netMode, PublicIP: *publicIP, UDPPort: *udpPort, STUNServers: splitCSV(*stun)},
	}
	if err := runNode(cfg, log); err != nil {
		log.Error("fatal", logging.KeyError, err.Error())
		os.Exit(1)
	}
}

type nodeConfig struct {
	role, addr, keyPath, mode, reportPath string
	nStreams, size, rate                  int
	loss                                  float64
	timeout, connectWait                  time.Duration
	rtcOpts                               rtc.Options
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func runNode(cfg nodeConfig, log *logging.Logger) (runErr error) {
	ctx, cancel := context.WithTimeout(context.Background(), cfg.timeout)
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() { <-sigCh; cancel() }()

	rep := &nodeReport{Role: cfg.role, Mode: cfg.mode, BytesPerStream: cfg.size}
	defer func() {
		if runErr != nil {
			rep.Error = runErr.Error()
		}
		if cfg.reportPath != "" {
			if b, err := json.MarshalIndent(rep, "", "  "); err == nil {
				_ = os.WriteFile(cfg.reportPath, append(b, '\n'), 0o644)
			}
		}
	}()

	var master []byte
	if cfg.mode == cryptox.ModeAEAD {
		k, err := cryptox.LoadKey(cfg.keyPath)
		if err != nil {
			return fmt.Errorf("load key: %w", err)
		}
		master = k
		log.Info("key_loaded", "fingerprint", cryptox.KeyFingerprint(master))
	}

	api, err := rtc.NewAPI(cfg.rtcOpts)
	if err != nil {
		return err
	}
	pc, err := api.NewPeerConnection(rtc.Config(cfg.rtcOpts))
	if err != nil {
		return err
	}
	defer pc.Close()
	log.Info("rtc_mode", "network", cfg.rtcOpts.Network, "public_ip", cfg.rtcOpts.PublicIP, "udp_port", cfg.rtcOpts.UDPPort, "stun", strings.Join(cfg.rtcOpts.STUNServers, ","))

	localTrack, err := media.NewVideoTrack("video", "h264-tunnel")
	if err != nil {
		return err
	}
	sender, err := pc.AddTrack(localTrack)
	if err != nil {
		return err
	}

	deferred := media.NewDeferredRTPReader()
	connected := make(chan struct{})
	var connOnce sync.Once
	var connectStart time.Time
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		log.Info("pc_state_change", logging.KeyPCState, s.String())
		switch s {
		case webrtc.PeerConnectionStateConnected:
			connOnce.Do(func() { close(connected) })
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed, webrtc.PeerConnectionStateDisconnected:
			deferred.Abort()
		}
	})
	pc.OnTrack(func(t *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		rep.NegotiatedCodec = t.Codec().MimeType
		rep.RemoteSSRC = uint32(t.SSRC())
		log.Info("track_received", logging.KeyCodec, t.Codec().MimeType, logging.KeySSRC, uint32(t.SSRC()))
		deferred.SetTrack(t)
	})

	// Role-specific signaling establishes the connection and the done-barrier.
	var sessionID uint64
	var barrier func() error
	connectStart = time.Now()
	switch cfg.role {
	case "listener":
		sessionID, barrier, err = signalListener(ctx, cfg, pc, log)
	case "dialer":
		sessionID, barrier, err = signalDialer(ctx, cfg, pc, log)
	}
	if err != nil {
		return err
	}
	rep.SessionID = fmt.Sprintf("%016x", sessionID)

	select {
	case <-connected:
	case <-ctx.Done():
		return fmt.Errorf("timeout waiting for connection: %w", ctx.Err())
	}
	rep.ConnectSeconds = time.Since(connectStart).Seconds()

	// Verify negotiated codec and record SSRCs.
	if params := sender.GetParameters(); len(params.Codecs) > 0 {
		if rep.NegotiatedCodec == "" {
			rep.NegotiatedCodec = params.Codecs[0].MimeType
		}
		if len(params.Encodings) > 0 {
			rep.LocalSSRC = uint32(params.Encodings[0].SSRC)
		}
	}
	if err := media.VerifyCodec(rep.NegotiatedCodec); err != nil {
		return err
	}

	// Build the mux session over the real media channel.
	var r2 [6]byte
	_, _ = rand.Read(r2[:])
	writer := media.NewRTPWriter(localTrack, binary.BigEndian.Uint16(r2[0:2]), binary.BigEndian.Uint32(r2[2:6]), media.ClockRate/30)
	var sink transfer.PacketSink = writer
	var lossy *netsim.LossySink
	if cfg.loss > 0 {
		lossy = netsim.NewLossySink(writer, cfg.loss, time.Now().UnixNano())
		sink = lossy
		log.Info("loss_injection_enabled", "fraction", cfg.loss)
	}
	role := mux.RoleListener
	if cfg.role == "dialer" {
		role = mux.RoleDialer
	}
	sess, err := mux.New(mux.Config{
		SessionID: sessionID, Role: role, Mode: cfg.mode, Master: master,
		Sink: sink, Source: deferred, Log: log, RateBytesPerSec: cfg.rate,
	})
	if err != nil {
		return err
	}
	defer sess.Close()
	// Preserve diagnostics even if a real-network workload times out.
	workloadStart := time.Now()
	captureStats := func() {
		st := sess.Stats()
		rep.Stats = st
		rep.Retransmits = st.Retransmits
		rep.SRTTMillis = st.SRTTMillis
		rep.RTOMillis = st.RTOMillis
		rep.CwndBytes = st.CwndBytes
		rep.MaxCwndBytes = st.MaxCwndBytes
		rep.SsthreshBytes = st.SsthreshBytes
		rep.CongLossEvents = st.CongLossEvents
		rep.DuplexConfirmed = st.FramesSent > 0 && st.FramesReceived > 0
		if lossy != nil {
			rep.FramesDropped = lossy.Dropped()
		}
		if rep.WorkloadSeconds == 0 {
			rep.WorkloadSeconds = time.Since(workloadStart).Seconds()
		}
	}
	defer captureStats()

	// Run the concurrent bidirectional workload.
	if err := runWorkload(ctx, log, sess, cfg.nStreams, cfg.size, rep); err != nil {
		return err
	}
	rep.WorkloadSeconds = time.Since(workloadStart).Seconds()

	captureStats()
	log.Info("workload_done",
		"streams_opened", rep.StreamsOpened,
		"recv_ok", rep.StreamsRecvOK,
		"recv_fail", rep.StreamsRecvFail,
		"duplex", rep.DuplexConfirmed,
		"retransmits", rep.Retransmits,
		"frames_dropped", rep.FramesDropped,
	)

	// Deterministic shutdown: wait until BOTH peers finished before tearing
	// down the media channel, so no in-flight stream is cut off.
	if err := barrier(); err != nil {
		log.Warn("barrier_error", logging.KeyError, err.Error())
	}

	if rep.StreamsRecvFail != 0 || rep.StreamsRecvOK != cfg.nStreams {
		rep.Error = fmt.Sprintf("recv incomplete: ok=%d fail=%d want=%d", rep.StreamsRecvOK, rep.StreamsRecvFail, cfg.nStreams)
		return fmt.Errorf("%s", rep.Error)
	}
	if !rep.DuplexConfirmed {
		rep.Error = "duplex not confirmed (a direction carried no frames)"
		return fmt.Errorf("%s", rep.Error)
	}
	log.Info("success", logging.KeyStatus, "ok")
	return nil
}

// runWorkload opens nStreams toward the peer and concurrently accepts nStreams
// from the peer, verifying each received stream's embedded SHA-256.
func runWorkload(ctx context.Context, log *logging.Logger, sess *mux.Session, nStreams, size int, rep *nodeReport) error {
	var wg sync.WaitGroup
	var recvOK, recvFail atomic.Int64

	// Sender.
	wg.Add(1)
	go func() {
		defer wg.Done()
		var sw sync.WaitGroup
		for i := 0; i < nStreams; i++ {
			s, err := sess.OpenStream()
			if err != nil {
				log.Error("open_stream_failed", logging.KeyError, err.Error())
				return
			}
			sw.Add(1)
			go func(s *mux.Stream) {
				defer sw.Done()
				blob := make([]byte, size)
				_, _ = rand.Read(blob)
				sum := sha256.Sum256(blob)
				if _, err := s.Write(sum[:]); err != nil {
					log.Error("write_hdr_failed", logging.KeyError, err.Error())
					return
				}
				if _, err := s.Write(blob); err != nil {
					log.Error("write_body_failed", logging.KeyError, err.Error())
					return
				}
				_ = s.Close()
			}(s)
		}
		sw.Wait()
		rep.StreamsOpened = nStreams
	}()

	// Receiver.
	wg.Add(1)
	go func() {
		defer wg.Done()
		var rw sync.WaitGroup
		for i := 0; i < nStreams; i++ {
			s, err := sess.Accept()
			if err != nil {
				log.Error("accept_failed", logging.KeyError, err.Error())
				return
			}
			rw.Add(1)
			go func(s *mux.Stream) {
				defer rw.Done()
				all, err := io.ReadAll(s)
				if err != nil || len(all) < sha256.Size {
					recvFail.Add(1)
					log.Error("stream_recv_failed", "stream_id", s.ID(), logging.KeyError, errStr(err))
					return
				}
				declared := all[:sha256.Size]
				body := all[sha256.Size:]
				actual := sha256.Sum256(body)
				if bytes.Equal(declared, actual[:]) {
					recvOK.Add(1)
				} else {
					recvFail.Add(1)
					log.Error("stream_sha_mismatch", "stream_id", s.ID())
				}
			}(s)
		}
		rw.Wait()
	}()

	doneCh := make(chan struct{})
	go func() { wg.Wait(); close(doneCh) }()
	progress := time.NewTicker(5 * time.Second)
	defer progress.Stop()
wait:
	for {
		select {
		case <-doneCh:
			break wait
		case <-ctx.Done():
			return fmt.Errorf("workload timeout: %w", ctx.Err())
		case <-progress.C:
			st := sess.Stats()
			log.Info("workload_progress", "bytes_sent", st.BytesSent,
				"bytes_received", st.BytesReceived, "srtt_ms", st.SRTTMillis,
				"cwnd_bytes", st.CwndBytes, "retransmits", st.Retransmits)
		}
	}
	rep.StreamsRecvOK = int(recvOK.Load())
	rep.StreamsRecvFail = int(recvFail.Load())
	return nil
}

func errStr(err error) string {
	if err == nil {
		return "short stream"
	}
	return err.Error()
}
