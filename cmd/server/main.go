// Command server establishes a real WebRTC session over loopback, negotiates an
// H.264 video track, and transmits an encrypted file as RTP payloads over the
// SRTP media channel. Signaling (SDP offer/answer) happens over a local HTTP
// endpoint; no test data crosses HTTP.
package main

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/pion/webrtc/v4"

	cryptox "github.com/roca/h264-tunnel-lab/internal/crypto"
	"github.com/roca/h264-tunnel-lab/internal/logging"
	"github.com/roca/h264-tunnel-lab/internal/media"
	"github.com/roca/h264-tunnel-lab/internal/metrics"
	"github.com/roca/h264-tunnel-lab/internal/rtc"
	"github.com/roca/h264-tunnel-lab/internal/transfer"
)

func main() {
	addr := flag.String("http", "127.0.0.1:8080", "local HTTP signaling address")
	keyPath := flag.String("key", "testdata/key.bin", "path to 32-byte key file (aead mode)")
	inPath := flag.String("in", "", "input file to transmit (required)")
	mode := flag.String("mode", cryptox.ModeAEAD, "payload mode: aead or raw")
	chunk := flag.Int("chunk", 900, "plaintext bytes per DATA chunk")
	rate := flag.Int("rate", 8_000_000, "payload send rate limit in bytes/sec (0=unlimited)")
	logPath := flag.String("log", "", "optional log file path (JSON lines)")
	reportPath := flag.String("report", "", "optional JSON metrics report path")
	timeout := flag.Duration("timeout", 10*time.Minute, "overall timeout")
	netMode := flag.Bool("net", false, "network mode (real deployment) instead of loopback")
	publicIP := flag.String("public-ip", "", "advertise this public IP as a 1:1-NAT host candidate (network mode)")
	udpPort := flag.Int("udp-port", 0, "fixed UDP port for ICE media (network mode; open it in the firewall)")
	stun := flag.String("stun", "", "comma-separated STUN URLs (network mode), e.g. stun:stun.l.google.com:19302")
	flag.Parse()

	if *inPath == "" {
		fmt.Fprintln(os.Stderr, "-in is required")
		os.Exit(2)
	}

	log, err := logging.New("server", *logPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "logger:", err)
		os.Exit(1)
	}
	defer logging.Close()

	opts := rtc.Options{Network: *netMode, PublicIP: *publicIP, UDPPort: *udpPort, STUNServers: splitCSV(*stun)}
	if err := run(*addr, *keyPath, *inPath, *mode, *chunk, *rate, *reportPath, *timeout, opts, log); err != nil {
		log.Error("fatal", logging.KeyError, err.Error())
		os.Exit(1)
	}
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

func run(addr, keyPath, inPath, mode string, chunk, rate int, reportPath string, timeout time.Duration, opts rtc.Options, log *logging.Logger) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() { <-sig; cancel() }()

	// Session id: random 64-bit.
	var sidBuf [8]byte
	if _, err := rand.Read(sidBuf[:]); err != nil {
		return err
	}
	sessionID := binary.BigEndian.Uint64(sidBuf[:])
	log = log.With(logging.KeySessionID, fmt.Sprintf("%016x", sessionID))

	rep := &metrics.Report{Role: "server", Mode: mode, SessionID: fmt.Sprintf("%016x", sessionID)}
	defer writeReport(reportPath, rep, log)

	// Build cipher + sealer.
	var key []byte
	if mode == cryptox.ModeAEAD {
		k, err := cryptox.LoadKey(keyPath)
		if err != nil {
			return fmt.Errorf("load key: %w", err)
		}
		key = k
		log.Info("key_loaded", "fingerprint", cryptox.KeyFingerprint(key))
	}
	cipher, err := cryptox.CipherForMode(mode, key)
	if err != nil {
		return err
	}
	sealer, err := cryptox.NewSealer(cipher)
	if err != nil {
		return err
	}

	// Build WebRTC API + PeerConnection.
	api, err := rtc.NewAPI(opts)
	if err != nil {
		return err
	}
	pc, err := api.NewPeerConnection(rtc.Config(opts))
	if err != nil {
		return err
	}
	log.Info("rtc_mode", "network", opts.Network, "public_ip", opts.PublicIP, "udp_port", opts.UDPPort, "stun", strings.Join(opts.STUNServers, ","))
	defer pc.Close()

	track, err := media.NewVideoTrack("video", "h264-tunnel")
	if err != nil {
		return err
	}
	sender, err := pc.AddTrack(track)
	if err != nil {
		return err
	}

	connected := make(chan struct{})
	var connectOnce sync.Once
	failed := make(chan error, 1)

	pc.OnICEConnectionStateChange(func(s webrtc.ICEConnectionState) {
		log.Info("ice_state_change", logging.KeyICEState, s.String())
	})
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		log.Info("pc_state_change", logging.KeyPCState, s.String())
		switch s {
		case webrtc.PeerConnectionStateConnected:
			connectOnce.Do(func() { close(connected) })
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			select {
			case failed <- fmt.Errorf("peer connection state %s", s):
			default:
			}
		}
	})

	// Create offer and gather (non-trickle).
	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return err
	}
	frozen, err := rtc.GatherAndFreeze(pc, offer)
	if err != nil {
		return err
	}
	log.Info("offer_ready", "sdp_type", frozen.Type.String())

	// HTTP signaling: GET /offer, POST /answer.
	answered := make(chan struct{})
	var answerOnce sync.Once
	mux := http.NewServeMux()
	mux.HandleFunc("/offer", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(rtc.Signal{SessionID: sessionID, SDP: *frozen})
		log.Info("offer_served")
	})
	mux.HandleFunc("/answer", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var sig rtc.Signal
		if err := json.Unmarshal(body, &sig); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := pc.SetRemoteDescription(sig.SDP); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			log.Error("set_remote_failed", logging.KeyError, err.Error())
			return
		}
		w.WriteHeader(http.StatusOK)
		log.Info("answer_received")
		answerOnce.Do(func() { close(answered) })
	})
	httpSrv := &http.Server{Addr: addr, Handler: mux}
	go func() {
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("http_error", logging.KeyError, err.Error())
		}
	}()
	defer httpSrv.Close()
	log.Info("http_listening", "addr", addr)

	// Wait for the answer, then for the connection to establish.
	connectStart := time.Now()
	select {
	case <-answered:
	case err := <-failed:
		return err
	case <-ctx.Done():
		return fmt.Errorf("timeout waiting for answer: %w", ctx.Err())
	}
	select {
	case <-connected:
	case err := <-failed:
		return err
	case <-ctx.Done():
		return fmt.Errorf("timeout waiting for connection: %w", ctx.Err())
	}
	rep.ConnectSeconds = time.Since(connectStart).Seconds()

	// Verify the negotiated codec is H.264 and record PT/SSRC.
	params := sender.GetParameters()
	if len(params.Codecs) == 0 {
		return fmt.Errorf("no negotiated codec")
	}
	negotiated := params.Codecs[0]
	rep.NegotiatedCodec = negotiated.MimeType
	rep.PayloadType = uint8(negotiated.PayloadType)
	if len(params.Encodings) > 0 {
		rep.SSRC = uint32(params.Encodings[0].SSRC)
	}
	log.Info("codec_negotiated",
		logging.KeyCodec, negotiated.MimeType,
		logging.KeyPayloadT, uint8(negotiated.PayloadType),
		logging.KeySSRC, rep.SSRC,
	)
	if err := media.VerifyCodec(negotiated.MimeType); err != nil {
		return err
	}

	// Randomize initial RTP sequence/timestamp.
	var r2 [6]byte
	_, _ = rand.Read(r2[:])
	startSeq := binary.BigEndian.Uint16(r2[0:2])
	startTS := binary.BigEndian.Uint32(r2[2:6])
	writer := media.NewRTPWriter(track, startSeq, startTS, media.ClockRate/30)

	// Give the receiver a brief moment to attach its OnTrack reader so the
	// first packets are not dropped before the track is wired up.
	time.Sleep(300 * time.Millisecond)

	log.Info("transfer_start", "in", inPath, "chunk", chunk, "rate", rate)
	sendOpts := transfer.SendOptions{SessionID: sessionID, InPath: inPath, ChunkSize: chunk, RateBytesPerSec: rate}
	if err := transfer.Send(ctx, log, writer, sealer, sendOpts, rep); err != nil {
		return fmt.Errorf("send: %w", err)
	}

	// Flush: keep the connection alive briefly so trailing UDP packets drain.
	time.Sleep(500 * time.Millisecond)
	return nil
}

func writeReport(path string, rep *metrics.Report, log *logging.Logger) {
	if path == "" {
		return
	}
	if err := rep.Write(path); err != nil {
		log.Error("report_write_failed", logging.KeyError, err.Error())
		return
	}
	log.Info("report_written", "path", path)
}
