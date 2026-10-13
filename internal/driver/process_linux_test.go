//go:build linux

package driver

import (
	"context"
	"os"
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
