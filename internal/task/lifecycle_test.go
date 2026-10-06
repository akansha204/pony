package task

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akansha204/pony/internal/agent"
	"github.com/akansha204/pony/internal/driver"
	"github.com/akansha204/pony/internal/workspace"
)

func TestTaskLifecycleStartStopRestart(t *testing.T) {
	repo := testRepository(t)
	workspaceRoot := filepath.Join(t.TempDir(), "workspaces")
	workspaces, err := workspace.NewManager(workspaceRoot)
	if err != nil {
		t.Fatalf("workspace.NewManager: %v", err)
	}
	agents := agent.NewManager(driver.NewProcessDriver())
	tasks := NewManager(workspaces, agents)
	if _, err := tasks.Create(Spec{ID: "task", Goal: "goal", Repository: repo, BaseRef: "main"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() {
		_ = agents.Stop("task")
		_ = workspaces.Release(repo, "ws-task")
	})

	started, err := tasks.Start("task", LaunchSpec{Command: "sleep", Args: []string{"1000"}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	firstSession, _ := agents.Get("task")
	if firstSession.Generation != 1 || started.State != StateRunning {
		t.Fatalf("initial state: task=%+v session=%+v", started, firstSession)
	}

	stopped, err := tasks.Stop("task")
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if stopped.State != StateStopped {
		t.Fatalf("state after Stop = %s", stopped.State)
	}
	if _, err := os.Stat(filepath.Join(workspaceRoot, "task")); err != nil {
		t.Fatalf("workspace after Stop: %v", err)
	}
	if again, err := tasks.Stop("task"); err != nil || again.State != StateStopped {
		t.Fatalf("second Stop = %+v, %v", again, err)
	}

	restarted, err := tasks.Restart("task")
	if err != nil {
		t.Fatalf("Restart: %v", err)
	}
	secondSession, _ := agents.Get("task")
	if restarted.State != StateRunning || restarted.WorkspaceID != started.WorkspaceID {
		t.Fatalf("task after Restart = %+v", restarted)
	}
	if secondSession.Generation != 2 || secondSession.PID == firstSession.PID {
		t.Fatalf("session after Restart = %+v, first = %+v", secondSession, firstSession)
	}
}

func TestRefreshTracksNaturalAgentExit(t *testing.T) {
	repo := testRepository(t)
	workspaces, err := workspace.NewManager(filepath.Join(t.TempDir(), "workspaces"))
	if err != nil {
		t.Fatalf("workspace.NewManager: %v", err)
	}
	agents := agent.NewManager(driver.NewProcessDriver())
	tasks := NewManager(workspaces, agents)
	if _, err := tasks.Create(Spec{ID: "task", Goal: "goal", Repository: repo, BaseRef: "main"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() {
		_ = agents.Stop("task")
		_ = workspaces.Release(repo, "ws-task")
	})
	if _, err := tasks.Start("task", LaunchSpec{Command: "sh", Args: []string{"-c", "exit 7"}}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		got, err := tasks.Refresh("task")
		if err != nil {
			t.Fatalf("Refresh: %v", err)
		}
		if got.State == StateFailed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("task state = %s, want %s", got.State, StateFailed)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestTaskLifecycleKeepsTasksIndependent(t *testing.T) {
	repo := testRepository(t)
	workspaces, err := workspace.NewManager(filepath.Join(t.TempDir(), "workspaces"))
	if err != nil {
		t.Fatalf("workspace.NewManager: %v", err)
	}
	agents := agent.NewManager(driver.NewProcessDriver())
	tasks := NewManager(workspaces, agents)
	for _, id := range []TaskID{"alpha", "beta"} {
		if _, err := tasks.Create(Spec{ID: id, Goal: string(id), Repository: repo, BaseRef: "main"}); err != nil {
			t.Fatalf("Create(%q): %v", id, err)
		}
		if _, err := tasks.Start(id, LaunchSpec{Command: "sleep", Args: []string{"1000"}}); err != nil {
			t.Fatalf("Start(%q): %v", id, err)
		}
	}
	t.Cleanup(func() {
		for _, id := range []string{"alpha", "beta"} {
			_ = agents.Stop(agent.AgentID(id))
			_ = workspaces.Release(repo, workspace.WorkspaceID("ws-"+id))
		}
	})

	if _, err := tasks.Stop("alpha"); err != nil {
		t.Fatalf("Stop(alpha): %v", err)
	}
	beta, err := tasks.Refresh("beta")
	if err != nil {
		t.Fatalf("Refresh(beta): %v", err)
	}
	if beta.State != StateRunning {
		t.Fatalf("beta state = %s", beta.State)
	}
	alpha, _ := tasks.Get("alpha")
	if alpha.State != StateStopped || alpha.WorkspaceID == beta.WorkspaceID {
		t.Fatalf("task snapshots: alpha=%+v beta=%+v", alpha, beta)
	}
}

func TestLifecycleRejectsUnstartedAndMissingTasks(t *testing.T) {
	repo := testRepository(t)
	tasks := newManager(t)
	if _, err := tasks.Create(Spec{ID: "pending", Goal: "goal", Repository: repo, BaseRef: "main"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := tasks.Stop("pending"); err == nil {
		t.Fatal("Stop accepted an unstarted task")
	}
	if _, err := tasks.Restart("pending"); err == nil {
		t.Fatal("Restart accepted an unstarted task")
	}
	if got, err := tasks.Refresh("pending"); err != nil || got.State != StatePending {
		t.Fatalf("Refresh(pending) = %+v, %v", got, err)
	}
	if _, err := tasks.Stop("missing"); err == nil {
		t.Fatal("Stop accepted a missing task")
	}
	if _, err := tasks.Restart("missing"); err == nil {
		t.Fatal("Restart accepted a missing task")
	}
	if _, err := tasks.Refresh("missing"); err == nil {
		t.Fatal("Refresh accepted a missing task")
	}
}
