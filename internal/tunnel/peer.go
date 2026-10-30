// Package tunnel establishes a long-lived, single-peer WebRTC mux transport.
package tunnel

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	cryptox "github.com/roca/h264-tunnel-lab/internal/crypto"
	"github.com/roca/h264-tunnel-lab/internal/logging"
	"github.com/roca/h264-tunnel-lab/internal/media"
	"github.com/roca/h264-tunnel-lab/internal/mux"
	"github.com/roca/h264-tunnel-lab/internal/rtc"
)

type Config struct {
	Role mux.Role
	HTTP string
	Key  []byte
	RTC  rtc.Options
	Rate int
	Log  *logging.Logger
}

type Peer struct {
	Session *mux.Session
	pc      *webrtc.PeerConnection
	reader  *media.DeferredRTPReader
	http    *http.Server
	once    sync.Once
}

func (p *Peer) ReceiveStats() media.ReceiveStats { return p.reader.Stats() }

func (p *Peer) Close() error {
	p.once.Do(func() {
		if p.Session != nil {
			p.Session.Close()
		}
		p.reader.Abort()
		if p.http != nil {
			p.http.Close()
		}
		p.pc.Close()
	})
	return nil
}

// Open uses ctx for both setup and the lifetime of the returned transport.
func Open(ctx context.Context, cfg Config) (_ *Peer, err error) {
	if cfg.Log == nil {
		return nil, fmt.Errorf("tunnel: logger required")
	}
	api, err := rtc.NewAPI(cfg.RTC)
	if err != nil {
		return nil, err
	}
	pc, err := api.NewPeerConnection(rtc.Config(cfg.RTC))
	if err != nil {
		return nil, err
	}
	p := &Peer{pc: pc, reader: media.NewDeferredRTPReader()}
	defer func() {
		if err != nil {
			p.Close()
		}
	}()
	track, err := media.NewVideoTrack("video", "h264-proxy")
	if err != nil {
		return nil, err
	}
	sender, err := pc.AddTrack(track)
	if err != nil {
		return nil, err
	}
	// Drain RTCP for the lifetime of the sender.
	go func() {
		b := make([]byte, 1500)
		for {
			if _, _, e := sender.Read(b); e != nil {
				return
			}
		}
	}()
	connected, failed := make(chan struct{}), make(chan struct{})
	var connOnce, failOnce sync.Once
	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		cfg.Log.Info("pc_state_change", "state", state.String())
		switch state {
		case webrtc.PeerConnectionStateConnected:
			connOnce.Do(func() { close(connected) })
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed, webrtc.PeerConnectionStateDisconnected:
			p.reader.Abort()
			failOnce.Do(func() { close(failed) })
		}
	})
	pc.OnTrack(func(t *webrtc.TrackRemote, _ *webrtc.RTPReceiver) { p.reader.SetTrack(t) })
	var sid uint64
	if cfg.Role == mux.RoleListener {
		var b [8]byte
		if _, err = rand.Read(b[:]); err != nil {
			return nil, err
		}
		sid = binary.BigEndian.Uint64(b[:])
		offer, e := pc.CreateOffer(nil)
		if e != nil {
			return nil, e
		}
		frozen, e := gather(ctx, pc, offer)
		if e != nil {
			return nil, e
		}
		signal := rtc.Signal{SessionID: sid, SDP: *frozen}
		handler := http.NewServeMux()
		handler.HandleFunc("GET /offer", func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode(signal) })
		var answerMu sync.Mutex
		answered := false
		handler.HandleFunc("POST /answer", func(w http.ResponseWriter, r *http.Request) {
			answerMu.Lock()
			defer answerMu.Unlock()
			if answered {
				http.Error(w, "already answered", http.StatusConflict)
				return
			}
			var answer rtc.Signal
			if e := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&answer); e != nil || answer.SessionID != sid || answer.SDP.Type != webrtc.SDPTypeAnswer {
				http.Error(w, "invalid answer", http.StatusBadRequest)
				return
			}
			if e := pc.SetRemoteDescription(answer.SDP); e != nil {
				http.Error(w, e.Error(), http.StatusBadRequest)
				return
			}
			answered = true
			w.WriteHeader(http.StatusOK)
		})
		ln, e := net.Listen("tcp", cfg.HTTP)
		if e != nil {
			return nil, e
		}
		p.http = &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second}
		go p.http.Serve(ln)
		cfg.Log.Info("http_listening", "addr", ln.Addr().String())
	} else {
		signal, e := fetch(ctx, cfg.HTTP)
		if e != nil {
			return nil, e
		}
		if signal.SDP.Type != webrtc.SDPTypeOffer {
			return nil, fmt.Errorf("tunnel: expected offer")
		}
		sid = signal.SessionID
		if e = pc.SetRemoteDescription(signal.SDP); e != nil {
			return nil, e
		}
		answer, e := pc.CreateAnswer(nil)
		if e != nil {
			return nil, e
		}
		frozen, e := gather(ctx, pc, answer)
		if e != nil {
			return nil, e
		}
		data, e := json.Marshal(rtc.Signal{SessionID: sid, SDP: *frozen})
		if e != nil {
			return nil, e
		}
		req, e := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+cfg.HTTP+"/answer", bytes.NewReader(data))
		if e != nil {
			return nil, e
		}
		resp, e := (&http.Client{Timeout: 10 * time.Second}).Do(req)
		if e != nil {
			return nil, e
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("tunnel: answer HTTP %d", resp.StatusCode)
		}
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-failed:
		return nil, fmt.Errorf("tunnel: peer failed during setup")
	case <-connected:
	}
	codecs := sender.GetParameters().Codecs
	if len(codecs) == 0 {
		return nil, fmt.Errorf("tunnel: no negotiated codec")
	}
	if err = media.VerifyCodec(codecs[0].MimeType); err != nil {
		return nil, err
	}
	var random [6]byte
	if _, err = rand.Read(random[:]); err != nil {
		return nil, err
	}
	writer := media.NewRTPWriter(track, binary.BigEndian.Uint16(random[:2]), binary.BigEndian.Uint32(random[2:]), media.ClockRate/30)
	p.Session, err = mux.New(mux.Config{SessionID: sid, Role: cfg.Role, Mode: cryptox.ModeAEAD, Master: cfg.Key, Sink: writer, Source: p.reader, Log: cfg.Log, RateBytesPerSec: cfg.Rate})
	if err != nil {
		return nil, err
	}
	cfg.Log.Info("tunnel_ready", "codec", codecs[0].MimeType, "session_id", fmt.Sprintf("%016x", sid))
	go func() {
		select {
		case <-ctx.Done():
		case <-failed:
		case <-p.Session.Closed():
		}
		p.Close()
	}()
	return p, nil
}

func gather(ctx context.Context, pc *webrtc.PeerConnection, sdp webrtc.SessionDescription) (*webrtc.SessionDescription, error) {
	done := webrtc.GatheringCompletePromise(pc)
	if err := pc.SetLocalDescription(sdp); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-done:
		return pc.LocalDescription(), nil
	}
}

func fetch(ctx context.Context, addr string) (*rtc.Signal, error) {
	client := &http.Client{Timeout: 3 * time.Second}
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+addr+"/offer", nil)
		if err != nil {
			return nil, err
		}
		resp, err := client.Do(req)
		if err == nil {
			var signal rtc.Signal
			// Bound SDP response memory, including for a misconfigured endpoint.
			if resp.StatusCode == http.StatusOK {
				err = json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 64*1024)).Decode(&signal)
			} else {
				err = fmt.Errorf("HTTP %d", resp.StatusCode)
			}
			resp.Body.Close()
			if err == nil {
				return &signal, nil
			}
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("fetch offer: %w", ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
}
