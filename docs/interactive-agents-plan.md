# Pony — Interactive Agents + State From Output (Plan)

> **Status:** Phases 1–2 implemented; later phases planned.
> This is a follow-on plan, not part of [v1-plan.md](./v1-plan.md).

> Build Pony one systems problem at a time.
> Do not build the final architecture upfront. Each phase should solve
> one real problem, add tests, and leave the previous behavior working.

Today Pony supports two ways to run programs. `pony run` starts a
task-oriented, headless run: it sends a goal, waits for the process to
exit, then validates. The lower-level agent-session commands (`start`,
`send`, `read`, and `attach`) already run interactive programs on a PTY
and leave them available for further input. This plan builds on those
interactive sessions; it does not replace or change task validation.

This plan adds the **smallest possible observation layer**: track a
best-effort semantic state for interactive agents, infer it from the
terminal they already use, and make it visible. Users can already type
into these terminals with `send` or `attach`; the later prompt phase
will add a higher-level prompt-and-wait command. Think *herdr minus the
daemon, the panes, the persistence, and the cross-platform story*.

It is deliberately **single-process and best-effort**. It is a hint
layer, not a control plane.

## Relationship to what exists

Pony already has the ingredients:

- a PTY driver that owns each session's terminal (`internal/driver/pty.go`)
- one reader per session fanning out to a bounded recent-output buffer
  (64 KiB by default, with a dropped-byte count)
- an `attach` viewer that hands the terminal to one session
- `send <id> <text>`, `read <id>`, `resize <id> <rows> <cols>`
- a manager with identities, generations, runtime states, and snapshots
- a CLI REPL and a TUI

What is missing is the part herdr is known for: **knowing which pane
holds an agent, and what state that agent is in**, without the agent
cooperating. This plan builds only that, in one process.

The first phase adds only the separate semantic-state field. It must
not claim to detect anything yet: every session starts as `unknown`
until later phases add observation and classification.

## Platform support

Linux only, same as V1. Process identity uses `/proc/<pid>` and the
tty foreground process group (`tcgetpgrp`). The screen model parses
bytes, so it is portable, but detection is Linux-gated.

## The shape of the layer

``` text
                 ┌──────────────────────────────────────┐
   PTY master ──►│ reader (one per session)             │
                 └───┬───────────────┬──────────────┬────┘
                     │               │              │
             attach viewer     scrollback ring   screen model
             (foreground)      (read <id>)        (bounded grid)
                                                       │
                                                       ▼
                                            detector (profiles)
                                                       │
                                                       ▼
                                    AgentState: idle | working | blocked | done | unknown
```

Invariant that must never break: **the reader is the only consumer of
the PTY master.** The screen model and detector are passive observers of
a copy. Attach, `read`, and resize keep working exactly as before, and
detection must never be able to stall the reader.

## State model (two axes, do not collapse them)

``` text
RuntimeState (process truth, exists today)
  idle · running · stopping · stopped · crashed

AgentState (semantic hint, new)
  unknown · idle · working · blocked · done
```

- `RuntimeState` answers "is there a process?" It stays authoritative
  and unchanged.
- `AgentState` answers "what does the agent look like it is doing?"
  It is **best-effort**. `unknown` is a first-class, honest answer.
- `done` is an attention state, not something the agent reports: the
  agent became idle while nobody was looking. It clears on view.

Never let `AgentState` override `RuntimeState`. A stopped process is
stopped, whatever the screen last said.

------------------------------------------------------------------------

# Phase 1 — Introduce the Semantic State

### Goal

Carry an `AgentState` alongside the existing runtime state, with no
detector yet. Everything reports `unknown`.

### Build

-   Add a distinct `AgentState` type and `AgentState` field to the agent
    package's session snapshots. Keep the existing `RuntimeState` and
    its `State` field authoritative and unchanged.
-   Default every session to `unknown`.
-   Surface it in `status` as an extra column.

### Invariants

-   `AgentState` never changes `RuntimeState`.
-   Adding the field breaks no existing behavior; `unknown` prints
    quietly.

### Done when

``` text
pony> start a sleep 100
pony> status
a    pid=1234   state=running   agent=unknown
```

------------------------------------------------------------------------

# Phase 2 — Build the Screen Model

### Goal

Turn the raw PTY byte stream into the **current visible screen**, so a
detector can look at what a human would look at.

### Learn

-   ANSI/CSI escape sequences: cursor moves, erase, scroll, colors
-   Why raw bytes are not a screen (wrapping, cursor, alternate buffer)
-   The alternate screen buffer (full-screen TUIs use it)
-   Ring buffer vs grid: a bounded tail is what detection needs
-   Cost of a screen model on the hot path of the reader

### Build

-   A small terminal screen model: rows × cols grid + cursor + a
    bounded number of scrollback lines.
-   Feed it from the reader (on the reader's goroutine or a copy handed
    off cheaply).
-   Expose a read-only "snapshot" of the visible tail.
-   `screen <id>` prints the latest snapshot on demand. If the bounded
    observer queue drops bytes, report that the snapshot may be incomplete.

The first implementation models common text, cursor, erase, scroll,
OSC, and alternate-screen controls. It is a small text screen model,
not a complete xterm emulator; color and advanced terminal modes are
not rendered.

### Invariants

-   Exactly one reader; the model never reads the PTY itself.
-   The model is bounded in memory (no unbounded grid growth).
-   A slow or expensive parse must not block forwarding to attach.

### Tests

-   [x] Given a scripted sequence, the visible tail matches expectation.
-   [x] Alternate-screen enter/leave is handled.
-   [x] Long scrollback stays bounded.
-   [x] A 2 MiB scripted PTY burst drains within 3 seconds with screen
    observation enabled.

### Done when

Pony can print the current visible screen of a running agent on demand.

------------------------------------------------------------------------

# Phase 3 — Detect Process Identity

### Goal

Know which program owns the foreground of a session, and whether it is a
known coding agent.

### Learn

-   `/proc/<pid>/stat`, `/proc/<pid>/cmdline`, `/proc/<pid>/comm`
-   The tty foreground process group: `tcgetpgrp` on the PTY slave
-   Why "foreground process" is more truthful than "child process"
-   Matching an executable basename to a known agent kind

### Build

-   A lookup: session → foreground pid → process name/argv.
-   A kind match against a short known list (`codex`, `opencode`, and a
    couple more), with an explicit `unknown` fallback.

### Invariants

-   Detection is read-only and never signals or reaps a process.
-   A missing/racing `/proc` entry yields `unknown`, never an error to
    the user.

### Done when

``` text
pony> status
a    pid=1234   runtime=running   agent=codex   state=idle
```

------------------------------------------------------------------------

# Phase 4 — Detection Profiles

### Goal

Classify `idle` / `working` / `blocked` for a known agent from the
screen tail, using **data**, not code.

### Learn

-   Pattern matching on the live bottom of the screen
-   Why the bottom, not the scrolled region
-   False positives and the cost of a wrong `blocked`
-   Rule ordering and a deliberate `unknown` fallback
-   Keeping profiles updatable without a rebuild

### Build

-   A profile per agent kind: an ordered set of rules over the screen
    snapshot. Same spirit as herdr's screen manifests, far smaller.
-   The evaluation pipeline:

``` text
foreground process  ─►  known kind?  ──no──►  state = unknown
                             │yes
                             ▼
                    load profile for kind
                             │
                             ▼
                 evaluate rules over screen tail
                             │
                             ▼
                   first match wins, else unknown
```

-   Example (illustrative, not final):

``` text
kind = "codex"
rules = [
  { state = "blocked", when_bottom_contains = ["approve", "allow", "y/n"] },
  { state = "working", when_bottom_matches  = ["esc to interrupt", "⏳", "Thinking"] },
  { state = "idle",    when_bottom_matches  = ["^>\\s*$"] },
]
```

### Invariants

-   Adding a supported agent means adding a profile, not touching the
    detector.
-   No rule means `unknown`; never guess a state to look smart.
-   Detection is cheap and runs on a bounded snapshot.

### Tests

-   [ ] A scripted "working" screen classifies as `working`.
-   [ ] A scripted approval screen classifies as `blocked`.
-   [ ] An unrecognized tail classifies as `unknown`.
-   [ ] A profile updates without recompiling the runtime.

### Done when

A running interactive `codex` reads `working` while generating and
`blocked`/`idle` when it is waiting on a human.

------------------------------------------------------------------------

# Phase 5 — The Prompt Primitive

### Goal

Drive an interactive agent the way a human does: type a prompt, submit
it, and optionally wait for the agent to settle.

### Learn

-   Text vs Enter as separate writes, and why a delay matters
-   Bracketed paste mode, so multi-line prompts do not fire early
-   Waiting on a **state**, not on a regex in scrollback
-   Timeouts that never wedge the REPL

### Build

-   `prompt <id> <text>` — write text, then submit Enter, honoring the
    terminal's live bracketed-paste mode. Reuse the existing `send`
    path rather than adding a second writer.
-   `prompt <id> --wait <state> <text>` — submit and block until the
    agent reaches a settled state (`idle`/`done`/`blocked`) or a
    timeout expires.

``` text
prompt  ──►  submit text+Enter  ──►  observe working  ──►  wait for settle
                                                │
                                          timeout ──► return, do not hang
```

### Invariants

-   Prompting a session whose runtime state is not running fails fast.
-   A wait always terminates: settled state, timeout, or agent exit.
-   The REPL/TUI stays responsive while a wait is in flight.

### Tests

-   [ ] Submit reaches a scripted reader as text + a single Enter.
-   [ ] A bracketed-paste prompt is submitted once.
-   [ ] `--wait idle` returns when the scripted agent goes idle.
-   [ ] Timeout returns without writing again and without hanging.

### Done when

``` text
pony> start a codex
pony> prompt a --wait idle "explain this repository"
a → working … a → idle
```

------------------------------------------------------------------------

# Phase 6 — Surface It

### Goal

Make state visible where people already look: the REPL and the TUI.

### Build

-   `status` gains the semantic state (and the detected kind).
-   The TUI shows per-agent state, and flags `blocked`/`done` as
    "needs attention".
-   Optional: a single terminal bell on entering `blocked`/`done`.

### Invariants

-   Purely additive to existing views; headless `run` tasks are
    unaffected and keep showing validation status.
-   No polling storm: state updates ride on output/events, not a tight
    loop.

### Done when

``` text
pony> status
reviewer   pid=…  runtime=running  agent=codex  state=blocked   ← needs you
builder    pid=…  runtime=running  agent=opencode  state=working
```

and the TUI highlights the blocked one.

------------------------------------------------------------------------

# What this deliberately is not

Out of scope, on purpose, because the daemon problem is what makes
herdr big:

``` text
background server / client split
persistent sessions across restarts
panes, tabs, workspaces, layout, mouse
SSH / remote machines
cross-platform detection
plugins / marketplace
agents driving Pony as a control surface
guaranteed-accurate state (we ship best-effort + unknown)
```

If any of these becomes a **real** problem, it gets its own plan and
its own phases — exactly like V1 did.

# Simplification vs herdr

| Concern            | herdr                              | this plan                          |
| ------------------ | ---------------------------------- | ---------------------------------- |
| Process model      | server owns PTYs, many clients     | one process, one terminal at a time |
| Agent launch       | canonical executable per `--kind`  | you give the command (already do)  |
| Driving an agent   | `agent prompt` / `send-keys`       | `prompt` / `send`                  |
| State authority    | hooks + screen manifests           | screen manifests only              |
| Layout             | panes/tabs/workspaces              | the existing TUI list              |
| Persistence        | survives restart, resumes sessions | none                               |

The point is not to match herdr. The point is to make Pony understand
the agents it already runs.

# Repository evolution

Do not create these until a phase needs them:

``` text
After the screen model

internal/
└── screen/        # terminal grid + bounded scrollback + snapshot

After detection

internal/
└── detect/        # process identity + profiles + classification
                   # profiles ship as data files next to the code

internal/agent/    # gains AgentState, stays authoritative for runtime
```

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

The goal is not to make Pony look like Herdr.

The goal is to let Pony understand the interactive agents it already
owns, without building a daemon to do it.
