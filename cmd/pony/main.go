package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/akansha204/pony/internal/agent"
	"github.com/akansha204/pony/internal/driver"
	"github.com/akansha204/pony/internal/shell_lexer"
)

func main() {
	mgr := agent.NewManager(driver.NewPTYDriver())

	defer func() {
		for _, snap := range mgr.Snapshots() {
			if err := mgr.Stop(snap.AgentID); err != nil {
				fmt.Println("cleanup:", err)
			}
		}
	}()

	lines := newLineReader(os.Stdin)
	for {
		fmt.Print("pony> ")
		line, readErr := lines.readLine()
		if readErr != nil && line == "" {
			if !errors.Is(readErr, io.EOF) {
				fmt.Fprintln(os.Stderr, "input:", readErr)
			}
			break
		}
		if strings.TrimSpace(line) == "" {
			continue
		}

		fields, err := shell_lexer.Fields(line)
		if err != nil {
			fmt.Println("malformed input:", err)
			continue
		}
		if len(fields) == 0 {
			continue
		}

		switch fields[0] {
		case "start":
			if len(fields) < 3 {
				fmt.Println("usage: start <id> <command> [args...]")
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

		case "read":
			if len(fields) != 2 {
				fmt.Println("usage: read <id>")
				continue
			}
			readAgent(mgr, agent.AgentID(fields[1]))

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
			for _, snap := range mgr.Snapshots() {
				fmt.Printf("%-10s pid=%-7d state=%s\n", snap.AgentID, snap.PID, snap.State)
			}

		case "quit", "exit":
			return

		default:
			fmt.Println("commands: start <id> <cmd> [args...] | attach <id> | send <id> <text> | read <id> | resize <id> <rows> <cols> | stop <id> | restart <id> | status | quit")
		}

		if errors.Is(readErr, io.EOF) {
			break
		}
	}
}

type lineReader struct {
	in *os.File
}

func newLineReader(in *os.File) *lineReader {
	return &lineReader{in: in}
}

func (r *lineReader) readLine() (string, error) {
	line := make([]byte, 0, 128)

	var b [1]byte
	for {
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

	if readErr != nil {
		fmt.Println("read:", readErr)
	}
}
