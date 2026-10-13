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

## Install Pony

Pony requires Go 1.26.4 or newer. Install the latest tagged version with:

```sh
go install github.com/akansha204/pony/cmd/pony@latest
```

Then run `pony` to open the TUI, or `pony --cli` to open the CLI.
Check the installed version with `pony --version`.
Pony checks for a newer tagged release when the TUI opens; press `u` to install
it. You can also run `pony --update` from a terminal. Restart Pony after an
update to use the new version. These update checks need an internet connection.

Go puts installed commands in `GOBIN` when it is set, otherwise in
`GOPATH/bin` (usually `~/go/bin`). That directory must be on your shell's
`PATH`. To add the default Go bin directory for the current terminal:

```sh
export PATH="$(go env GOPATH)/bin:$PATH"
```

Add that line to your shell startup file to keep it after opening a new
terminal (for example, `~/.bashrc` for Bash or `~/.zshrc` for Zsh). If you
set a custom `GOBIN`, add that directory instead.

`@latest` installs the latest tagged release; it does not mean the newest
unreleased commit on `master`. To install a specific release, replace
`@latest` with its tag, for example `@v0.2.0`.

## Development

Requires Go 1.26.4 or newer.

```sh
make build      # compile to bin/pony
make run        # open the TUI in a terminal
make test       # run tests with the race detector
make check      # fmt + tidy + vet + test (mirrors CI)
```

`make build` and `make run` include the current Git version in `pony --version`.
For the release and version-tagging steps, see [docs/versioning.md](docs/versioning.md).

## Run a task

Start Pony with `make run`. Press `:` and enter a command like:

```text
run --id go-todo --goal "Build a simple in-memory todo backend in Go" --command codex
```

The initial TUI shows task status and details. Use `j`/`k` or the arrow
keys to select a task, `r` to refresh, and `q` to quit. Press `?` to see
the full guide. Press `:` to enter task or agent-session commands, including
`start`, `send`, `read`, `attach`, `resize`, and `status`. Use the CLI with
`pony --cli` (or `go run ./cmd/pony --cli`) for the same commands:

```text
pony> run --id go-todo --goal "Build a simple in-memory todo backend in Go" --command codex
pony> list
pony> attach example
```

Run Pony from the Git repository you want to work on; Pony uses that current
directory's repository by default. Add `--repo PATH` to choose a different
repository. Use `--arg VALUE` for each agent argument,
`--base-ref REF` to choose a base commit (default `HEAD`), and repeat
`--validate "COMMAND"` when you want checks to run after the agent exits.
Validation is optional; each check has a five-minute timeout. A task with
checks becomes `verified` only when they all pass. Without checks, a clean
agent exit marks the task `completed` without claiming it was verified.
The working directory is still isolated in a new Git worktree based on the
selected base ref; uncommitted changes in the original checkout are not copied.

`attach` returns to Pony with Ctrl+\\. `stop ID` stops a task without
validating it; `validate ID` runs configured checks manually. `restart ID`
reuses its worktree, and `clean ID` releases a stopped or completed task's clean
worktree. Pony refuses to clean a worktree with uncommitted changes.
`clean` leaves its Git branch intact, so use a new task ID for the next run.

Task state lives in the current Pony process. Closing Pony stops running
agents and leaves worktrees available for inspection. The default
worktree root is `$XDG_DATA_HOME/pony/workspaces`, or
`$HOME/.local/share/pony/workspaces` when `XDG_DATA_HOME` is unset.
