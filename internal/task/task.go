package task

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/akansha204/pony/internal/agent"
	"github.com/akansha204/pony/internal/workspace"
)

type TaskID string

type State string

const (
	StatePending    State = "pending"
	StateRunning    State = "running"
	StateValidating State = "validating"
	StateVerified   State = "verified"
	StateFailed     State = "failed"
	StateStopped    State = "stopped"
)

type Spec struct {
	ID         TaskID
	Goal       string
	Repository string
	BaseRef    string
}

type Snapshot struct {
	ID          TaskID
	Goal        string
	Repository  string
	BaseRef     string
	WorkspaceID workspace.WorkspaceID
	SessionID   agent.SessionID
	State       State
}

type Manager struct {
	mu    sync.RWMutex
	tasks map[TaskID]Snapshot
}

var validID = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

func NewManager() *Manager {
	return &Manager{tasks: make(map[TaskID]Snapshot)}
}

func (m *Manager) Create(spec Spec) (Snapshot, error) {
	if !validID.MatchString(string(spec.ID)) || spec.ID == "." || spec.ID == ".." {
		return Snapshot{}, fmt.Errorf("invalid task id %q", spec.ID)
	}
	if strings.TrimSpace(spec.Goal) == "" {
		return Snapshot{}, fmt.Errorf("goal must not be empty")
	}

	repo, err := workspace.ResolveRepository(spec.Repository, spec.BaseRef)
	if err != nil {
		return Snapshot{}, err
	}

	snapshot := Snapshot{
		ID:         spec.ID,
		Goal:       spec.Goal,
		Repository: repo,
		BaseRef:    spec.BaseRef,
		State:      StatePending,
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.tasks[spec.ID]; exists {
		return Snapshot{}, fmt.Errorf("task %q already exists", spec.ID)
	}
	m.tasks[spec.ID] = snapshot
	return snapshot, nil
}

func (m *Manager) Get(id TaskID) (Snapshot, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	snapshot, ok := m.tasks[id]
	return snapshot, ok
}

func (m *Manager) List() []Snapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()

	tasks := make([]Snapshot, 0, len(m.tasks))
	for _, snapshot := range m.tasks {
		tasks = append(tasks, snapshot)
	}
	sort.Slice(tasks, func(i, j int) bool {
		return tasks[i].ID < tasks[j].ID
	})
	return tasks
}
