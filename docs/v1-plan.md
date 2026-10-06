# Pony — Development Plan

> Build Pony one systems problem at a time.
> Do not build the final architecture upfront. Each phase should solve
> one real problem, add tests, and leave the previous behavior working.

> **v0** (the process supervisor) is released and documented in
> [v0-plan.md](./v0-plan.md).

The next goal is **not** to jump directly into tasks, worktrees, MCP,
orchestration, or persistence.

The next goal is to turn the process supervisor into a runtime that can
reliably run **one real interactive coding agent**.

## Platform support (V1 decision)

Pony V1 supports **Linux only**. The runtime relies on Linux/Unix
primitives: process groups (`Setpgid`), `SIGTERM`/`SIGKILL`, and
`/proc/<pid>` inspection, so portability is declared rather than
accidental.

Later, platform-specific code will be split into tagged files such as
`process_unix.go` / `process_windows.go` (`//go:build unix`), and the
`/proc`-based test assertions will be isolated into Linux-only test
files appended with `_linux_test.go`.

------------------------------------------------------------------------

# Phase 1 — Prove Multiple-Agent Lifecycle

### Goal

Prove that Pony can independently manage multiple running
processes.

### Build

-   Start two agents.
-   Stop one without affecting the other.
-   Restart one while the other keeps running.
-   Show both in `status`.
-   Verify independent PIDs and states.

### Tests

-   [x] Two agents can run simultaneously.
-   [x] Stopping agent A does not affect agent B.
-   [x] Restarting A does not affect B.
-   [x] Natural exit of A does not change B.
-   [x] Duplicate IDs are rejected.

### Done when

``` text
agent-1 → running
agent-2 → running

stop agent-1

agent-1 → stopped
agent-2 → running
```

Do not add new architecture yet.

------------------------------------------------------------------------

# Phase 2 — Make Shutdown Correct

### Goal

A process must never make Pony hang forever during shutdown.

### Learn

-   Process groups
-   SIGTERM vs SIGKILL
-   Parent/child processes
-   Why killing only the parent can leak children
-   Grace periods

### Build

Implement:

``` text
Stop
 ↓
SIGTERM process group
 ↓
wait up to N seconds
 ↓
still alive?
 ├── no  → clean exit
 └── yes → SIGKILL process group
```

Start with a small grace period such as 3--5 seconds.

### Tests

-   [x] Normal process stops with SIGTERM.
-   [x] Process ignoring SIGTERM is eventually killed.
-   [x] Child process does not remain running.
-   [x] Stop called twice is safe.
-   [x] Restart always produces a new generation.

### Done when

A badly behaved process cannot permanently block Pony.

------------------------------------------------------------------------

# Phase 3 — Separate Agent Identity From Runtime

### Goal

Stop treating a PID as the identity of an agent.

Introduce only the concepts that are now justified:

``` text
AgentID
SessionID
Generation
RuntimeState
SessionSnapshot
```

Mental model:

``` text
Agent
  └── Session
        └── Generation
              └── OS Process
                    └── PID
```

A restart creates a new generation.

``` text
agent-1
  generation 1 → PID 1001
  generation 2 → PID 1042
```

### Invariants

-   [x] PID is metadata, not identity.
-   [x] Every restart creates a newer generation.
-   [x] An old process exit cannot modify the state of a newer
    generation.
-   [x] Callers receive snapshots instead of mutable internal state.

### Done when

The lifecycle model still works without relying on PID identity.

------------------------------------------------------------------------

# Phase 4 — Introduce the Driver Boundary

### Goal

Stop making the rest of Pony depend directly on `os/exec`.

Introduce the smallest useful abstraction:

``` go
type Driver interface {
    Start(ctx context.Context, spec Spec) (*Handle, error)
    Write(h *Handle, data []byte) (int, error)
    Resize(h *Handle, rows, cols uint16) error
    Stop(ctx context.Context, h *Handle) error
    Wait(h *Handle) ExitResult
}
```

`Spec` carries `Path`, `Args`, `Cwd` and `Env` so launch details (working
directory, environment, later a PTY) live behind the boundary.

Do **not** build multiple drivers.

First implementation:

``` text
ProcessDriver
```

It can simply wrap the process behavior Pony already has.

### Done when

-   [x] Supervisor owns lifecycle.
-   [x] Driver owns process interaction.
-   [x] Higher-level code does not call `exec.Command` directly.
-   [x] Existing lifecycle tests still pass.

------------------------------------------------------------------------

# Phase 5 — Build the PTY Driver

### Goal

Run a real interactive terminal program.

This is the first major jump from "process supervisor" to "agent
runtime".

### Learn

-   PTY vs pipe
-   TTY behavior
-   stdin/stdout/stderr
-   terminal dimensions
-   ANSI escape sequences
-   process groups and terminal sessions

### Build

Start with something simple:

``` text
Pony
   ↓
PTY Driver
   ↓
bash / sh
```

Required operations:

``` text
Start
Write
Read
Resize
Wait
Stop
```

### Test manually

``` text
start shell
↓
send: echo hello
↓
read: hello
↓
resize terminal
↓
send: exit
↓
observe clean exit
```

### Then

Run one real coding agent through the PTY.

Do not add orchestration yet.

### Done when

Pony can reliably start, interact with, and stop one interactive
agent.

### Delivered

-   [x] `internal/driver/pty.go` — `PTYDriver` runs a process on a
    real pseudo-terminal (`pty.Open`, slave on stdio, `Setsid` /
    `Setctty`).
-   [x] Terminal window size — 24x80 default on start,
    `Resize` via `TIOCSWINSZ`, verified with `TIOCGWINSZ`.
-   [x] Full I/O surface on the driver: `Start`, `Write`, `Read`,
    `ReadTimeout`, `Resize`, `Wait`, `Stop`.
-   [x] `ReadTimeout` — a read that returns when the process has been
    quiet for a deadline. It watches the fd with `poll(2)` (fd-number
    based, so no `select(2)` 1024-fd ceiling), so a REPL `read` never
    blocks forever on an idle agent.
-   [x] Manager plumbing: `Write`, `Read`, `Resize`, `ReadTimeout`
    on agents.
-   [x] REPL commands: `send <id> <text>`, `read <id>`,
    `resize <id> <rows> <cols>`; the CLI starts agents on a PTY.
-   [x] Acceptance ride passes end to end: `start sh` → `echo hello`
    → read `hello` / prompt → resize → `exit` → clean stop, no
    stray processes.
-   [x] Concurrent I/O is safe and full-duplex: separate `read`/`write`
    locks on `Handle` (a single lock deadlocked a blocked read against
    a concurrent write), with a `stateMu` guarding fd teardown in
    `Wait` (race-tested).
-   [x] `Stop` terminates the whole terminal session: every process
    group still in the PTY session receives `SIGTERM`, then `SIGKILL`
    after a grace period, and `Stop` waits until the session is empty —
    job-control children do not survive a stopped shell
    (regression-tested against a `SIGTERM`-ignoring child).
-   [x] **Dependency decision:** Pony now depends on
    `github.com/creack/pty` (MIT) and `golang.org/x/sys` (BSD) instead
    of hand-rolling `/dev/ptmx` ioctls. These are the first
    non-stdlib dependencies and a deliberate platform (Linux) choice.

``` text
pony> start shell sh
started pid=41283 state=running
pony> send shell echo hello
sent shell: echo hello
pony> read shell
echo hello
sh-5.3$ echo hello
hello
sh-5.3$
pony> send shell exit
pony>
```

------------------------------------------------------------------------

# Phase 6 — Terminal Attach / Detach

### Goal

Make the runtime actually usable from the terminal.

### The constraint this phase actually hits

Everything starts from main and ends from main. `main` builds the
`Manager` in-process and in-memory (`cmd/pony/main.go:20`), and a
`defer` stops every agent on exit (`cmd/pony/main.go:22-28`). There is
no daemon, and Phase 6 does not add one.

Herdr runs a background server with many attached clients, so its
"attach" survives the terminal closing and works from another shell.
That is the `daemon/client split` and `persistent sessions` work this
document lists as After-V1.

For Pony as it stands, attach means: **hand the current terminal over
to one running agent, and take it back without stopping that agent.**
Both the runtime and the terminal are in one process, so there is no
process migration and nothing to reconnect to.

The idea worth taking from herdr is that the PTY output is owned by
the runtime at all times and a viewer subscribes to it. In a
single-process Pony, the viewer is just a foreground loop in `main`
while the manager and its reader keep running.

### Learn

-   termios: `TCGETS`/`TCSETS`, and why `ICANON`, `ECHO`, `ISIG` must
    be cleared for a bridge to work
-   Raw mode: `VMIN`/`VTIME`, and the cost of never restoring it
-   PTY line discipline: Ctrl+C is only a signal because the child's
    own terminal has `ISIG` set
-   `SIGWINCH` on the local terminal, and the `TIOCSWINSZ` that raises
    `SIGWINCH` in the child
-   Blocking reads: how to unblock a goroutine parked in `read(2)`
    without closing the fd out from under it
-   Bounded buffering: a small ring between one reader and at most one
    foreground viewer

### Build

-   [x] input forwarding
-   [x] output forwarding
-   [x] terminal resize
-   [x] Ctrl+C behavior
-   [x] Ctrl+D behavior
-   [x] clean detach without killing the agent

Concretely:

-   [x] `internal/terminal` — `GetState`, `MakeRaw`, `Restore`, `Size`.
        The original termios is restored on every exit path: normal
        return, panic, SIGINT, SIGTERM, SIGHUP, and attach failure.
        Restore is idempotent. A killed Pony must never leave the user
        with an unusable shell.
-   [x] One always-on reader per running session, started with the
        session and owned by its lifecycle, feeding a small bounded
        ring buffer. Exactly one reader on the master at any time,
        bounded memory no matter how chatty the agent is or how long
        nobody is watching, and no client ever closes the fd --
        teardown belongs to `Wait` alone.
-   [x] `pony attach <id>` — enter raw mode, subscribe to that
        session's output, forward local stdin to its PTY and its PTY
        to local stdout, byte transparent. Apply the real terminal
        size before the first byte is forwarded.
-   [x] Resize — catch `SIGWINCH` while attached and push the new size
        to the agent, so a full-screen agent reflows when the window is
        dragged.
-   [x] Keys — forward Ctrl+C as `0x03` and Ctrl+D as `0x04` so the
        agent's own terminal decides what they mean. Pony interprets
        neither, so neither kills Pony. Detach gets its own key
        (Ctrl+\\) that cannot collide with agent keybindings.
-   [x] Clean detach — restore termios, unsubscribe, return to the
        prompt. It must not go through `Manager.Stop`. The session is
        left with the same generation, the same PID, and still
        `StateRunning`.
-   [x] REPL coexistence — while attached, input is read from stdin as
        a raw byte stream rather than through the REPL's buffered
        scanner, which would otherwise swallow keystrokes meant for the
        agent. `quit` still exits and still stops every agent.

### Multiple agents

Unchanged by any of this. Each session gets its own PTY, its own
driver handle, and its own reader and buffer, and `attach` binds the
terminal to exactly one of them at a time. Two agents can run at once
and be attached one after the other, and the detached ones keep
running and keep buffering. Phase 1 already guarantees the
independence; Phase 6 only adds a viewer.

### Done when

``` text
pony> start shell sh
pony> start helper sh
pony> attach shell
$ echo hello
hello
$ Ctrl+\                       # detach, back to the prompt
pony> attach helper             # the other agent, same terminal
$ Ctrl+\ 
pony> status                    # both still running, untouched
```

One agent is attached, both keep running, and the terminal is handed
back and forth between them.

The general case, which is what the phase is really for:

``` text
start agent
↓
detach
↓
agent keeps running
↓
attach again
↓
continue interacting
```

The same terminal is handed between agents, one at a time, and the
detached ones keep running and keep buffering.

### Tests

-   [x] Term state: `GetState`/`MakeRaw`/`Restore` idempotent; restored on
        panic and on a fatal signal.
-   [x] One reader on the master per session, and buffers stay
        independent between agents.
-   [x] Output survives detach and is visible on re-attach.
-   [x] The buffer stays bounded under a flood.
-   [x] Input and output are byte transparent.
-   [x] Ctrl+C and Ctrl+D reach the child; neither kills Pony.
-   [x] Detach leaves the session running, same generation and PID.
-   [x] Resize is applied before the first forward and reaches the
        child.
-   [x] A second agent is unaffected while the first one is attached.
-   [x] `go test -race ./...` stays green locally.
-   [ ] CI stays green after these changes are pushed.

# Phase 7 — Workspace Isolation

### Goal

Two coding agents must never accidentally edit the same checkout.

Introduce a small workspace manager.

``` text
Task
 ↓
Workspace
 ↓
Git worktree
 ↓
Agent
```

Use Git itself:

``` bash
git worktree add
git worktree list --porcelain
git worktree remove
git worktree prune
```

### Build

``` text
Allocate(task)
Release(task)
List()
```

Each workspace should have:

``` text
WorkspaceID
Path
Branch
TaskID
```

### Implementation steps

-   [x] Define workspace identity and validate task IDs, repositories,
        base refs, paths, and branch conflicts.
-   [x] Discover Git worktrees with `git worktree list --porcelain -z`
        and identify worktrees safely owned by Pony.
-   [x] Allocate a worktree and roll back partial failures.
-   [x] Release only Pony-owned worktrees; make cleanup idempotent.
-   [x] Start agents in their allocated worktrees and verify isolation.

The agent's working directory becomes the allocated worktree.

### Tests

-   [x] Two tasks get different worktrees.
-   [x] Both can run simultaneously.
-   [x] Releasing one does not remove the other.
-   [x] Cleanup is idempotent.

### Done when

Two agents can work against the same repository without sharing a
writable checkout.

------------------------------------------------------------------------

# Phase 8 — Introduce Tasks

### Goal

Separate the **software task** from the **agent process**.

Minimal model:

``` text
Task
 ├── Goal
 ├── Repository
 ├── BaseRef
 ├── Workspace
 └── Session
```

Example:

``` yaml
id: raft-snapshot

goal: Implement Raft snapshot installation.

repository: ~/code/ragnordb
base_ref: master
```

Do not add a database.

Keep task state in memory first.

### Task states

``` text
pending
running
validating
verified
failed
stopped
```

### Implementation steps

-   [x] Define task identity, immutable snapshots, states, and validation.
-   [x] Store tasks in memory with create, get, and deterministic list operations.
-   [ ] Start a task transactionally by allocating its workspace and agent.
-   [ ] Add stop, restart, and refresh lifecycle controls.
-   [ ] Verify the complete task lifecycle with end-to-end tests.

### Done when

A task can create a workspace and launch an agent inside it.

------------------------------------------------------------------------

# Phase 9 — Structured Lifecycle Events

### Goal

Make runtime behavior observable without relying only on current mutable
state.

Start with a small event model:

``` text
TaskCreated
WorkspaceCreated
SessionStarted
RuntimeStarted
RuntimeExited
RuntimeCrashed
ValidationStarted
ValidationPassed
ValidationFailed
TaskCompleted
TaskFailed
```

Example:

``` go
type Event struct {
    Time       time.Time
    TaskID     TaskID
    SessionID  SessionID
    Generation uint64
    Type       EventType
    Message    string
}
```

Initially:

``` text
in-memory events
+
optional JSONL log
```

No SQLite yet.

### Invariants

-   [ ] Events have ordering.
-   [ ] A runtime generation has one final exit event.
-   [ ] Old generations cannot emit events against new generations.
-   [ ] Event recording does not mutate lifecycle state directly.

### Done when

You can reconstruct what happened to a task from its event stream.

------------------------------------------------------------------------

# Phase 10 — Validation

### Goal

Separate:

``` text
agent finished
```

from:

``` text
task succeeded
```

Introduce:

``` go
type ValidationStep struct {
    Command string
    Args    []string
    Timeout time.Duration
}
```

Run validation inside the task's worktree.

Capture:

-   [ ] exit code
-   [ ] stdout
-   [ ] stderr
-   [ ] duration
-   [ ] timeout

Example:

``` bash
pony run \
  --id example \
  --repo ./repo \
  --command codex \
  --validate "go test ./..."
```

### Rule

``` text
Task verified
=
every required validation step succeeded
```

### Done when

Pony can tell the difference between an agent claiming completion
and a validated result.

------------------------------------------------------------------------

# Phase 11 — End-to-End `run`

### Goal

Make the whole system usable through one command.

Target:

``` bash
pony run \
  --id example \
  --repo ~/code/project \
  --command codex \
  --validate "go test ./..."
```

Expected lifecycle:

``` text
create task
   ↓
allocate worktree
   ↓
start session
   ↓
start agent in PTY
   ↓
interact / observe
   ↓
agent exits
   ↓
run validation
   ↓
verified / failed
```

CLI surface:

``` bash
pony run
pony list
pony attach
pony stop
pony restart
pony validate
pony clean
```

Keep the CLI simple.

No TUI yet.

### Done when

A new user can clone Pony and run one coding task from start to
validated result.

------------------------------------------------------------------------

# Phase 12 — Hardening

Only after the end-to-end flow works.

Focus on correctness rather than features.

### Process

-   [ ] process-tree cleanup
-   [ ] race conditions
-   [ ] shutdown edge cases
-   [ ] generation races
-   [ ] PTY cleanup
-   [ ] terminal resize edge cases
-   [ ] child-process leaks

### Git

-   [ ] worktree cleanup failures
-   [ ] dirty worktrees
-   [ ] missing branches
-   [ ] repository errors

### CLI

-   [ ] invalid arguments
-   [ ] useful errors
-   [ ] interrupted commands
-   [ ] cleanup on exit

### Testing

-   [ ] unit tests
-   [ ] integration tests
-   [ ] `go test -race ./...`
-   [ ] end-to-end smoke test
-   [ ] CI green

------------------------------------------------------------------------

# V1 Definition of Done

Pony V1 is complete when this works reliably:

``` text
             ┌──────────────┐
             │     Task     │
             └──────┬───────┘
                    ↓
             ┌──────────────┐
             │ Git Worktree │
             └──────┬───────┘
                    ↓
             ┌──────────────┐
             │    Session   │
             └──────┬───────┘
                    ↓
             ┌──────────────┐
             │  PTY Driver  │
             └──────┬───────┘
                    ↓
             ┌──────────────┐
             │ Coding Agent │
             └──────┬───────┘
                    ↓
             ┌──────────────┐
             │  Validation  │
             └──────┬───────┘
                    ↓
              VERIFIED / FAILED
```

Checklist:

-   [x] multiple agents run independently
-   [x] process shutdown is bounded
-   [x] child processes are cleaned up
-   [x] session identity is independent of PID
-   [x] generations protect against stale process events
-   [ ] one driver abstraction exists
-   [ ] one PTY driver works
-   [ ] terminal input/output works
-   [ ] terminal resize works
-   [x] agents can run in isolated Git worktrees
-   [ ] tasks are separate from sessions
-   [ ] lifecycle events are observable
-   [ ] validation is deterministic
-   [ ] execution status and verification status are separate
-   [ ] end-to-end `pony run` works
-   [ ] `go test -race ./...` passes
-   [ ] CI is green

------------------------------------------------------------------------

# After V1 — Only When a Real Problem Appears

Potential V2+ work:

``` text
ACP driver
daemon/client split
persistent sessions
SQLite
crash recovery
semantic agent states
permission policies
Docker / sandboxing
cgroups
remote workers
SSH
scheduling
concurrency limits
reviewer agents
best-of-N
task DAGs
MCP/API
TUI
web UI
plugins
```

These are **not milestones for V1**.

They are a backlog.

------------------------------------------------------------------------

# Repository Evolution

Do not create all of these directories now.

Let the repository grow with the problems:

``` text
Today

internal/
└── agent/


After process abstraction

internal/
├── agent/
├── driver/
└── supervisor/


After PTY

internal/
├── agent/
├── driver/
│   └── pty/
└── supervisor/


After workspaces/tasks

internal/
├── agent/
├── driver/
│   └── pty/
├── supervisor/
├── workspace/
└── task/


After events/validation

internal/
├── agent/
├── driver/
│   └── pty/
├── supervisor/
├── workspace/
├── task/
├── event/
└── validation/
```

Do not create empty packages just because the final architecture
contains them.

------------------------------------------------------------------------

# The Rule For Every Step

Before implementing something, answer:

1.  **What problem am I solving?**
2.  **What OS/system concept do I need to understand?**
3.  **What is the smallest abstraction needed?**
4.  **What invariant must remain true?**
5.  **How will I test it?**
6.  **What does "done" look like?**

Then implement only that step.

> Learn → implement → test → observe → refactor → move on.

Do not let the final architecture dictate today's code.

The goal is not to make Pony look like Herdr.

The goal is to make Pony a runtime whose architecture emerges from
real problems encountered while running coding agents.
