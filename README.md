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
checks becomes `checks_passed` only when every configured command exits
successfully. This confirms the checks passed; it does not prove the requested
goal was achieved. Without checks, a clean agent exit marks the task
`completed`.
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

## Interactive agent sessions

Use an interactive session when you want to start an agent and keep its
terminal open while you send it more work, check what it printed, or take over
typing yourself. This is separate from `run`: `run` gives a task its own Git
worktree and goal, while `start` launches a long-running terminal session in
the current directory.

### Start Pony

Open the TUI (the default):

```sh
pony
```

Or open the text-command CLI:

```sh
pony --cli
```

The examples below use an agent named `reviewer`. Pick any name that helps you
remember what that session is doing. The command after the name must be
installed and available on your `PATH` (for example, `codex`):

```text
start reviewer codex
```

In the TUI, press `:` first, type the command, and press Enter. In the CLI,
type it at the `pony>` prompt. Pony starts the agent in a terminal session and
keeps that session running while you use Pony.

### Give the agent work

To send one prompt, use `prompt`:

```text
prompt reviewer "Look through this project and explain how login works"
```

Pony submits the text as a prompt and returns control to you. If you want Pony
to wait until the agent appears idle, ask for that explicitly:

```text
prompt reviewer --wait idle "Look through this project and explain how login works"
```

You can also wait for the agent to need you (`blocked`) or to finish (`done`):

```text
prompt reviewer --wait blocked "Try the small fix we discussed"
prompt reviewer --wait done "Run the checks and summarize the result"
```

Waiting is based on Pony observing the agent work and then observing the
requested state. It can take up to two minutes. In the TUI, the wait happens
in the background, so you can still use the interface while it waits.

Use `send` when you want to type a line into the agent's terminal directly:

```text
send reviewer "Please explain that last error"
```

`send` presses Enter after the line. Use `prompt` for a prompt that Pony should
submit as a prompt; use `send` for direct terminal input.

### See what is happening

Use these commands to inspect or take control of a session:

```text
status
read reviewer
screen reviewer
attach reviewer
```

`status` lists sessions and their process and agent states. `read` shows recent
output in the CLI and opens a readable, scrollable view of captured terminal
output and scrollback in the TUI. The CLI's `screen` command shows the current
visible screen;
Pony warns you if its screen view may be incomplete.
`attach` gives you direct keyboard control of the agent's terminal. Press
Ctrl+\\ to detach and return to Pony.

In the TUI, press `p` to open the session status panel, or press `:` and enter
`status`. Use `read`, `attach`, and the other session commands the same way:
press `:` first, then type the command. The TUI's `read` view shows the current
terminal output with line wrapping and keyboard scrolling. Press `?` at any
time for the built-in help. The TUI does not currently offer the CLI's `screen`
command.

### Understand session status

Pony reports two different kinds of state:

- `runtime` describes the process Pony started: for example, `running` or
  `exited`. This is about the program itself.
- `agent` is the program Pony recognized, such as `codex` or `claude`.
- `agent_state` is Pony's best guess about what that program is doing: for example,
  `working`, `idle`, `blocked`, `done`, or `unknown`.

For example, `runtime=running agent=codex agent_state=blocked` means the
Codex process is still alive, but Pony thinks it may be waiting for you.
`attention` is `needs-you` when the agent is blocked or has reached `done`. In
the TUI these sessions are highlighted so they are easier to spot. Opening the
status panel also acknowledges a `done` state and clears that attention marker.

The agent state comes from text Pony can see on the terminal screen, using the
profile for that agent type. It is an estimate, not a message from the agent
or proof that a task is complete. `unknown` means Pony could not confidently
match the screen to a known state. Use `read` or `attach` to check for yourself.
Pony recognizes common agent commands such as Codex, Claude, OpenCode, and
Aider; other commands can still run, but may show as `unknown`. Profiles are
stored in `profiles/agents/`; set `PONY_AGENT_PROFILES` to use a different
profile directory.

### Stop or restart a session

```text
stop reviewer
restart reviewer
```

`stop` ends the session. `restart` starts it again with the same command and
arguments. Quit Pony when you're finished; Pony stops its running sessions on
exit. `resize reviewer 30 100` changes the session terminal to 30 rows by 100
columns if you need to match your terminal size.

### A complete example

Here is a typical CLI session from start to finish:

```text
pony> start reviewer codex
pony> prompt reviewer --wait idle "Explain how login works in this project"
reviewer reached idle
pony> read reviewer
pony> status
reviewer   pid=... runtime=running agent=codex agent_state=idle attention=none ...
pony> prompt reviewer --wait done "Make the agreed change and run the checks"
reviewer reached done
pony> status
reviewer   pid=... runtime=running agent=codex agent_state=done attention=needs-you ...
pony> attach reviewer
```

In the TUI, use the same commands after pressing `:`; use `p` to check status
and `?` to revisit the guide. Once attached, work directly in the agent's
terminal and press Ctrl+\\ to return to Pony.
