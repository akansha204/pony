package task

import (
	"fmt"

	"github.com/akansha204/pony/internal/agent"
	"github.com/akansha204/pony/internal/event"
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
	if task.State == StateValidating || task.State == StateVerified || task.State == StateFailed {
		return Snapshot{}, fmt.Errorf("task %q is %s", id, task.State)
	}
	if err := m.agents.Stop(agent.AgentID(id)); err != nil {
		return Snapshot{}, fmt.Errorf("stop task %q: %w", id, err)
	}
	session, ok := m.agents.Get(agent.AgentID(id))
	if !ok {
		return Snapshot{}, fmt.Errorf("stop task %q: agent session is missing", id)
	}

	task.State = StateStopped
	m.tasks[id] = task
	m.record(task, event.RuntimeExited, session.Generation, "runtime stopped")
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
	if task.State == StateValidating {
		return Snapshot{}, fmt.Errorf("task %q is validating", id)
	}
	previous, ok := m.agents.Get(agent.AgentID(id))
	if !ok {
		return Snapshot{}, fmt.Errorf("restart task %q: agent session is missing", id)
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
	if previous.State == agent.StateRunning || previous.State == agent.StateStopping {
		m.record(task, event.RuntimeExited, previous.Generation, "runtime restarted")
	}
	m.record(task, event.RuntimeStarted, session.Generation, "runtime started")
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
	if task.State == StateValidating || task.State == StateVerified || task.State == StateFailed {
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
		changed := task.State != StateFailed
		task.State = StateFailed
		if changed {
			m.record(task, event.RuntimeCrashed, session.Generation, "runtime crashed")
			m.record(task, event.TaskFailed, session.Generation, "task failed")
		}
	case agent.StateStopped, agent.StateIdle:
		changed := task.State != StateStopped
		task.State = StateStopped
		if changed {
			m.record(task, event.RuntimeExited, session.Generation, "runtime exited")
		}
	case agent.StateStarting, agent.StateRunning, agent.StateStopping:
		task.State = StateRunning
	default:
		return Snapshot{}, fmt.Errorf("refresh task %q: unexpected agent state %q", id, session.State)
	}
	m.tasks[id] = task
	return task, nil
}
