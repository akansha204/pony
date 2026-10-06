package task

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akansha204/pony/internal/event"
)

func TestEventStreamReconstructsTaskHistory(t *testing.T) {
	tasks, store, workspaces, agents, repo := eventManager(t)
	marker := filepath.Join(t.TempDir(), "first-generation")
	t.Cleanup(func() {
		_ = agents.Stop("task")
		_ = workspaces.Release(repo, "ws-task")
	})
	if _, err := tasks.Create(Spec{ID: "task", Goal: "goal", Repository: repo, BaseRef: "main"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	script := "if [ -f " + marker + " ]; then exit 7; fi; touch " + marker + "; exec sleep 1000"
	if _, err := tasks.Start("task", LaunchSpec{Command: "sh", Args: []string{"-c", script}}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	markerDeadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(markerDeadline) {
			t.Fatal("first generation did not create its marker")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := tasks.Restart("task"); err != nil {
		t.Fatalf("Restart: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		snapshot, err := tasks.Refresh("task")
		if err != nil {
			t.Fatalf("Refresh: %v", err)
		}
		if snapshot.State == StateFailed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("task state = %s", snapshot.State)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := tasks.Refresh("task"); err != nil {
		t.Fatalf("second Refresh: %v", err)
	}

	got := store.ListTask("task")
	want := []event.Type{
		event.TaskCreated,
		event.WorkspaceCreated,
		event.SessionStarted,
		event.RuntimeStarted,
		event.RuntimeExited,
		event.RuntimeStarted,
		event.RuntimeCrashed,
		event.TaskFailed,
	}
	if len(got) != len(want) {
		t.Fatalf("event count = %d, want %d: %+v", len(got), len(want), got)
	}
	for i, recorded := range got {
		if recorded.Sequence != uint64(i+1) || recorded.Type != want[i] {
			t.Fatalf("event %d = %+v, want type %s", i, recorded, want[i])
		}
	}
	if got[3].Generation != 1 || got[4].Generation != 1 {
		t.Fatalf("generation 1 history = %+v", got[3:5])
	}
	if got[5].Generation != 2 || got[6].Generation != 2 {
		t.Fatalf("generation 2 history = %+v", got[5:7])
	}
	if got[2].SessionID == "" || got[2].SessionID != got[7].SessionID {
		t.Fatalf("session identity changed: %+v", got)
	}
}
