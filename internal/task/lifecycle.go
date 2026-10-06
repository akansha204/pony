package task

import (
	"fmt"

	"github.com/akansha204/pony/internal/agent"
)

func (m *Manager) Stop(id TaskID) (Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	task, ok := m.tasks[id]
	if !ok {
		return Snapshot{}, fmt.Errorf("no task %q", id)
	}
	if task.SessionID == "" {
		return Snapshot{}, fmt.Errorf("task %q has not been started", id)
	}
	if err := m.agents.Stop(agent.AgentID(id)); err != nil {
		return Snapshot{}, fmt.Errorf("stop task %q: %w", id, err)
	}

	task.State = StateStopped
	m.tasks[id] = task
	return task, nil
}

func (m *Manager) Restart(id TaskID) (Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	task, ok := m.tasks[id]
	if !ok {
		return Snapshot{}, fmt.Errorf("no task %q", id)
	}
	if task.SessionID == "" || task.WorkspaceID == "" {
		return Snapshot{}, fmt.Errorf("task %q has not been started", id)
	}
	if err := m.agents.Restart(agent.AgentID(id)); err != nil {
		return Snapshot{}, fmt.Errorf("restart task %q: %w", id, err)
	}

	session, ok := m.agents.Get(agent.AgentID(id))
	if !ok {
		return Snapshot{}, fmt.Errorf("restart task %q: agent session is missing", id)
	}
	task.SessionID = session.SessionID
	task.State = StateRunning
	m.tasks[id] = task
	return task, nil
}

func (m *Manager) Refresh(id TaskID) (Snapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	task, ok := m.tasks[id]
	if !ok {
		return Snapshot{}, fmt.Errorf("no task %q", id)
	}
	if task.SessionID == "" {
		return task, nil
	}

	session, ok := m.agents.Get(agent.AgentID(id))
	if !ok {
		task.State = StateFailed
		m.tasks[id] = task
		return task, fmt.Errorf("refresh task %q: agent session is missing", id)
	}

	switch session.State {
	case agent.StateCrashed:
		task.State = StateFailed
	case agent.StateStopped, agent.StateIdle:
		task.State = StateStopped
	case agent.StateStarting, agent.StateRunning, agent.StateStopping:
		task.State = StateRunning
	default:
		return Snapshot{}, fmt.Errorf("refresh task %q: unexpected agent state %q", id, session.State)
	}
	m.tasks[id] = task
	return task, nil
}
