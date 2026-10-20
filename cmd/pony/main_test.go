package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/akansha204/pony/internal/agent"
	"github.com/akansha204/pony/internal/driver"
)

func tempFileWith(t *testing.T, content string) *os.File {
	t.Helper()

	path := filepath.Join(t.TempDir(), "input")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })

	return f
}

func TestReadAgentOutputUsesModeledScreenInsteadOfRawControls(t *testing.T) {
	m := agent.NewManager(driver.NewPTYDriver())
	if _, err := m.Start(agent.AgentSpec{ID: "safe-read", Command: "sh", Args: []string{"-c", "printf '\\033[?1000hSAFE_READ\\n'; sleep 30"}}); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() { _ = m.Stop("safe-read") })
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		snapshot, err := m.ScreenSnapshot("safe-read")
		if err == nil && strings.Contains(snapshot.Text, "SAFE_READ") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	text, _, err := readAgentOutput(m, "safe-read")
	if err != nil {
		t.Fatalf("readAgentOutput: %v", err)
	}
	if !strings.Contains(text, "SAFE_READ") || strings.Contains(text, "\x1b") {
		t.Fatalf("read output %q should contain modeled text without raw terminal controls", text)
	}
}

func TestSafeScreenLinesRemovesC1ControlsAndPreservesRows(t *testing.T) {
	got := strings.Join(safeScreenLines("first\u009bsecond\nthird"), "\n")
	want := "first second\nthird"
	if got != want {
		t.Fatalf("safe screen output %q, want %q", got, want)
	}
}

func TestLineReaderReturnsLinesWithoutNewlines(t *testing.T) {
	r := newLineReader(tempFileWith(t, "one\ntwo\n"))

	for _, want := range []string{"one", "two"} {
		got, err := r.readLine()
		if err != nil {
			t.Fatalf("readLine: %v", err)
		}
		if got != want {
			t.Errorf("read %q, want %q", got, want)
		}
	}

	if _, err := r.readLine(); err != io.EOF {
		t.Errorf("at end of input err = %v, want io.EOF", err)
	}
}

func TestLineReaderLeavesNothingBufferedAhead(t *testing.T) {
	f := tempFileWith(t, "one\ntwo\n")
	r := newLineReader(f)

	got, err := r.readLine()
	if err != nil {
		t.Fatalf("readLine: %v", err)
	}
	if got != "one" {
		t.Fatalf("read %q, want %q", got, "one")
	}

	offset, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		t.Fatalf("Seek: %v", err)
	}
	if offset != int64(len("one\n")) {
		t.Errorf("file advanced to %d, want %d; the line reader read ahead", offset, len("one\n"))
	}
}

func TestLineReaderReturnsAFinalLineWithNoNewline(t *testing.T) {
	r := newLineReader(tempFileWith(t, "one\ntrailing"))

	if _, err := r.readLine(); err != nil {
		t.Fatalf("readLine: %v", err)
	}

	got, err := r.readLine()
	if got != "trailing" {
		t.Errorf("read %q, want %q", got, "trailing")
	}
	if err != io.EOF {
		t.Errorf("err = %v, want io.EOF alongside the final line", err)
	}
}

func TestLineReaderOnEmptyInput(t *testing.T) {
	r := newLineReader(tempFileWith(t, ""))

	if got, err := r.readLine(); err != io.EOF {
		t.Errorf("read %q, err %v; want io.EOF", got, err)
	}
}

func TestParsePromptOptions(t *testing.T) {
	tests := []struct {
		name   string
		fields []string
		id     string
		text   string
		state  string
		bad    bool
	}{
		{name: "plain prompt", fields: []string{"agent", "explain", "this repo"}, id: "agent", text: "explain this repo"},
		{name: "wait for idle", fields: []string{"agent", "--wait", "idle", "explain", "this repo"}, id: "agent", text: "explain this repo", state: "idle"},
		{name: "missing text", fields: []string{"agent"}, bad: true},
		{name: "unsupported wait state", fields: []string{"agent", "--wait", "working", "explain"}, bad: true},
		{name: "unknown option", fields: []string{"agent", "--fast", "explain"}, bad: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parsePrompt(tc.fields)
			if tc.bad {
				if err == nil {
					t.Fatalf("parsePrompt(%v) succeeded, want error", tc.fields)
				}
				return
			}
			if err != nil {
				t.Fatalf("parsePrompt: %v", err)
			}
			if string(got.id) != tc.id || got.text != tc.text || string(got.waitFor) != tc.state {
				t.Fatalf("parsePrompt = %+v", got)
			}
		})
	}
}

func TestSessionAttentionLabel(t *testing.T) {
	for _, tc := range []struct {
		state agent.AgentState
		want  string
	}{
		{state: agent.AgentStateBlocked, want: "needs-you"},
		{state: agent.AgentStateDone, want: "needs-you"},
		{state: agent.AgentStateWorking, want: "no"},
		{state: agent.AgentStateUnknown, want: "no"},
	} {
		if got := sessionAttention(tc.state); got != tc.want {
			t.Errorf("sessionAttention(%q) = %q, want %q", tc.state, got, tc.want)
		}
	}
}

func TestLineReaderHandlesBlankLines(t *testing.T) {
	r := newLineReader(tempFileWith(t, "\n\none\n"))

	for i, want := range []string{"", "", "one"} {
		got, err := r.readLine()
		if err != nil {
			t.Fatalf("line %d: %v", i, err)
		}
		if got != want {
			t.Errorf("line %d read %q, want %q", i, got, want)
		}
	}
}

func TestLineReaderGrowsPastItsInitialCapacity(t *testing.T) {
	long := strings.Repeat("x", 4096)
	r := newLineReader(tempFileWith(t, long+"\n"))

	got, err := r.readLine()
	if err != nil {
		t.Fatalf("readLine: %v", err)
	}
	if got != long {
		t.Errorf("read %d bytes, want %d; the line was truncated", len(got), len(long))
	}
}

func TestLineReaderHandlesCarriageReturns(t *testing.T) {
	r := newLineReader(tempFileWith(t, "one\r\ntwo\r\n"))

	got, err := r.readLine()
	if err != nil {
		t.Fatalf("readLine: %v", err)
	}
	if got != "one\r" {
		t.Errorf("read %q, want %q; the CR should survive for the lexer", got, "one\r")
	}
}

func TestLineReaderFromAPipe(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe: %v", err)
	}
	t.Cleanup(func() {
		_ = pr.Close()
		_ = pw.Close()
	})

	go func() {
		_, _ = pw.Write([]byte("first\n"))
		_ = pw.Close()
	}()

	r := newLineReader(pr)
	got, err := r.readLine()
	if err != nil {
		t.Fatalf("readLine: %v", err)
	}
	if got != "first" {
		t.Errorf("read %q, want %q", got, "first")
	}
}

func TestLineReaderHandsFollowingInputToTheNextReader(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe: %v", err)
	}
	t.Cleanup(func() {
		_ = pr.Close()
		_ = pw.Close()
	})

	go func() {
		_, _ = pw.Write([]byte("attach shell\ntyped while attaching"))
	}()

	r := newLineReader(pr)
	line, err := r.readLine()
	if err != nil {
		t.Fatalf("readLine: %v", err)
	}
	if line != "attach shell" {
		t.Fatalf("read %q, want %q", line, "attach shell")
	}

	buf := make([]byte, len("typed while attaching"))
	if _, err := io.ReadFull(pr, buf); err != nil {
		t.Fatalf("reading what followed: %v", err)
	}
	if string(buf) != "typed while attaching" {
		t.Errorf("next reader got %q, want %q", buf, "typed while attaching")
	}
}

func TestLineReaderStopsOnSignalWithoutInput(t *testing.T) {
	read, write, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	defer write.Close()
	signals := make(chan os.Signal, 1)
	signals <- syscall.SIGTERM
	if _, err := newLineReader(read).readLineWithSignals(signals); err == nil {
		t.Fatal("readLineWithSignals ignored SIGTERM")
	}
}
