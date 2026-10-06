package event

import (
	"sync"
	"time"
)

type Recorder interface {
	Record(Event) Event
}

type MemoryStore struct {
	mu     sync.RWMutex
	next   uint64
	events []Event
	now    func() time.Time
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{now: time.Now}
}

func (s *MemoryStore) Record(event Event) Event {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.next++
	event.Sequence = s.next
	event.Time = s.now()
	s.events = append(s.events, event)
	return event
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
