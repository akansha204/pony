package event

import (
	"errors"
	"sync"
	"time"
)

var (
	ErrStaleGeneration  = errors.New("stale runtime generation")
	ErrRuntimeFinalized = errors.New("runtime generation already finalized")
)

type Recorder interface {
	Record(Event) (Event, error)
}

type MemoryStore struct {
	mu        sync.RWMutex
	next      uint64
	events    []Event
	latest    map[runtimeKey]uint64
	finalized map[runtimeGeneration]bool
	now       func() time.Time
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{
		latest:    make(map[runtimeKey]uint64),
		finalized: make(map[runtimeGeneration]bool),
		now:       time.Now,
	}
}

type runtimeKey struct {
	taskID    string
	sessionID string
}

type runtimeGeneration struct {
	runtimeKey
	generation uint64
}

func (s *MemoryStore) Record(event Event) (Event, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if event.Generation > 0 {
		key := runtimeKey{taskID: event.TaskID, sessionID: event.SessionID}
		if event.Generation < s.latest[key] {
			return Event{}, ErrStaleGeneration
		}
		generation := runtimeGeneration{runtimeKey: key, generation: event.Generation}
		if s.finalized[generation] && isRuntimeEvent(event.Type) {
			return Event{}, ErrRuntimeFinalized
		}
		if event.Generation > s.latest[key] {
			s.latest[key] = event.Generation
		}
		if isFinalRuntimeEvent(event.Type) {
			s.finalized[generation] = true
		}
	}

	s.next++
	event.Sequence = s.next
	event.Time = s.now()
	s.events = append(s.events, event)
	return event, nil
}

func (s *MemoryStore) List() []Event {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]Event(nil), s.events...)
}

func (s *MemoryStore) ListTask(taskID string) []Event {
	s.mu.RLock()
	defer s.mu.RUnlock()

	events := make([]Event, 0)
	for _, event := range s.events {
		if event.TaskID == taskID {
			events = append(events, event)
		}
	}
	return events
}

func isRuntimeEvent(eventType Type) bool {
	return eventType == RuntimeStarted || isFinalRuntimeEvent(eventType)
}

func isFinalRuntimeEvent(eventType Type) bool {
	return eventType == RuntimeExited || eventType == RuntimeCrashed
}
