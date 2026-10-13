package agent

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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

type burstScreenDriver struct {
	driver.Driver
	remaining int
	done      chan struct{}
	once      sync.Once
}

type promptTestDriver struct {
	mu       sync.Mutex
	writes   [][]byte
	output   chan []byte
	done     chan struct{}
	stopOnce sync.Once
	onSubmit func()
}

func newPromptTestDriver() *promptTestDriver {
	return &promptTestDriver{output: make(chan []byte, 8), done: make(chan struct{})}
}

func (d *promptTestDriver) Start(context.Context, driver.Spec) (*driver.Handle, error) {
	return &driver.Handle{PID: 123}, nil
}

func (d *promptTestDriver) Write(_ *driver.Handle, data []byte) (int, error) {
	d.mu.Lock()
	d.writes = append(d.writes, append([]byte(nil), data...))
	onSubmit := d.onSubmit
	d.mu.Unlock()
	if string(data) == "\n" && onSubmit != nil {
		onSubmit()
	}
	return len(data), nil
}

func (d *promptTestDriver) Read(_ *driver.Handle, p []byte) (int, error) {
	select {
	case data := <-d.output:
		return copy(p, data), nil
	case <-d.done:
		return 0, io.EOF
	}
}

func (d *promptTestDriver) ReadTimeout(h *driver.Handle, p []byte, _ time.Duration) (int, error) {
	return d.Read(h, p)
}

func (d *promptTestDriver) Resize(*driver.Handle, uint16, uint16) error { return nil }

func (d *promptTestDriver) Stop(context.Context, *driver.Handle) error {
	d.stopOnce.Do(func() { close(d.done) })
	return nil
}

func (d *promptTestDriver) Wait(*driver.Handle) driver.ExitResult {
	<-d.done
	return driver.ExitResult{}
}

func (d *promptTestDriver) recordedWrites() [][]byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	writes := make([][]byte, len(d.writes))
	for i := range d.writes {
		writes[i] = append([]byte(nil), d.writes[i]...)
	}
	return writes
}

func promptManager(t *testing.T, d *promptTestDriver) *Manager {
	t.Helper()
	t.Setenv("PONY_AGENT_PROFILES", filepath.Join("..", "..", "profiles", "agents"))
	m := NewManager(d)
	spec := AgentSpec{ID: "test", Command: "scripted"}
	if _, err := m.Start(spec); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = m.Stop(spec.ID) })
	return m
}

func TestPromptSubmitsTextAndExactlyOneEnter(t *testing.T) {
	d := newPromptTestDriver()
	m := promptManager(t, d)
	if _, err := m.Prompt(context.Background(), "test", "explain this", "", 0); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	writes := d.recordedWrites()
	if len(writes) != 2 || string(writes[0]) != "explain this" || string(writes[1]) != "\n" {
		t.Fatalf("writes = %q, want prompt text then one Enter", writes)
	}
}

func TestPromptUsesBracketedPasteAndSubmitsOnce(t *testing.T) {
	d := newPromptTestDriver()
	m := promptManager(t, d)
	m.mu.Lock()
	m.agents["test"].session.screen.Feed([]byte("\x1b[?2004h"))
	m.mu.Unlock()
	if _, err := m.Prompt(context.Background(), "test", "line one\nline two", "", 0); err != nil {
		t.Fatalf("Prompt: %v", err)
	}
	writes := d.recordedWrites()
	want := "\x1b[200~line one\nline two\x1b[201~"
	if len(writes) != 2 || string(writes[0]) != want || string(writes[1]) != "\n" {
		t.Fatalf("writes = %q, want bracketed paste and one Enter", writes)
	}
}

func TestPromptWaitsForWorkingThenIdle(t *testing.T) {
	d := newPromptTestDriver()
	m := promptManager(t, d)
	m.mu.Lock()
	m.agents["test"].session.agentKind = AgentKindCodex
	m.mu.Unlock()
	d.onSubmit = func() {
		go func() {
			d.output <- []byte("Thinking...\r\n")
			time.Sleep(750 * time.Millisecond)
			d.output <- []byte("\x1b[2J\x1b[H❯")
		}()
	}
	state, err := m.Prompt(context.Background(), "test", "do work", AgentStateIdle, 3*time.Second)
	if err != nil {
		t.Fatalf("Prompt wait: %v", err)
	}
	if state != AgentStateIdle {
		t.Fatalf("returned state = %q, want idle", state)
	}
}

func TestPromptWaitTimesOutWithoutResubmitting(t *testing.T) {
	d := newPromptTestDriver()
	m := promptManager(t, d)
	_, err := m.Prompt(context.Background(), "test", "do work", AgentStateIdle, 120*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("Prompt error = %v, want timeout", err)
	}
	writes := d.recordedWrites()
	if len(writes) != 2 {
		t.Fatalf("Prompt wrote %d times, want text and one Enter only: %q", len(writes), writes)
	}
}

func TestPromptWaitReturnsWhenAgentExits(t *testing.T) {
	d := newPromptTestDriver()
	d.onSubmit = func() { d.stopOnce.Do(func() { close(d.done) }) }
	m := promptManager(t, d)
	_, err := m.Prompt(context.Background(), "test", "do work", AgentStateIdle, time.Second)
	if err == nil || !strings.Contains(err.Error(), "exited") {
		t.Fatalf("Prompt error = %v, want process exit", err)
	}
}

func TestPromptRejectsNonRunningSession(t *testing.T) {
	d := newPromptTestDriver()
	m := promptManager(t, d)
	if err := m.Stop("test"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if _, err := m.Prompt(context.Background(), "test", "do work", "", 0); err == nil {
		t.Fatal("Prompt succeeded for a stopped session")
	}
}

func TestPromptWaitRejectsUnsupportedTarget(t *testing.T) {
	d := newPromptTestDriver()
	m := promptManager(t, d)
	_, err := m.Prompt(context.Background(), "test", "do work", "working", time.Second)
	if err == nil || !strings.Contains(err.Error(), "wait state") {
		t.Fatalf("Prompt error = %v, want invalid wait state", err)
	}
	if writes := d.recordedWrites(); len(writes) != 0 {
		t.Fatalf("invalid wait state wrote to process: %q", writes)
	}
}

var _ driver.Driver = (*promptTestDriver)(nil)

func (d *burstScreenDriver) Start(context.Context, driver.Spec) (*driver.Handle, error) {
	return &driver.Handle{PID: 1}, nil
}

func (d *burstScreenDriver) Read(_ *driver.Handle, p []byte) (int, error) {
	if d.remaining == 0 {
		d.once.Do(func() { close(d.done) })
		return 0, io.EOF
	}
	n := min(len(p), d.remaining)
	for i := 0; i < n; i++ {
		p[i] = 'x'
	}
	d.remaining -= n
	return n, nil
}

func (d *burstScreenDriver) Wait(*driver.Handle) driver.ExitResult {
	<-d.done
	return driver.ExitResult{}
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

func TestScreenSnapshotObservesPTYWithoutReadingItAgain(t *testing.T) {
	m := pumpPTY(t)
	spec := AgentSpec{ID: "screen", Command: "sh"}
	t.Cleanup(func() { _ = m.Stop(spec.ID) })
	if _, err := m.Start(spec); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := m.Write(spec.ID, []byte("printf 'SCREEN_TOKEN\\n'\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		snap, err := m.ScreenSnapshot(spec.ID)
		if err != nil {
			t.Fatalf("ScreenSnapshot: %v", err)
		}
		if strings.Contains(snap.Text, "SCREEN_TOKEN") {
			if !snap.Reliable {
				t.Fatal("screen observer unexpectedly lost bytes")
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	snap, _ := m.ScreenSnapshot(spec.ID)
	t.Fatalf("screen did not observe PTY output: %q", snap.Text)
}

func TestScreenObserverDoesNotBlockPTYPump(t *testing.T) {
	const burstBytes = 2 << 20
	d := &burstScreenDriver{remaining: burstBytes, done: make(chan struct{})}
	m := NewManager(d)
	if _, err := m.Start(AgentSpec{ID: "burst", Command: "scripted"}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	started := time.Now()
	buf := make([]byte, 4096)
	for {
		_, err := m.ReadTimeout("burst", buf, time.Second)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("ReadTimeout: %v", err)
		}
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("PTY pump took %s to drain %d bytes with screen observation enabled", elapsed, burstBytes)
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
