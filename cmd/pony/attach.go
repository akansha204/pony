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
	"github.com/akansha204/pony/internal/terminal"
)

const (
	detachKey         = 0x1c // Ctrl+\ detaches from the agent.
	attachPollTimeout = 100 * time.Millisecond
	drainGrace        = 250 * time.Millisecond
)

// attachInputFilter removes terminal mouse reports from input forwarded to
// the agent. Agent output enables mouse reporting in the user's terminal, so
// mouse events arrive here as bytes and would otherwise be typed at the prompt.
type attachInputFilter struct {
	pending    []byte
	discardCSI bool
	discardX10 int
}

func (f *attachInputFilter) feed(b byte) []byte {
	if f.discardCSI {
		if b >= 0x40 && b <= 0x7e {
			f.discardCSI = false
		}
		return nil
	}
	if f.discardX10 > 0 {
		f.discardX10--
		return nil
	}
	if len(f.pending) == 0 {
		if b == 0x1b {
			f.pending = append(f.pending, b)
			return nil
		}
		return []byte{b}
	}
	if len(f.pending) == 1 {
		if b == '[' {
			f.pending = append(f.pending, b)
			return nil
		}
		out := append(f.pending, b)
		f.pending = nil
		return out
	}

	// Hold only a possible mouse prefix. Other CSI key sequences pass through
	// byte-for-byte once their first parameter byte identifies them as non-mouse.
	f.pending = nil
	switch b {
	case '<': // SGR mouse: CSI < button ; x ; y M/m
		f.discardCSI = true
		return nil
	case 'M': // Legacy X10 mouse: CSI M followed by three data bytes.
		f.discardX10 = 3
		return nil
	default:
		return []byte{0x1b, '[', b}
	}
}

func (f *attachInputFilter) flush() []byte {
	// A lone ESC may be the complete Escape key. Keep ESC [ buffered until its
	// next byte arrives; flushing it on a timeout can leak a split mouse report.
	if len(f.pending) > 1 {
		return nil
	}
	out := append([]byte(nil), f.pending...)
	f.pending = nil
	return out
}

const disableMouseTracking = "\x1b[?1000l\x1b[?1002l\x1b[?1003l\x1b[?1006l"

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
	if dropped := mgr.OutputDroppedBytes(id); dropped > 0 {
		_, _ = fmt.Fprintf(out, "[pony: %d earlier output bytes were dropped from the recent-output buffer]\r\n", dropped)
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
	term := newTermOut(out)
	// The agent's mouse mode is not useful through this byte-stream bridge:
	// mouse reports are fed back into the agent as input. Keep the outer
	// terminal in keyboard-only mode while attached.
	_ = term.write([]byte(disableMouseTracking))
	done := make(chan struct{})
	copied := make(chan struct{})
	fatal := make(chan os.Signal, 1)

	go watchSignals(in, mgr, id, done, fatal, sig)
	go func() {
		defer close(copied)
		copyOutput(mgr, id, term, done)
	}()

	err := readUntilDetach(mgr, id, in, fatal)
	close(done)
	<-copied
	return err
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
		ready, err := unix.Poll(poll, int(attachPollTimeout/time.Millisecond))
		if err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return err
		}
		if ready == 0 {
			if pending := inputFilter.flush(); len(pending) > 0 {
				if _, err := mgr.Write(id, pending); err != nil {
					return nil
				}
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
		if filtered := inputFilter.feed(buf[0]); len(filtered) > 0 {
			if _, err := mgr.Write(id, filtered); err != nil {
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
			// Agent output may re-enable mouse tracking. Reset it after each
			// chunk so the outer terminal does not emit reports as typed input.
			if out.write([]byte(disableMouseTracking)) != nil {
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
