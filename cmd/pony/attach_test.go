package main

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/akansha204/pony/internal/agent"
	"github.com/akansha204/pony/internal/driver"
)

func TestAttachRejectsMissingAgentBeforeOpeningTerminal(t *testing.T) {
	m := agent.NewManager(driver.NewProcessDriver())
	if err := attach(m, "missing"); err == nil {
		t.Fatal("attach succeeded for a missing agent")
	}
}

func TestReadUntilDetachReturnsFatalSignal(t *testing.T) {
	m := agent.NewManager(driver.NewProcessDriver())
	spec := agent.AgentSpec{ID: "running", Command: "sleep", Args: []string{"10"}}
	if _, err := m.Start(spec); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = m.Stop(spec.ID) })

	in, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe: %v", err)
	}
	t.Cleanup(func() {
		_ = in.Close()
		_ = writer.Close()
	})

	_, term := captureFile(t)
	fatal := make(chan os.Signal, 1)
	fatal <- syscall.SIGTERM

	err = readUntilDetach(m, spec.ID, in, term, fatal)
	var signalErr *attachSignalError
	if !errors.As(err, &signalErr) || signalErr.signal != syscall.SIGTERM {
		t.Fatalf("error = %v, want SIGTERM attachSignalError", err)
	}
}

func TestReadUntilDetachForwardsControlBytesAndLeavesFollowingInput(t *testing.T) {
	m := agent.NewManager(driver.NewProcessDriver())
	spec := agent.AgentSpec{ID: "cat", Command: "cat"}
	if _, err := m.Start(spec); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = m.Stop(spec.ID) })

	in, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe: %v", err)
	}
	t.Cleanup(func() {
		_ = in.Close()
		_ = writer.Close()
	})
	if _, err := writer.Write([]byte{0x03, 0x04, detachKey, 'x'}); err != nil {
		t.Fatalf("Write: %v", err)
	}

	_, term := captureFile(t)
	if err := readUntilDetach(m, spec.ID, in, term, make(chan os.Signal)); err != nil {
		t.Fatalf("readUntilDetach: %v", err)
	}

	var next [1]byte
	if _, err := io.ReadFull(in, next[:]); err != nil || next[0] != 'x' {
		t.Fatalf("following input = %q, %v; want x", next, err)
	}

	got := make([]byte, 0, 2)
	deadline := time.Now().Add(time.Second)
	for len(got) < 2 && time.Now().Before(deadline) {
		buf := make([]byte, 2-len(got))
		n, err := m.ReadTimeout(spec.ID, buf, time.Until(deadline))
		if err != nil {
			t.Fatalf("ReadTimeout: %v", err)
		}
		got = append(got, buf[:n]...)
	}
	if len(got) != 2 || got[0] != 0x03 || got[1] != 0x04 {
		t.Fatalf("forwarded bytes = %v, want [3 4]", got)
	}
}

func TestDetachKeyIsCtrlBackslash(t *testing.T) {
	if detachKey != 0x1c {
		t.Errorf("detachKey = %#x, want 0x1c (Ctrl+\\)", detachKey)
	}
}

func captured(t *testing.T, out *termOut) string {
	t.Helper()

	b, err := os.ReadFile(out.f.Name())
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	return string(b)
}

func captureFile(t *testing.T) (*os.File, *termOut) {
	t.Helper()

	path := filepath.Join(t.TempDir(), "out")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })

	return f, newTermOut(f)
}

func TestTermOutTracksTheLastByteWritten(t *testing.T) {
	_, term := captureFile(t)

	if err := term.write([]byte("no newline here")); err != nil {
		t.Fatalf("write: %v", err)
	}

	term.endLine()

	if got, want := captured(t, term), "no newline here\r\n"; got != want {
		t.Errorf("output %q, want %q", got, want)
	}
}

func TestTermOutEndLineIsANoOpAtColumnZero(t *testing.T) {
	_, term := captureFile(t)

	if err := term.write([]byte("done\n")); err != nil {
		t.Fatalf("write: %v", err)
	}

	term.endLine()

	if got := captured(t, term); got != "done\n" {
		t.Errorf("output %q, want %q; a blank line was added", got, "done\n")
	}
}

func TestTermOutWritesEmptySlicesAsNothing(t *testing.T) {
	_, term := captureFile(t)

	if err := term.write(nil); err != nil {
		t.Fatalf("write: %v", err)
	}

	term.endLine()

	if got := captured(t, term); got != "" {
		t.Errorf("output %q, want empty", got)
	}
}

func TestTermOutConcurrentWritersLeaveALineBoundary(t *testing.T) {
	_, term := captureFile(t)

	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 10 {
				_ = term.write([]byte("x"))
			}
		}()
	}
	wg.Wait()

	term.endLine()

	got := captured(t, term)
	if !strings.HasSuffix(got, "\r\n") {
		t.Errorf("output %q does not end in a line boundary", got)
	}
	if strings.Count(got, "x") != 40 {
		t.Errorf("wrote %d bytes, want 40", strings.Count(got, "x"))
	}
}
