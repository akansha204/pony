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

// resolveGoalLaunch passes the goal as one process argument. A custom command
// must name the goal's argument position explicitly; writing into a new TUI's
// terminal can race its startup and silently lose the request.
func resolveGoalLaunch(opts runOptions) (task.LaunchSpec, error) {
	args := append([]string(nil), opts.args...)
	placeholder := -1
	for i, arg := range args {
		if arg == "{goal}" {
			if placeholder >= 0 {
				return task.LaunchSpec{}, fmt.Errorf("use {goal} in exactly one --arg")
			}
			placeholder = i
		} else if strings.Contains(arg, "{goal}") {
			return task.LaunchSpec{}, fmt.Errorf("{goal} must occupy a whole --arg; interpolation is not supported")
		}
	}
	if placeholder >= 0 {
		args[placeholder] = opts.goal
		return task.LaunchSpec{Command: opts.command, Args: args}, nil
	}
	switch strings.ToLower(filepath.Base(opts.command)) {
	case "opencode":
		for _, arg := range args {
			if arg == "--" || arg == "--prompt" || strings.HasPrefix(arg, "--prompt=") {
				return task.LaunchSpec{}, fmt.Errorf("opencode arguments conflict with the built-in goal adapter; place {goal} in one --arg")
			}
		}
		if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
			return task.LaunchSpec{}, fmt.Errorf("opencode positional arguments and subcommands require one --arg '{goal}' at the goal position")
		}
		args = append(args, "--prompt="+opts.goal)
	case "codex":
		for _, arg := range args {
			if arg == "--" {
				return task.LaunchSpec{}, fmt.Errorf("codex arguments conflict with the built-in goal adapter; place {goal} in one --arg")
			}
		}
		if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
			return task.LaunchSpec{}, fmt.Errorf("codex positional arguments and subcommands require one --arg '{goal}' at the goal position")
		}
		args = append(args, "--", opts.goal)
	case "claude":
		if len(args) != 0 {
			return task.LaunchSpec{}, fmt.Errorf("claude with custom arguments requires one --arg '{goal}' at the goal position")
		}
		args = append(args, "--", opts.goal)
	case "aider":
		if len(args) != 0 {
			return task.LaunchSpec{}, fmt.Errorf("aider with custom arguments requires one --arg '{goal}' at the goal position")
		}
		args = append(args, "--message="+opts.goal)
	default:
		return task.LaunchSpec{}, fmt.Errorf("no built-in goal adapter for %q; add --arg '{goal}' at the command's goal argument position", opts.command)
	}
	return task.LaunchSpec{Command: opts.command, Args: args}, nil
}

func parseRun(fields []string) (runOptions, error) {
	opts := runOptions{baseRef: "HEAD", repo: "."}
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
	if opts.id == "" || strings.TrimSpace(opts.goal) == "" || strings.TrimSpace(opts.repo) == "" || strings.TrimSpace(opts.command) == "" {
		return runOptions{}, fmt.Errorf("usage: run --id ID --goal TEXT --command CMD [--repo PATH] [--arg ARG] [--validate COMMAND]")
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
	launch, err := resolveGoalLaunch(opts)
	if err != nil {
		return task.Snapshot{}, err
	}
	if _, exists := a.agents.Get(agent.AgentID(opts.id)); exists {
		return task.Snapshot{}, fmt.Errorf("agent %q already exists", opts.id)
	}
	created, err := a.tasks.Create(task.Spec{ID: opts.id, Goal: opts.goal, Repository: opts.repo, BaseRef: opts.baseRef})
	if err != nil {
		return task.Snapshot{}, err
	}
	started, err := a.tasks.Start(created.ID, launch)
	if err != nil && started.SessionID == "" {
		// Preserve a workspace that could not be rolled back for inspection.
		if started.WorkspaceID != "" {
			return started, errors.Join(err, fmt.Errorf("task %q retains workspace %q; use list to inspect it and clean %s when safe", opts.id, started.WorkspacePath, opts.id))
		}
		// Start could not launch a session and rolled back its new workspace.
		if cleanErr := a.tasks.Clean(created.ID); cleanErr != nil {
			return started, errors.Join(err, fmt.Errorf("clean failed launch %q: %w", opts.id, cleanErr))
		}
		return task.Snapshot{}, err
	}
	startErr := err
	a.mu.Lock()
	a.runs[opts.id] = &runState{steps: opts.steps}
	a.mu.Unlock()
	a.watch(opts.id)
	return started, startErr
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
			steps := append([]validation.Step(nil), run.steps...)
			if current.State == agent.StateStopped && len(steps) > 0 {
				run.validating = true
			}
			a.mu.Unlock()
			snapshot, err := a.tasks.Refresh(id)
			if err != nil {
				fmt.Fprintf(os.Stderr, "pony: refresh task %q: %v\n", id, err)
				a.mu.Lock()
				if run := a.runs[id]; run != nil {
					run.validating = false
				}
				a.mu.Unlock()
				return
			}
			if snapshot.State == task.StateStopped {
				if len(steps) > 0 {
					if _, _, err := a.tasks.ValidateContext(a.ctx, id, steps); err != nil {
						fmt.Fprintf(os.Stderr, "pony: validate task %q: %v\n", id, err)
					}
				} else {
					if _, err := a.tasks.Complete(id, current.Generation); err != nil {
						fmt.Fprintf(os.Stderr, "pony: complete task %q: %v\n", id, err)
					}
				}
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
	if err != nil && snapshot.SessionID == "" {
		return snapshot, err
	}
	a.mu.Lock()
	if run := a.runs[id]; run != nil {
		run.stopRequested = false
	}
	a.mu.Unlock()
	a.watch(id)
	return snapshot, err
}

func (a *app) validate(id task.TaskID) (task.Snapshot, []validation.Result, error) {
	a.mu.Lock()
	run := a.runs[id]
	a.mu.Unlock()
	if run == nil {
		return task.Snapshot{}, nil, fmt.Errorf("no task %q", id)
	}
	if len(run.steps) == 0 {
		return task.Snapshot{}, nil, fmt.Errorf("task %q has no validation commands configured; add --validate when starting the task", id)
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
