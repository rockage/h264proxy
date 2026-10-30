package tun

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	"golang.org/x/net/proxy"
)

func TestSingBoxConfigPolicy(t *testing.T) {
	b, err := singBoxConfig(Config{Device: "utun123", SOCKS: "127.0.0.1:1080", DirectIPs: []string{"207.148.110.180"}})
	if err != nil {
		t.Fatal(err)
	}
	var c map[string]any
	if err = json.Unmarshal(b, &c); err != nil {
		t.Fatal(err)
	}
	// Assert security/lifecycle invariants, not JSON formatting or field order.
	outs := c["outbounds"].([]any)
	if len(outs) != 1 || outs[0].(map[string]any)["type"] != "socks" {
		t.Fatal("unexpected direct fallback")
	}
	dns := c["dns"].(map[string]any)
	server := dns["servers"].([]any)[0].(map[string]any)
	if server["detour"] != "h264" || server["server"] != "1.1.1.1" {
		t.Fatal("DNS bootstrap escapes tunnel")
	}
	tun := c["inbounds"].([]any)[0].(map[string]any)
	if tun["auto_route"] != true || tun["dns_mode"] != "disabled" {
		t.Fatal("DNS mutation must be journaled by parent")
	}
	if !bytes.Contains(b, []byte("207.148.110.180/32")) {
		t.Fatal("transport not excluded")
	}
}

// Exercise the actual pinned executable without root/network takeover. Replace
// only its TUN ingress with mixed ingress and move DNS off privileged port 53.
// The production DNS/routing/outbound rules remain the same.
func TestSingBoxExecutable(t *testing.T) {
	bin := os.Getenv("H264_SINGBOX_TEST_BINARY")
	if bin == "" {
		t.Skip("set H264_SINGBOX_TEST_BINARY for real executable integration")
	}
	doh := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var wire []byte
		if r.Method == "GET" {
			wire, _ = base64.RawURLEncoding.DecodeString(r.URL.Query().Get("dns"))
		} else {
			wire, _ = io.ReadAll(r.Body)
		}
		w.Header().Set("Content-Type", "application/dns-message")
		w.Write(dnsAnswer(t, wire, false))
	}))
	defer doh.Close()
	payload := bytes.Repeat([]byte("sing-box-webRTC-backend-integrity"), 32768)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(payload) }))
	defer origin.Close()
	// The SOCKS stub records literal DoH bootstrap and only reaches local fixtures.
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				c.SetDeadline(time.Now().Add(10 * time.Second))
				var h [4]byte
				if _, err := io.ReadFull(c, h[:2]); err != nil {
					return
				}
				methods := make([]byte, int(h[1]))
				if _, err := io.ReadFull(c, methods); err != nil {
					return
				}
				c.Write([]byte{5, 0})
				if _, err := io.ReadFull(c, h[:]); err != nil {
					return
				}
				if h[0] != 5 || h[1] != 1 || h[3] != 1 {
					t.Error("expected literal IPv4 CONNECT")
					return
				}
				var dest [6]byte
				if _, err := io.ReadFull(c, dest[:]); err != nil {
					return
				}
				address := net.JoinHostPort(net.IP(dest[:4]).String(), strconv.Itoa(int(binary.BigEndian.Uint16(dest[4:]))))
				if address == "1.1.1.1:443" {
					address = doh.Listener.Addr().String()
				} else if address != origin.Listener.Addr().String() {
					t.Error("unexpected SOCKS target", address)
					return
				}
				remote, err := net.DialTimeout("tcp", address, time.Second)
				if err != nil {
					t.Error(err)
					return
				}
				defer remote.Close()
				c.Write([]byte{5, 0, 0, 1, 127, 0, 0, 1, 0, 1})
				done := make(chan struct{})
				go func() { io.Copy(remote, c); close(done) }()
				io.Copy(c, remote)
				c.Close()
				<-done
			}()
		}
	}()
	port := func() int {
		l, e := net.Listen("tcp4", "127.0.0.1:0")
		if e != nil {
			t.Fatal(e)
		}
		defer l.Close()
		return l.Addr().(*net.TCPAddr).Port
	}
	dnsPort, mixedPort := port(), port()
	b, _ := singBoxConfig(Config{Device: "utun123", SOCKS: ln.Addr().String()})
	productionPath := filepath.Join(t.TempDir(), "production.json")
	if err := os.WriteFile(productionPath, b, 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command(bin, "check", "-c", productionPath).CombinedOutput(); err != nil {
		t.Fatalf("production config: %v %s", err, out)
	}
	var c map[string]any
	json.Unmarshal(b, &c)
	in := c["inbounds"].([]any)
	in[0] = map[string]any{"type": "mixed", "tag": "test-mixed", "listen": "127.0.0.1", "listen_port": mixedPort}
	in[1].(map[string]any)["listen_port"] = dnsPort
	// Local test CA only; the production config above always verifies TLS.
	c["dns"].(map[string]any)["servers"].([]any)[0].(map[string]any)["tls"].(map[string]any)["insecure"] = true
	path := filepath.Join(t.TempDir(), "config.json")
	b, _ = json.Marshal(c)
	os.WriteFile(path, b, 0600)
	if out, err := exec.Command(bin, "check", "-c", path).CombinedOutput(); err != nil {
		t.Fatalf("check: %v %s", err, out)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	process := exec.CommandContext(ctx, bin, "run", "-c", path)
	var logs bytes.Buffer
	process.Stdout = &logs
	process.Stderr = &logs
	if err := process.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cancel()
		process.Wait()
		if t.Failed() {
			t.Log(logs.String())
		}
	}()
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(dnsPort))
	for i := 0; i < 100; i++ {
		c, e := net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if e == nil {
			c.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	for _, network := range []string{"udp4", "tcp4"} {
		for _, qtype := range []dnsmessage.Type{dnsmessage.TypeA, dnsmessage.TypeAAAA, 64, 65} {
			m := dnsmessage.Message{Header: dnsmessage.Header{ID: 43, RecursionDesired: true}, Questions: []dnsmessage.Question{{Name: dnsmessage.MustNewName("example.com."), Type: qtype, Class: dnsmessage.ClassINET}}}
			wire, _ := m.Pack()
			conn, e := net.DialTimeout(network, addr, time.Second)
			if e != nil {
				t.Fatal(e)
			}
			conn.SetDeadline(time.Now().Add(5 * time.Second))
			if network == "tcp4" {
				wire = append([]byte{byte(len(wire) >> 8), byte(len(wire))}, wire...)
			}
			conn.Write(wire)
			buf := make([]byte, 4096)
			var n int
			if network == "tcp4" {
				var size [2]byte
				_, e = io.ReadFull(conn, size[:])
				if e == nil {
					n = int(binary.BigEndian.Uint16(size[:]))
					_, e = io.ReadFull(conn, buf[:n])
				}
			} else {
				n, e = conn.Read(buf)
			}
			conn.Close()
			if e != nil {
				t.Fatal(e)
			}
			var answer dnsmessage.Message
			if e = answer.Unpack(buf[:n]); e != nil {
				t.Fatal(e)
			}
			if answer.ID != 43 || answer.RCode != dnsmessage.RCodeSuccess {
				t.Fatalf("bad response: %+v", answer.Header)
			}
			if qtype == dnsmessage.TypeA && len(answer.Answers) != 1 {
				t.Fatal("missing A")
			}
			if qtype != dnsmessage.TypeA && len(answer.Answers) != 0 {
				t.Fatal("IPv4 policy bypassed")
			}
		}
	}
	dialer, err := proxy.SOCKS5("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(mixedPort)), nil, proxy.Direct)
	if err != nil {
		t.Fatal(err)
	}
	tr := &http.Transport{Dial: dialer.Dial}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: 5 * time.Second}
	resp, err := client.Get(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil || sha256.Sum256(data) != sha256.Sum256(payload) {
		t.Fatal("TCP payload corruption", err)
	}
	// Stop before inspecting logs or counters owned by server goroutines.
	tr.CloseIdleConnections()
	doh.CloseClientConnections()
}
