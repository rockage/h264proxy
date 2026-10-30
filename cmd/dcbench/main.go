// Temporary reliable WebRTC DataChannel control experiment; not the H264 carrier.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"github.com/pion/webrtc/v4"
	"github.com/roca/h264-tunnel-lab/internal/rtc"
	"io"
	"net"
	"net/http"
	"os"
	"sync"
	"time"
)

type result struct {
	Role        string  `json:"role"`
	Bytes       int     `json:"bytes"`
	Seconds     float64 `json:"seconds"`
	SHA256Match bool    `json:"sha256_match"`
	Error       string  `json:"error,omitempty"`
}
type trailer struct {
	Bytes  int
	SHA256 string
}

func main() {
	role := flag.String("role", "client", "server or client")
	addr := flag.String("http", "127.0.0.1:19300", "signaling")
	ip := flag.String("public-ip", "", "server public IPv4")
	port := flag.Int("udp-port", 0, "UDP")
	size := flag.Int("size", 5000000, "payload bytes")
	flag.Parse()
	if (*role != "server" && *role != "client") || *size <= 0 || *size > 1000000000 {
		fmt.Fprintln(os.Stderr, "invalid role or size (1..1000000000)")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()
	r := result{Role: *role}
	if err := run(ctx, *role, *addr, *ip, *port, *size, &r); err != nil {
		r.Error = err.Error()
	}
	json.NewEncoder(os.Stdout).Encode(r)
	if r.Error != "" {
		os.Exit(1)
	}
}
func run(ctx context.Context, role, addr, ip string, port, size int, rep *result) error {
	opts := rtc.Options{Network: true, PublicIP: ip, UDPPort: port}
	api, err := rtc.NewAPI(opts)
	if err != nil {
		return err
	}
	pc, err := api.NewPeerConnection(rtc.Config(opts))
	if err != nil {
		return err
	}
	defer pc.Close()
	done := make(chan error, 1)
	var once sync.Once
	finish := func(err error) { once.Do(func() { done <- err }) }
	pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		if s == webrtc.PeerConnectionStateFailed {
			finish(fmt.Errorf("peer failed"))
		}
	})
	if role == "server" {
		dc, e := pc.CreateDataChannel("control-benchmark", nil)
		if e != nil {
			return e
		}
		low := make(chan struct{}, 1)
		dc.SetBufferedAmountLowThreshold(64 * 1024)
		dc.OnBufferedAmountLow(func() {
			select {
			case low <- struct{}{}:
			default:
			}
		})
		var start time.Time
		var startMu sync.Mutex
		dc.OnMessage(func(m webrtc.DataChannelMessage) {
			var ok bool
			if json.Unmarshal(m.Data, &ok) == nil && ok {
				startMu.Lock()
				elapsed := time.Since(start).Seconds()
				startMu.Unlock()
				rep.Bytes = size
				rep.SHA256Match = true
				rep.Seconds = elapsed
				finish(nil)
			}
		})
		dc.OnOpen(func() {
			go func() {
				startMu.Lock()
				start = time.Now()
				startMu.Unlock()
				h := sha256.New()
				sent := 0
				for sent < size {
					n := 16 * 1024
					if size-sent < n {
						n = size - sent
					}
					b := make([]byte, n+1)
					if _, e := rand.Read(b[1:]); e != nil {
						finish(e)
						return
					}
					h.Write(b[1:])
					for dc.BufferedAmount() > 256*1024 {
						select {
						case <-low:
						case <-ctx.Done():
							finish(ctx.Err())
							return
						}
					}
					if e := dc.Send(b); e != nil {
						finish(e)
						return
					}
					sent += n
				}
				b, _ := json.Marshal(trailer{Bytes: sent, SHA256: hex.EncodeToString(h.Sum(nil))})
				if e := dc.Send(append([]byte{1}, b...)); e != nil {
					finish(e)
				}
			}()
		})
		offer, e := pc.CreateOffer(nil)
		if e != nil {
			return e
		}
		gather := webrtc.GatheringCompletePromise(pc)
		if e = pc.SetLocalDescription(offer); e != nil {
			return e
		}
		select {
		case <-gather:
		case <-ctx.Done():
			return ctx.Err()
		}
		mux := http.NewServeMux()
		mux.HandleFunc("GET /offer", func(w http.ResponseWriter, r *http.Request) { json.NewEncoder(w).Encode(pc.LocalDescription()) })
		mux.HandleFunc("POST /answer", func(w http.ResponseWriter, r *http.Request) {
			var s webrtc.SessionDescription
			if e := json.NewDecoder(http.MaxBytesReader(w, r.Body, 65536)).Decode(&s); e != nil {
				http.Error(w, e.Error(), 400)
				return
			}
			if e := pc.SetRemoteDescription(s); e != nil {
				http.Error(w, e.Error(), 400)
			}
		})
		ln, e := net.Listen("tcp", addr)
		if e != nil {
			return e
		}
		srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, IdleTimeout: 10 * time.Second}
		defer srv.Close()
		go srv.Serve(ln)
	} else {
		pc.OnDataChannel(func(dc *webrtc.DataChannel) {
			h := sha256.New()
			n := 0
			var start time.Time
			dc.OnMessage(func(m webrtc.DataChannelMessage) {
				if len(m.Data) == 0 {
					return
				}
				if m.Data[0] == 0 {
					if start.IsZero() {
						start = time.Now()
					}
					h.Write(m.Data[1:])
					n += len(m.Data) - 1
					return
				}
				var t trailer
				if e := json.Unmarshal(m.Data[1:], &t); e != nil {
					finish(e)
					return
				}
				rep.Bytes = n
				rep.Seconds = time.Since(start).Seconds()
				rep.SHA256Match = n == t.Bytes && hex.EncodeToString(h.Sum(nil)) == t.SHA256
				if !rep.SHA256Match {
					finish(fmt.Errorf("hash mismatch"))
					return
				}
				if e := dc.SendText("true"); e != nil {
					finish(e)
					return
				}
				finish(nil)
			})
		})
		client := &http.Client{Timeout: 3 * time.Second}
		var offer webrtc.SessionDescription
		for {
			req, _ := http.NewRequestWithContext(ctx, "GET", "http://"+addr+"/offer", nil)
			resp, e := client.Do(req)
			if e == nil {
				e = json.NewDecoder(io.LimitReader(resp.Body, 65536)).Decode(&offer)
				resp.Body.Close()
				if e == nil {
					break
				}
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(200 * time.Millisecond):
			}
		}
		if e := pc.SetRemoteDescription(offer); e != nil {
			return e
		}
		answer, e := pc.CreateAnswer(nil)
		if e != nil {
			return e
		}
		gather := webrtc.GatheringCompletePromise(pc)
		if e = pc.SetLocalDescription(answer); e != nil {
			return e
		}
		select {
		case <-gather:
		case <-ctx.Done():
			return ctx.Err()
		}
		b, _ := json.Marshal(pc.LocalDescription())
		req, _ := http.NewRequestWithContext(ctx, "POST", "http://"+addr+"/answer", bytes.NewReader(b))
		resp, e := client.Do(req)
		if e != nil {
			return e
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			return fmt.Errorf("answer HTTP %d", resp.StatusCode)
		}
	}
	select {
	case e := <-done:
		if e == nil && role != "server" {
			time.Sleep(500 * time.Millisecond)
		}
		return e
	case <-ctx.Done():
		return ctx.Err()
	}
}
