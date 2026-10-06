package task

import (
	"errors"
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

type LaunchSpec struct {
	Command string
	Args    []string
	Env     []string
}

type Manager struct {
	mu         sync.RWMutex
	tasks      map[TaskID]Snapshot
	workspaces *workspace.Manager
	agents     *agent.Manager
}

var validID = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

func NewManager(workspaces *workspace.Manager, agents *agent.Manager) *Manager {
	return &Manager{
		tasks:      make(map[TaskID]Snapshot),
		workspaces: workspaces,
		agents:     agents,
	}
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

func (m *Manager) Start(id TaskID, launch LaunchSpec) (Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	task, ok := m.tasks[id]
	if !ok {
		return Snapshot{}, fmt.Errorf("no task %q", id)
	}
	if task.State != StatePending {
		return Snapshot{}, fmt.Errorf("task %q is %s", id, task.State)
	}
	if m.workspaces == nil || m.agents == nil {
		return Snapshot{}, fmt.Errorf("task runtime is not configured")
	}

	ws, err := m.workspaces.Allocate(workspace.Spec{
		TaskID:     string(task.ID),
		Repository: task.Repository,
		BaseRef:    task.BaseRef,
	})
	if err != nil {
		return Snapshot{}, fmt.Errorf("start task %q: %w", id, err)
	}

	session, err := m.agents.Start(agent.AgentSpec{
		ID:      agent.AgentID(task.ID),
		Command: launch.Command,
		Args:    launch.Args,
		Cwd:     ws.Path,
		Env:     launch.Env,
	})
	if err != nil {
		releaseErr := m.workspaces.Release(task.Repository, ws.ID)
		if releaseErr != nil {
			return Snapshot{}, errors.Join(
				fmt.Errorf("start task %q: %w", id, err),
				fmt.Errorf("roll back workspace %q: %w", ws.ID, releaseErr),
			)
		}
		return Snapshot{}, fmt.Errorf("start task %q: %w", id, err)
	}

	task.WorkspaceID = ws.ID
	task.SessionID = session.SessionID
	task.State = StateRunning
	m.tasks[id] = task
	return task, nil
}
