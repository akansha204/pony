package task

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/akansha204/pony/internal/agent"
	"github.com/akansha204/pony/internal/event"
	"github.com/akansha204/pony/internal/validation"
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
	StateCompleted  State = "completed"
)

type Spec struct {
	ID         TaskID
	Goal       string
	Repository string
	BaseRef    string
}

type Snapshot struct {
	ID            TaskID
	Goal          string
	Repository    string
	BaseRef       string
	WorkspaceID   workspace.WorkspaceID
	WorkspacePath string
	SessionID     agent.SessionID
	State         State
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
	events     event.Recorder
	validator  *validation.Runner
}

var validID = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*$`)

func NewManager(workspaces *workspace.Manager, agents *agent.Manager, recorder ...event.Recorder) *Manager {
	m := &Manager{
		tasks:      make(map[TaskID]Snapshot),
		workspaces: workspaces,
		agents:     agents,
		validator:  validation.NewRunner(),
	}
	if len(recorder) > 0 {
		m.events = recorder[0]
	}
	return m
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
	if err := m.record(snapshot, event.TaskCreated, 0, "task created"); err != nil {
		return Snapshot{}, err
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

func (m *Manager) Clean(id TaskID) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	task, ok := m.tasks[id]
	if !ok {
		return fmt.Errorf("no task %q", id)
	}
	if task.State == StateRunning || task.State == StateValidating {
		return fmt.Errorf("task %q is %s", id, task.State)
	}
	if task.WorkspaceID != "" {
		if err := m.workspaces.Release(task.Repository, task.WorkspaceID); err != nil {
			return fmt.Errorf("clean task %q: %w", id, err)
		}
	}
	delete(m.tasks, id)
	return nil
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
		rollbackErr := m.workspaces.RollbackUnusedAllocation(ws)
		if rollbackErr != nil {
			return Snapshot{}, errors.Join(
				fmt.Errorf("start task %q: %w", id, err),
				fmt.Errorf("roll back workspace %q: %w", ws.ID, rollbackErr),
			)
		}
		return Snapshot{}, fmt.Errorf("start task %q: %w", id, err)
	}

	task.WorkspaceID = ws.ID
	task.WorkspacePath = ws.Path
	task.SessionID = session.SessionID
	task.State = StateRunning
	m.tasks[id] = task
	var recordErr error
	recordErr = errors.Join(recordErr, m.record(task, event.WorkspaceCreated, 0, "workspace created"))
	recordErr = errors.Join(recordErr, m.record(task, event.SessionStarted, session.Generation, "session started"))
	recordErr = errors.Join(recordErr, m.record(task, event.RuntimeStarted, session.Generation, "runtime started"))
	if recordErr != nil {
		return task, fmt.Errorf("task %q started but event recording failed: %w", id, recordErr)
	}
	return task, nil
}

func (m *Manager) record(task Snapshot, eventType event.Type, generation uint64, message string) error {
	if m.events == nil {
		return nil
	}
	_, err := m.events.Record(event.Event{
		TaskID:     string(task.ID),
		SessionID:  string(task.SessionID),
		Generation: generation,
		Type:       eventType,
		Message:    message,
	})
	if err != nil {
		return fmt.Errorf("record %s for task %q generation %d: %w", eventType, task.ID, generation, err)
	}
	return nil
}
