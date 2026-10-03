package agent

import (
	"context"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/akansha204/pony/internal/driver"
)

func pumpPTY(t *testing.T) *Manager {
	t.Helper()
	return NewManager(driver.NewPTYDriver())
}

type delayedFinalReadDriver struct {
	driver.Driver
	readStarted chan struct{}
	releaseRead chan struct{}
	waitStarted chan struct{}
}

func (d *delayedFinalReadDriver) Start(_ context.Context, _ driver.Spec) (*driver.Handle, error) {
	return &driver.Handle{PID: 1}, nil
}

func (d *delayedFinalReadDriver) Read(_ *driver.Handle, p []byte) (int, error) {
	close(d.readStarted)
	<-d.releaseRead
	return copy(p, []byte("final output")), io.EOF
}

func (d *delayedFinalReadDriver) Wait(_ *driver.Handle) driver.ExitResult {
	close(d.waitStarted)
	return driver.ExitResult{}
}

func readAll(t *testing.T, m *Manager, id AgentID, within time.Duration) string {
	t.Helper()

	var got strings.Builder
	buf := make([]byte, 4096)
	deadline := time.Now().Add(within)

	for time.Now().Before(deadline) {
		n, err := m.ReadTimeout(id, buf, 150*time.Millisecond)

		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("ReadTimeout: %v", err)
		}

		if n > 0 {
			got.Write(buf[:n])
			continue
		}
		if strings.Contains(got.String(), "\n$") || strings.Contains(got.String(), "$ ") {
			break
		}
	}

	return got.String()
}

func TestOutputSurvivesUntilAnAgentIsRead(t *testing.T) {
	m := pumpPTY(t)
	spec := AgentSpec{ID: "shell", Command: "sh"}
	t.Cleanup(func() { _ = m.Stop(spec.ID) })

	if _, err := m.Start(spec); err != nil {
		t.Fatalf("Start: %v", err)
	}

	if _, err := m.Write(spec.ID, []byte("printf 'ready\\n'\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	time.Sleep(500 * time.Millisecond)

	got := readAll(t, m, spec.ID, 2*time.Second)
	if !strings.Contains(got, "ready") {
		t.Errorf("output %q does not contain the line the agent printed before anyone read", got)
	}
}

func TestDetachedAgentKeepsRunningPastTheKernelBuffer(t *testing.T) {
	m := pumpPTY(t)
	spec := AgentSpec{ID: "chatty", Command: "sh"}
	t.Cleanup(func() { _ = m.Stop(spec.ID) })

	if _, err := m.Start(spec); err != nil {
		t.Fatalf("Start: %v", err)
	}

	const lines = 600
	cmd := "i=0; while [ $i -lt " + strconv.Itoa(lines) + " ]; do echo line$i; i=$((i+1)); done; printf 'DONE\\n'\n"
	if _, err := m.Write(spec.ID, []byte(cmd)); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got := readAll(t, m, spec.ID, 10*time.Second)

	if !strings.Contains(got, "DONE") {
		t.Fatalf("agent never reached DONE while detached; it stalled on a full buffer.\nsaw %d bytes", len(got))
	}
	if !strings.Contains(got, "line599") {
		t.Error("the newest output was lost; overflow must discard the oldest bytes")
	}
}

func TestAgentOutputStaysIndependent(t *testing.T) {
	m := pumpPTY(t)
	first := AgentSpec{ID: "first", Command: "sh"}
	second := AgentSpec{ID: "second", Command: "sh"}
	t.Cleanup(func() {
		_ = m.Stop(first.ID)
		_ = m.Stop(second.ID)
	})

	if _, err := m.Start(first); err != nil {
		t.Fatalf("Start first: %v", err)
	}
	if _, err := m.Start(second); err != nil {
		t.Fatalf("Start second: %v", err)
	}

	if _, err := m.Write(first.ID, []byte("printf 'FIRSTTOKEN\\n'\n")); err != nil {
		t.Fatalf("Write first: %v", err)
	}
	if _, err := m.Write(second.ID, []byte("printf 'SECONDTOKEN\\n'\n")); err != nil {
		t.Fatalf("Write second: %v", err)
	}

	firstOut := readAll(t, m, first.ID, 3*time.Second)
	secondOut := readAll(t, m, second.ID, 3*time.Second)

	if !strings.Contains(firstOut, "FIRSTTOKEN") {
		t.Errorf("first agent output %q does not contain its own token", firstOut)
	}
	if strings.Contains(firstOut, "SECONDTOKEN") {
		t.Error("first agent's output contains the second agent's token")
	}
	if !strings.Contains(secondOut, "SECONDTOKEN") {
		t.Errorf("second agent output %q does not contain its own token", secondOut)
	}
	if strings.Contains(secondOut, "FIRSTTOKEN") {
		t.Error("second agent's output contains the first agent's token")
	}
}

func TestOutputRemainsReadableAfterTheAgentExits(t *testing.T) {
	m := pumpPTY(t)
	spec := AgentSpec{ID: "brief", Command: "sh", Args: []string{"-c", "printf 'BYEBYE\\n'"}}

	if _, err := m.Start(spec); err != nil {
		t.Fatalf("Start: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		snap, _ := m.Get(spec.ID)
		if snap.State == StateStopped || snap.State == StateCrashed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("agent did not exit, state %s", snap.State)
		}
		time.Sleep(20 * time.Millisecond)
	}

	got := readAll(t, m, spec.ID, 2*time.Second)
	if !strings.Contains(got, "BYEBYE") {
		t.Errorf("output %q does not contain the agent's final line", got)
	}
}

func TestOutputClosesAfterPumpDrainsFinalRead(t *testing.T) {
	d := &delayedFinalReadDriver{
		readStarted: make(chan struct{}),
		releaseRead: make(chan struct{}),
		waitStarted: make(chan struct{}),
	}
	m := NewManager(d)
	if _, err := m.Start(AgentSpec{ID: "delayed", Command: "unused"}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	select {
	case <-d.readStarted:
	case <-time.After(time.Second):
		t.Fatal("output pump did not start reading")
	}
	select {
	case <-d.waitStarted:
	case <-time.After(time.Second):
		t.Fatal("monitor did not start waiting")
	}
	waitForState(t, m, "delayed", StateStopped)

	if n, err := m.ReadTimeout("delayed", make([]byte, 64), 20*time.Millisecond); n != 0 || err != nil {
		t.Fatalf("read before final pump result = (%d, %v), want timeout without EOF", n, err)
	}

	close(d.releaseRead)
	got := make([]byte, 64)
	n, err := m.Read("delayed", got)
	if err != nil || string(got[:n]) != "final output" {
		t.Fatalf("final read = (%q, %v), want final output", got[:n], err)
	}
	if n, err = m.Read("delayed", got); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("read after final output = (%d, %v), want EOF", n, err)
	}
}

func TestBlockedReaderIsReleasedWhenTheAgentExits(t *testing.T) {
	m := pumpPTY(t)
	spec := AgentSpec{ID: "quiet", Command: "sh", Args: []string{"-c", "sleep 0.2"}}

	if _, err := m.Start(spec); err != nil {
		t.Fatalf("Start: %v", err)
	}

	type result struct {
		err error
	}
	done := make(chan result, 1)
	go func() {
		_, err := m.Read(spec.ID, make([]byte, 64))
		done <- result{err: err}
	}()

	select {
	case r := <-done:
		if r.err != nil && r.err != io.EOF {
			t.Fatalf("Read: %v", r.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a blocked read was never released when the agent exited")
	}
}

func TestRestartStartsWithCleanOutput(t *testing.T) {
	m := pumpPTY(t)
	spec := AgentSpec{ID: "again", Command: "sh"}
	t.Cleanup(func() { _ = m.Stop(spec.ID) })

	if _, err := m.Start(spec); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := m.Write(spec.ID, []byte("printf 'BEFORE\\n'\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	before := readAll(t, m, spec.ID, 3*time.Second)
	if !strings.Contains(before, "BEFORE") {
		t.Fatalf("first generation output %q missing its token", before)
	}

	if err := m.Restart(spec.ID); err != nil {
		t.Fatalf("Restart: %v", err)
	}

	after := readAll(t, m, spec.ID, 500*time.Millisecond)
	if strings.Contains(after, "BEFORE") {
		t.Errorf("new generation served the previous generation's output: %q", after)
	}
}
