package workspace

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akansha204/pony/internal/agent"
	"github.com/akansha204/pony/internal/driver"
)

func TestAgentsRunSimultaneouslyInIsolatedWorktrees(t *testing.T) {
	repo := testRepository(t)
	workspaces, err := NewManager(filepath.Join(t.TempDir(), "workspaces"))
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	first, err := workspaces.Allocate(Spec{TaskID: "first-agent", Repository: repo, BaseRef: "main"})
	if err != nil {
		t.Fatalf("Allocate first: %v", err)
	}
	second, err := workspaces.Allocate(Spec{TaskID: "second-agent", Repository: repo, BaseRef: "main"})
	if err != nil {
		t.Fatalf("Allocate second: %v", err)
	}

	agents := agent.NewManager(driver.NewProcessDriver())
	firstSnap, err := agents.Start(agent.AgentSpec{
		ID:      "first-agent",
		Command: "sh",
		Args:    []string{"-c", "printf first > agent-output.txt; sleep 30"},
		Cwd:     first.Path,
	})
	if err != nil {
		t.Fatalf("Start first: %v", err)
	}
	secondSnap, err := agents.Start(agent.AgentSpec{
		ID:      "second-agent",
		Command: "sh",
		Args:    []string{"-c", "printf second > agent-output.txt; sleep 30"},
		Cwd:     second.Path,
	})
	if err != nil {
		_ = agents.Stop("first-agent")
		t.Fatalf("Start second: %v", err)
	}
	t.Cleanup(func() {
		_ = agents.Stop("first-agent")
		_ = agents.Stop("second-agent")
	})

	if firstSnap.PID == secondSnap.PID || firstSnap.State != agent.StateRunning || secondSnap.State != agent.StateRunning {
		t.Fatalf("agents are not independently running: first=%+v second=%+v", firstSnap, secondSnap)
	}
	waitForFileContent(t, filepath.Join(first.Path, "agent-output.txt"), "first")
	waitForFileContent(t, filepath.Join(second.Path, "agent-output.txt"), "second")
	if _, err := os.Stat(filepath.Join(repo, "agent-output.txt")); !os.IsNotExist(err) {
		t.Fatalf("agent wrote into the original checkout: %v", err)
	}

	if err := agents.Stop("first-agent"); err != nil {
		t.Fatalf("Stop first: %v", err)
	}
	if err := agents.Stop("second-agent"); err != nil {
		t.Fatalf("Stop second: %v", err)
	}
	if err := os.Remove(filepath.Join(first.Path, "agent-output.txt")); err != nil {
		t.Fatalf("clean first workspace: %v", err)
	}
	if err := os.Remove(filepath.Join(second.Path, "agent-output.txt")); err != nil {
		t.Fatalf("clean second workspace: %v", err)
	}
	if err := workspaces.Release(repo, first.ID); err != nil {
		t.Fatalf("Release first workspace: %v", err)
	}
	if _, err := os.Stat(second.Path); err != nil {
		t.Fatalf("second workspace was affected: %v", err)
	}
	if err := workspaces.Release(repo, second.ID); err != nil {
		t.Fatalf("Release second workspace: %v", err)
	}
}

func waitForFileContent(t *testing.T, path, want string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		if err == nil && string(data) == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	data, err := os.ReadFile(path)
	t.Fatalf("file %q = %q, %v; want %q", path, data, err, want)
}
