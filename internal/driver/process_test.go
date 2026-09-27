package driver

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestStartSpawnsAndStopTerminates(t *testing.T) {
	d := NewProcessDriver()
	h, err := d.Start(context.Background(), Spec{Path: "sleep", Args: []string{"1000"}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if h.PID == 0 {
		t.Fatal("expected a real PID")
	}

	done := make(chan ExitResult, 1)
	go func() { done <- d.Wait(h) }()

	if err := d.Stop(context.Background(), h); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	res := <-done
	var exitErr *exec.ExitError
	if !errors.As(res.Err, &exitErr) {
		t.Fatalf("Err = %T, want *exec.ExitError", res.Err)
	}
	if res.Signal != syscall.SIGTERM {
		t.Fatalf("Signal = %v, want SIGTERM", res.Signal)
	}
}

func TestWaitReportsNonZeroExit(t *testing.T) {
	d := NewProcessDriver()
	h, err := d.Start(context.Background(), Spec{Path: "sh", Args: []string{"-c", "exit 7"}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	res := d.Wait(h)
	if res.Err == nil {
		t.Fatal("expected non-zero exit to be reported as an error")
	}
	if res.ExitCode != 7 {
		t.Fatalf("ExitCode = %d, want 7", res.ExitCode)
	}
	if res.Signal != -1 {
		t.Fatalf("Signal = %v, want -1 for a plain exit", res.Signal)
	}
}

// A clean exit must report ExitCode 0 and Signal -1 per the ExitResult
// contract; historically Signal was left at the zero value 0.
func TestWaitCleanExitIsConsistent(t *testing.T) {
	d := NewProcessDriver()
	h, err := d.Start(context.Background(), Spec{Path: "sh", Args: []string{"-c", "exit 0"}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	res := d.Wait(h)
	if res.Err != nil {
		t.Fatalf("Err = %v, want nil for exit 0", res.Err)
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", res.ExitCode)
	}
	if res.Signal != -1 {
		t.Fatalf("Signal = %v, want -1 on a clean exit", res.Signal)
	}
}

func TestWaitAfterWaitReportsClosed(t *testing.T) {
	d := NewProcessDriver()
	h, err := d.Start(context.Background(), Spec{Path: "sh", Args: []string{"-c", "exit 0"}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if res := d.Wait(h); res.Err != nil {
		t.Fatalf("first Wait: %v", res.Err)
	}
	res := d.Wait(h)
	if !errors.Is(res.Err, ErrClosed) {
		t.Fatalf("second Wait Err = %v, want ErrClosed", res.Err)
	}
}

func TestWriteFeedsStdin(t *testing.T) {
	d := NewProcessDriver()
	h, err := d.Start(context.Background(), Spec{Path: "sh", Args: []string{"-c", `read -r line; [ "$line" = "ping" ]`}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	n, err := d.Write(h, []byte("ping\n"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	if n != len("ping\n") {
		t.Fatalf("wrote %d bytes, want %d", n, len("ping\n"))
	}

	res := d.Wait(h)
	if res.Err != nil {
		t.Fatalf("process should exit 0 after reading its line: %v", res.Err)
	}
}

func TestStartAppliesCwdAndEnv(t *testing.T) {
	d := NewProcessDriver()
	dir := t.TempDir()
	h, err := d.Start(context.Background(), Spec{
		Path: "sleep",
		Args: []string{"1000"},
		Cwd:  dir,
		Env:  []string{"PONY_VAL=1"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		done := make(chan struct{})
		go func() {
			d.Wait(h)
			close(done)
		}()
		_ = d.Stop(context.Background(), h)
		<-done
	})

	got, err := os.Readlink(fmt.Sprintf("/proc/%d/cwd", h.PID))
	if err != nil {
		t.Fatalf("read cwd: %v", err)
	}
	if got != dir {
		t.Fatalf("cwd = %q, want %q", got, dir)
	}

	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", h.PID))
	if err != nil {
		t.Fatalf("read environ: %v", err)
	}
	if !strings.Contains(string(data), "PONY_VAL=1") {
		t.Fatal("PONY_VAL=1 not present in the process environment")
	}

	if path := os.Getenv("PATH"); path != "" && !strings.Contains(string(data), "PATH=") {
		t.Fatal("PATH was not inherited")
	}
}

func TestProcessDriverReadOutput(t *testing.T) {
	d := NewProcessDriver()
	h, err := d.Start(context.Background(), Spec{Path: "sh", Args: []string{"-c", "echo hi"}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	var got strings.Builder
	buf := make([]byte, 64)
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(got.String(), "hi") && time.Now().Before(deadline) {
		n, err := d.Read(h, buf)
		if err != nil {
			break
		}
		if n > 0 {
			got.Write(buf[:n])
		}
	}
	if !strings.Contains(got.String(), "hi") {
		t.Fatalf("output %q does not contain hi", got.String())
	}

	res := d.Wait(h)
	if res.Err != nil {
		t.Fatalf("expected clean exit, got: %v", res.Err)
	}
}

func TestEnvOverridesDoNotReplaceHostEnv(t *testing.T) {
	d := NewProcessDriver()
	h, err := d.Start(context.Background(), Spec{
		Path: "sh",
		Args: []string{"-c", "env"},
		Env:  []string{"PONY_OVERRIDE=1"},
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	var got strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := d.Read(h, buf)
		got.Write(buf[:n])
		if err != nil {
			break
		}
	}
	d.Wait(h)

	out := got.String()
	if !strings.Contains(out, "PONY_OVERRIDE=1") {
		t.Fatalf("override missing from env:\n%s", out)
	}
	if !strings.Contains(out, "PATH=") {
		t.Fatalf("host PATH was replaced by the overrides:\n%s", out)
	}
}

func TestStartFailureDoesNotLeakDescriptors(t *testing.T) {
	d := NewProcessDriver()
	countFDs := func() int {
		entries, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			t.Fatalf("read /proc/self/fd: %v", err)
		}
		return len(entries)
	}

	before := countFDs()
	for i := 0; i < 100; i++ {
		if h, err := d.Start(context.Background(), Spec{Path: "definitely-no-such-binary-pony-test"}); err == nil {
			_ = h
			t.Fatal("expected Start to fail for a missing binary")
		}
	}
	after := countFDs()

	const slack = 4
	if after > before+slack {
		t.Fatalf("failed starts leaked descriptors: %d -> %d", before, after)
	}
}

func TestStopIsBoundedWithoutWait(t *testing.T) {
	d := &ProcessDriver{Grace: 100 * time.Millisecond}
	h, err := d.Start(context.Background(), Spec{Path: "sleep", Args: []string{"1000"}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	started := time.Now()
	err = d.Stop(context.Background(), h)
	elapsed := time.Since(started)

	bound := d.Grace + killTimeout + time.Second
	if elapsed > bound {
		t.Fatalf("Stop took %v, want bounded by ~%v", elapsed, bound)
	}
	if err == nil {
		t.Fatal("expected an error: process was never reaped, so done can never close")
	}
}
