package validation

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRunnerCapturesSuccessfulCommand(t *testing.T) {
	cwd := t.TempDir()
	result, err := NewRunner().Run(cwd, Step{
		Command: "sh",
		Args:    []string{"-c", "printf '%s' \"$PWD\"; printf error >&2"},
		Timeout: time.Second,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.Succeeded() || result.ExitCode != 0 {
		t.Fatalf("result = %+v", result)
	}
	if result.Stdout != cwd || result.Stderr != "error" {
		t.Fatalf("output = stdout %q, stderr %q", result.Stdout, result.Stderr)
	}
	if result.Duration <= 0 || result.Duration >= result.Step.Timeout {
		t.Fatalf("duration = %s", result.Duration)
	}
}

func TestRunnerCapturesNonzeroExit(t *testing.T) {
	result, err := NewRunner().Run(t.TempDir(), Step{
		Command: "sh",
		Args:    []string{"-c", "printf out; printf problem >&2; exit 7"},
		Timeout: time.Second,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Succeeded() || result.ExitCode != 7 || result.TimedOut {
		t.Fatalf("result = %+v", result)
	}
	if result.Stdout != "out" || result.Stderr != "problem" {
		t.Fatalf("output = stdout %q, stderr %q", result.Stdout, result.Stderr)
	}
}

func TestRunnerRejectsInvalidInput(t *testing.T) {
	runner := NewRunner()
	if _, err := runner.Run(t.TempDir(), Step{Command: "sh"}); err == nil {
		t.Fatal("Run accepted a zero timeout")
	}
	if _, err := runner.Run("", Step{Command: "sh", Timeout: time.Second}); err == nil {
		t.Fatal("Run accepted an empty working directory")
	}
	if _, err := runner.Run(t.TempDir(), Step{Command: "pony-command-that-does-not-exist", Timeout: time.Second}); err == nil {
		t.Fatal("Run accepted a missing command")
	}
}

func TestRunnerCopiesStepArguments(t *testing.T) {
	step := Step{Command: "printf", Args: []string{"original"}, Timeout: time.Second}
	result, err := NewRunner().Run(t.TempDir(), step)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	step.Args[0] = "changed"
	if result.Step.Args[0] != "original" {
		t.Fatalf("stored args = %q", result.Step.Args)
	}
}

func TestRunnerTimeoutKillsProcessGroup(t *testing.T) {
	cwd := t.TempDir()
	pidFile := filepath.Join(cwd, "child.pid")
	result, err := NewRunner().Run(cwd, Step{
		Command: "sh",
		Args:    []string{"-c", "sleep 1000 & echo $! > child.pid; printf started; wait"},
		Timeout: 200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !result.TimedOut || result.Succeeded() || result.ExitCode != -1 {
		t.Fatalf("result = %+v", result)
	}
	if result.Stdout != "started" {
		t.Fatalf("stdout = %q", result.Stdout)
	}
	if result.Duration < 150*time.Millisecond || result.Duration > 2*time.Second {
		t.Fatalf("duration = %s", result.Duration)
	}

	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read child PID: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatalf("parse child PID: %v", err)
	}
	deadline := time.Now().Add(time.Second)
	for processAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if processAlive(pid) {
		t.Fatalf("child process %d survived timeout", pid)
	}
}

func processAlive(pid int) bool {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	closeParen := strings.LastIndex(string(raw), ")")
	if closeParen < 0 {
		return true
	}
	fields := strings.Fields(string(raw)[closeParen+1:])
	return len(fields) > 0 && fields[0] != "Z"
}

func TestRunnerLeavesNoSignalableChild(t *testing.T) {
	if testing.Short() {
		t.Skip("uses a real process timeout")
	}
	cwd := t.TempDir()
	pidFile := filepath.Join(cwd, "child.pid")
	_, err := NewRunner().Run(cwd, Step{
		Command: "sh",
		Args:    []string{"-c", "sleep 1000 & echo $! > child.pid; wait"},
		Timeout: 100 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatalf("read child PID: %v", err)
	}
	pid, _ := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err := syscall.Kill(pid, 0); err == nil && processAlive(pid) {
		t.Fatalf("child process %d is still signalable and alive", pid)
	}
}
