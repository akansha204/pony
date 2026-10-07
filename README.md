# Pony

Pony is a lightweight Go runtime for running and managing
multiple AI coding agents.

## v0 goal

Pony should be able to:

1. Start an agent
2. Keep its process/session alive
3. Observe its state
4. Stop/restart it
5. Manage multiple agents independently

## Platform

V1 targets **Linux only**. Process-group termination (`Setpgid`,
`SIGTERM`/`SIGKILL`), exit-signal decoding, and the `/proc/<pid>`
checks used by tests are Linux/Unix-specific behaviors. Cross-platform
support is a later milestone (platform-tagged files such as
`process_unix.go` / `process_windows.go`).

## Development

Requires Go 1.26+.

```sh
make build      # compile to bin/pony
make run        # open the TUI in a terminal
make test       # run tests with the race detector
make check      # fmt + tidy + vet + test (mirrors CI)
```

## Run a coding task

Start Pony with `make run`. Press `:` and enter a command like:

```text
run --id example --goal "Fix the failing test" --repo /path/to/repo --command codex --validate "go test ./..."
```

The initial TUI shows task status and details. Use `j`/`k` or the arrow
keys to select a task, `r` to refresh, and `q` to quit. Other task
actions are still in the CLI: start Pony with `pony --cli` (or
`go run ./cmd/pony --cli`) to use commands such as:

```text
pony> run --id example --goal "Fix the failing test" --repo /path/to/repo --command codex --validate "go test ./..."
pony> list
pony> attach example
```

Use `--arg VALUE` for each agent argument, `--base-ref REF` to choose a
base commit (default `HEAD`), and repeat `--validate "COMMAND"` for
additional checks. Each check has a five-minute timeout. The task's
goal is sent to the agent on startup. A clean agent exit runs the checks
in order; all must pass for the task to become `verified`.

`attach` returns to Pony with Ctrl+\\. `stop ID` stops a task without
validating it; `validate ID` runs its checks manually. `restart ID`
reuses its worktree, and `clean ID` releases a stopped task's clean
worktree. Pony refuses to clean a worktree with uncommitted changes.
`clean` leaves its Git branch intact, so use a new task ID for the next run.

Task state lives in the current Pony process. Closing Pony stops running
agents and leaves worktrees available for inspection. The default
worktree root is `$XDG_DATA_HOME/pony/workspaces`, or
`$HOME/.local/share/pony/workspaces` when `XDG_DATA_HOME` is unset.
