//go:build linux

package driver

import (
	"context"
	"os"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func fdCount(t *testing.T) int {
	t.Helper()

	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatalf("ReadDir /proc/self/fd: %v", err)
	}

	return len(entries)
}

func TestFailedStartDoesNotLeakPipes(t *testing.T) {
	d := NewProcessDriver()
	before := fdCount(t)

	for i := 0; i < 100; i++ {
		h, err := d.Start(
			context.Background(),
			Spec{Path: "/definitely/not/a/real/pony-command"},
		)

		if err == nil {
			if h != nil {
				t.Fatalf("unexpected successful start: %+v", h)
			}
			t.Fatal("expected failed Start")
		}
	}

	after := fdCount(t)

	if after > before+5 {
		t.Fatalf(
			"failed starts leaked descriptors: before=%d after=%d",
			before,
			after,
		)
	}
}

func TestProcessDriverStopWaitsForRemainingGroupMembers(t *testing.T) {
	d := &ProcessDriver{Grace: 150 * time.Millisecond}
	ready := t.TempDir() + "/child-ready"
	h, err := d.Start(context.Background(), Spec{
		Path: "sh",
		Args: []string{"-c", `trap 'exit 0' TERM; (trap '' TERM; printf ready > "$PONY_CHILD_READY"; exec sleep 30) & wait`},
		Env:  []string{"PONY_CHILD_READY=" + ready},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = syscall.Kill(-h.PID, syscall.SIGKILL) })

	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("TERM-ignoring child did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	members, err := processGroupMembers(h.PID)
	if err != nil {
		t.Fatalf("inspect process group: %v", err)
	}
	if len(members) < 2 {
		t.Fatalf("process group has %d live members before Stop, want leader and child", len(members))
	}

	waitDone := make(chan ExitResult, 1)
	go func() { waitDone <- d.Wait(h) }()
	started := time.Now()
	if err := d.Stop(context.Background(), h); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if elapsed := time.Since(started); elapsed < d.Grace {
		t.Fatalf("Stop returned after %v, before waiting for the TERM-ignoring child", elapsed)
	}
	members, err = processGroupMembers(h.PID)
	if err != nil {
		t.Fatalf("inspect process group after Stop: %v", err)
	}
	if len(members) != 0 {
		t.Fatalf("Stop returned while live process-group members remained: %v", members)
	}
	select {
	case <-waitDone:
	case <-time.After(time.Second):
		t.Fatal("leader wait did not complete")
	}
}

func TestBlockingReadDoesNotPreventProcessWait(t *testing.T) {
	d := NewProcessDriver()
	pidFile := t.TempDir() + "/child.pid"
	h, err := d.Start(context.Background(), Spec{
		Path: "sh",
		Args: []string{"-c", `setsid sh -c 'echo $$ > "$PONY_CHILD_PID"; exec sleep 30' & exit`},
		Env:  []string{"PONY_CHILD_PID=" + pidFile},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		if raw, err := os.ReadFile(pidFile); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(raw))); err == nil {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
		}
	})

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.ReadFile(pidFile); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := os.ReadFile(pidFile); err != nil {
		t.Fatalf("child did not publish its PID: %v", err)
	}

	// Drain the shell's output so the next read blocks on the silent child,
	// which inherited the pipe's write end.
	buf := make([]byte, 256)
	for time.Now().Before(deadline) {
		n, err := d.ReadTimeout(h, buf, 50*time.Millisecond)
		if err != nil {
			t.Fatalf("ReadTimeout while draining: %v", err)
		}
		if n == 0 {
			break
		}
	}

	readDone := make(chan error, 1)
	go func() {
		_, err := d.Read(h, make([]byte, 1))
		readDone <- err
	}()
	for time.Now().Before(deadline) {
		if h.readMu.TryLock() {
			h.readMu.Unlock()
			time.Sleep(10 * time.Millisecond)
			continue
		}
		break
	}

	waitDone := make(chan ExitResult, 1)
	go func() { waitDone <- d.Wait(h) }()
	select {
	case <-waitDone:
	case <-time.After(time.Second):
		t.Fatalf("Wait deadlocked behind a blocking pipe read (PID %d)", h.PID)
	}
	select {
	case <-readDone:
	case <-time.After(time.Second):
		t.Fatal("closing stdout did not unblock Read")
	}
}
