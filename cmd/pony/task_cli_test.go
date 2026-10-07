package main

import (
	"bytes"
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
	"github.com/akansha204/pony/internal/event"
	"github.com/akansha204/pony/internal/task"
	"github.com/akansha204/pony/internal/validation"
)

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
		{"--id", "task", "--goal", "goal", "--repo", "/repo", "--command", "sh"},
		{"--unknown", "value"},
		{"--validate"},
	} {
		if _, err := parseRun(fields); err == nil {
			t.Fatalf("parseRun accepted %v", fields)
		}
	}
}

func TestRunCompletesAndValidatesInWorkspace(t *testing.T) {
	a, repo := cliApp(t)
	opts := runOptions{
		id: "task", goal: "do the work", repo: repo, baseRef: "HEAD",
		command: "sh", args: []string{"-c", "read -r goal; test \"$goal\" = 'do the work'"},
		steps: []validation.Step{{Command: "sh", Args: []string{"-c", "test -f README.md && pwd"}, Timeout: time.Second}},
	}
	started, err := a.run(opts)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	got := waitCLIState(t, a, "task", task.StateVerified)
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
		command: "sh", args: []string{"-c", "read -r goal; exit 0"},
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
		command: "sh", args: []string{"-c", "read -r goal; exit 7"},
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

func TestTaskAttachDetachKeepsSession(t *testing.T) {
	a, repo := cliApp(t)
	started, err := a.run(runOptions{
		id: "task", goal: "goal", repo: repo, baseRef: "HEAD",
		command: "sh", args: []string{"-c", "read -r goal; while IFS= read -r line; do eval \"$line\"; done"},
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

func TestExplicitStopSkipsAutomaticValidation(t *testing.T) {
	a, repo := cliApp(t)
	_, err := a.run(runOptions{
		id: "task", goal: "goal", repo: repo, baseRef: "HEAD",
		command: "sh", args: []string{"-c", "read -r goal; sleep 1000"},
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
	if err != nil || validated.State != task.StateVerified {
		t.Fatalf("manual validation = %+v, %v", validated, err)
	}
}

func TestCleanRefusesRunningAndDirtyTasks(t *testing.T) {
	a, repo := cliApp(t)
	_, err := a.run(runOptions{
		id: "task", goal: "goal", repo: repo, baseRef: "HEAD",
		command: "sh", args: []string{"-c", "read -r goal; sleep 1000"},
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
	_, err := a.run(runOptions{id: "taken", goal: "goal", repo: repo, baseRef: "HEAD", command: "sh", steps: []validation.Step{{Command: "true", Timeout: time.Second}}})
	if err == nil {
		t.Fatal("run accepted duplicate agent ID")
	}
}

func TestCLIHelperProcess(t *testing.T) {
	if os.Getenv("PONY_CLI_TEST_HELPER") == "1" {
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
	line := "run --id task --goal goal --repo " + repo + " --command sh --arg -c --arg 'read -r goal; exit 0' --validate 'test -f README.md'\n"
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
	if !strings.Contains(output.String(), "task running in") || !strings.Contains(output.String(), "task             verified") {
		t.Fatalf("CLI output does not show verified task:\n%s", output.String())
	}
}

func TestStopWaitsForAutomaticValidation(t *testing.T) {
	a, repo := cliApp(t)
	_, err := a.run(runOptions{
		id: "task", goal: "goal", repo: repo, baseRef: "HEAD",
		command: "sh", args: []string{"-c", "read -r goal; exit 0"},
		steps: []validation.Step{{Command: "sleep", Args: []string{"0.2"}, Timeout: time.Second}},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitCLIState(t, a, "task", task.StateValidating)
	if _, err := a.stop("task"); err == nil {
		t.Fatal("stop interrupted automatic validation")
	}
	waitCLIState(t, a, "task", task.StateVerified)
}

func TestShutdownCancelsActiveValidation(t *testing.T) {
	a, repo := cliApp(t)
	started, err := a.run(runOptions{
		id: "task", goal: "goal", repo: repo, baseRef: "HEAD",
		command: "sh", args: []string{"-c", "read -r goal; exit 0"},
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
	line := "run --id task --goal goal --repo " + repo + " --command sh --arg -c --arg 'read -r goal; echo $$ > agent.pid; sleep 1000' --validate true\n"
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
