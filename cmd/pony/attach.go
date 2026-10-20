package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/akansha204/pony/internal/agent"
	"github.com/akansha204/pony/internal/screen"
	"github.com/akansha204/pony/internal/terminal"
)

const (
	detachKey         = 0x1c // Ctrl+\ detaches from the agent.
	attachPollTimeout = 100 * time.Millisecond
	drainGrace        = 250 * time.Millisecond
)

const resetAttachedTerminalModes = "\x1b[?9l\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1004l\x1b[?1005l\x1b[?1006l\x1b[?1015l\x1b[?1016l\x1b[?2004l\x1b[=0u\x1b[>4;0m\x1b[?25h\x1b[?47l\x1b[?1047l\x1b[?1049l"

type attachSignalError struct{ signal os.Signal }

func (e *attachSignalError) Error() string {
	return fmt.Sprintf("received %s", e.signal)
}

func attach(mgr *agent.Manager, id agent.AgentID) error {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGWINCH, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGINT)
	defer signal.Stop(sig)

	return attachWithIO(mgr, id, os.Stdin, os.Stdout, sig)
}

func attachWithIO(mgr *agent.Manager, id agent.AgentID, in, out *os.File, sig <-chan os.Signal) error {
	if running(mgr, id) != agent.StateRunning {
		return fmt.Errorf("agent %q has no running session", id)
	}
	return withRawTerminal(in, func() error {
		return bridgeTerminal(mgr, id, in, out, sig)
	})
}

func withRawTerminal(in *os.File, run func() error) error {
	prior, err := terminal.MakeRaw(in.Fd())
	if err != nil {
		return err
	}
	defer func() { _ = terminal.Restore(in.Fd(), prior) }()
	return run()
}

func bridgeTerminal(mgr *agent.Manager, id agent.AgentID, in, out *os.File, sig <-chan os.Signal) error {
	resizeTo(in, mgr, id)
	snapshot, err := mgr.PrepareAttach(id)
	if err != nil {
		return err
	}
	term := newTermOut(out)
	// Restore the outer CLI terminal even if any later setup write fails.
	defer func() { _ = term.write([]byte(resetAttachedTerminalModes)) }()
	if err := term.write([]byte(resetAttachedTerminalModes)); err != nil {
		return err
	}
	if snapshot.AlternateScreen {
		if err := term.write([]byte("\x1b[?1049h")); err != nil {
			return err
		}
	}
	if err := restoreAttachModes(term, snapshot); err != nil {
		return err
	}
	if err := renderAttachSnapshot(term, snapshot); err != nil {
		return err
	}
	done := make(chan struct{})
	copied := make(chan struct{})
	fatal := make(chan os.Signal, 1)

	go watchSignals(in, mgr, id, done, fatal, sig)
	go func() {
		defer close(copied)
		copyOutput(mgr, id, term, done)
	}()

	err = readUntilDetach(mgr, id, in, fatal)
	close(done)
	<-copied
	return err
}

func restoreAttachModes(out *termOut, snapshot screen.Snapshot) error {
	if snapshot.BracketedPaste {
		if err := out.write([]byte("\x1b[?2004h")); err != nil {
			return err
		}
	}
	for _, mode := range snapshot.MouseModes {
		sequence := fmt.Sprintf("\x1b[?%dh", mode)
		if err := out.write([]byte(sequence)); err != nil {
			return err
		}
	}
	if snapshot.FocusReporting {
		return out.write([]byte("\x1b[?1004h"))
	}
	return nil
}

func renderAttachSnapshot(out *termOut, snapshot screen.Snapshot) error {
	if err := out.write([]byte("\x1b[2J\x1b[H")); err != nil {
		return err
	}
	for row, line := range snapshot.Visible {
		sequence := fmt.Sprintf("\x1b[%d;1H\x1b[2K%s", row+1, line)
		if err := out.write([]byte(sequence)); err != nil {
			return err
		}
	}
	if snapshot.Rows > 0 && snapshot.Columns > 0 {
		sequence := fmt.Sprintf("\x1b[%d;%dH", snapshot.CursorRow+1, snapshot.CursorColumn+1)
		return out.write([]byte(sequence))
	}
	return out.write([]byte("\x1b[H"))
}

type termOut struct {
	mu sync.Mutex
	f  *os.File
}

func newTermOut(f *os.File) *termOut {
	return &termOut{f: f}
}

func (t *termOut) write(p []byte) error {
	if len(p) == 0 {
		return nil
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if _, err := t.f.Write(p); err != nil {
		return err
	}
	return nil
}

func resizeTo(in *os.File, mgr *agent.Manager, id agent.AgentID) {
	rows, cols, err := terminal.Size(in.Fd())
	if err == nil {
		_ = mgr.Resize(id, rows, cols)
	}
}

func watchSignals(in *os.File, mgr *agent.Manager, id agent.AgentID, done <-chan struct{}, fatal chan<- os.Signal, sig <-chan os.Signal) {
	for {
		select {
		case <-done:
			return
		case s := <-sig:
			if s == syscall.SIGWINCH {
				resizeTo(in, mgr, id)
				continue
			}
			select {
			case fatal <- s:
			case <-done:
			}
			return
		}
	}
}

func readUntilDetach(mgr *agent.Manager, id agent.AgentID, in *os.File, fatal <-chan os.Signal) error {
	flags, err := unix.FcntlInt(in.Fd(), unix.F_GETFL, 0)
	if err != nil {
		return err
	}
	if err := unix.SetNonblock(int(in.Fd()), true); err != nil {
		return err
	}
	defer func() { _ = unix.SetNonblock(int(in.Fd()), flags&unix.O_NONBLOCK != 0) }()

	// Keep input after Ctrl+\ available to the REPL.
	var buf [1]byte
	var inputFilter attachInputFilter
	var pendingSince time.Time
	allowMouse, allowFocus := false, false
	for {
		select {
		case sig := <-fatal:
			return &attachSignalError{signal: sig}
		default:
		}

		if running(mgr, id) != agent.StateRunning {
			return nil
		}

		poll := []unix.PollFd{{Fd: int32(in.Fd()), Events: unix.POLLIN}}
		ready, err := unix.Poll(poll, 25)
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return err
		}
		if ready == 0 {
			if !pendingSince.IsZero() && time.Since(pendingSince) >= inputFilter.flushAfter() {
				if pending := inputFilter.flush(); len(pending) > 0 {
					if _, err := mgr.Write(id, pending); err != nil {
						return nil
					}
				}
				pendingSince = time.Time{}
			}
			continue
		}

		n, err := in.Read(buf[:])
		switch {
		case errors.Is(err, unix.EAGAIN):
			continue
		case err != nil:
			return nil
		case n == 0:
			continue
		}

		if buf[0] == detachKey {
			return nil
		}
		if pendingSince.IsZero() && (buf[0] == 0x1b || len(inputFilter.pending) > 0) {
			pendingSince = time.Now()
		}
		if len(inputFilter.pending) == 0 && buf[0] == 0x1b {
			allowMouse, allowFocus, _ = mgr.InputModes(id)
		}
		forward := inputFilter.feed(buf[0], allowMouse, allowFocus)
		if len(inputFilter.pending) == 0 {
			pendingSince = time.Time{}
		}
		if len(forward) > 0 {
			if _, err := mgr.Write(id, forward); err != nil {
				return nil
			}
		}
	}
}

func running(mgr *agent.Manager, id agent.AgentID) agent.RuntimeState {
	snap, ok := mgr.Get(id)
	if !ok {
		return ""
	}
	return snap.State
}

func copyOutput(mgr *agent.Manager, id agent.AgentID, out *termOut, done <-chan struct{}) {
	buf := make([]byte, 4096)
	for {
		select {
		case <-done:
			drainOutput(mgr, id, out, buf, drainGrace)
			return
		default:
		}

		n, err := mgr.ReadTimeout(id, buf, attachPollTimeout)
		if n > 0 {
			if out.write(buf[:n]) != nil {
				return
			}
			continue
		}
		if errors.Is(err, io.EOF) {
			return
		}
		if err != nil {
			return
		}
	}
}

func drainOutput(mgr *agent.Manager, id agent.AgentID, out *termOut, buf []byte, grace time.Duration) {
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		n, err := mgr.ReadTimeout(id, buf, 50*time.Millisecond)
		if n > 0 {
			if out.write(buf[:n]) != nil {
				return
			}
			continue
		}
		if err != nil {
			return
		}
	}
}
