// Package integration runs a genuine two-process WebRTC transfer: it compiles
// the server and client, starts them as separate OS processes, and verifies
// that a file sent through the real SRTP media channel is recovered byte-for-
// byte (SHA-256) and that the negotiated codec was H.264.
//
// This is NOT a mocked unit test: data travels Server RTP -> Pion SRTP -> local
// UDP -> Client SRTP -> Client RTP. If the environment cannot bind loopback
// UDP, the test skips with an explicit message rather than pretending to pass.
package integration

import (
	"encoding/json"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

type report struct {
	Role            string `json:"role"`
	NegotiatedCodec string `json:"negotiated_codec"`
	PayloadType     uint8  `json:"payload_type"`
	SSRC            uint32 `json:"ssrc"`
	SHA256Expected  string `json:"sha256_expected"`
	SHA256Actual    string `json:"sha256_actual"`
	SHA256Match     bool   `json:"sha256_match"`
	SendComplete    bool   `json:"send_complete"`
	ReceiveComplete bool   `json:"receive_complete"`
	RTPPacketsSent  uint64 `json:"rtp_packets_sent"`
	RTPPacketsRecv  uint64 `json:"rtp_packets_received"`
	MissingChunks   uint64 `json:"missing_chunks"`
}

func freeUDPPort(t *testing.T) int {
	t.Helper()
	addr, err := net.ResolveUDPAddr("udp4", "127.0.0.1:0")
	if err != nil {
		t.Skipf("loopback UDP unavailable: %v", err)
	}
	c, err := net.ListenUDP("udp4", addr)
	if err != nil {
		t.Skipf("cannot bind loopback UDP (real network test not possible here): %v", err)
	}
	port := c.LocalAddr().(*net.UDPAddr).Port
	c.Close()
	return port
}

func freeTCPPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("cannot bind loopback TCP for signaling: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func build(t *testing.T, root, pkg, out string) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), out)
	cmd := exec.Command("go", "build", "-o", bin, pkg)
	cmd.Dir = root
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build %s: %v\n%s", pkg, err, b)
	}
	return bin
}

func TestTwoProcessWebRTCTransfer(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping real WebRTC integration test in -short mode")
	}
	// Confirm loopback UDP works before compiling anything.
	_ = freeUDPPort(t)

	root := "../.." // module root relative to this test package
	serverBin := build(t, root, "./cmd/server", "server")
	clientBin := build(t, root, "./cmd/client", "client")
	genkeyBin := build(t, root, "./cmd/genkey", "genkey")

	work := t.TempDir()
	keyPath := filepath.Join(work, "key.bin")
	if b, err := exec.Command(genkeyBin, "-out", keyPath, "-force").CombinedOutput(); err != nil {
		t.Fatalf("genkey: %v\n%s", err, b)
	}

	// Random 256 KiB payload.
	inPath := filepath.Join(work, "in.bin")
	data := make([]byte, 256*1024)
	for i := range data {
		data[i] = byte(i*7 + 3)
	}
	if err := os.WriteFile(inPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	outPath := filepath.Join(work, "out.bin")

	port := freeTCPPort(t)
	addr := "127.0.0.1:" + strconv.Itoa(port)
	srvReport := filepath.Join(work, "srv.json")
	cliReport := filepath.Join(work, "cli.json")

	srv := exec.Command(serverBin,
		"-http", addr, "-key", keyPath, "-in", inPath, "-mode", "aead",
		"-chunk", "900", "-rate", "20000000", "-report", srvReport, "-timeout", "90s")
	srv.Stdout, srv.Stderr = os.Stderr, os.Stderr
	if err := srv.Start(); err != nil {
		t.Fatalf("start server: %v", err)
	}
	defer func() { _ = srv.Process.Kill() }()

	// Client retries fetching the offer, so a short delay is enough.
	time.Sleep(300 * time.Millisecond)

	cli := exec.Command(clientBin,
		"-http", addr, "-key", keyPath, "-out", outPath, "-mode", "aead",
		"-report", cliReport, "-timeout", "90s", "-connect-wait", "30s")
	cli.Stdout, cli.Stderr = os.Stderr, os.Stderr
	if err := cli.Run(); err != nil {
		t.Fatalf("client failed: %v", err)
	}
	if err := srv.Wait(); err != nil {
		t.Fatalf("server exited with error: %v", err)
	}

	sr := readReport(t, srvReport)
	cr := readReport(t, cliReport)

	if sr.NegotiatedCodec != "video/H264" {
		t.Fatalf("server negotiated non-H264 codec: %q", sr.NegotiatedCodec)
	}
	if cr.NegotiatedCodec != "video/H264" {
		t.Fatalf("client negotiated non-H264 codec: %q", cr.NegotiatedCodec)
	}
	if cr.SSRC != sr.SSRC || cr.SSRC == 0 {
		t.Fatalf("SSRC mismatch (client did not read server's stream): server=%d client=%d", sr.SSRC, cr.SSRC)
	}
	if !sr.SendComplete {
		t.Fatal("server did not complete send")
	}
	if !cr.ReceiveComplete || !cr.SHA256Match {
		t.Fatalf("client did not fully recover file: %+v", cr)
	}
	if cr.MissingChunks != 0 {
		t.Fatalf("client reported %d missing chunks", cr.MissingChunks)
	}
	if cr.RTPPacketsRecv == 0 || cr.RTPPacketsRecv != sr.RTPPacketsSent {
		t.Fatalf("packet count mismatch: sent=%d recv=%d", sr.RTPPacketsSent, cr.RTPPacketsRecv)
	}

	// Independent byte comparison of the delivered file.
	got, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if len(got) != len(data) {
		t.Fatalf("size mismatch: want %d got %d", len(data), len(got))
	}
	for i := range data {
		if data[i] != got[i] {
			t.Fatalf("byte %d differs", i)
		}
	}
}

func readReport(t *testing.T, path string) report {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read report %s: %v", path, err)
	}
	var r report
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatalf("parse report %s: %v", path, err)
	}
	return r
}
