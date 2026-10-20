package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/akansha204/pony/internal/agent"
	"github.com/akansha204/pony/internal/driver"
	"github.com/akansha204/pony/internal/event"
	"github.com/akansha204/pony/internal/task"
	"github.com/akansha204/pony/internal/validation"
	"github.com/akansha204/pony/internal/workspace"
)

type noGoalWriteDriver struct {
	driver.Driver
	writes int
}

type dirtyLaunchFailureDriver struct {
	driver.Driver
	err error
}

func (d *dirtyLaunchFailureDriver) Start(_ context.Context, spec driver.Spec) (*driver.Handle, error) {
	if err := os.WriteFile(filepath.Join(spec.Cwd, "keep.txt"), []byte("keep"), 0o644); err != nil {
		return nil, err
	}
	return nil, d.err
}

func (d *noGoalWriteDriver) Write(_ *driver.Handle, _ []byte) (int, error) {
	d.writes++
	return 0, errors.New("unexpected PTY goal write")
}

func cliRepository(t *testing.T) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-b", "main"}, {"add", "README.md"}, {"commit", "-m", "initial"}} {
		if args[0] == "add" {
			if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("ready\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Pony Test", "GIT_AUTHOR_EMAIL=pony@example.test", "GIT_COMMITTER_NAME=Pony Test", "GIT_COMMITTER_EMAIL=pony@example.test")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return repo
}

func cliApp(t *testing.T) (*app, string) {
	t.Helper()
	repo := cliRepository(t)
	a, err := newApp(filepath.Join(t.TempDir(), "workspaces"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, snapshot := range a.agents.Snapshots() {
			_ = a.agents.Stop(snapshot.AgentID)
		}
	})
	return a, repo
}

func waitCLIState(t *testing.T, a *app, id task.TaskID, want task.State) task.Snapshot {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, ok := a.tasks.Get(id)
		if ok && got.State == want {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("task %q state = %s, want %s", id, got.State, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestParseRun(t *testing.T) {
	opts, err := parseRun([]string{"--id", "task", "--goal", "fix this", "--repo", "/repo", "--command", "sh", "--arg", "-i", "--validate", "go test ./...", "--validate", "go vet ./..."})
	if err != nil {
		t.Fatalf("parseRun: %v", err)
	}
	if opts.id != "task" || opts.goal != "fix this" || opts.baseRef != "HEAD" || len(opts.args) != 1 || len(opts.steps) != 2 {
		t.Fatalf("options = %+v", opts)
	}
	if opts.steps[0].Command != "go" || strings.Join(opts.steps[0].Args, " ") != "test ./..." || opts.steps[0].Timeout != 5*time.Minute {
		t.Fatalf("validation step = %+v", opts.steps[0])
	}
	for _, fields := range [][]string{
		{"--id", "task"},
		{"--id", "task", "--goal", "goal", "--repo", "/repo"},
		{"--unknown", "value"},
		{"--validate"},
	} {
		if _, err := parseRun(fields); err == nil {
			t.Fatalf("parseRun accepted %v", fields)
		}
	}
}

func TestResolveGoalLaunch(t *testing.T) {
	goal := "--start\nquote ' \" ; $(touch nope)"
	for _, tc := range []struct {
		name    string
		command string
		args    []string
		want    []string
		bad     bool
	}{
		{"opencode", "opencode", nil, []string{"--prompt=" + goal}, false},
		{"codex", "codex", nil, []string{"--", goal}, false},
		{"claude", "claude", nil, []string{"--", goal}, false},
		{"aider", "aider", nil, []string{"--message=" + goal}, false},
		{"custom", "/usr/bin/custom", []string{"--", "{goal}"}, []string{"--", goal}, false},
		{"unknown", "custom", nil, nil, true},
		{"embedded", "sh", []string{"-c", "echo {goal}"}, nil, true},
		{"duplicate", "sh", []string{"{goal}", "{goal}"}, nil, true},
		{"opencode subcommand", "opencode", []string{"run"}, nil, true},
		{"opencode old positional goal", "opencode", []string{"old goal"}, nil, true},
		{"opencode option value", "opencode", []string{"--model", "review"}, []string{"--model", "review", "--prompt=" + goal}, false},
		{"opencode duplicate", "opencode", []string{"--prompt", "old"}, nil, true},
		{"opencode option terminator", "opencode", []string{"--"}, nil, true},
		{"codex subcommand", "codex", []string{"exec"}, nil, true},
		{"codex old positional goal", "codex", []string{"old goal"}, nil, true},
		{"codex option value", "codex", []string{"--model", "review"}, []string{"--model", "review", "--", goal}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			launch, err := resolveGoalLaunch(runOptions{command: tc.command, args: tc.args, goal: goal})
			if (err != nil) != tc.bad {
				t.Fatalf("launch = %+v, error = %v", launch, err)
			}
			if !tc.bad && !equalStrings(launch.Args, tc.want) {
				t.Fatalf("args = %#v, want %#v", launch.Args, tc.want)
			}
		})
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestRunPassesGoalAfterDelayedStartupWithoutTerminalInput(t *testing.T) {
	a, repo := cliApp(t)
	goal := "--first line\nsecond ' \" ; $(touch unwanted)"
	goalFile := filepath.Join(t.TempDir(), "goal")
	started, err := a.run(runOptions{
		id: "delayed", goal: goal, repo: repo, baseRef: "HEAD", command: "sh",
		args: []string{"-c", `sleep 0.2; printf '%s' "$1" > "$2"`, "pony-goal", "{goal}", goalFile},
	})
	if err != nil || started.SessionID == "" {
		t.Fatalf("run = %+v, %v", started, err)
	}
	waitCLIState(t, a, "delayed", task.StateCompleted)
	raw, err := os.ReadFile(goalFile)
	if err != nil || string(raw) != goal {
		t.Fatalf("delivered goal = %q, error = %v", raw, err)
	}
	if _, err := os.Stat(filepath.Join(started.WorkspacePath, "unwanted")); !os.IsNotExist(err) {
		t.Fatalf("goal text was executed: %v", err)
	}
}

func TestRunInvalidTransportDoesNotAllocateAndFailedLaunchCanRetry(t *testing.T) {
	a, repo := cliApp(t)
	opts := runOptions{id: "retry", goal: "goal", repo: repo, baseRef: "HEAD", command: "unknown-tool"}
	if _, err := a.run(opts); err == nil {
		t.Fatal("unknown command accepted without goal argument")
	}
	if _, ok := a.tasks.Get(opts.id); ok {
		t.Fatal("invalid transport created a task")
	}
	opts.args = []string{"{goal}"}
	if _, err := a.run(opts); err == nil {
		t.Fatal("missing executable launched")
	}
	if _, ok := a.tasks.Get(opts.id); ok {
		t.Fatal("failed launch left a pending task")
	}
	opts.command = "sh"
	opts.args = []string{"-c", `test "$1" = goal`, "pony-goal", "{goal}"}
	if _, err := a.run(opts); err != nil {
		t.Fatalf("same ID retry: %v", err)
	}
	waitCLIState(t, a, opts.id, task.StateCompleted)
}

func TestRunRetainsChangedWorkspaceWhenFailedLaunchCannotRollback(t *testing.T) {
	repo := cliRepository(t)
	workspaces, err := workspace.NewManager(filepath.Join(t.TempDir(), "workspaces"))
	if err != nil {
		t.Fatal(err)
	}
	wantErr := errors.New("launch failed after worktree changed")
	agents := agent.NewManager(&dirtyLaunchFailureDriver{Driver: driver.NewPTYDriver(), err: wantErr})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	a := &app{agents: agents, workspaces: workspaces, tasks: task.NewManager(workspaces, agents), ctx: ctx, cancel: cancel, runs: make(map[task.TaskID]*runState)}

	got, err := a.run(runOptions{id: "keep", goal: "goal", repo: repo, baseRef: "HEAD", command: "sh", args: []string{"{goal}"}})
	if !errors.Is(err, wantErr) || got.State != task.StateFailed || got.WorkspaceID == "" {
		t.Fatalf("run = %+v, %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(got.WorkspacePath, "keep.txt")); err != nil {
		t.Fatalf("changed worktree was lost: %v", err)
	}
	if stored, ok := a.tasks.Get("keep"); !ok || stored.WorkspaceID != got.WorkspaceID {
		t.Fatalf("failed task not tracked: %+v, found=%t", stored, ok)
	}
	if err := a.clean("keep"); err == nil {
		t.Fatal("clean removed changed worktree")
	}
	if err := os.Remove(filepath.Join(got.WorkspacePath, "keep.txt")); err != nil {
		t.Fatal(err)
	}
	if err := a.clean("keep"); err != nil {
		t.Fatalf("clean recovered task: %v", err)
	}
}

func TestRunRetainsHookChangedWorkspaceWhenAllocationCannotRollback(t *testing.T) {
	a, repo := cliApp(t)
	hook := filepath.Join(repo, ".git", "hooks", "post-checkout")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\nprintf 'keep me\\n' > hook-produced.txt\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := a.run(runOptions{id: "hook-data", goal: "goal", repo: repo, baseRef: "HEAD", command: "sh", args: []string{"{goal}"}})
	if err == nil || got.State != task.StateFailed || got.WorkspaceID == "" {
		t.Fatalf("run = %+v, %v", got, err)
	}
	if raw, err := os.ReadFile(filepath.Join(got.WorkspacePath, "hook-produced.txt")); err != nil || string(raw) != "keep me\n" {
		t.Fatalf("hook file = %q, error = %v", raw, err)
	}
	if stored, ok := a.tasks.Get("hook-data"); !ok || stored.WorkspaceID != got.WorkspaceID {
		t.Fatalf("failed allocation not tracked: %+v, found=%t", stored, ok)
	}
	if err := a.clean("hook-data"); err == nil {
		t.Fatal("clean removed hook data")
	}
}

func TestRunCompletesAndValidatesInWorkspace(t *testing.T) {
	a, repo := cliApp(t)
	opts := runOptions{
		id: "task", goal: "do the work", repo: repo, baseRef: "HEAD",
		command: "sh", args: []string{"-c", "test \"$1\" = 'do the work'", "pony-goal", "{goal}"},
		steps: []validation.Step{{Command: "sh", Args: []string{"-c", "test -f README.md && pwd"}, Timeout: time.Second}},
	}
	started, err := a.run(opts)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	got := waitCLIState(t, a, "task", task.StateChecksPassed)
	if got.WorkspacePath != started.WorkspacePath || got.WorkspacePath == repo {
		t.Fatalf("workspace = %q", got.WorkspacePath)
	}
	events := a.events.ListTask("task")
	if events[len(events)-1].Type != event.TaskCompleted {
		t.Fatalf("final event = %+v", events[len(events)-1])
	}
	if err := a.clean("task"); err != nil {
		t.Fatalf("clean: %v", err)
	}
	if _, err := os.Stat(got.WorkspacePath); !os.IsNotExist(err) {
		t.Fatalf("workspace remains: %v", err)
	}
}

func TestRunValidationFailure(t *testing.T) {
	a, repo := cliApp(t)
	_, err := a.run(runOptions{
		id: "task", goal: "goal", repo: repo, baseRef: "HEAD",
		command: "sh", args: []string{"-c", "test -n \"$1\"; exit 0", "pony-goal", "{goal}"},
		steps: []validation.Step{{Command: "false", Timeout: time.Second}},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	waitCLIState(t, a, "task", task.StateFailed)
}

func TestRunCrashSkipsValidation(t *testing.T) {
	a, repo := cliApp(t)
	_, err := a.run(runOptions{
		id: "task", goal: "goal", repo: repo, baseRef: "HEAD",
		command: "sh", args: []string{"-c", "test -n \"$1\"; exit 7", "pony-goal", "{goal}"},
		steps: []validation.Step{{Command: "true", Timeout: time.Second}},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	waitCLIState(t, a, "task", task.StateFailed)
	for _, recorded := range a.events.ListTask("task") {
		if recorded.Type == event.ValidationStarted {
			t.Fatal("validation ran after agent crash")
		}
	}
}

func TestRunAndRestartNeverWriteGoalToPTY(t *testing.T) {
	repo := cliRepository(t)
	workspaces, err := workspace.NewManager(filepath.Join(t.TempDir(), "workspaces"))
	if err != nil {
		t.Fatal(err)
	}
	d := &noGoalWriteDriver{Driver: driver.NewPTYDriver()}
	agents := agent.NewManager(d)
	events := event.NewMemoryStore()
	ctx, cancel := context.WithCancel(context.Background())
	a := &app{
		agents: agents, workspaces: workspaces, tasks: task.NewManager(workspaces, agents, events),
		events: events, ctx: ctx, cancel: cancel, runs: make(map[task.TaskID]*runState),
	}
	t.Cleanup(func() {
		_ = a.shutdown()
		_ = workspaces.Release(repo, "ws-no-write")
		cancel()
	})

	snapshot, err := a.run(runOptions{
		id: "no-write", goal: "do the work", repo: repo, baseRef: "HEAD",
		command: "sh", args: []string{"-c", "sleep 1000", "pony-goal", "{goal}"},
	})
	if err != nil || snapshot.SessionID == "" {
		t.Fatalf("run = %+v, %v", snapshot, err)
	}
	if _, err := a.stop("no-write"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.restart("no-write"); err != nil {
		t.Fatal(err)
	}
	if d.writes != 0 {
		t.Fatalf("goal written to PTY %d times", d.writes)
	}
	if _, err := a.stop("no-write"); err != nil {
		t.Fatal(err)
	}
	if err := a.clean("no-write"); err != nil {
		t.Fatal(err)
	}
}

func TestTaskAttachDetachKeepsSession(t *testing.T) {
	a, repo := cliApp(t)
	started, err := a.run(runOptions{
		id: "task", goal: "goal", repo: repo, baseRef: "HEAD",
		command: "sh", args: []string{"-c", "test \"$1\" = goal; while IFS= read -r line; do eval \"$line\"; done", "pony-goal", "{goal}"},
		steps: []validation.Step{{Command: "true", Timeout: time.Second}},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	p := openAttachPTY(t, 24, 80)
	p.start(t, a.agents, "task")
	p.write(t, []byte("echo FIRST_MARKER\n"))
	p.waitFor(t, "FIRST_MARKER", 3*time.Second)
	p.detach(t)

	p.start(t, a.agents, "task")
	p.write(t, []byte("echo SECOND_MARKER\n"))
	p.waitFor(t, "SECOND_MARKER", 3*time.Second)
	p.detach(t)

	session, ok := a.agents.Get("task")
	if !ok || session.State != agent.StateRunning || session.SessionID != started.SessionID || session.Generation != 1 {
		t.Fatalf("session after reattach = %+v, found = %t", session, ok)
	}
}

func TestRestartResendsTaskGoal(t *testing.T) {
	a, repo := cliApp(t)
	goalFile := filepath.Join(t.TempDir(), "received-goals.txt")
	goal := "fix the widget"
	command := fmt.Sprintf(`printf '%%s\n' "$1" >> '%s'; sleep 1000`, goalFile)
	if _, err := a.run(runOptions{
		id: "restart-goal", goal: goal, repo: repo, baseRef: "HEAD",
		command: "sh", args: []string{"-c", command, "pony-goal", "{goal}"},
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
	waitForGoalCount := func(want int) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			raw, err := os.ReadFile(goalFile)
			if err == nil && len(strings.Split(strings.TrimSpace(string(raw)), "\n")) == want {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		raw, _ := os.ReadFile(goalFile)
		t.Fatalf("received goals = %q, want %d copies of %q", raw, want, goal)
	}
	waitForGoalCount(1)
	if _, err := a.stop("restart-goal"); err != nil {
		t.Fatalf("stop before restart: %v", err)
	}
	if _, err := a.restart("restart-goal"); err != nil {
		t.Fatalf("restart: %v", err)
	}
	waitForGoalCount(2)
	if _, err := a.stop("restart-goal"); err != nil {
		t.Fatalf("stop after restart: %v", err)
	}
	if err := a.clean("restart-goal"); err != nil {
		t.Fatalf("clean: %v", err)
	}
}

func TestExplicitStopSkipsAutomaticValidation(t *testing.T) {
	a, repo := cliApp(t)
	_, err := a.run(runOptions{
		id: "task", goal: "goal", repo: repo, baseRef: "HEAD",
		command: "sh", args: []string{"-c", "test -n \"$1\"; sleep 1000", "pony-goal", "{goal}"},
		steps: []validation.Step{{Command: "true", Timeout: time.Second}},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, err := a.stop("task"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	time.Sleep(60 * time.Millisecond)
	got, _ := a.tasks.Get("task")
	if got.State != task.StateStopped {
		t.Fatalf("task state = %s", got.State)
	}
	for _, recorded := range a.events.ListTask("task") {
		if recorded.Type == event.ValidationStarted {
			t.Fatal("validation ran after explicit stop")
		}
	}
	validated, _, err := a.validate("task")
	if err != nil || validated.State != task.StateChecksPassed {
		t.Fatalf("manual validation = %+v, %v", validated, err)
	}
}

func TestCleanRefusesRunningAndDirtyTasks(t *testing.T) {
	a, repo := cliApp(t)
	_, err := a.run(runOptions{
		id: "task", goal: "goal", repo: repo, baseRef: "HEAD",
		command: "sh", args: []string{"-c", "test -n \"$1\"; sleep 1000", "pony-goal", "{goal}"},
		steps: []validation.Step{{Command: "true", Timeout: time.Second}},
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := a.clean("task"); err == nil {
		t.Fatal("clean accepted a running task")
	}
	if _, err := a.stop("task"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	snapshot, _ := a.tasks.Get("task")
	marker := filepath.Join(snapshot.WorkspacePath, "uncommitted.txt")
	if err := os.WriteFile(marker, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := a.clean("task"); err == nil {
		t.Fatal("clean removed dirty worktree")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("dirty file missing: %v", err)
	}
	if _, ok := a.tasks.Get("task"); !ok {
		t.Fatal("clean failure removed the task")
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if err := a.clean("task"); err != nil {
		t.Fatalf("clean: %v", err)
	}
}

func TestRunRejectsAgentIDCollision(t *testing.T) {
	a, repo := cliApp(t)
	if _, err := a.agents.Start(agent.AgentSpec{ID: "taken", Command: "sleep", Args: []string{"1000"}}); err != nil {
		t.Fatal(err)
	}
	_, err := a.run(runOptions{id: "taken", goal: "goal", repo: repo, baseRef: "HEAD", command: "sh", args: []string{"{goal}"}, steps: []validation.Step{{Command: "true", Timeout: time.Second}}})
	if err == nil {
		t.Fatal("run accepted duplicate agent ID")
	}
}

func TestCLIHelperProcess(t *testing.T) {
	if os.Getenv("PONY_CLI_TEST_HELPER") == "1" {
		os.Args = []string{"pony", "--cli"}
		main()
	} else if os.Getenv("PONY_TUI_TEST_HELPER") == "1" {
		os.Args = []string{"pony"}
		main()
	}
}

func TestCLICommandRunsTaskToVerification(t *testing.T) {
	repo := cliRepository(t)
	cmd := exec.Command(os.Args[0], "-test.run=^TestCLIHelperProcess$")
	cmd.Env = append(os.Environ(), "PONY_CLI_TEST_HELPER=1", "XDG_DATA_HOME="+t.TempDir())
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var output, errorsOut bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &errorsOut
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	line := "run --id task --goal goal --repo " + repo + " --command sh --arg -c --arg 'test -n \"$1\"; exit 0' --arg pony-goal --arg '{goal}' --validate 'test -f README.md'\n"
	if _, err := stdin.Write([]byte(line)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	if _, err := stdin.Write([]byte("list\nquit\n")); err != nil {
		t.Fatal(err)
	}
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("Pony: %v\nstdout: %s\nstderr: %s", err, output.String(), errorsOut.String())
	}
	if !strings.Contains(output.String(), "task process started in") || !strings.Contains(output.String(), "task             checks_passed") {
		t.Fatalf("CLI output does not show checks-passed task:\n%s", output.String())
	}
}

func TestCLIScreenCommandPrintsModeledPTYScreen(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestCLIHelperProcess$")
	cmd.Env = append(os.Environ(), "PONY_CLI_TEST_HELPER=1", "XDG_DATA_HOME="+t.TempDir())
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var output, errorsOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &errorsOut
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := stdin.Write([]byte("start screen sh\nsend screen \"printf 'SCREEN_TOKEN\\n'\"\n")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if _, err := stdin.Write([]byte("screen screen\nquit\n")); err != nil {
		t.Fatal(err)
	}
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("Pony: %v\nstdout: %s\nstderr: %s", err, output.String(), errorsOut.String())
	}
	if !strings.Contains(output.String(), "screen screen (24x80)") || !strings.Contains(output.String(), "SCREEN_TOKEN") {
		t.Fatalf("screen command did not print the visible PTY content:\n%s\nstderr: %s", output.String(), errorsOut.String())
	}
}

func TestCLIHandlesCRLFTerminatedQuit(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=^TestCLIHelperProcess$")
	cmd.Env = append(os.Environ(), "PONY_CLI_TEST_HELPER=1", "XDG_DATA_HOME="+t.TempDir())
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var output, errorsOut bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &errorsOut
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if _, err := stdin.Write([]byte("quit\r\n")); err != nil {
		t.Fatal(err)
	}
	if err := stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("Pony: %v\nstdout: %s\nstderr: %s", err, output.String(), errorsOut.String())
	}
	if strings.Contains(output.String(), "unknown command") {
		t.Fatalf("CRLF quit was not recognized:\n%s", output.String())
	}
}

func TestStopWaitsForAutomaticValidation(t *testing.T) {
	a, repo := cliApp(t)
	_, err := a.run(runOptions{
		id: "task", goal: "goal", repo: repo, baseRef: "HEAD",
		command: "sh", args: []string{"-c", "test -n \"$1\"; exit 0", "pony-goal", "{goal}"},
		steps: []validation.Step{{Command: "sleep", Args: []string{"0.2"}, Timeout: time.Second}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitCLIState(t, a, "task", task.StateValidating)
	if _, err := a.stop("task"); err == nil {
		t.Fatal("stop interrupted automatic validation")
	}
	waitCLIState(t, a, "task", task.StateChecksPassed)
}

func TestShutdownCancelsActiveValidation(t *testing.T) {
	a, repo := cliApp(t)
	started, err := a.run(runOptions{
		id: "task", goal: "goal", repo: repo, baseRef: "HEAD",
		command: "sh", args: []string{"-c", "test -n \"$1\"; exit 0", "pony-goal", "{goal}"},
		steps: []validation.Step{{Command: "sh", Args: []string{"-c", "sleep 1000 & echo $! > child.pid; wait"}, Timeout: time.Minute}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitCLIState(t, a, "task", task.StateValidating)
	pidFile := filepath.Join(started.WorkspacePath, "child.pid")
	deadline := time.Now().Add(3 * time.Second)
	var raw []byte
	for {
		raw, err = os.ReadFile(pidFile)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("validation child did not start: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	startedAt := time.Now()
	if err := a.shutdown(); err != nil {
		t.Fatalf("shutdown: %v", err)
	}
	if time.Since(startedAt) > 3*time.Second {
		t.Fatal("shutdown waited for the validation timeout")
	}
	if cliProcessAlive(pid) {
		t.Fatalf("validation child %d survived shutdown", pid)
	}
}

func cliProcessAlive(pid int) bool {
	if err := syscall.Kill(pid, 0); err != nil {
		return false
	}
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	end := strings.LastIndex(string(raw), ")")
	if end < 0 {
		return true
	}
	fields := strings.Fields(string(raw)[end+1:])
	return len(fields) > 0 && fields[0] != "Z"
}

func TestCLIShutdownOnSIGTERMStopsAgent(t *testing.T) {
	repo := cliRepository(t)
	dataRoot := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestCLIHelperProcess$")
	cmd.Env = append(os.Environ(), "PONY_CLI_TEST_HELPER=1", "XDG_DATA_HOME="+dataRoot)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	var output, errorsOut bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &errorsOut
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	line := "run --id task --goal goal --repo " + repo + " --command sh --arg -c --arg 'test -n \"$1\"; echo $$ > agent.pid; sleep 1000' --arg pony-goal --arg '{goal}' --validate true\n"
	if _, err := stdin.Write([]byte(line)); err != nil {
		t.Fatal(err)
	}
	pidFile := filepath.Join(dataRoot, "pony", "workspaces", "task", "agent.pid")
	deadline := time.Now().Add(5 * time.Second)
	var raw []byte
	for {
		raw, err = os.ReadFile(pidFile)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("agent did not start: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Pony shutdown: %v\nstdout: %s\nstderr: %s", err, output.String(), errorsOut.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Pony did not exit after SIGTERM")
	}
	if cliProcessAlive(pid) {
		t.Fatalf("agent process %d survived Pony shutdown", pid)
	}
}
