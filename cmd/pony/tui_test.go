package main

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/creack/pty"

	"github.com/akansha204/pony/internal/agent"
	"github.com/akansha204/pony/internal/driver"
	"github.com/akansha204/pony/internal/task"
	"github.com/akansha204/pony/internal/validation"
)

func TestTUIEmptyAndNavigation(t *testing.T) {
	m := tuiModel{list: func() []task.Snapshot { return nil }}
	if !strings.Contains(ansi.Strip(m.View().Content), "No tasks") {
		t.Fatal("empty screen did not explain how to start")
	}
	m.tasks = []task.Snapshot{{ID: "one", State: task.StateRunning}, {ID: "two", State: task.StateChecksPassed}}
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

func TestTUIHelpKeyAndCommand(t *testing.T) {
	m := tuiModel{}
	next, _ := m.Update(tea.KeyPressMsg{Code: '?', Text: "?"})
	m = next.(tuiModel)
	if !m.showHelp || !strings.Contains(ansi.Strip(m.View().Content), "Start a task") || !strings.Contains(ansi.Strip(m.View().Content), "prompt ID TEXT") {
		t.Fatalf("help key did not show help: %+v", m)
	}
	next, _ = m.Update(tea.KeyPressMsg{Code: tea.KeyEsc})
	m = next.(tuiModel)
	if m.showHelp {
		t.Fatal("Esc did not close help")
	}
	m.command = "help"
	m.submit()
	if !m.showHelp {
		t.Fatal("help command did not show help")
	}
}

func TestTUIPromptCommandRunsAsynchronously(t *testing.T) {
	m := tuiModel{agentsAPI: agent.NewManager(driver.NewProcessDriver())}
	m.command = `prompt demo --wait idle "explain this repository"`
	cmd := m.submit()
	if cmd == nil || !m.actionPending || m.message != "prompting demo..." {
		t.Fatalf("submit returned cmd=%t pending=%t message=%q", cmd != nil, m.actionPending, m.message)
	}
	next, _ := m.Update(agentPromptMsg{id: "demo", state: agent.AgentStateIdle, waitFor: agent.AgentStateIdle})
	m = next.(tuiModel)
	if m.actionPending || m.message != "demo reached idle" {
		t.Fatalf("completed prompt pending=%t message=%q", m.actionPending, m.message)
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
	var stopped task.TaskID
	m := tuiModel{
		list:    func() []task.Snapshot { return []task.Snapshot{{ID: got.id, State: task.StateRunning}} },
		run:     func(opts runOptions) error { got = opts; return nil },
		stop:    func(id task.TaskID) error { stopped = id; return nil },
		command: "run --id demo --goal 'fix bug' --repo /tmp/repo --command sh --validate true",
	}
	cmd := m.submit()
	if cmd == nil {
		t.Fatal("run command did not schedule background action")
	}
	next, _ := m.Update(cmd())
	m = next.(tuiModel)
	if got.id != "demo" || got.goal != "fix bug" || len(m.tasks) != 1 || m.message != "started demo" {
		t.Fatalf("run = %+v, tasks = %+v, message = %q", got, m.tasks, m.message)
	}
	m.command = "stop demo"
	cmd = m.submit()
	if cmd == nil {
		t.Fatal("stop command did not schedule background action")
	}
	next, _ = m.Update(cmd())
	m = next.(tuiModel)
	if stopped != "demo" || m.message != "stop demo" {
		t.Fatalf("stop command got id %q and message %q", stopped, m.message)
	}
}

func TestUpdateAvailable(t *testing.T) {
	cases := []struct {
		current, latest string
		want            bool
	}{
		{"v0.1.0", "v0.2.0", true},
		{"v0.2.0", "v0.2.0", false},
		{"v0.3.0", "v0.2.0", false},
		{"v0.3.0-0.20261013.123456-abcdef123456", "v0.2.0", false},
		{"dev", "v0.2.0", true},
	}
	for _, tc := range cases {
		if got := updateAvailable(tc.current, tc.latest); got != tc.want {
			t.Errorf("updateAvailable(%q, %q) = %t, want %t", tc.current, tc.latest, got, tc.want)
		}
	}
}

func TestTUILifecycleKeys(t *testing.T) {
	var gotStop, gotRestart, gotClean task.TaskID
	m := tuiModel{
		list:    func() []task.Snapshot { return []task.Snapshot{{ID: "demo", State: task.StateRunning}} },
		stop:    func(id task.TaskID) error { gotStop = id; return nil },
		restart: func(id task.TaskID) error { gotRestart = id; return nil },
		clean:   func(id task.TaskID) error { gotClean = id; return nil },
		tasks:   []task.Snapshot{{ID: "demo", State: task.StateRunning}},
	}
	cases := []struct {
		key  string
		got  *task.TaskID
		want string
	}{
		{"s", &gotStop, "stop demo"},
		{"R", &gotRestart, "restart demo"},
		{"c", &gotClean, "clean demo"},
	}
	for _, tc := range cases {
		next, cmd := m.Update(tea.KeyPressMsg{Code: rune(tc.key[0]), Text: tc.key})
		m = next.(tuiModel)
		if cmd == nil {
			t.Fatalf("%q did not schedule action", tc.key)
		}
		next, _ = m.Update(cmd())
		m = next.(tuiModel)
		if *tc.got != "demo" {
			t.Fatalf("%q did not call the action", tc.key)
		}
		if m.message != tc.want {
			t.Fatalf("%q message = %q, want %q", tc.key, m.message, tc.want)
		}
	}
}

func TestTUILifecycleKeysReportErrors(t *testing.T) {
	m := tuiModel{
		list:  func() []task.Snapshot { return []task.Snapshot{{ID: "demo"}} },
		tasks: []task.Snapshot{{ID: "demo", State: task.StateRunning}},
		stop:  func(task.TaskID) error { return errors.New("agent is stopping") },
	}
	next, cmd := m.Update(tea.KeyPressMsg{Code: 's', Text: "s"})
	m = next.(tuiModel)
	if cmd == nil {
		t.Fatal("stop action was not scheduled")
	}
	next, _ = m.Update(cmd())
	m = next.(tuiModel)
	if !strings.Contains(m.message, "stop demo:") {
		t.Fatalf("error message = %q", m.message)
	}
	empty := tuiModel{}
	next, _ = empty.Update(tea.KeyPressMsg{Code: 's', Text: "s"})
	if next.(tuiModel).message != "no task selected" {
		t.Fatalf("empty selection message = %q", next.(tuiModel).message)
	}
}

func TestTUILifecycleActionRunsOutsideUpdate(t *testing.T) {
	started := false
	m := tuiModel{list: func() []task.Snapshot { return []task.Snapshot{{ID: "demo"}} }, tasks: []task.Snapshot{{ID: "demo"}}, stop: func(task.TaskID) error { started = true; return nil }}
	next, cmd := m.Update(tea.KeyPressMsg{Code: 's', Text: "s"})
	m = next.(tuiModel)
	if started || cmd == nil || m.message != "stop demo..." {
		t.Fatalf("action ran in Update: started=%t message=%q cmd=%t", started, m.message, cmd != nil)
	}
	next, duplicate := m.Update(tea.KeyPressMsg{Code: 's', Text: "s"})
	m = next.(tuiModel)
	if duplicate != nil || m.message != "another action is still running" {
		t.Fatalf("duplicate action not blocked: cmd=%t message=%q", duplicate != nil, m.message)
	}
	msg := cmd()
	if !started {
		t.Fatal("scheduled action did not run")
	}
	next, _ = m.Update(msg)
	if got := next.(tuiModel).message; got != "stop demo" {
		t.Fatalf("completion message = %q", got)
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

func TestTUIValidationReportsTruncatedOutput(t *testing.T) {
	m := tuiModel{list: func() []task.Snapshot { return nil }}
	next, _ := m.Update(validationMsg{
		snapshot: task.Snapshot{ID: "demo"},
		results: []validation.Result{{
			ExitCode: 0, StdoutDroppedBytes: 123, StderrDroppedBytes: 45,
		}},
	})
	m = next.(tuiModel)
	if !strings.Contains(m.message, "stdout truncated (123 bytes omitted)") || !strings.Contains(m.message, "stderr truncated (45 bytes omitted)") {
		t.Fatalf("validation message = %q", m.message)
	}
}

func TestTUIReadReportsDroppedOutput(t *testing.T) {
	m := tuiModel{}
	next, _ := m.Update(agentReadMsg{id: "demo", out: "recent", dropped: 4096})
	m = next.(tuiModel)
	if !strings.Contains(m.message, "4096 earlier output bytes were dropped") || m.agentOutput != "recent" {
		t.Fatalf("read status=%q output=%q", m.message, m.agentOutput)
	}
}

func TestTUISessionStatusShowsAgentIdentityAndState(t *testing.T) {
	m := tuiModel{showSessions: true, width: 140, agents: []agent.SessionSnapshot{{AgentID: "demo", PID: 42, State: agent.StateRunning, AgentState: agent.AgentStateUnknown, AgentKind: agent.AgentKindCodex, ForegroundPID: 43, ForegroundProcess: "codex"}}}
	view := ansi.Strip(m.View().Content)
	if !strings.Contains(view, "runtime=running kind=codex agent=unknown fgpid=43 process=codex") {
		t.Fatalf("session status view = %q", view)
	}
}
