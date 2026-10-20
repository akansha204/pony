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

const resetAttachedTerminalModes = "\x1b[0m\x1b[r\x1b[?6l\x1b[?9l\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1004l\x1b[?1005l\x1b[?1006l\x1b[?1015l\x1b[?1016l\x1b[?2004l\x1b[=0u\x1b[>4;0m\x1b[?25h\x1b[?47l\x1b[?1047l\x1b[?1049l"

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

func withRawTerminal(in *os.File, run func() error) (returnErr error) {
	prior, err := terminal.MakeRaw(in.Fd())
	if err != nil {
		return err
	}
	defer func() { returnErr = errors.Join(returnErr, terminal.Restore(in.Fd(), prior)) }()
	return run()
}

func bridgeTerminal(mgr *agent.Manager, id agent.AgentID, in, out *os.File, sig <-chan os.Signal) (returnErr error) {
	if err := resizeTo(in, mgr, id); err != nil {
		return err
	}
	snapshot, err := mgr.PrepareAttach(id)
	if err != nil {
		return err
	}
	term := newTermOut(out)
	// Restore the outer CLI terminal even if any later setup write fails.
	defer func() {
		// Move below the agent's visible screen, then scroll it clear so the
		// CLI prompt cannot overwrite stale agent output. CSI S scrolls the
		// current region by one screen and works with normal terminal sizes.
		returnErr = errors.Join(returnErr, term.write([]byte(resetAttachedTerminalModes+"\x1b[S\x1b[H\x1b[2J\x1b[H")))
	}()
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
	if _, focus, err := mgr.InputModes(id); err != nil {
		return err
	} else if focus {
		if _, err := mgr.Write(id, []byte("\x1b[I")); err != nil {
			return err
		}
	}
	done := make(chan struct{})
	copied := make(chan error, 1)
	fatal := make(chan error, 2)

	go watchSignals(in, mgr, id, done, fatal, sig)
	go func() {
		copyErr := copyOutput(mgr, id, term, done)
		if copyErr != nil {
			select {
			case fatal <- copyErr:
			case <-done:
			}
		}
		copied <- copyErr
	}()

	err = readUntilDetach(mgr, id, in, fatal)
	if running(mgr, id) == agent.StateRunning {
		if _, focus, modeErr := mgr.InputModes(id); modeErr == nil && focus {
			_, writeErr := mgr.Write(id, []byte("\x1b[O"))
			err = errors.Join(err, writeErr)
		} else if modeErr != nil {
			err = errors.Join(err, modeErr)
		}
	}
	close(done)
	copyErr := <-copied
	if copyErr != nil && errors.Is(err, copyErr) {
		return err
	}
	return errors.Join(err, copyErr)
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
		if row < len(snapshot.StyledVisible) {
			line = snapshot.StyledVisible[row]
		}
		sequence := fmt.Sprintf("\x1b[0m\x1b[%d;1H\x1b[2K%s", row+1, line)
		if err := out.write([]byte(sequence)); err != nil {
			return err
		}
	}
	if snapshot.ScrollTop > 0 || (snapshot.Rows > 0 && snapshot.ScrollBottom != snapshot.Rows-1) {
		if err := out.write([]byte(fmt.Sprintf("\x1b[%d;%dr", snapshot.ScrollTop+1, snapshot.ScrollBottom+1))); err != nil {
			return err
		}
	}
	if snapshot.OriginMode {
		if err := out.write([]byte("\x1b[?6h")); err != nil {
			return err
		}
	}
	if snapshot.Rows > 0 && snapshot.Columns > 0 {
		row := snapshot.CursorRow + 1
		if snapshot.OriginMode {
			row -= snapshot.ScrollTop
		}
		sequence := fmt.Sprintf("\x1b[%d;%dH\x1b[0m%s", row, snapshot.CursorColumn+1, snapshot.CurrentStyle)
		if !snapshot.CursorVisible {
			sequence += "\x1b[?25l"
		}
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

	n, err := t.f.Write(p)
	if err != nil {
		return err
	}
	if n != len(p) {
		return io.ErrShortWrite
	}
	return nil
}

func resizeTo(in *os.File, mgr *agent.Manager, id agent.AgentID) error {
	rows, cols, err := terminal.Size(in.Fd())
	if err != nil {
		return fmt.Errorf("read local terminal size: %w", err)
	}
	return mgr.Resize(id, rows, cols)
}

func watchSignals(in *os.File, mgr *agent.Manager, id agent.AgentID, done <-chan struct{}, fatal chan<- error, sig <-chan os.Signal) {
	for {
		select {
		case <-done:
			return
		case s := <-sig:
			if s == syscall.SIGWINCH {
				if err := resizeTo(in, mgr, id); err != nil {
					select {
					case fatal <- err:
					case <-done:
					}
					return
				}
				continue
			}
			select {
			case fatal <- &attachSignalError{signal: s}:
			case <-done:
			}
			return
		}
	}
}

func readUntilDetach(mgr *agent.Manager, id agent.AgentID, in *os.File, fatal <-chan error) error {
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
		case err := <-fatal:
			return err
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
						return err
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
			return err
		case n == 0:
			return io.EOF
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
				return err
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

func copyOutput(mgr *agent.Manager, id agent.AgentID, out *termOut, done <-chan struct{}) error {
	buf := make([]byte, 4096)
	for {
		select {
		case <-done:
			return drainOutput(mgr, id, out, buf, drainGrace)
		default:
		}

		n, err := mgr.ReadTimeout(id, buf, attachPollTimeout)
		if n > 0 {
			if writeErr := out.write(buf[:n]); writeErr != nil {
				return writeErr
			}
			continue
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func drainOutput(mgr *agent.Manager, id agent.AgentID, out *termOut, buf []byte, grace time.Duration) error {
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		n, err := mgr.ReadTimeout(id, buf, 50*time.Millisecond)
		if n > 0 {
			if writeErr := out.write(buf[:n]); writeErr != nil {
				return writeErr
			}
			continue
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
	return nil
}
