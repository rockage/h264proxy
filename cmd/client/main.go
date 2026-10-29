// Command client connects to the server's local HTTP signaling endpoint,
// completes a real WebRTC H.264 session, receives the encrypted file over the
// SRTP media channel, decrypts and reassembles it, and verifies the SHA-256
// against the sender's. It exits non-zero unless the file is recovered exactly.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
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
	addr := flag.String("http", "127.0.0.1:8080", "server HTTP signaling address")
	keyPath := flag.String("key", "testdata/key.bin", "path to 32-byte key file (aead mode)")
	outPath := flag.String("out", "", "output file path (required)")
	mode := flag.String("mode", cryptox.ModeAEAD, "payload mode: aead or raw")
	logPath := flag.String("log", "", "optional log file path (JSON lines)")
	reportPath := flag.String("report", "", "optional JSON metrics report path")
	timeout := flag.Duration("timeout", 10*time.Minute, "overall timeout")
	connectWait := flag.Duration("connect-wait", 30*time.Second, "how long to retry fetching the offer")
	netMode := flag.Bool("net", false, "network mode (real deployment) instead of loopback")
	stun := flag.String("stun", "", "comma-separated STUN URLs (network mode), e.g. stun:stun.l.google.com:19302")
	udpPort := flag.Int("udp-port", 0, "fixed local UDP port for ICE (network mode; 0=ephemeral)")
	flag.Parse()

	if *outPath == "" {
		fmt.Fprintln(os.Stderr, "-out is required")
		os.Exit(2)
	}

	log, err := logging.New("client", *logPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "logger:", err)
		os.Exit(1)
	}
	defer logging.Close()

	opts := rtc.Options{Network: *netMode, UDPPort: *udpPort, STUNServers: splitCSV(*stun)}
	if err := run(*addr, *keyPath, *outPath, *mode, *reportPath, *timeout, *connectWait, opts, log); err != nil {
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

func run(addr, keyPath, outPath, mode, reportPath string, timeout, connectWait time.Duration, opts rtc.Options, log *logging.Logger) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() { <-sig; cancel() }()

	rep := &metrics.Report{Role: "client", Mode: mode}
	defer writeReport(reportPath, rep, log)

	// Cipher.
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

	// Fetch offer (retry until the server is ready).
	offerSig, err := fetchOffer(ctx, addr, connectWait, log)
	if err != nil {
		return err
	}
	sessionID := offerSig.SessionID
	rep.SessionID = fmt.Sprintf("%016x", sessionID)
	log = log.With(logging.KeySessionID, rep.SessionID)

	// PeerConnection.
	api, err := rtc.NewAPI(opts)
	if err != nil {
		return err
	}
	pc, err := api.NewPeerConnection(rtc.Config(opts))
	if err != nil {
		return err
	}
	defer pc.Close()
	log.Info("rtc_mode", "network", opts.Network, "stun", strings.Join(opts.STUNServers, ","))

	type recvResult struct {
		err error
	}
	recvDone := make(chan recvResult, 1)
	var trackOnce sync.Once
	connectStart := time.Now()
	var connectSeconds float64

	pc.OnICEConnectionStateChange(func(s webrtc.ICEConnectionState) {
		log.Info("ice_state_change", logging.KeyICEState, s.String())
	})
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		log.Info("pc_state_change", logging.KeyPCState, s.String())
		if s == webrtc.PeerConnectionStateConnected && connectSeconds == 0 {
			connectSeconds = time.Since(connectStart).Seconds()
		}
	})

	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		trackOnce.Do(func() {
			codec := track.Codec()
			rep.NegotiatedCodec = codec.MimeType
			rep.PayloadType = uint8(track.PayloadType())
			rep.SSRC = uint32(track.SSRC())
			log.Info("track_received",
				logging.KeyCodec, codec.MimeType,
				logging.KeyPayloadT, uint8(track.PayloadType()),
				logging.KeySSRC, uint32(track.SSRC()),
			)
			if err := media.VerifyCodec(codec.MimeType); err != nil {
				recvDone <- recvResult{err: err}
				return
			}
			reader := media.NewRTPReader(track)
			opts := transfer.ReceiveOptions{OutPath: outPath, TmpDir: filepath.Dir(outPath)}
			err := transfer.Receive(ctx, log, reader, cipher, opts, rep)
			rep.ConnectSeconds = connectSeconds
			recvDone <- recvResult{err: err}
		})
	})

	// SetRemote(offer) -> CreateAnswer -> gather -> POST /answer.
	if err := pc.SetRemoteDescription(offerSig.SDP); err != nil {
		return fmt.Errorf("set remote: %w", err)
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		return err
	}
	frozen, err := rtc.GatherAndFreeze(pc, answer)
	if err != nil {
		return err
	}
	if err := postAnswer(ctx, addr, rtc.Signal{SessionID: sessionID, SDP: *frozen}); err != nil {
		return fmt.Errorf("post answer: %w", err)
	}
	log.Info("answer_sent")

	select {
	case res := <-recvDone:
		if res.err != nil {
			return fmt.Errorf("receive: %w", res.err)
		}
	case <-ctx.Done():
		return fmt.Errorf("timeout during transfer: %w", ctx.Err())
	}

	if !rep.SHA256Match {
		return fmt.Errorf("sha256 mismatch")
	}
	log.Info("success", logging.KeyStatus, "ok", "sha256", rep.SHA256Actual)
	return nil
}

func fetchOffer(ctx context.Context, addr string, wait time.Duration, log *logging.Logger) (*rtc.Signal, error) {
	deadline := time.Now().Add(wait)
	url := "http://" + addr + "/offer"
	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		resp, err := http.DefaultClient.Do(req)
		if err == nil && resp.StatusCode == http.StatusOK {
			var sig rtc.Signal
			dec := json.NewDecoder(resp.Body)
			derr := dec.Decode(&sig)
			resp.Body.Close()
			if derr == nil {
				log.Info("offer_fetched")
				return &sig, nil
			}
			err = derr
		} else if resp != nil {
			resp.Body.Close()
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("could not fetch offer within %s: %v", wait, err)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func postAnswer(ctx context.Context, addr string, sig rtc.Signal) error {
	b, err := json.Marshal(sig)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+addr+"/answer", bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("server returned %s", resp.Status)
	}
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
