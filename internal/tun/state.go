package tun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"
)

type command struct {
	Name string
	Args []string
}
type runner func(command) (string, error)

func execute(c command) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, c.Name, c.Args...).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %w: %s", c.Name, strings.Join(c.Args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

type state struct {
	Version      int
	PID          int
	ChildPID     int
	ChildCommand string
	Undo         []command
}
type transaction struct {
	path  string
	state state
	run   runner
}

func newTransaction(path string, run runner) (*transaction, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, fmt.Errorf("create TUN recovery file (another instance or stale state; use -tun-recover): %w", err)
	}
	t := &transaction{path: path, state: state{Version: 1, PID: os.Getpid()}, run: run}
	err = json.NewEncoder(f).Encode(t.state)
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		os.Remove(path)
		return nil, errors.Join(err, closeErr)
	}
	return t, nil
}

func (t *transaction) save() error {
	b, err := json.MarshalIndent(t.state, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(t.path), ".tun-state-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), t.path)
}

// Record recovery before executing a mutation. Failed atomic commands remove
// their undo entry, so an unsuccessful route add cannot delete an existing route.
func (t *transaction) apply(do, undo command) error {
	t.state.Undo = append(t.state.Undo, undo)
	if err := t.save(); err != nil {
		t.state.Undo = t.state.Undo[:len(t.state.Undo)-1]
		return err
	}
	if _, err := t.run(do); err != nil {
		t.state.Undo = t.state.Undo[:len(t.state.Undo)-1]
		return errors.Join(err, t.save())
	}
	return nil
}

func (t *transaction) rollback() error {
	var failures []command
	var errs []error
	for i := len(t.state.Undo) - 1; i >= 0; i-- {
		c := t.state.Undo[i]
		out, err := t.undo(c)
		// Routes tied to a closed utun disappear automatically. Recovery is idempotent.
		gone := c.Name == "/sbin/route" && (strings.Contains(out, "not in table") || strings.Contains(out, "No such process"))
		if err != nil && !gone {
			failures = append([]command{c}, failures...)
			errs = append(errs, err)
		}
	}
	t.state.Undo = failures
	if len(failures) == 0 {
		return errors.Join(errors.Join(errs...), os.Remove(t.path))
	}
	return errors.Join(errors.Join(errs...), t.save())
}

// utun routes disappear with their interface, including when Ctrl-C reaches
// tun2socks before the parent's cleanup. Do not retry a delete against a
// missing device or remove a replacement route installed by another VPN.
func (t *transaction) undo(c command) (string, error) {
	if c.Name != "/sbin/route" || len(c.Args) < 2 || c.Args[1] != "delete" {
		return t.run(c)
	}
	for i, arg := range c.Args {
		if arg == "-interface" && i+1 < len(c.Args) {
			out, err := t.run(cmd("/sbin/ifconfig", c.Args[i+1]))
			if err != nil {
				if strings.Contains(out, "does not exist") || strings.Contains(out, "No such interface") {
					return "", nil
				}
				return out, err
			}
		}
		if arg == "-host" && i+1 < len(c.Args) {
			out, err := t.run(cmd("/sbin/route", "-n", "get", c.Args[i+1]))
			if err != nil {
				return out, err
			}
			flags := routeFlags(out)
			if routeField(out, "destination") != c.Args[i+1] || !flags["HOST"] || !flags["STATIC"] || flags["WASCLONED"] || flags["IFSCOPE"] {
				return "", nil
			}
		}
	}
	return t.run(c)
}

// Recover restores changes left by a terminated client. It refuses to disturb a
// running instance. Only the journal created by this program should be supplied.
func Recover(path string) error {
	if runtime.GOOS != "darwin" {
		return fmt.Errorf("TUN recovery currently supports macOS only")
	}
	if os.Geteuid() != 0 {
		return fmt.Errorf("TUN recovery requires root")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var s state
	if err = json.Unmarshal(b, &s); err != nil {
		return err
	}
	if s.Version != 1 || s.PID <= 0 {
		return fmt.Errorf("invalid TUN recovery state")
	}
	if p, e := os.FindProcess(s.PID); e == nil && p.Signal(syscall.Signal(0)) == nil {
		return fmt.Errorf("TUN owner PID %d is still running; stop it first", s.PID)
	}
	t := &transaction{path: path, state: s, run: execute}
	restoreErr := t.rollback()
	// SIGKILL of the parent can leave the separately running stack behind.
	// Check the complete command to avoid signalling a reused unrelated PID.
	if s.ChildPID > 0 {
		out, psErr := execute(cmd("/bin/ps", "-ww", "-p", fmt.Sprint(s.ChildPID), "-o", "command="))
		if psErr == nil {
			if strings.TrimSpace(out) != s.ChildCommand {
				return errors.Join(restoreErr, fmt.Errorf("PID %d no longer matches the recorded tun2socks; left it running", s.ChildPID))
			}
			if p, e := os.FindProcess(s.ChildPID); e == nil {
				if e = p.Kill(); e != nil {
					return errors.Join(restoreErr, e)
				}
			}
		}
	}
	return restoreErr
}
