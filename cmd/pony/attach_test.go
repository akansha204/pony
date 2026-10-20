package main

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/akansha204/pony/internal/agent"
	"github.com/akansha204/pony/internal/driver"
	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

type attachPTY struct {
	master *os.File
	slave  *os.File
	sig    chan os.Signal
	done   chan error
	output chan []byte
	seen   []byte
}

type resizeFailureDriver struct {
	driver.Driver
	want error
}

func (d *resizeFailureDriver) Resize(*driver.Handle, uint16, uint16) error { return d.want }

func openAttachPTY(t *testing.T, rows, cols uint16) *attachPTY {
	t.Helper()

	master, slave, err := pty.Open()
	if err != nil {
		t.Fatalf("open pty: %v", err)
	}
	if err := pty.Setsize(master, &pty.Winsize{Rows: rows, Cols: cols}); err != nil {
		t.Fatalf("set pty size: %v", err)
	}
	t.Cleanup(func() {
		_ = slave.Close()
		_ = master.Close()
	})

	p := &attachPTY{
		master: master,
		slave:  slave,
		sig:    make(chan os.Signal, 2),
		done:   make(chan error, 1),
		output: make(chan []byte, 16),
	}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := master.Read(buf)
			if n > 0 {
				chunk := append([]byte(nil), buf[:n]...)
				p.output <- chunk
			}
			if err != nil {
				return
			}
		}
	}()
	return p
}

func (p *attachPTY) start(t *testing.T, mgr *agent.Manager, id agent.AgentID) {
	t.Helper()
	go func() { p.done <- attachWithIO(mgr, id, p.slave, p.slave, p.sig) }()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		tio, err := unix.IoctlGetTermios(int(p.slave.Fd()), unix.TCGETS)
		if err == nil && tio.Lflag&unix.ICANON == 0 && tio.Lflag&unix.ECHO == 0 {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("attach did not put the local terminal in raw mode")
}

func (p *attachPTY) write(t *testing.T, data []byte) {
	t.Helper()
	if _, err := p.master.Write(data); err != nil {
		t.Fatalf("write to local pty: %v", err)
	}
}

func (p *attachPTY) waitFor(t *testing.T, marker string, timeout time.Duration) string {
	t.Helper()
	deadline := time.After(timeout)
	for !strings.Contains(string(p.seen), marker) {
		select {
		case chunk := <-p.output:
			p.seen = append(p.seen, chunk...)
		case <-deadline:
			t.Fatalf("output %q did not contain %q", p.seen, marker)
		}
	}
	return string(p.seen)
}

func (p *attachPTY) assertNoOutput(t *testing.T, marker string, wait time.Duration) {
	t.Helper()
	deadline := time.After(wait)
	for {
		if strings.Contains(string(p.seen), marker) {
			t.Fatalf("output %q unexpectedly contained %q", p.seen, marker)
		}
		select {
		case chunk := <-p.output:
			p.seen = append(p.seen, chunk...)
		case <-deadline:
			return
		}
	}
}

func (p *attachPTY) detach(t *testing.T) {
	t.Helper()
	p.write(t, []byte{detachKey})
	select {
	case err := <-p.done:
		if err != nil {
			t.Fatalf("attach: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("attach did not return after Ctrl+\\")
	}
}

func termios(t *testing.T, f *os.File) *unix.Termios {
	t.Helper()
	tio, err := unix.IoctlGetTermios(int(f.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatalf("get termios: %v", err)
	}
	return tio
}

func startShell(t *testing.T, m *agent.Manager, id agent.AgentID, script string) agent.SessionSnapshot {
	t.Helper()
	snap, err := m.Start(agent.AgentSpec{ID: id, Command: "sh", Args: []string{"-c", script}})
	if err != nil {
		t.Fatalf("start %s: %v", id, err)
	}
	t.Cleanup(func() { _ = m.Stop(id) })
	return snap
}

func TestAttachPTYResizeBeforeInputAndCleanDetachRestore(t *testing.T) {
	m := agent.NewManager(driver.NewPTYDriver())
	p := openAttachPTY(t, 41, 113)
	start := startShell(t, m, "size", "read line; stty size; sleep 30")
	original := termios(t, p.slave)

	p.start(t, m, "size")
	deadline := time.Now().Add(3 * time.Second)
	for {
		frame, err := m.ScreenSnapshot("size")
		if err == nil && frame.Rows == 41 && frame.Columns == 113 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("screen model still has stale size: %+v, %v", frame, err)
		}
		time.Sleep(time.Millisecond)
	}
	if got := termios(t, p.slave); got.Lflag&unix.ICANON != 0 || got.Lflag&unix.ECHO != 0 || got.Lflag&unix.ISIG != 0 {
		t.Fatalf("local terminal is not raw during attach: %+v", got)
	}
	p.write(t, []byte("go\n"))
	p.waitFor(t, "41 113", 3*time.Second)
	p.detach(t)

	if got := termios(t, p.slave); !reflect.DeepEqual(got, original) {
		t.Errorf("termios after detach differs from original\noriginal: %+v\n     got: %+v", original, got)
	}
	final, ok := m.Get("size")
	if !ok || final.State != agent.StateRunning || final.Generation != start.Generation || final.PID != start.PID {
		t.Fatalf("session changed on detach: start=%+v final=%+v exists=%v", start, final, ok)
	}
}

func TestAttachResizeSignalKeepsPTYAndScreenDimensionsTogether(t *testing.T) {
	m := agent.NewManager(driver.NewPTYDriver())
	p := openAttachPTY(t, 27, 91)
	startShell(t, m, "resized", "stty raw -echo; dd bs=1 count=1 2>/dev/null >/dev/null; stty size; sleep 30")
	p.start(t, m, "resized")
	if err := pty.Setsize(p.master, &pty.Winsize{Rows: 43, Cols: 117}); err != nil {
		t.Fatal(err)
	}
	p.sig <- syscall.SIGWINCH
	deadline := time.Now().Add(3 * time.Second)
	for {
		frame, err := m.ScreenSnapshot("resized")
		if err == nil && frame.Rows == 43 && frame.Columns == 117 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("screen size after resize = %+v, %v", frame, err)
		}
		time.Sleep(time.Millisecond)
	}
	p.write(t, []byte("x"))
	p.waitFor(t, "43 117", 3*time.Second)
	p.detach(t)
}

func TestAttachMainScreenDetachRestoresCursorAndStyle(t *testing.T) {
	m := agent.NewManager(driver.NewPTYDriver())
	p := openAttachPTY(t, 24, 80)
	startShell(t, m, "main", "printf '\\033[31m\\033[3;8r\\033[?6h\\033[?25lMAIN_SCREEN'; sleep 30")
	p.start(t, m, "main")
	p.waitFor(t, "MAIN_SCREEN", 3*time.Second)
	p.detach(t)
	cleanup := resetAttachedTerminalModes + "\x1b[S\x1b[H\x1b[2J\x1b[H"
	output := p.waitFor(t, cleanup, 3*time.Second)
	if !strings.Contains(output, "\x1b[0m\x1b[r\x1b[?6l") || !strings.Contains(output, "\x1b[?25h") || !strings.HasSuffix(output, cleanup) {
		t.Fatalf("detach did not restore a clean CLI line: %q", output)
	}
}

func TestAttachResizeFailureRestoresTerminalAndReportsError(t *testing.T) {
	want := errors.New("PTY resize failed")
	m := agent.NewManager(&resizeFailureDriver{Driver: driver.NewPTYDriver(), want: want})
	p := openAttachPTY(t, 24, 80)
	startShell(t, m, "resize-fails", "sleep 30")
	original := termios(t, p.slave)
	err := attachWithIO(m, "resize-fails", p.slave, p.slave, p.sig)
	if !errors.Is(err, want) {
		t.Fatalf("attach error = %v, want resize failure", err)
	}
	if got := termios(t, p.slave); !reflect.DeepEqual(got, original) {
		t.Fatal("terminal flags remained raw after resize failure")
	}
}

func TestAttachOutputFailureRestoresTerminalAndReportsError(t *testing.T) {
	m := agent.NewManager(driver.NewPTYDriver())
	p := openAttachPTY(t, 24, 80)
	startShell(t, m, "output-fails", "sleep 30")
	original := termios(t, p.slave)
	out, err := os.CreateTemp(t.TempDir(), "closed-output")
	if err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	err = attachWithIO(m, "output-fails", p.slave, out, p.sig)
	if !errors.Is(err, os.ErrClosed) {
		t.Fatalf("attach error = %v, want closed output", err)
	}
	if got := termios(t, p.slave); !reflect.DeepEqual(got, original) {
		t.Fatal("terminal flags remained raw after output failure")
	}
}

func TestAttachFocusAndTypingReachAgent(t *testing.T) {
	m := agent.NewManager(driver.NewPTYDriver())
	p := openAttachPTY(t, 24, 80)
	startShell(t, m, "focus", "stty raw -echo; printf '\\033[?1004h\\033[?1000h\\033[?1006hFOCUS_READY'; dd bs=1 count=4 2>/dev/null | od -An -tx1; dd bs=1 count=3 2>/dev/null | od -An -tx1; sleep 30")
	deadline := time.Now().Add(3 * time.Second)
	for {
		frame, err := m.ScreenSnapshot("focus")
		if err == nil && frame.FocusReporting && strings.Contains(frame.Text, "FOCUS_READY") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("focus reporting was not observed before attach")
		}
		time.Sleep(time.Millisecond)
	}
	p.start(t, m, "focus")
	p.write(t, []byte("x"))
	p.waitFor(t, "1b 5b 49 78", 3*time.Second)
	p.detach(t)
	p.waitFor(t, "1b 5b 4f", 3*time.Second)
}

func TestAttachReplaysStyledAlternateScreen(t *testing.T) {
	m := agent.NewManager(driver.NewPTYDriver())
	p := openAttachPTY(t, 25, 83)
	startShell(t, m, "styled", "printf '\\033[?1049h\\033[2J\\033[31mRED\\033[0m \\033[1;34mBLUE\\033[0m\\033[?25l'; sleep 30")
	deadline := time.Now().Add(3 * time.Second)
	for {
		frame, err := m.ScreenSnapshot("styled")
		if err == nil && strings.Contains(frame.Text, "RED BLUE") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("styled frame was not observed")
		}
		time.Sleep(time.Millisecond)
	}
	p.start(t, m, "styled")
	p.waitFor(t, "BLUE", 3*time.Second)
	output := p.waitFor(t, "\x1b[?25l", 3*time.Second)
	if !strings.Contains(output, "\x1b[31mRED") || !strings.Contains(output, "\x1b[1;34mBLUE") || !strings.Contains(output, "\x1b[?25l") {
		t.Fatalf("styled alternate frame was not replayed: %q", output)
	}
	p.detach(t)
}

func TestAttachPTYFatalSignalRestoresTerminal(t *testing.T) {
	m := agent.NewManager(driver.NewPTYDriver())
	p := openAttachPTY(t, 24, 80)
	startShell(t, m, "waiting", "sleep 30")
	original := termios(t, p.slave)

	p.start(t, m, "waiting")
	p.sig <- syscall.SIGTERM
	select {
	case err := <-p.done:
		var signalErr *attachSignalError
		if !errors.As(err, &signalErr) || signalErr.signal != syscall.SIGTERM {
			t.Fatalf("attach error = %v, want SIGTERM attachSignalError", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("attach did not return after supplied fatal signal")
	}
	if got := termios(t, p.slave); !reflect.DeepEqual(got, original) {
		t.Errorf("termios after fatal signal differs from original\noriginal: %+v\n     got: %+v", original, got)
	}
}

func TestRawTerminalRestoresAfterPanic(t *testing.T) {
	p := openAttachPTY(t, 24, 80)
	original := termios(t, p.slave)

	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("expected panic")
			}
		}()
		_ = withRawTerminal(p.slave, func() error {
			panic("test panic")
		})
	}()

	if got := termios(t, p.slave); !reflect.DeepEqual(got, original) {
		t.Errorf("termios after panic differs from original\noriginal: %+v\n     got: %+v", original, got)
	}
}

func TestControlSignalHelper(t *testing.T) {
	if os.Getenv("GO_ATTACH_CONTROL_HELPER") != "1" {
		return
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT)
	defer signal.Stop(sig)
	fmt.Println("CONTROL_READY")
	<-sig
	fmt.Println("CTRL_C_OK")
	time.Sleep(30 * time.Second)
}

func TestAttachPTYForwardsControlKeys(t *testing.T) {
	tests := []struct {
		name   string
		script string
		key    byte
		marker string
	}{
		{"ctrl-c", "trap 'printf CTRL_C_OK' INT; while :; do sleep 10; done", 0x03, "CTRL_C_OK"},
		{"ctrl-d", "if ! IFS= read -r line; then printf CTRL_D_OK; fi; sleep 30", 0x04, "CTRL_D_OK"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := agent.NewManager(driver.NewPTYDriver())
			p := openAttachPTY(t, 24, 80)
			if tc.key == 0x03 {
				spec := agent.AgentSpec{
					ID:      "control",
					Command: os.Args[0],
					Args:    []string{"-test.run=^TestControlSignalHelper$"},
					Env:     []string{"GO_ATTACH_CONTROL_HELPER=1"},
				}
				if _, err := m.Start(spec); err != nil {
					t.Fatalf("start control helper: %v", err)
				}
				t.Cleanup(func() { _ = m.Stop(spec.ID) })
			} else {
				startShell(t, m, "control", tc.script)
			}

			p.start(t, m, "control")
			if tc.key == 0x03 {
				p.waitFor(t, "CONTROL_READY", 3*time.Second)
			}
			p.write(t, []byte{tc.key})
			p.waitFor(t, tc.marker, 3*time.Second)
			p.detach(t)

			snap, ok := m.Get("control")
			if !ok || snap.State != agent.StateRunning {
				t.Fatalf("agent state after %s = %s, exists=%v", tc.name, snap.State, ok)
			}
		})
	}
}

func TestAttachForwardsEscapeAfterShortDelay(t *testing.T) {
	m := agent.NewManager(driver.NewPTYDriver())
	p := openAttachPTY(t, 24, 80)
	startShell(t, m, "escape", "stty raw -echo; dd bs=1 count=1 2>/dev/null | od -An -tx1; sleep 30")
	p.start(t, m, "escape")
	p.write(t, []byte{0x1b})
	p.waitFor(t, "1b", time.Second)
	p.detach(t)
}

func TestAttachPTYReattachOutputAndAgentIsolation(t *testing.T) {
	m := agent.NewManager(driver.NewPTYDriver())
	p := openAttachPTY(t, 30, 100)
	first := startShell(t, m, "first", "while IFS= read -r line; do eval \"$line\"; done")
	second := startShell(t, m, "second", "while IFS= read -r line; do eval \"$line\"; done")

	p.start(t, m, "first")
	p.write(t, []byte("echo FIRST_VISIBLE\n"))
	p.waitFor(t, "FIRST_VISIBLE", 3*time.Second)
	if _, err := m.Write("second", []byte("echo SECOND_BACKLOG\n")); err != nil {
		t.Fatalf("write to second agent: %v", err)
	}
	p.assertNoOutput(t, "SECOND_BACKLOG", 300*time.Millisecond)
	p.detach(t)

	if _, err := m.Write("first", []byte("echo FIRST_BACKLOG\n")); err != nil {
		t.Fatalf("write to detached first agent: %v", err)
	}
	p.start(t, m, "first")
	p.waitFor(t, "FIRST_BACKLOG", 3*time.Second)
	p.assertNoOutput(t, "SECOND_BACKLOG", 300*time.Millisecond)
	p.detach(t)

	p.start(t, m, "second")
	p.waitFor(t, "SECOND_BACKLOG", 3*time.Second)
	p.detach(t)

	finalFirst, okFirst := m.Get("first")
	finalSecond, okSecond := m.Get("second")
	if !okFirst || finalFirst.State != agent.StateRunning || finalFirst.PID != first.PID || finalFirst.Generation != first.Generation {
		t.Errorf("first agent changed: start=%+v final=%+v exists=%v", first, finalFirst, okFirst)
	}
	if !okSecond || finalSecond.State != agent.StateRunning || finalSecond.PID != second.PID || finalSecond.Generation != second.Generation {
		t.Errorf("second agent changed: start=%+v final=%+v exists=%v", second, finalSecond, okSecond)
	}
}

func TestAttachRedrawsCurrentAlternateScreenAfterOutputBufferDrops(t *testing.T) {
	m := agent.NewManager(driver.NewPTYDriver())
	p := openAttachPTY(t, 30, 100)
	script := "printf '\\033[?1049h\\033[?1000h\\033[?1006h\\033[?2004h\\033[2J\\033[HATTACH_CURRENT_SCREEN\\n'; i=0; while [ \"$i\" -lt 12000 ]; do printf '\\033[?25l'; i=$((i + 1)); done; stty raw -echo; dd bs=1 count=1 2>/dev/null | od -An -tx1; dd bs=1 count=6 2>/dev/null | od -An -tx1; sleep 30"
	startShell(t, m, "screen", script)

	deadline := time.Now().Add(3 * time.Second)
	for m.OutputDroppedBytes("screen") == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if dropped := m.OutputDroppedBytes("screen"); dropped == 0 {
		t.Fatal("test agent output did not overflow the recent-output buffer")
	}

	p.start(t, m, "screen")
	output := p.waitFor(t, "ATTACH_CURRENT_SCREEN", 3*time.Second)
	if !strings.Contains(output, "\x1b[?1049h") {
		t.Fatal("attach did not enter the agent's alternate screen")
	}
	if !strings.Contains(output, "\x1b[?1000h") || !strings.Contains(output, "\x1b[?1006h") || !strings.Contains(output, "\x1b[?2004h") {
		t.Fatal("attach did not restore the agent's mouse and bracketed-paste modes")
	}
	if strings.Contains(output, "earlier output bytes were dropped") {
		t.Fatal("attach showed a dropped-output warning instead of redrawing the current screen")
	}
	p.write(t, []byte("x"))
	p.waitFor(t, "78", 3*time.Second)
	p.write(t, []byte("\x1b[<0;4;5M"))
	p.waitFor(t, "1b 5b 3c 30 3b 34", 3*time.Second)
	p.detach(t)
}

func TestAttachCleanupLeavesAlternateScreenEnteredAfterSnapshot(t *testing.T) {
	m := agent.NewManager(driver.NewPTYDriver())
	p := openAttachPTY(t, 30, 100)
	startShell(t, m, "late-alt", "read -r line; printf '\\033[?1049h\\033[?1003hLATE_ALT_STARTED\\n'; dd bs=1 count=1 2>/dev/null; sleep 30")
	p.start(t, m, "late-alt")
	p.write(t, []byte("go\n"))
	p.waitFor(t, "LATE_ALT_STARTED", 3*time.Second)
	p.detach(t)
	output := string(p.seen)
	if !strings.Contains(output, "\x1b[?1049l") {
		t.Fatal("attach cleanup did not leave alternate screen entered after initial snapshot")
	}
	for _, mode := range []string{"9", "1000", "1002", "1003", "1004", "1005", "1006", "1015", "1016", "2004"} {
		if !strings.Contains(output, "\x1b[?"+mode+"l") {
			t.Errorf("attach output did not reset terminal mode %s", mode)
		}
	}
}

func TestAttachMouseSessionDoesNotLeakReportsIntoFollowingKeyboardSession(t *testing.T) {
	m := agent.NewManager(driver.NewPTYDriver())
	p := openAttachPTY(t, 30, 100)
	startShell(t, m, "mouse-app", "stty raw -echo; printf '\\033[?1000h\\033[?1006hMOUSE_READY\\n'; dd bs=1 count=10 2>/dev/null | od -An -tx1; sleep 30")
	startShell(t, m, "keyboard-app", "stty raw -echo; dd bs=1 count=1 2>/dev/null | od -An -tx1; printf KEYBOARD_OK; sleep 30")

	p.start(t, m, "mouse-app")
	p.waitFor(t, "MOUSE_READY", 3*time.Second)
	p.write(t, []byte("\x1b[<0;4;5Mx"))
	p.waitFor(t, "1b 5b 3c 30 3b 34 3b 35 4d 78", 3*time.Second)
	p.detach(t)

	p.start(t, m, "keyboard-app")
	p.write(t, []byte("\x1b[<35;12;14Mx"))
	p.waitFor(t, "KEYBOARD_OK", 3*time.Second)
	if strings.Contains(string(p.seen), "1b 5b 3c 33 35") {
		t.Fatal("unsolicited mouse report reached the keyboard-only session")
	}
	p.detach(t)
}

func TestAttachForwardsRequestedSGRMouseWheel(t *testing.T) {
	m := agent.NewManager(driver.NewPTYDriver())
	p := openAttachPTY(t, 30, 100)
	startShell(t, m, "mouse-wheel", "stty raw -echo; printf '\\033[?1000h\\033[?1006hWHEEL_READY'; dd bs=1 count=10 2>/dev/null | od -An -tx1; sleep 30")
	p.start(t, m, "mouse-wheel")
	p.waitFor(t, "WHEEL_READY", 3*time.Second)
	p.write(t, []byte("\x1b[<64;4;5M"))
	p.waitFor(t, "1b 5b 3c 36 34 3b 34 3b 35 4d", 3*time.Second)
	p.detach(t)
}

func TestTermOutPreservesBytesWithoutAddingLineEndings(t *testing.T) {
	path := t.TempDir() + "/output"
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create output: %v", err)
	}
	defer f.Close()

	term := newTermOut(f)
	if err := term.write([]byte("tail")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := f.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if string(got) != "tail" {
		t.Fatalf("output = %q, want exact bytes %q", got, "tail")
	}
}
