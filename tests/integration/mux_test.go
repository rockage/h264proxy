package integration

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

type muxReport struct {
	Role            string `json:"role"`
	NegotiatedCodec string `json:"negotiated_codec"`
	LocalSSRC       uint32 `json:"local_ssrc"`
	RemoteSSRC      uint32 `json:"remote_ssrc"`
	StreamsOpened   int    `json:"streams_opened"`
	StreamsRecvOK   int    `json:"streams_recv_ok"`
	StreamsRecvFail int    `json:"streams_recv_fail"`
	DuplexConfirmed bool   `json:"duplex_confirmed"`
	Retransmits     uint64 `json:"retransmits"`
	FramesDropped   uint64 `json:"frames_dropped_locally"`
	Error           string `json:"error"`
	Stats           struct {
		FramesSent     uint64 `json:"FramesSent"`
		FramesReceived uint64 `json:"FramesReceived"`
		DecodeFailures uint64 `json:"DecodeFailures"`
	} `json:"stats"`
}

// TestTwoProcessMuxDuplex runs two muxnode processes and verifies a real,
// full-duplex, multi-stream transfer over the WebRTC H.264 SRTP channel:
// each side opens N streams to the other and recovers all N it receives.
func TestTwoProcessMuxDuplex(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping real WebRTC mux integration test in -short mode")
	}
	_ = freeUDPPort(t)

	root := "../.."
	muxBin := build(t, root, "./cmd/muxnode", "muxnode")
	genkeyBin := build(t, root, "./cmd/genkey", "genkey")

	work := t.TempDir()
	keyPath := filepath.Join(work, "key.bin")
	if b, err := exec.Command(genkeyBin, "-out", keyPath, "-force").CombinedOutput(); err != nil {
		t.Fatalf("genkey: %v\n%s", err, b)
	}

	port := freeTCPPort(t)
	addr := "127.0.0.1:" + strconv.Itoa(port)
	const n = 6
	lRep := filepath.Join(work, "l.json")
	dRep := filepath.Join(work, "d.json")

	mk := func(role, report string) *exec.Cmd {
		c := exec.Command(muxBin,
			"-role", role, "-http", addr, "-key", keyPath, "-mode", "aead",
			"-streams", strconv.Itoa(n), "-size", "131072",
			"-report", report, "-timeout", "90s")
		c.Stdout, c.Stderr = os.Stderr, os.Stderr
		return c
	}

	listener := mk("listener", lRep)
	if err := listener.Start(); err != nil {
		t.Fatalf("start listener: %v", err)
	}
	defer func() { _ = listener.Process.Kill() }()

	time.Sleep(300 * time.Millisecond)
	dialer := mk("dialer", dRep)

	var wg sync.WaitGroup
	wg.Add(2)
	var lErr, dErr error
	go func() { defer wg.Done(); dErr = dialer.Run() }()
	go func() { defer wg.Done(); lErr = listener.Wait() }()

	doneCh := make(chan struct{})
	go func() { wg.Wait(); close(doneCh) }()
	select {
	case <-doneCh:
	case <-time.After(100 * time.Second):
		t.Fatal("timed out waiting for mux nodes")
	}
	if dErr != nil {
		t.Fatalf("dialer failed: %v", dErr)
	}
	if lErr != nil {
		t.Fatalf("listener failed: %v", lErr)
	}

	l := readMuxReport(t, lRep)
	d := readMuxReport(t, dRep)

	for _, r := range []muxReport{l, d} {
		if r.NegotiatedCodec != "video/H264" {
			t.Fatalf("%s negotiated non-H264: %q", r.Role, r.NegotiatedCodec)
		}
		if r.StreamsOpened != n || r.StreamsRecvOK != n || r.StreamsRecvFail != 0 {
			t.Fatalf("%s stream accounting wrong: %+v", r.Role, r)
		}
		if !r.DuplexConfirmed {
			t.Fatalf("%s duplex not confirmed", r.Role)
		}
		if r.Stats.FramesSent == 0 || r.Stats.FramesReceived == 0 {
			t.Fatalf("%s a direction carried no frames", r.Role)
		}
		if r.Stats.DecodeFailures != 0 {
			t.Fatalf("%s had %d decode failures", r.Role, r.Stats.DecodeFailures)
		}
	}
	// Cross-check SSRCs: each side's local SSRC is the other's remote SSRC.
	if l.LocalSSRC != d.RemoteSSRC || d.LocalSSRC != l.RemoteSSRC || l.LocalSSRC == 0 || d.LocalSSRC == 0 {
		t.Fatalf("SSRC cross-check failed: l.local=%d d.remote=%d d.local=%d l.remote=%d",
			l.LocalSSRC, d.RemoteSSRC, d.LocalSSRC, l.RemoteSSRC)
	}
}

func readMuxReport(t *testing.T, path string) muxReport {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var r muxReport
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return r
}

// TestTwoProcessMuxReliableUnderLoss runs two muxnode processes that each drop
// ~10% of outbound frames on the real SRTP channel, and verifies that the
// selective-repeat ARQ still recovers every stream byte-exactly, with
// retransmissions actually occurring.
func TestTwoProcessMuxReliableUnderLoss(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping real WebRTC mux loss test in -short mode")
	}
	_ = freeUDPPort(t)

	root := "../.."
	muxBin := build(t, root, "./cmd/muxnode", "muxnode")
	genkeyBin := build(t, root, "./cmd/genkey", "genkey")

	work := t.TempDir()
	keyPath := filepath.Join(work, "key.bin")
	if b, err := exec.Command(genkeyBin, "-out", keyPath, "-force").CombinedOutput(); err != nil {
		t.Fatalf("genkey: %v\n%s", err, b)
	}

	port := freeTCPPort(t)
	addr := "127.0.0.1:" + strconv.Itoa(port)
	const n = 5
	lRep := filepath.Join(work, "l.json")
	dRep := filepath.Join(work, "d.json")

	mk := func(role, report string) *exec.Cmd {
		c := exec.Command(muxBin,
			"-role", role, "-http", addr, "-key", keyPath, "-mode", "aead",
			"-streams", strconv.Itoa(n), "-size", "131072", "-loss", "0.10",
			"-report", report, "-timeout", "90s")
		c.Stdout, c.Stderr = os.Stderr, os.Stderr
		return c
	}

	listener := mk("listener", lRep)
	if err := listener.Start(); err != nil {
		t.Fatalf("start listener: %v", err)
	}
	defer func() { _ = listener.Process.Kill() }()
	time.Sleep(300 * time.Millisecond)
	dialer := mk("dialer", dRep)

	var wg sync.WaitGroup
	wg.Add(2)
	var lErr, dErr error
	go func() { defer wg.Done(); dErr = dialer.Run() }()
	go func() { defer wg.Done(); lErr = listener.Wait() }()
	doneCh := make(chan struct{})
	go func() { wg.Wait(); close(doneCh) }()
	select {
	case <-doneCh:
	case <-time.After(100 * time.Second):
		t.Fatal("timed out waiting for mux loss nodes")
	}
	if dErr != nil {
		t.Fatalf("dialer failed: %v", dErr)
	}
	if lErr != nil {
		t.Fatalf("listener failed: %v", lErr)
	}

	l := readMuxReport(t, lRep)
	d := readMuxReport(t, dRep)
	for _, r := range []muxReport{l, d} {
		if r.StreamsRecvOK != n || r.StreamsRecvFail != 0 {
			t.Fatalf("%s did not recover all streams under loss: %+v", r.Role, r)
		}
		if r.Stats.DecodeFailures != 0 {
			t.Fatalf("%s had decode failures: %d", r.Role, r.Stats.DecodeFailures)
		}
		if r.FramesDropped == 0 {
			t.Fatalf("%s dropped no frames (loss not exercised)", r.Role)
		}
		if r.Retransmits == 0 {
			t.Fatalf("%s performed no retransmissions despite loss", r.Role)
		}
	}
}
