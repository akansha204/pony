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

type attachSignalError struct{ signal os.Signal }

func (e *attachSignalError) Error() string {
	return fmt.Sprintf("received %s", e.signal)
}

func attach(mgr *agent.Manager, id agent.AgentID) error {
	if running(mgr, id) != agent.StateRunning {
		return fmt.Errorf("agent %q has no running session", id)
	}

	in, out := os.Stdin, os.Stdout
	prior, err := terminal.MakeRaw(in.Fd())
	if err != nil {
		return err
	}
	defer func() { _ = terminal.Restore(in.Fd(), prior) }()

	resizeTo(in, mgr, id)
	term := newTermOut(out)
	done := make(chan struct{})
	copied := make(chan struct{})
	fatal := make(chan os.Signal, 1)

	go watchSignals(in, mgr, id, done, fatal)
	go func() {
		defer close(copied)
		copyOutput(mgr, id, term, done)
	}()

	err = readUntilDetach(mgr, id, in, term, fatal)
	close(done)
	<-copied
	term.endLine()
	return err
}

type termOut struct {
	mu   sync.Mutex
	f    *os.File
	last byte
}

func newTermOut(f *os.File) *termOut {
	return &termOut{f: f, last: '\n'}
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
	t.last = p[len(p)-1]
	return nil
}

func (t *termOut) endLine() {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.last != '\n' {
		_, _ = t.f.Write([]byte("\r\n"))
		t.last = '\n'
	}
}

func resizeTo(in *os.File, mgr *agent.Manager, id agent.AgentID) {
	rows, cols, err := terminal.Size(in.Fd())
	if err == nil {
		_ = mgr.Resize(id, rows, cols)
	}
}

func watchSignals(in *os.File, mgr *agent.Manager, id agent.AgentID, done <-chan struct{}, fatal chan<- os.Signal) {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGWINCH, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGINT)
	defer signal.Stop(sig)

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

func readUntilDetach(mgr *agent.Manager, id agent.AgentID, in *os.File, out *termOut, fatal <-chan os.Signal) error {
	if err := unix.SetNonblock(int(in.Fd()), true); err != nil {
		return err
	}
	defer func() { _ = unix.SetNonblock(int(in.Fd()), false) }()

	// Keep input after Ctrl+\ available to the REPL.
	var buf [1]byte
	for {
		select {
		case sig := <-fatal:
			return &attachSignalError{signal: sig}
		default:
		}

		if running(mgr, id) != agent.StateRunning {
			out.endLine()
			return nil
		}

		poll := []unix.PollFd{{Fd: int32(in.Fd()), Events: unix.POLLIN}}
		if _, err := unix.Poll(poll, int(attachPollTimeout/time.Millisecond)); err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return err
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
			out.endLine()
			return nil
		}
		if _, err := mgr.Write(id, buf[:n]); err != nil {
			return nil
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
			out.endLine()
			fmt.Fprintln(out.f, "attach:", err)
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
