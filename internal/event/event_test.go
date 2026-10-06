package event

import (
	"testing"
	"time"
)

func TestEventTypesAreStableAndDistinct(t *testing.T) {
	types := []Type{
		TaskCreated,
		WorkspaceCreated,
		SessionStarted,
		RuntimeStarted,
		RuntimeExited,
		RuntimeCrashed,
		ValidationStarted,
		ValidationPassed,
		ValidationFailed,
		TaskCompleted,
		TaskFailed,
	}
	seen := make(map[Type]bool, len(types))
	for _, eventType := range types {
		if eventType == "" {
			t.Fatal("event type must not be empty")
		}
		if seen[eventType] {
			t.Fatalf("duplicate event type %q", eventType)
		}
		seen[eventType] = true
	}
}

func TestEventIsAnIndependentValue(t *testing.T) {
	original := Event{
		Sequence:   7,
		Time:       time.Unix(10, 0),
		TaskID:     "task",
		SessionID:  "sess-1",
		Generation: 2,
		Type:       RuntimeStarted,
		Message:    "started",
	}
	copy := original
	copy.Message = "changed"

	if original.Message != "started" {
		t.Fatalf("original event changed: %+v", original)
	}
	if copy.Sequence != original.Sequence || copy.Type != original.Type {
		t.Fatalf("copied event lost identity: %+v", copy)
	}
}
