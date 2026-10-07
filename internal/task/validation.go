package task

import (
	"context"
	"fmt"

	"github.com/akansha204/pony/internal/agent"
	"github.com/akansha204/pony/internal/event"
	"github.com/akansha204/pony/internal/validation"
)

func (m *Manager) Validate(id TaskID, steps []validation.Step) (Snapshot, []validation.Result, error) {
	return m.ValidateContext(context.Background(), id, steps)
}

func (m *Manager) ValidateContext(ctx context.Context, id TaskID, steps []validation.Step) (Snapshot, []validation.Result, error) {
	if len(steps) == 0 {
		return Snapshot{}, nil, fmt.Errorf("task %q has no validation steps", id)
	}
	for i, step := range steps {
		if err := step.Validate(); err != nil {
			return Snapshot{}, nil, fmt.Errorf("validation step %d: %w", i+1, err)
		}
	}

	m.mu.Lock()
	task, ok := m.tasks[id]
	if !ok {
		m.mu.Unlock()
		return Snapshot{}, nil, fmt.Errorf("no task %q", id)
	}
	if task.State != StateStopped {
		m.mu.Unlock()
		return Snapshot{}, nil, fmt.Errorf("task %q is %s, want stopped", id, task.State)
	}
	if task.WorkspacePath == "" {
		m.mu.Unlock()
		return Snapshot{}, nil, fmt.Errorf("task %q has no workspace", id)
	}
	session, ok := m.agents.Get(agent.AgentID(id))
	if !ok {
		m.mu.Unlock()
		return Snapshot{}, nil, fmt.Errorf("validate task %q: agent session is missing", id)
	}
	task.State = StateValidating
	m.tasks[id] = task
	m.record(task, event.ValidationStarted, session.Generation, "validation started")
	m.mu.Unlock()

	results := make([]validation.Result, 0, len(steps))
	for _, step := range steps {
		result, err := m.validator.RunContext(ctx, task.WorkspacePath, step)
		results = append(results, result)
		if err != nil {
			failed := m.finishValidation(task, session.Generation, false)
			return failed, results, fmt.Errorf("validate task %q: %w", id, err)
		}
		if !result.Succeeded() {
			return m.finishValidation(task, session.Generation, false), results, nil
		}
	}

	return m.finishValidation(task, session.Generation, true), results, nil
}

func (m *Manager) finishValidation(task Snapshot, generation uint64, passed bool) Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()

	if passed {
		task.State = StateVerified
		m.tasks[task.ID] = task
		m.record(task, event.ValidationPassed, generation, "validation passed")
		m.record(task, event.TaskCompleted, generation, "task completed")
		return task
	}

	task.State = StateFailed
	m.tasks[task.ID] = task
	m.record(task, event.ValidationFailed, generation, "validation failed")
	m.record(task, event.TaskFailed, generation, "task failed")
	return task
}
