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
	term := newTermOut(out)
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
