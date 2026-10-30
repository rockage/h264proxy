package tunnel

import (
	"context"
	"fmt"
	"github.com/roca/h264-tunnel-lab/internal/logging"
	"github.com/roca/h264-tunnel-lab/internal/mux"
	"github.com/roca/h264-tunnel-lab/internal/rtc"
	"io"
	"net"
	"testing"
	"time"
)

func TestTCPMuxReleasedWhenSignalingSetupFails(t *testing.T) {
	occupied, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	reserve, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := reserve.Addr().(*net.TCPAddr).Port
	reserve.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	_, err = Open(ctx, Config{Role: mux.RoleListener, HTTP: occupied.Addr().String(), RTC: rtc.Options{Transport: "tcp", TCPPort: port}, Log: logging.NewTo("tcp-release-test", io.Discard)})
	if err == nil {
		t.Fatal("occupied signaling address accepted")
	}
	ln, err := net.Listen("tcp4", net.JoinHostPort("127.0.0.1", fmt.Sprint(port)))
	if err != nil {
		t.Fatalf("ICE TCP listener leaked after setup failure: %v", err)
	}
	ln.Close()
}
