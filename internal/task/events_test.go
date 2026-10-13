package task

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/akansha204/pony/internal/agent"
	"github.com/akansha204/pony/internal/driver"
	"github.com/akansha204/pony/internal/event"
	"github.com/akansha204/pony/internal/workspace"
)

type failingRecorder struct{ err error }

func (r failingRecorder) Record(event.Event) (event.Event, error) {
	return event.Event{}, r.err
}

func eventManager(t *testing.T) (*Manager, *event.MemoryStore, *workspace.Manager, *agent.Manager, string) {
	t.Helper()
	repo := testRepository(t)
	workspaces, err := workspace.NewManager(filepath.Join(t.TempDir(), "workspaces"))
	if err != nil {
		t.Fatalf("workspace.NewManager: %v", err)
	}
	agents := agent.NewManager(driver.NewProcessDriver())
	store := event.NewMemoryStore()
	return NewManager(workspaces, agents, store), store, workspaces, agents, repo
}

func TestTaskStartRecordsLifecycleEvents(t *testing.T) {
	tasks, store, workspaces, agents, repo := eventManager(t)
	t.Cleanup(func() {
		_ = agents.Stop("task")
		_ = workspaces.Release(repo, "ws-task")
	})
	if _, err := tasks.Create(Spec{ID: "task", Goal: "goal", Repository: repo, BaseRef: "main"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	started, err := tasks.Start("task", LaunchSpec{Command: "sleep", Args: []string{"1000"}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	got := store.ListTask("task")
	want := []event.Type{event.TaskCreated, event.WorkspaceCreated, event.SessionStarted, event.RuntimeStarted}
	if len(got) != len(want) {
		t.Fatalf("events = %+v", got)
	}
	for i, event := range got {
		if event.Type != want[i] || event.Sequence != uint64(i+1) {
			t.Fatalf("event %d = %+v, want type %s", i, event, want[i])
		}
	}
	if got[2].SessionID != string(started.SessionID) || got[2].Generation != 1 {
		t.Fatalf("session event = %+v", got[2])
	}
}

func TestCreateReturnsEventRecordFailure(t *testing.T) {
	tasks, _, _, _, repo := eventManager(t)
	wantErr := errors.New("recorder unavailable")
	tasks.events = failingRecorder{err: wantErr}
	if _, err := tasks.Create(Spec{ID: "task", Goal: "goal", Repository: repo, BaseRef: "main"}); !errors.Is(err, wantErr) {
		t.Fatalf("Create error = %v, want %v", err, wantErr)
	}
	if _, ok := tasks.Get("task"); ok {
		t.Fatal("task was published even though its creation event was not recorded")
	}
}

func TestStopReturnsEventRecordFailure(t *testing.T) {
	tasks, _, workspaces, agents, repo := eventManager(t)
	t.Cleanup(func() {
		_ = agents.Stop("task")
		_ = workspaces.Release(repo, "ws-task")
	})
	if _, err := tasks.Create(Spec{ID: "task", Goal: "goal", Repository: repo, BaseRef: "main"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := tasks.Start("task", LaunchSpec{Command: "sleep", Args: []string{"1000"}}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	wantErr := errors.New("recorder unavailable")
	tasks.events = failingRecorder{err: wantErr}
	got, err := tasks.Stop("task")
	if !errors.Is(err, wantErr) {
		t.Fatalf("Stop error = %v, want %v", err, wantErr)
	}
	if got.State != StateStopped {
		t.Fatalf("task state = %s, want stopped", got.State)
	}
}

func TestTaskStopAndRestartRecordRuntimeEvents(t *testing.T) {
	tasks, store, workspaces, agents, repo := eventManager(t)
	t.Cleanup(func() {
		_ = agents.Stop("task")
		_ = workspaces.Release(repo, "ws-task")
	})
	if _, err := tasks.Create(Spec{ID: "task", Goal: "goal", Repository: repo, BaseRef: "main"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := tasks.Start("task", LaunchSpec{Command: "sleep", Args: []string{"1000"}}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := tasks.Restart("task"); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if _, err := tasks.Stop("task"); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	got := store.ListTask("task")
	if got[4].Type != event.RuntimeExited || got[4].Generation != 1 {
		t.Fatalf("restart exit event = %+v", got[4])
	}
	if len(got) != 7 {
		t.Fatalf("events = %+v", got)
	}
	if got[5].Type != event.RuntimeStarted || got[5].Generation != 2 {
		t.Fatalf("restart start event = %+v", got[5])
	}
	if got[6].Type != event.RuntimeExited || got[6].Generation != 2 {
		t.Fatalf("stop event = %+v", got[6])
	}
}

func TestRefreshRecordsCrashAndTaskFailure(t *testing.T) {
	tasks, store, workspaces, agents, repo := eventManager(t)
	t.Cleanup(func() {
		_ = agents.Stop("task")
		_ = workspaces.Release(repo, "ws-task")
	})
	if _, err := tasks.Create(Spec{ID: "task", Goal: "goal", Repository: repo, BaseRef: "main"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
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
			t.Fatalf("task state = %s", got.State)
		}
		time.Sleep(10 * time.Millisecond)
	}

	got := store.ListTask("task")
	last := got[len(got)-2:]
	if last[0].Type != event.RuntimeCrashed || last[1].Type != event.TaskFailed {
		t.Fatalf("final events = %+v", last)
	}
}

func TestStopAfterCrashRecordsCrash(t *testing.T) {
	tasks, store, workspaces, agents, repo := eventManager(t)
	t.Cleanup(func() {
		_ = agents.Stop("task")
		_ = workspaces.Release(repo, "ws-task")
	})
	if _, err := tasks.Create(Spec{ID: "task", Goal: "goal", Repository: repo, BaseRef: "main"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := tasks.Start("task", LaunchSpec{Command: "sh", Args: []string{"-c", "exit 7"}}); err != nil {
		t.Fatalf("Start: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		session, ok := agents.Get("task")
		if ok && session.State == agent.StateCrashed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("agent state = %+v, want crashed", session)
		}
		time.Sleep(10 * time.Millisecond)
	}

	got, err := tasks.Stop("task")
	if err == nil {
		t.Fatal("Stop reported success after the agent had crashed")
	}
	if got.State != StateFailed {
		t.Fatalf("task state = %s, want failed", got.State)
	}
	events := store.ListTask("task")
	last := events[len(events)-2:]
	if last[0].Type != event.RuntimeCrashed || last[1].Type != event.TaskFailed {
		t.Fatalf("final events = %+v, want crash and task failure", last)
	}
}

func TestStoppingTaskTwiceRecordsOneRuntimeExit(t *testing.T) {
	tasks, store, workspaces, agents, repo := eventManager(t)
	t.Cleanup(func() {
		_ = agents.Stop("task")
		_ = workspaces.Release(repo, "ws-task")
	})
	if _, err := tasks.Create(Spec{ID: "task", Goal: "goal", Repository: repo, BaseRef: "main"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := tasks.Start("task", LaunchSpec{Command: "sleep", Args: []string{"1000"}}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if _, err := tasks.Stop("task"); err != nil {
		t.Fatalf("first Stop: %v", err)
	}
	if _, err := tasks.Stop("task"); err != nil {
		t.Fatalf("second Stop: %v", err)
	}

	exits := 0
	for _, recorded := range store.ListTask("task") {
		if recorded.Type == event.RuntimeExited || recorded.Type == event.RuntimeCrashed {
			exits++
		}
	}
	if exits != 1 {
		t.Fatalf("runtime final events = %d, want 1: %+v", exits, store.ListTask("task"))
	}
}

func TestFailedStartDoesNotRecordCreatedResources(t *testing.T) {
	tasks, store, _, _, repo := eventManager(t)
	if _, err := tasks.Create(Spec{ID: "task", Goal: "goal", Repository: repo, BaseRef: "main"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := tasks.Start("task", LaunchSpec{Command: "pony-command-that-does-not-exist"}); err == nil {
		t.Fatal("Start succeeded")
	}

	got := store.ListTask("task")
	if len(got) != 1 || got[0].Type != event.TaskCreated {
		t.Fatalf("events after failed Start = %+v", got)
	}
}
