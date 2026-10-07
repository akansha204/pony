package main

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/creack/pty"

	"github.com/akansha204/pony/internal/task"
)

func TestTUIEmptyAndNavigation(t *testing.T) {
	m := tuiModel{list: func() []task.Snapshot { return nil }}
	if !strings.Contains(ansi.Strip(m.View().Content), "No tasks") {
		t.Fatal("empty screen did not explain how to start")
	}
	m.tasks = []task.Snapshot{{ID: "one", State: task.StateRunning}, {ID: "two", State: task.StateVerified}}
	next, _ := m.Update(tea.KeyPressMsg{Code: 'j', Text: "j"})
	m = next.(tuiModel)
	if m.selected != 1 || !strings.Contains(ansi.Strip(m.View().Content), "Task: two") {
		t.Fatalf("selection = %d, view = %q", m.selected, m.View().Content)
	}
	m.list = func() []task.Snapshot { return []task.Snapshot{{ID: "new"}, {ID: "one"}, {ID: "two"}} }
	m.reload()
	if m.selected != 2 {
		t.Fatalf("refresh selected index %d, want task two", m.selected)
	}
}

func TestTUIStartsInTerminal(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestCLIHelperProcess$")
	cmd.Env = append(os.Environ(), "PONY_TUI_TEST_HELPER=1", "XDG_DATA_HOME="+t.TempDir())
	master, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 24, Cols: 100})
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	output := make(chan []byte, 1)
	go func() {
		var seen bytes.Buffer
		buf := make([]byte, 4096)
		for {
			n, err := master.Read(buf)
			seen.Write(buf[:n])
			if bytes.Contains(seen.Bytes(), []byte("PONY")) || err != nil {
				output <- seen.Bytes()
				return
			}
		}
	}()
	select {
	case seen := <-output:
		if !bytes.Contains(seen, []byte("PONY")) {
			t.Fatalf("TUI did not render: %q", seen)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("TUI did not render")
	}
	if _, err := master.Write([]byte("q")); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("TUI exit: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("TUI did not quit")
	}
}

func TestTUIRunCommand(t *testing.T) {
	var got runOptions
	m := tuiModel{
		list:    func() []task.Snapshot { return []task.Snapshot{{ID: got.id, State: task.StateRunning}} },
		run:     func(opts runOptions) error { got = opts; return nil },
		command: "run --id demo --goal 'fix bug' --repo /tmp/repo --command sh --validate true",
	}
	m.submit()
	if got.id != "demo" || got.goal != "fix bug" || len(m.tasks) != 1 || m.message != "started demo" {
		t.Fatalf("run = %+v, tasks = %+v, message = %q", got, m.tasks, m.message)
	}
	m.command = "stop demo"
	m.submit()
	if !strings.Contains(m.message, "only run") {
		t.Fatalf("unsupported command message = %q", m.message)
	}
}

func TestTUIStylesAndCompactLayout(t *testing.T) {
	m := tuiModel{
		tasks: []task.Snapshot{{ID: "demo", Goal: "fix\nthis", Repository: "/repo", State: task.StateRunning}},
		width: 80, height: 24,
	}
	full := m.View().Content
	if !strings.Contains(full, "\x1b[") || !strings.Contains(ansi.Strip(full), " ____   ___  _   _ __   __") || !strings.Contains(ansi.Strip(full), "Goal: fix this") {
		t.Fatalf("styled detail view = %q", full)
	}
	m.width = 30
	compact := ansi.Strip(m.View().Content)
	if strings.Contains(compact, " ____   ___") || strings.Contains(compact, "Workspace:") || !strings.Contains(compact, "demo") {
		t.Fatalf("compact view = %q", compact)
	}
	m.width, m.height = 80, 12
	if strings.Contains(ansi.Strip(m.View().Content), " ____   ___") {
		t.Fatal("wordmark did not collapse on a short terminal")
	}
}
