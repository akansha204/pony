package task

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/akansha204/pony/internal/event"
	"github.com/akansha204/pony/internal/validation"
)

func stoppedTask(t *testing.T) (*Manager, *event.MemoryStore, string, func()) {
	t.Helper()
	tasks, store, workspaces, agents, repo := eventManager(t)
	if _, err := tasks.Create(Spec{ID: "task", Goal: "goal", Repository: repo, BaseRef: "main"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := tasks.Start("task", LaunchSpec{Command: "sh", Args: []string{"-c", "exit 0"}}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitTaskState(t, tasks, "task", StateStopped)
	cleanup := func() {
		_ = agents.Stop("task")
		_ = workspaces.Release(repo, "ws-task")
	}
	return tasks, store, repo, cleanup
}

func waitTaskState(t *testing.T, tasks *Manager, id TaskID, want State) Snapshot {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		snapshot, err := tasks.Refresh(id)
		if err != nil {
			t.Fatalf("Refresh: %v", err)
		}
		if snapshot.State == want {
			return snapshot
		}
		if time.Now().After(deadline) {
			t.Fatalf("task state = %s, want %s", snapshot.State, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestValidateMarksTaskVerifiedWhenEveryStepPasses(t *testing.T) {
	tasks, store, _, cleanup := stoppedTask(t)
	t.Cleanup(cleanup)
	before, _ := tasks.Get("task")

	got, results, err := tasks.Validate("task", []validation.Step{
		{Command: "sh", Args: []string{"-c", "test -f README.md; printf checked"}, Timeout: time.Second},
		{Command: "pwd", Timeout: time.Second},
	})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got.State != StateVerified || len(results) != 2 {
		t.Fatalf("validation = task %+v, results %+v", got, results)
	}
	if results[0].Stdout != "checked" || results[1].Stdout != before.WorkspacePath+"\n" {
		t.Fatalf("results = %+v", results)
	}
	for _, result := range results {
		if !result.Succeeded() {
			t.Fatalf("failed result = %+v", result)
		}
	}

	events := store.ListTask("task")
	last := events[len(events)-3:]
	if last[0].Type != event.ValidationStarted || last[1].Type != event.ValidationPassed || last[2].Type != event.TaskCompleted {
		t.Fatalf("final events = %+v", last)
	}
	if refreshed, err := tasks.Refresh("task"); err != nil || refreshed.State != StateVerified {
		t.Fatalf("Refresh after verification = %+v, %v", refreshed, err)
	}
}

func TestValidateStopsAfterFirstFailedStep(t *testing.T) {
	tasks, store, _, cleanup := stoppedTask(t)
	t.Cleanup(cleanup)
	task, _ := tasks.Get("task")
	marker := filepath.Join(task.WorkspacePath, "should-not-exist")

	got, results, err := tasks.Validate("task", []validation.Step{
		{Command: "sh", Args: []string{"-c", "printf failure >&2; exit 7"}, Timeout: time.Second},
		{Command: "touch", Args: []string{marker}, Timeout: time.Second},
	})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got.State != StateFailed || len(results) != 1 || results[0].ExitCode != 7 {
		t.Fatalf("validation = task %+v, results %+v", got, results)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("later validation step ran: %v", err)
	}

	events := store.ListTask("task")
	last := events[len(events)-3:]
	if last[0].Type != event.ValidationStarted || last[1].Type != event.ValidationFailed || last[2].Type != event.TaskFailed {
		t.Fatalf("final events = %+v", last)
	}
	if refreshed, err := tasks.Refresh("task"); err != nil || refreshed.State != StateFailed {
		t.Fatalf("Refresh after failure = %+v, %v", refreshed, err)
	}
}

func TestValidateMarksTimeoutAsFailure(t *testing.T) {
	tasks, _, _, cleanup := stoppedTask(t)
	t.Cleanup(cleanup)

	got, results, err := tasks.Validate("task", []validation.Step{
		{Command: "sleep", Args: []string{"1000"}, Timeout: 100 * time.Millisecond},
	})
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got.State != StateFailed || len(results) != 1 || !results[0].TimedOut {
		t.Fatalf("validation = task %+v, results %+v", got, results)
	}
}

func TestValidateRejectsWrongStateAndInvalidSteps(t *testing.T) {
	repo := testRepository(t)
	tasks := newManager(t)
	if _, err := tasks.Create(Spec{ID: "pending", Goal: "goal", Repository: repo, BaseRef: "main"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	valid := []validation.Step{{Command: "true", Timeout: time.Second}}
	if _, _, err := tasks.Validate("missing", valid); err == nil {
		t.Fatal("Validate accepted a missing task")
	}
	if _, _, err := tasks.Validate("pending", valid); err == nil {
		t.Fatal("Validate accepted a pending task")
	}
	if _, _, err := tasks.Validate("pending", nil); err == nil {
		t.Fatal("Validate accepted no steps")
	}
	if _, _, err := tasks.Validate("pending", []validation.Step{{Command: "true"}}); err == nil {
		t.Fatal("Validate accepted an invalid step")
	}
}

func TestLifecycleCannotInterruptValidation(t *testing.T) {
	tasks, _, _, cleanup := stoppedTask(t)
	t.Cleanup(cleanup)
	done := make(chan error, 1)
	go func() {
		_, _, err := tasks.Validate("task", []validation.Step{
			{Command: "sleep", Args: []string{"0.2"}, Timeout: time.Second},
		})
		done <- err
	}()

	deadline := time.Now().Add(time.Second)
	for {
		got, _ := tasks.Get("task")
		if got.State == StateValidating {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("task did not enter validating state")
		}
		time.Sleep(time.Millisecond)
	}
	if _, err := tasks.Stop("task"); err == nil {
		t.Fatal("Stop interrupted validation")
	}
	if _, err := tasks.Restart("task"); err == nil {
		t.Fatal("Restart interrupted validation")
	}
	if err := <-done; err != nil {
		t.Fatalf("Validate: %v", err)
	}
}
