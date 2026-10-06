package event

import (
	"errors"
	"sync"
	"testing"
)

func TestMemoryStoreAssignsOrderedIdentity(t *testing.T) {
	store := NewMemoryStore()
	first, err := store.Record(Event{Sequence: 99, TaskID: "alpha", Type: TaskCreated})
	if err != nil {
		t.Fatalf("Record(first): %v", err)
	}
	second, err := store.Record(Event{TaskID: "beta", Type: TaskCreated})
	if err != nil {
		t.Fatalf("Record(second): %v", err)
	}

	if first.Sequence != 1 || second.Sequence != 2 {
		t.Fatalf("sequences = %d, %d", first.Sequence, second.Sequence)
	}
	if first.Time.IsZero() || second.Time.IsZero() {
		t.Fatal("store did not assign timestamps")
	}
	listed := store.List()
	if len(listed) != 2 || listed[0] != first || listed[1] != second {
		t.Fatalf("List = %+v", listed)
	}
}

func TestMemoryStoreRejectsSecondFinalEvent(t *testing.T) {
	store := NewMemoryStore()
	base := Event{TaskID: "task", SessionID: "sess-1", Generation: 1}
	if _, err := store.Record(withType(base, RuntimeStarted)); err != nil {
		t.Fatalf("Record(started): %v", err)
	}
	if _, err := store.Record(withType(base, RuntimeExited)); err != nil {
		t.Fatalf("Record(exited): %v", err)
	}
	if _, err := store.Record(withType(base, RuntimeCrashed)); !errors.Is(err, ErrRuntimeFinalized) {
		t.Fatalf("Record(second final) error = %v", err)
	}
	if len(store.List()) != 2 {
		t.Fatalf("events = %+v", store.List())
	}
}

func TestMemoryStoreRejectsOldGeneration(t *testing.T) {
	store := NewMemoryStore()
	base := Event{TaskID: "task", SessionID: "sess-1"}
	current := base
	current.Generation = 2
	current.Type = RuntimeStarted
	if _, err := store.Record(current); err != nil {
		t.Fatalf("Record(current): %v", err)
	}
	stale := base
	stale.Generation = 1
	stale.Type = RuntimeExited
	if _, err := store.Record(stale); !errors.Is(err, ErrStaleGeneration) {
		t.Fatalf("Record(stale) error = %v", err)
	}
	if len(store.List()) != 1 {
		t.Fatalf("events = %+v", store.List())
	}
}

func withType(event Event, eventType Type) Event {
	event.Type = eventType
	return event
}

func TestMemoryStoreReturnsIndependentSlices(t *testing.T) {
	store := NewMemoryStore()
	store.Record(Event{TaskID: "task", Type: TaskCreated, Message: "original"})

	listed := store.List()
	listed[0].Message = "changed"
	again := store.List()
	if again[0].Message != "original" {
		t.Fatalf("stored event changed: %+v", again[0])
	}
}

func TestMemoryStoreFiltersByTask(t *testing.T) {
	store := NewMemoryStore()
	store.Record(Event{TaskID: "alpha", Type: TaskCreated})
	store.Record(Event{TaskID: "beta", Type: TaskCreated})
	store.Record(Event{TaskID: "alpha", Type: WorkspaceCreated})

	got := store.ListTask("alpha")
	if len(got) != 2 || got[0].Type != TaskCreated || got[1].Type != WorkspaceCreated {
		t.Fatalf("ListTask = %+v", got)
	}
	if got := store.ListTask("missing"); len(got) != 0 {
		t.Fatalf("ListTask(missing) = %+v", got)
	}
}

func TestMemoryStoreOrdersConcurrentRecords(t *testing.T) {
	store := NewMemoryStore()
	const count = 100

	var wg sync.WaitGroup
	for range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			store.Record(Event{TaskID: "task", Type: RuntimeStarted})
		}()
	}
	wg.Wait()

	got := store.List()
	if len(got) != count {
		t.Fatalf("event count = %d", len(got))
	}
	for i, event := range got {
		if want := uint64(i + 1); event.Sequence != want {
			t.Fatalf("event %d sequence = %d, want %d", i, event.Sequence, want)
		}
	}
}
