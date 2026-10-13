package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime/debug"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/term"

	"github.com/akansha204/pony/internal/agent"
	"github.com/akansha204/pony/internal/shell_lexer"
	"github.com/akansha204/pony/internal/task"
)

func main() {
	if len(os.Args) == 2 && os.Args[1] == "--version" {
		fmt.Println("Pony " + currentVersion())
		return
	}
	if len(os.Args) == 2 && os.Args[1] == "--update" {
		if err := updatePonyCommand(); err != nil {
			fmt.Fprintln(os.Stderr, "update:", err)
			os.Exit(1)
		}
		return
	}

	root, err := defaultWorkspaceRoot()
	if err != nil {
		fmt.Fprintln(os.Stderr, "workspace root:", err)
		os.Exit(1)
	}
	a, err := newApp(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "start Pony:", err)
		os.Exit(1)
	}
	mgr := a.agents
	defer func() {
		if err := a.shutdown(); err != nil {
			fmt.Fprintln(os.Stderr, "cleanup:", err)
		}
	}()
	if len(os.Args) > 1 && (len(os.Args) != 2 || os.Args[1] != "--cli") {
		fmt.Fprintln(os.Stderr, "usage: pony [--cli | --version | --update]")
		return
	}
	if len(os.Args) == 1 && term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stdout.Fd())) {
		for {
			id, err := runTUI(a)
			if err != nil {
				fmt.Fprintln(os.Stderr, "tui:", err)
				break
			}
			if id == "" {
				break
			}
			if err := attach(mgr, id); err != nil {
				var signalErr *attachSignalError
				if errors.As(err, &signalErr) {
					break
				}
				fmt.Fprintln(os.Stderr, "attach:", err)
			}
		}
		return
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer signal.Stop(signals)

	lines := newLineReader(os.Stdin)
	for {
		fmt.Print("pony> ")
		line, readErr := lines.readLineWithSignals(signals)
		if readErr != nil && line == "" {
			if !errors.Is(readErr, io.EOF) {
				fmt.Fprintln(os.Stderr, "input:", readErr)
			}
			break
		}
		if strings.TrimSpace(line) == "" {
			continue
		}

		line = strings.TrimSuffix(line, "\r")
		fields, err := shell_lexer.Fields(line)
		if err != nil {
			fmt.Println("malformed input:", err)
			continue
		}
		if len(fields) == 0 {
			continue
		}

		switch fields[0] {
		case "help":
			printCLIHelp()

		case "run":
			opts, err := parseRun(fields[1:])
			if err != nil {
				fmt.Println("run:", err)
				continue
			}
			snapshot, err := a.run(opts)
			if err != nil {
				fmt.Println("run:", err)
				continue
			}
			fmt.Printf("task %s running in %s (attach %s)\n", snapshot.ID, snapshot.WorkspacePath, snapshot.ID)

		case "list":
			for _, snapshot := range a.tasks.List() {
				fmt.Printf("%-16s %-11s %s\n", snapshot.ID, snapshot.State, snapshot.WorkspacePath)
			}

		case "validate":
			if len(fields) != 2 {
				fmt.Println("usage: validate <task-id>")
				continue
			}
			snapshot, results, err := a.validate(task.TaskID(fields[1]))
			for i, result := range results {
				fmt.Printf("step %d: exit=%d timeout=%t duration=%s\n", i+1, result.ExitCode, result.TimedOut, result.Duration.Round(time.Millisecond))
				if result.Stdout != "" {
					fmt.Print(result.Stdout)
				}
				if result.Stderr != "" {
					fmt.Fprint(os.Stderr, result.Stderr)
				}
				if result.StdoutDroppedBytes > 0 {
					fmt.Fprintf(os.Stderr, "step %d: stdout truncated; %d bytes omitted\n", i+1, result.StdoutDroppedBytes)
				}
				if result.StderrDroppedBytes > 0 {
					fmt.Fprintf(os.Stderr, "step %d: stderr truncated; %d bytes omitted\n", i+1, result.StderrDroppedBytes)
				}
			}
			if err != nil {
				fmt.Println("validate:", err)
				continue
			}
			fmt.Printf("task %s %s\n", snapshot.ID, snapshot.State)

		case "clean":
			if len(fields) != 2 {
				fmt.Println("usage: clean <task-id>")
				continue
			}
			if err := a.clean(task.TaskID(fields[1])); err != nil {
				fmt.Println("clean:", err)
				continue
			}
			fmt.Printf("cleaned %s\n", fields[1])

		case "start":
			if len(fields) < 3 {
				fmt.Println("usage: start <id> <command> [args...]")
				continue
			}
			if _, exists := a.tasks.Get(task.TaskID(fields[1])); exists {
				fmt.Printf("start: %s is a task ID; use restart %s\n", fields[1], fields[1])
				continue
			}
			spec := agent.AgentSpec{ID: agent.AgentID(fields[1]), Command: fields[2], Args: fields[3:]}
			snap, err := mgr.Start(spec)
			if err != nil {
				fmt.Println("start:", err)
				continue
			}
			fmt.Printf("started pid=%d state=%s\n", snap.PID, snap.State)

		case "attach":
			if len(fields) != 2 {
				fmt.Println("usage: attach <id>")
				continue
			}
			id := agent.AgentID(fields[1])
			if err := attach(mgr, id); err != nil {
				var signalErr *attachSignalError
				if errors.As(err, &signalErr) {
					return
				}
				fmt.Println("attach:", err)
				continue
			}
			snap, ok := mgr.Get(id)
			if !ok {
				fmt.Printf("detached %s\n", id)
				continue
			}
			fmt.Printf("detached %s state=%s pid=%d\n", id, snap.State, snap.PID)

		case "stop":
			if len(fields) != 2 {
				fmt.Println("usage: stop <id>")
				continue
			}
			id := agent.AgentID(fields[1])
			if _, ok := a.tasks.Get(task.TaskID(id)); ok {
				snapshot, err := a.stop(task.TaskID(id))
				if err != nil {
					fmt.Println("stop:", err)
				} else {
					fmt.Printf("task %s %s\n", snapshot.ID, snapshot.State)
				}
				continue
			}
			if err := mgr.Stop(id); err != nil {
				fmt.Println("stop:", err)
				continue
			}
			fmt.Printf("stopped %s\n", id)

		case "restart":
			if len(fields) != 2 {
				fmt.Println("usage: restart <id>")
				continue
			}
			id := agent.AgentID(fields[1])
			if _, ok := a.tasks.Get(task.TaskID(id)); ok {
				snapshot, err := a.restart(task.TaskID(id))
				if err != nil {
					fmt.Println("restart:", err)
				} else {
					fmt.Printf("task %s %s\n", snapshot.ID, snapshot.State)
				}
				continue
			}
			if err := mgr.Restart(id); err != nil {
				fmt.Println("restart:", err)
				continue
			}
			snap, _ := mgr.Get(id)
			fmt.Printf("restarted pid=%d state=%s\n", snap.PID, snap.State)

		case "send":
			if len(fields) < 3 {
				fmt.Println("usage: send <id> <text>")
				continue
			}
			id := agent.AgentID(fields[1])
			text := strings.Join(fields[2:], " ")
			if _, err := mgr.Write(id, []byte(text+"\n")); err != nil {
				fmt.Println("send:", err)
				continue
			}
			fmt.Printf("sent %s: %s\n", id, text)

		case "prompt":
			opts, err := parsePrompt(fields[1:])
			if err != nil {
				fmt.Println("prompt:", err)
				continue
			}
			state, err := mgr.Prompt(context.Background(), opts.id, opts.text, opts.waitFor, 0)
			if err != nil {
				fmt.Println("prompt:", err)
				continue
			}
			if opts.waitFor == "" {
				fmt.Printf("prompt sent to %s\n", opts.id)
			} else {
				fmt.Printf("%s reached %s\n", opts.id, state)
			}

		case "read":
			if len(fields) != 2 {
				fmt.Println("usage: read <id>")
				continue
			}
			readAgent(mgr, agent.AgentID(fields[1]))

		case "screen":
			if len(fields) != 2 {
				fmt.Println("usage: screen <id>")
				continue
			}
			snap, err := mgr.ScreenSnapshot(agent.AgentID(fields[1]))
			if err != nil {
				fmt.Println("screen:", err)
				continue
			}
			fmt.Printf("screen %s (%dx%d)\n", fields[1], snap.Rows, snap.Columns)
			if !snap.Reliable {
				fmt.Printf("[screen may be incomplete: %d observer bytes were dropped]\n", snap.DroppedBytes)
			}
			for _, line := range snap.Visible {
				fmt.Println(line)
			}

		case "resize":
			if len(fields) != 4 {
				fmt.Println("usage: resize <id> <rows> <cols>")
				continue
			}

			rows, err1 := strconv.Atoi(fields[2])
			cols, err2 := strconv.Atoi(fields[3])

			if err1 != nil || err2 != nil {
				fmt.Println("resize: rows and cols must be numbers")
				continue
			}

			if rows < 1 || rows > 65535 || cols < 1 || cols > 65535 {
				fmt.Println("resize: rows and cols must be in the range 1..65535")
				continue
			}

			id := agent.AgentID(fields[1])

			if err := mgr.Resize(id, uint16(rows), uint16(cols)); err != nil {
				fmt.Println("resize:", err)
				continue
			}

			fmt.Printf("resized %s to %dx%d\n", id, rows, cols)

		case "status":
			for _, snap := range mgr.ViewSnapshots() {
				fmt.Printf("%-10s pid=%-7d runtime=%s kind=%s agent=%s attention=%s fgpid=%d process=%s\n", snap.AgentID, snap.PID, snap.State, snap.AgentKind, snap.AgentState, sessionAttention(snap.AgentState), snap.ForegroundPID, snap.ForegroundProcess)
			}

		case "quit", "exit":
			return

		default:
			fmt.Println("unknown command; type help for usage")
		}

		if errors.Is(readErr, io.EOF) {
			break
		}
	}
}

// version is set for local builds with Makefile ldflags. Versioned go install
// builds fall back to the module version embedded by the Go toolchain.
var version = "dev"

func currentVersion() string {
	if version != "dev" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return version
}

func printCLIHelp() {
	fmt.Println(`Pony CLI help

Task commands:
  run --id ID --goal TEXT --command CMD [--repo PATH] [--arg ARG] [--validate COMMAND]
      Start a task in its own Git worktree. --repo defaults to the current Git repo.
      Add --validate to run checks after a clean exit; without checks, task ends as completed.
  list                         List tasks and their states
  attach <id>                  Interact with a running task; Ctrl+\\ detaches
  stop <id>                    Stop a task without validating it
  restart <id>                 Restart a task in its existing worktree
  validate <id>                Run the task's configured validation commands
  clean <id>                   Release a stopped/completed task's clean worktree

Agent session commands:
  start <id> <cmd> [args...]   Start a named interactive program session
  send <id> <text>             Send text followed by Enter
  prompt <id> <text>           Send a prompt followed by Enter
  prompt <id> --wait STATE TEXT  Send a prompt and wait (idle, blocked, done; 2m timeout)
  read <id>                    Show buffered terminal output
  screen <id>                  Show the latest modeled terminal screen
  attach <id>                  Type directly into a running session; Ctrl+\\ detaches
  resize <id> <rows> <cols>    Set a session's terminal size
  status                       Show agent process IDs and runtime states
  stop <id> / restart <id>     Stop or restart a named agent session

General:
  help                         Show this guide
  quit                         Stop running agents and exit Pony

To update, exit this CLI and run pony --update in your terminal.
For the screen-based task list, run Pony without --cli in an interactive terminal.
In the TUI, press ? for help, : to enter a command, u to install an available update,
j/k or arrows to select, s to stop, R to restart, c to clean, r to refresh, and q to quit.`)
}

type lineReader struct {
	in *os.File
}

type promptOptions struct {
	id      agent.AgentID
	text    string
	waitFor agent.AgentState
}

func sessionAttention(state agent.AgentState) string {
	if state == agent.AgentStateBlocked || state == agent.AgentStateDone {
		return "needs-you"
	}
	return "no"
}

func parsePrompt(fields []string) (promptOptions, error) {
	if len(fields) < 2 {
		return promptOptions{}, fmt.Errorf("usage: prompt <id> [--wait idle|blocked|done] <text>")
	}
	opts := promptOptions{id: agent.AgentID(fields[0])}
	textStart := 1
	if fields[1] == "--wait" {
		if len(fields) < 4 {
			return promptOptions{}, fmt.Errorf("usage: prompt <id> --wait idle|blocked|done <text>")
		}
		opts.waitFor = agent.AgentState(fields[2])
		if opts.waitFor != agent.AgentStateIdle && opts.waitFor != agent.AgentStateBlocked && opts.waitFor != agent.AgentStateDone {
			return promptOptions{}, fmt.Errorf("wait state must be idle, blocked, or done")
		}
		textStart = 3
	} else if strings.HasPrefix(fields[1], "--") {
		return promptOptions{}, fmt.Errorf("unknown prompt option %q", fields[1])
	}
	opts.text = strings.Join(fields[textStart:], " ")
	if strings.TrimSpace(opts.text) == "" {
		return promptOptions{}, fmt.Errorf("prompt text must not be empty")
	}
	return opts, nil
}

func newLineReader(in *os.File) *lineReader {
	return &lineReader{in: in}
}

func (r *lineReader) readLine() (string, error) {
	return r.readLineWithSignals(nil)
}

func (r *lineReader) readLineWithSignals(signals <-chan os.Signal) (string, error) {
	line := make([]byte, 0, 128)

	var b [1]byte
	for {
		if signals != nil {
			select {
			case sig := <-signals:
				return "", fmt.Errorf("received %s", sig)
			default:
			}
			fds := []unix.PollFd{{Fd: int32(r.in.Fd()), Events: unix.POLLIN | unix.POLLHUP}}
			ready, err := unix.Poll(fds, 100)
			if errors.Is(err, syscall.EINTR) {
				continue
			}
			if err != nil {
				return "", err
			}
			if ready == 0 {
				continue
			}
		}
		n, err := r.in.Read(b[:])
		if n > 0 {
			if b[0] == '\n' {
				return string(line), nil
			}
			line = append(line, b[0])
		}

		if err != nil {
			if len(line) > 0 {
				return string(line), err
			}
			return "", err
		}
	}
}

func readAgent(mgr *agent.Manager, id agent.AgentID) {
	const chunkWait = 200 * time.Millisecond

	buf := make([]byte, 4096)
	var out strings.Builder
	deadline := time.Now().Add(2 * time.Second)
	var readErr error

	for time.Now().Before(deadline) {
		n, err := mgr.ReadTimeout(id, buf, chunkWait)
		if n > 0 {
			out.Write(buf[:n])
		}

		if err != nil {
			if !errors.Is(err, io.EOF) {
				readErr = err
			}
			break
		}

		if n == 0 {
			break
		}
	}

	if out.Len() > 0 {
		fmt.Print(out.String())
		if !strings.HasSuffix(out.String(), "\n") {
			fmt.Println()
		}
	}
	if dropped := mgr.OutputDroppedBytes(id); dropped > 0 {
		fmt.Fprintf(os.Stderr, "read: %d earlier output bytes were dropped from the recent-output buffer\n", dropped)
	}

	if readErr != nil {
		fmt.Println("read:", readErr)
	}
}
