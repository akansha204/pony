package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/akansha204/pony/internal/agent"
	"github.com/akansha204/pony/internal/driver"
	"github.com/akansha204/pony/internal/event"
	"github.com/akansha204/pony/internal/shell_lexer"
	"github.com/akansha204/pony/internal/task"
	"github.com/akansha204/pony/internal/validation"
	"github.com/akansha204/pony/internal/workspace"
)

type runOptions struct {
	id      task.TaskID
	goal    string
	repo    string
	baseRef string
	command string
	args    []string
	steps   []validation.Step
}

func parseRun(fields []string) (runOptions, error) {
	opts := runOptions{baseRef: "HEAD"}
	for i := 0; i < len(fields); i++ {
		if i+1 >= len(fields) {
			return runOptions{}, fmt.Errorf("%s needs a value", fields[i])
		}
		value := fields[i+1]
		i++
		switch fields[i-1] {
		case "--id":
			opts.id = task.TaskID(value)
		case "--goal":
			opts.goal = value
		case "--repo":
			opts.repo = value
		case "--base-ref":
			opts.baseRef = value
		case "--command":
			opts.command = value
		case "--arg":
			opts.args = append(opts.args, value)
		case "--validate":
			parts, err := shell_lexer.Fields(value)
			if err != nil || len(parts) == 0 {
				return runOptions{}, fmt.Errorf("invalid validation command %q", value)
			}
			opts.steps = append(opts.steps, validation.Step{Command: parts[0], Args: parts[1:], Timeout: 5 * time.Minute})
		default:
			return runOptions{}, fmt.Errorf("unknown run flag %q", fields[i-1])
		}
	}
	if opts.id == "" || strings.TrimSpace(opts.goal) == "" || strings.TrimSpace(opts.repo) == "" || strings.TrimSpace(opts.command) == "" || len(opts.steps) == 0 {
		return runOptions{}, fmt.Errorf("usage: run --id ID --goal TEXT --repo PATH --command CMD [--arg ARG] --validate COMMAND")
	}
	return opts, nil
}

func defaultWorkspaceRoot() (string, error) {
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "pony", "workspaces"), nil
}

type runState struct {
	steps         []validation.Step
	stopRequested bool
	validating    bool
}

type app struct {
	agents     *agent.Manager
	workspaces *workspace.Manager
	tasks      *task.Manager
	events     *event.MemoryStore
	ctx        context.Context
	cancel     context.CancelFunc
	mu         sync.Mutex
	watchers   sync.WaitGroup
	runs       map[task.TaskID]*runState
}

func newApp(root string) (*app, error) {
	workspaces, err := workspace.NewManager(root)
	if err != nil {
		return nil, err
	}
	agents := agent.NewManager(driver.NewPTYDriver())
	events := event.NewMemoryStore()
	ctx, cancel := context.WithCancel(context.Background())
	return &app{
		agents: agents, workspaces: workspaces,
		tasks: task.NewManager(workspaces, agents, events), events: events,
		ctx: ctx, cancel: cancel,
		runs: make(map[task.TaskID]*runState),
	}, nil
}

func (a *app) run(opts runOptions) (task.Snapshot, error) {
	if _, exists := a.agents.Get(agent.AgentID(opts.id)); exists {
		return task.Snapshot{}, fmt.Errorf("agent %q already exists", opts.id)
	}
	created, err := a.tasks.Create(task.Spec{ID: opts.id, Goal: opts.goal, Repository: opts.repo, BaseRef: opts.baseRef})
	if err != nil {
		return task.Snapshot{}, err
	}
	started, err := a.tasks.Start(created.ID, task.LaunchSpec{Command: opts.command, Args: opts.args})
	if err != nil {
		return task.Snapshot{}, err
	}
	if _, err := a.agents.Write(agent.AgentID(opts.id), []byte(opts.goal+"\n")); err != nil {
		_, _ = a.tasks.Stop(opts.id)
		return task.Snapshot{}, fmt.Errorf("send goal to task %q: %w", opts.id, err)
	}
	a.mu.Lock()
	a.runs[opts.id] = &runState{steps: opts.steps}
	a.mu.Unlock()
	a.watch(opts.id)
	return started, nil
}

func (a *app) watch(id task.TaskID) {
	session, ok := a.agents.Get(agent.AgentID(id))
	if !ok {
		return
	}
	a.watchers.Add(1)
	go func(generation uint64) {
		defer a.watchers.Done()
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-a.ctx.Done():
				return
			case <-ticker.C:
			}
			current, ok := a.agents.Get(agent.AgentID(id))
			if !ok || current.Generation != generation {
				return
			}
			if current.State != agent.StateStopped && current.State != agent.StateCrashed {
				continue
			}
			a.mu.Lock()
			run := a.runs[id]
			if run == nil || run.stopRequested {
				a.mu.Unlock()
				return
			}
			if current.State == agent.StateStopped {
				run.validating = true
			}
			a.mu.Unlock()
			snapshot, err := a.tasks.Refresh(id)
			if err == nil && snapshot.State == task.StateStopped {
				_, _, _ = a.tasks.ValidateContext(a.ctx, id, run.steps)
			}
			a.mu.Lock()
			run.validating = false
			a.mu.Unlock()
			return
		}
	}(session.Generation)
}

func (a *app) stop(id task.TaskID) (task.Snapshot, error) {
	a.mu.Lock()
	if run := a.runs[id]; run != nil {
		if run.validating {
			a.mu.Unlock()
			return task.Snapshot{}, fmt.Errorf("task %q is validating", id)
		}
		run.stopRequested = true
	}
	a.mu.Unlock()
	snapshot, err := a.tasks.Stop(id)
	if err != nil {
		a.mu.Lock()
		if run := a.runs[id]; run != nil {
			run.stopRequested = false
		}
		a.mu.Unlock()
	}
	return snapshot, err
}

func (a *app) restart(id task.TaskID) (task.Snapshot, error) {
	a.mu.Lock()
	if run := a.runs[id]; run != nil && run.validating {
		a.mu.Unlock()
		return task.Snapshot{}, fmt.Errorf("task %q is validating", id)
	}
	a.mu.Unlock()
	snapshot, err := a.tasks.Restart(id)
	if err != nil {
		return snapshot, err
	}
	a.mu.Lock()
	if run := a.runs[id]; run != nil {
		run.stopRequested = false
	}
	a.mu.Unlock()
	a.watch(id)
	return snapshot, nil
}

func (a *app) validate(id task.TaskID) (task.Snapshot, []validation.Result, error) {
	a.mu.Lock()
	run := a.runs[id]
	a.mu.Unlock()
	if run == nil {
		return task.Snapshot{}, nil, fmt.Errorf("no task %q", id)
	}
	return a.tasks.Validate(id, run.steps)
}

func (a *app) clean(id task.TaskID) error {
	a.mu.Lock()
	if run := a.runs[id]; run != nil && run.validating {
		a.mu.Unlock()
		return fmt.Errorf("task %q is validating", id)
	}
	a.mu.Unlock()
	if err := a.tasks.Clean(id); err != nil {
		return err
	}
	a.mu.Lock()
	delete(a.runs, id)
	a.mu.Unlock()
	return nil
}

func (a *app) shutdown() error {
	a.cancel()
	a.watchers.Wait()
	var stopErrors []error
	for _, snapshot := range a.agents.Snapshots() {
		if err := a.agents.Stop(snapshot.AgentID); err != nil {
			stopErrors = append(stopErrors, err)
		}
	}
	return errors.Join(stopErrors...)
}
