package integration

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/proxy"
)

// This builds the proxy with the race detector too: the worker goroutines in
// the actual subprocesses are checked, not only the test harness.
func TestTwoProcessSOCKSProxy(t *testing.T) {
	if testing.Short() {
		t.Skip("real two-process WebRTC proxy")
	}
	if runtime.GOOS == "windows" {
		t.Skip("test shutdown uses POSIX interrupts")
	}
	_ = freeUDPPort(t)
	work := t.TempDir()
	bin := filepath.Join(work, "proxy")
	buildCmd := exec.Command("go", "build", "-race", "-o", bin, "./cmd/proxy")
	buildCmd.Dir = "../.."
	if b, e := buildCmd.CombinedOutput(); e != nil {
		t.Fatalf("build: %v\n%s", e, b)
	}
	key := make([]byte, 32)
	rand.Read(key)
	keyPath := filepath.Join(work, "key.bin")
	if e := os.WriteFile(keyPath, key, 0600); e != nil {
		t.Fatal(e)
	}
	sigAddr := fmt.Sprintf("127.0.0.1:%d", freeTCPPort(t))
	socksAddr := fmt.Sprintf("127.0.0.1:%d", freeTCPPort(t))
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	var children []*exec.Cmd
	reports := make([]string, 0, 2)
	logs := make([]string, 0, 2)
	for _, role := range []string{"gateway", "client"} {
		report := filepath.Join(work, role+".json")
		reports = append(reports, report)
		logPath := filepath.Join(work, role+".log")
		logs = append(logs, logPath)
		cmd := exec.CommandContext(ctx, bin, "-role", role, "-http", sigAddr, "-socks", socksAddr, "-key", keyPath, "-connect-timeout", "15s", "-report", report, "-log", logPath)
		cmd.Stderr = os.Stderr
		if e := cmd.Start(); e != nil {
			t.Fatal(e)
		}
		children = append(children, cmd)
		t.Cleanup(func() { cmd.Process.Kill() })
	}
	ready := false
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
		c, e := net.DialTimeout("tcp", socksAddr, 100*time.Millisecond)
		if e == nil {
			c.Close()
			ready = true
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if !ready {
		for _, p := range logs {
			b, _ := os.ReadFile(p)
			t.Logf("%s:\n%s", p, b)
		}
		t.Fatal("SOCKS listener never ready")
	}
	dialer, e := proxy.SOCKS5("tcp", socksAddr, nil, proxy.Direct)
	if e != nil {
		t.Fatal(e)
	}
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, e := io.ReadAll(r.Body)
		if e != nil {
			http.Error(w, e.Error(), 500)
			return
		}
		sum := sha256.Sum256(body)
		w.Header().Set("Connection", "close")
		w.Write(sum[:])
		w.Write(body)
	}))
	defer target.Close()
	// The command defaults to remote DNS only: an already resolved IP must
	// fail locally, while a hostname traverses the real WebRTC tunnel.
	if c, err := dialer.Dial("tcp", target.Listener.Addr().String()); err == nil {
		c.Close()
		t.Fatal("default client accepted an IP-only CONNECT")
	}
	targetURL := strings.Replace(target.URL, "127.0.0.1", "localhost", 1)
	transport := &http.Transport{DialContext: dialer.(proxy.ContextDialer).DialContext, DisableKeepAlives: true}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, Timeout: 20 * time.Second}
	errs := make(chan error, 4)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			body := bytes.Repeat([]byte{byte(i + 1)}, 300*1024)
			resp, e := client.Post(targetURL, "application/octet-stream", bytes.NewReader(body))
			if e != nil {
				errs <- e
				return
			}
			defer resp.Body.Close()
			got, e := io.ReadAll(resp.Body)
			if e != nil {
				errs <- e
				return
			}
			sum := sha256.Sum256(body)
			if resp.StatusCode != 200 || !bytes.Equal(got, append(sum[:], body...)) {
				errs <- fmt.Errorf("HTTP body mismatch for stream %d", i)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	// Target waits for TCP FIN before generating its response. This verifies
	// CloseWrite through real SOCKS -> mux -> SRTP -> remote TCP and back.
	fin, e := net.Listen("tcp", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	defer fin.Close()
	finDone := make(chan struct{})
	go func() {
		defer close(finDone)
		c, e := fin.Accept()
		if e != nil {
			return
		}
		defer c.Close()
		b, e := io.ReadAll(c)
		if e == nil {
			c.Write(append([]byte("after-fin:"), b...))
		}
	}()
	_, finPort, _ := net.SplitHostPort(fin.Addr().String())
	stream, e := dialer.Dial("tcp", net.JoinHostPort("localhost", finPort))
	if e != nil {
		t.Fatal(e)
	}
	tcp := stream.(*net.TCPConn)
	tcp.SetDeadline(time.Now().Add(5 * time.Second))
	tcp.Write([]byte("half-close"))
	tcp.CloseWrite()
	got, e := io.ReadAll(tcp)
	tcp.Close()
	if e != nil || string(got) != "after-fin:half-close" {
		t.Fatalf("half-close: %q %v", got, e)
	}
	<-finDone
	time.Sleep(300 * time.Millisecond) // allow final stream ACKs and reclamation
	for _, cmd := range children {
		if e := cmd.Process.Signal(os.Interrupt); e != nil {
			t.Fatal(e)
		}
	}
	for _, cmd := range children {
		if e := cmd.Wait(); e != nil {
			t.Errorf("proxy process: %v", e)
		}
	}
	for i, p := range reports {
		b, e := os.ReadFile(p)
		if e != nil {
			t.Fatal(e)
		}
		var st struct {
			ActiveStreams                            int
			BytesSent, BytesReceived, DecodeFailures uint64
		}
		if e = json.Unmarshal(b, &st); e != nil {
			t.Fatal(e)
		}
		if st.ActiveStreams != 0 || st.BytesSent == 0 || st.BytesReceived == 0 || st.DecodeFailures != 0 {
			t.Errorf("proxy stats: %+v", st)
		}
		log, _ := os.ReadFile(logs[i])
		if !bytes.Contains(log, []byte(`"codec":"video/H264"`)) {
			t.Errorf("missing H264 negotiation: %s", log)
		}
	}
}
