package tun

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestPartialSetupRollsBackOnlySuccessfulChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	var calls []command
	run := func(c command) (string, error) {
		calls = append(calls, c)
		// The undo must already be on disk if the process is interrupted here.
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var s state
		if err = json.Unmarshal(b, &s); err != nil {
			t.Fatal(err)
		}
		if c.Name == "fail" {
			return "", errors.New("route already exists")
		}
		return "", nil
	}
	tx, err := newTransaction(path, run)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = newTransaction(path, run); err == nil {
		t.Fatal("second client accepted")
	}
	if err = tx.apply(cmd("dns-new"), cmd("dns-old")); err != nil {
		t.Fatal(err)
	}
	if err = tx.apply(cmd("route-new"), cmd("route-delete")); err != nil {
		t.Fatal(err)
	}
	if err = tx.apply(cmd("fail"), cmd("delete-someone-elses-route")); err == nil {
		t.Fatal("expected setup failure")
	}
	if err = tx.rollback(); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, c := range calls {
		names = append(names, c.Name)
	}
	if !reflect.DeepEqual(names, []string{"dns-new", "route-new", "fail", "route-delete", "dns-old"}) {
		t.Fatalf("unexpected mutation order: %v", names)
	}
	if _, err = os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal not removed: %v", err)
	}
}

func TestFailedRestoreRemainsRecoverable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	fail := true
	run := func(c command) (string, error) {
		if c.Name == "restore-dns" && fail {
			return "", errors.New("networksetup failed")
		}
		return "", nil
	}
	tx, err := newTransaction(path, run)
	if err != nil {
		t.Fatal(err)
	}
	if err = tx.apply(cmd("change-dns"), cmd("restore-dns")); err != nil {
		t.Fatal(err)
	}
	if err = tx.apply(cmd("add-route"), cmd("remove-route")); err != nil {
		t.Fatal(err)
	}
	if err = tx.rollback(); err == nil {
		t.Fatal("restore error lost")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var s state
	if err = json.Unmarshal(b, &s); err != nil {
		t.Fatal(err)
	}
	if len(s.Undo) != 1 || s.Undo[0].Name != "restore-dns" {
		t.Fatalf("wrong retry journal: %+v", s)
	}
	fail = false
	if err = (&transaction{path: path, state: s, run: run}).rollback(); err != nil {
		t.Fatal(err)
	}
}

func TestBypassPreservesExistingHostRoutesAndLAN(t *testing.T) {
	for _, tc := range []struct {
		name, out string
		want      []string
	}{
		{"existing", "destination: 203.0.113.1\nflags: <UP,GATEWAY,HOST,STATIC>\ninterface: en1\n", nil},
		{"scoped clone", "destination: 203.0.113.1\ngateway: 192.168.1.1\nflags: <UP,GATEWAY,HOST,DONE,WASCLONED,IFSCOPE,IFREF>\ninterface: en1\n", []string{"-n", "add", "-host", "203.0.113.1", "192.168.1.1"}},
		{"scoped static", "destination: 203.0.113.1\ngateway: 192.168.1.1\nflags: <UP,GATEWAY,HOST,STATIC,IFSCOPE>\ninterface: en1\n", []string{"-n", "add", "-host", "203.0.113.1", "192.168.1.1"}},
		{"dynamic host", "destination: 203.0.113.1\ngateway: 192.168.1.1\nflags: <UP,GATEWAY,HOST,DYNAMIC>\ninterface: en1\n", []string{"-n", "add", "-host", "203.0.113.1", "192.168.1.1"}},
		{"internet", "destination: default\ngateway: 192.168.1.1\ninterface: en1\n", []string{"-n", "add", "-host", "203.0.113.1", "192.168.1.1"}},
		{"LAN", "destination: 192.168.1\ngateway: link#16\ninterface: en1\n", []string{"-n", "add", "-host", "203.0.113.1", "-interface", "en1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var added []string
			run := func(c command) (string, error) {
				if len(c.Args) > 1 && c.Args[1] == "get" {
					return tc.out, nil
				}
				added = c.Args
				return "", nil
			}
			tx, err := newTransaction(filepath.Join(t.TempDir(), "state"), run)
			if err != nil {
				t.Fatal(err)
			}
			m := Manager{tx: tx}
			if err = m.pin("203.0.113.1"); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(added, tc.want) {
				t.Fatalf("got %v want %v", added, tc.want)
			}
		})
	}
}

func TestSystemConfigurationParsing(t *testing.T) {
	out := "An asterisk (*) denotes that a network service is disabled.\n(1) Ethernet\n(Hardware Port: USB LAN, Device: en1)\n(2) Wi-Fi\n(Hardware Port: Wi-Fi, Device: en0)\n"
	if got := serviceDevice(out, "Ethernet"); got != "en1" {
		t.Fatal(got)
	}
	if got := serviceDevice(out, "Wi-Fi"); got != "en0" {
		t.Fatal(got)
	}
	if got := serviceDevice(out, "missing"); got != "" {
		t.Fatal(got)
	}
	for _, tc := range []struct {
		input string
		want  []string
	}{
		{"There aren't any DNS Servers set on Ethernet.\n", []string{"Empty"}},
		{"1.1.1.1\n2606:4700:4700::1111\n", []string{"1.1.1.1", "2606:4700:4700::1111"}},
		{"fe80::1%en1\n", []string{"fe80::1%en1"}},
	} {
		got, err := parseDNS(tc.input)
		if err != nil || !reflect.DeepEqual(got, tc.want) {
			t.Fatalf("%q: %v %v", tc.input, got, err)
		}
	}
	if _, err := parseDNS("networksetup: service not found"); err == nil {
		t.Fatal("accepted error as DNS servers")
	}
}

func TestResolveBypassIPv4Only(t *testing.T) {
	ips, err := ResolveBypass(t.Context(), "203.0.113.1:9300", []string{"stun:203.0.113.2:19302"}, []string{"203.0.113.1", "127.0.0.1", "203.0.113.3"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(ips, ",") != "203.0.113.1,203.0.113.2,203.0.113.3" {
		t.Fatal(ips)
	}
}

func TestBypassRejectsExistingHostRouteThroughAnotherVPN(t *testing.T) {
	mutations := 0
	run := func(c command) (string, error) {
		if len(c.Args) > 1 && c.Args[1] == "get" {
			return "destination: 203.0.113.1\nflags: <UP,GATEWAY,HOST>\ninterface: utun70\n", nil
		}
		mutations++
		return "", nil
	}
	tx, err := newTransaction(filepath.Join(t.TempDir(), "state"), run)
	if err != nil {
		t.Fatal(err)
	}
	m := Manager{tx: tx}
	if err = m.pin("203.0.113.1"); err == nil || !strings.Contains(err.Error(), "utun70") {
		t.Fatalf("stacked VPN accepted: %v", err)
	}
	if mutations != 0 {
		t.Fatal("modified route owned by another VPN")
	}
}

func TestBypassVerificationAfterTakeoverDetectsRouteLoop(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state")
	takeover := false
	var removed []command
	run := func(c command) (string, error) {
		if len(c.Args) > 1 && c.Args[1] == "get" {
			if takeover {
				return "destination: 128.0\ninterface: utun123\n", nil
			}
			return "destination: default\ngateway: 192.168.1.1\ninterface: en1\n", nil
		}
		if len(c.Args) > 1 && c.Args[1] == "delete" {
			removed = append(removed, c)
		}
		return "", nil
	}
	tx, err := newTransaction(path, run)
	if err != nil {
		t.Fatal(err)
	}
	m := Manager{cfg: Config{Device: "utun123"}, tx: tx}
	if err = m.pin("203.0.113.1"); err != nil {
		t.Fatal(err)
	}
	if err = m.verifyBypass(); err != nil {
		t.Fatal(err)
	}
	if err = m.installRoutes(); err != nil {
		t.Fatal(err)
	}
	takeover = true
	if err = m.verifyBypass(); err == nil || !strings.Contains(err.Error(), "rolling back") {
		t.Fatalf("route loop accepted: %v", err)
	}
	if err = tx.rollback(); err != nil {
		t.Fatal(err)
	}
	if len(removed) != 4 {
		t.Fatalf("must restore split routes and leave the already-replaced host route alone: %v", removed)
	}
}

func TestRecoveryAfterDeviceExitLeavesOtherVPNRoutesAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state")
	mutations := 0
	run := func(c command) (string, error) {
		if c.Name == "/sbin/ifconfig" {
			return "ifconfig: interface utun123 does not exist", errors.New("exit status 1")
		}
		if c.Name == "/sbin/route" && len(c.Args) > 1 && c.Args[1] == "get" {
			return "destination: 203.0.113.1\nflags: <UP,GATEWAY,HOST,WASCLONED,IFSCOPE>\ninterface: utun87\n", nil
		}
		mutations++
		return "", nil
	}
	tx, err := newTransaction(path, run)
	if err != nil {
		t.Fatal(err)
	}
	tx.state.Undo = []command{
		cmd("/sbin/route", "-n", "delete", "-host", "203.0.113.1"),
		cmd("/sbin/route", "-n", "delete", "-inet", "-net", "128.0.0.0/1", "-interface", "utun123"),
	}
	if err = tx.save(); err != nil {
		t.Fatal(err)
	}
	if err = tx.rollback(); err != nil {
		t.Fatal(err)
	}
	if mutations != 0 {
		t.Fatal("deleted a route after ownership changed")
	}
	if _, err = os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale journal remains: %v", err)
	}
}
