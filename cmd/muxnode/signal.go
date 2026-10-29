package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"

	"github.com/roca/h264-tunnel-lab/internal/logging"
	"github.com/roca/h264-tunnel-lab/internal/rtc"
)

// barrier2 is a two-party rendezvous: both parties block in Wait until both
// have arrived.
type barrier2 struct {
	mu    sync.Mutex
	count int
	ch    chan struct{}
}

func newBarrier2() *barrier2 { return &barrier2{ch: make(chan struct{})} }

func (b *barrier2) arrive() {
	b.mu.Lock()
	b.count++
	if b.count == 2 {
		close(b.ch)
	}
	b.mu.Unlock()
	<-b.ch
}

// signalListener runs the offerer side: it generates the session id, serves the
// offer, accepts the answer, and exposes a /done rendezvous endpoint. It
// returns the session id and a barrier function the caller invokes when locally
// finished.
func signalListener(ctx context.Context, cfg nodeConfig, pc *webrtc.PeerConnection, log *logging.Logger) (uint64, func() error, error) {
	var sidBuf [8]byte
	if _, err := rand.Read(sidBuf[:]); err != nil {
		return 0, nil, err
	}
	sessionID := binary.BigEndian.Uint64(sidBuf[:])

	offer, err := pc.CreateOffer(nil)
	if err != nil {
		return 0, nil, err
	}
	frozen, err := rtc.GatherAndFreeze(pc, offer)
	if err != nil {
		return 0, nil, err
	}

	bar := newBarrier2()
	mux := http.NewServeMux()
	mux.HandleFunc("/offer", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(rtc.Signal{SessionID: sessionID, SDP: *frozen})
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
			return
		}
		w.WriteHeader(http.StatusOK)
		log.Info("answer_received")
	})
	// /done: the dialer calls this when finished; it returns once BOTH sides
	// have arrived at the barrier.
	mux.HandleFunc("/done", func(w http.ResponseWriter, r *http.Request) {
		bar.arrive()
		w.WriteHeader(http.StatusOK)
	})

	srv := &http.Server{Addr: cfg.addr, Handler: mux}
	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Error("http_error", logging.KeyError, err.Error())
		}
	}()
	log.Info("http_listening", "addr", cfg.addr)

	barrierFn := func() error {
		// Listener arrives locally; unblocks once the dialer also hits /done.
		done := make(chan struct{})
		go func() { bar.arrive(); close(done) }()
		select {
		case <-done:
		case <-ctx.Done():
		}
		// Give the dialer's /done response a moment to flush, then stop HTTP.
		time.Sleep(300 * time.Millisecond)
		shutCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutCtx)
		return nil
	}
	return sessionID, barrierFn, nil
}

// signalDialer runs the answerer side: fetches the offer, sends an answer, and
// returns a barrier function that POSTs /done and waits for the mutual release.
func signalDialer(ctx context.Context, cfg nodeConfig, pc *webrtc.PeerConnection, log *logging.Logger) (uint64, func() error, error) {
	offerSig, err := fetchOffer(ctx, cfg.addr, cfg.connectWait, log)
	if err != nil {
		return 0, nil, err
	}
	if err := pc.SetRemoteDescription(offerSig.SDP); err != nil {
		return 0, nil, fmt.Errorf("set remote: %w", err)
	}
	answer, err := pc.CreateAnswer(nil)
	if err != nil {
		return 0, nil, err
	}
	frozen, err := rtc.GatherAndFreeze(pc, answer)
	if err != nil {
		return 0, nil, err
	}
	if err := postJSON(ctx, "http://"+cfg.addr+"/answer", rtc.Signal{SessionID: offerSig.SessionID, SDP: *frozen}); err != nil {
		return 0, nil, fmt.Errorf("post answer: %w", err)
	}
	log.Info("answer_sent")

	barrierFn := func() error {
		// POST /done blocks server-side until both parties arrive.
		req, err := http.NewRequest(http.MethodPost, "http://"+cfg.addr+"/done", nil)
		if err != nil {
			return err
		}
		client := &http.Client{Timeout: 60 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			return err
		}
		resp.Body.Close()
		return nil
	}
	return offerSig.SessionID, barrierFn, nil
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
			derr := json.NewDecoder(resp.Body).Decode(&sig)
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

func postJSON(ctx context.Context, url string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(b))
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
