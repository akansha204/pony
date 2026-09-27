package driver

import (
	"context"
	"fmt"
	"os/exec"
	"syscall"
	"time"

	"github.com/creack/pty"
)

// defaultWinsize is the terminal size applied to a freshly started PTY.
var defaultWinsize = &pty.Winsize{Rows: 24, Cols: 80}

type PTYDriver struct {
	*ProcessDriver
}

func NewPTYDriver() *PTYDriver {
	return &PTYDriver{ProcessDriver: NewProcessDriver()}
}

func (d *PTYDriver) Start(ctx context.Context, spec Spec) (*Handle, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// creack/pty.Open returns the master (we read/write here) and the slave
	// (the tty the child runs on).
	master, slave, err := pty.Open()
	if err != nil {
		return nil, err
	}
	if err := pty.Setsize(master, defaultWinsize); err != nil {
		master.Close()
		slave.Close()
		return nil, err
	}

	cmd := exec.CommandContext(ctx, spec.Path, spec.Args...)
	cmd.Dir = spec.Cwd
	cmd.Env = mergedEnv(spec.Env)
	cmd.Stdin = slave
	cmd.Stdout = slave
	cmd.Stderr = slave
	// Setsid: new session/group; Setctty: slave is its controlling terminal.
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid:  true,
		Setctty: true,
		Ctty:    0,
	}

	startErr := cmd.Start()
	slave.Close() // only the child keeps the slave open after this
	if startErr != nil {
		master.Close()
		return nil, startErr
	}

	return &Handle{
		PID:    cmd.Process.Pid,
		done:   make(chan struct{}),
		proc:   cmd.Process,
		cmd:    cmd,
		master: master,
	}, nil
}

func (d *PTYDriver) Write(h *Handle, data []byte) (int, error) {
	h.writeMu.Lock()
	defer h.writeMu.Unlock()

	// Held across the write so Wait cannot close the master mid-write.
	h.stateMu.RLock()
	defer h.stateMu.RUnlock()

	if h.master == nil {
		return 0, fmt.Errorf("%w: process %d is not on a terminal", ErrClosed, h.PID)
	}
	return h.master.Write(data)
}

func (d *PTYDriver) Read(h *Handle, p []byte) (int, error) {
	h.readMu.Lock()
	defer h.readMu.Unlock()

	// readMu is separate from writeMu, so a read and a write stay full-duplex.
	h.stateMu.RLock()
	defer h.stateMu.RUnlock()

	if h.master == nil {
		return 0, fmt.Errorf("%w: process %d is not on a terminal", ErrClosed, h.PID)
	}
	return h.master.Read(p)
}

// Resize publishes a new window size to the process (SIGWINCH). Held under
// the state lock so it never resizes a master Wait is closing.
func (d *PTYDriver) Resize(h *Handle, rows, cols uint16) error {
	if rows == 0 || cols == 0 {
		return fmt.Errorf("terminal rows and columns must be > 0")
	}

	// Held across Setsize so Wait cannot close the master mid-call.
	h.stateMu.RLock()
	defer h.stateMu.RUnlock()

	if h.master == nil {
		return fmt.Errorf("%w: process %d has no PTY", ErrClosed, h.PID)
	}

	return pty.Setsize(h.master, &pty.Winsize{
		Rows: rows,
		Cols: cols,
	})
}

// Stop kills every process group in the session, not just the leader's, so
// job-control background jobs die with the shell that owned them.
func (d *PTYDriver) Stop(ctx context.Context, h *Handle) error {
	if ctx == nil {
		ctx = context.Background()
	}

	grace := d.Grace
	if grace <= 0 {
		grace = defaultGrace
	}

	if err := signalSessionProcessGroups(h.PID, syscall.SIGTERM); err != nil {
		return fmt.Errorf("signal PTY session %d: %w", h.PID, err)
	}

	empty, err := waitSessionEmpty(
		ctx.Done(),
		h.PID,
		grace,
	)
	if err != nil {
		return fmt.Errorf(
			"wait for PTY session %d after SIGTERM: %w",
			h.PID,
			err,
		)
	}

	if err := ctx.Err(); err != nil {
		return err
	}

	if !empty {
		if err := signalSessionProcessGroups(
			h.PID,
			syscall.SIGKILL,
		); err != nil {
			return fmt.Errorf(
				"kill PTY session %d: %w",
				h.PID,
				err,
			)
		}

		empty, err = waitSessionEmpty(
			ctx.Done(),
			h.PID,
			killTimeout,
		)
		if err != nil {
			return fmt.Errorf(
				"wait for PTY session %d after SIGKILL: %w",
				h.PID,
				err,
			)
		}

		if err := ctx.Err(); err != nil {
			return err
		}

		if !empty {
			pgids, scanErr := sessionProcessGroups(h.PID)
			if scanErr != nil {
				return fmt.Errorf(
					"PTY session %d did not terminate after SIGKILL",
					h.PID,
				)
			}

			return fmt.Errorf(
				"PTY session %d still has live process groups after SIGKILL: %v",
				h.PID,
				pgids,
			)
		}
	}

	select {
	case <-h.done:
		return nil

	case <-ctx.Done():
		return ctx.Err()

	case <-time.After(killTimeout):
		return fmt.Errorf(
			"PTY session %d is empty but its leader was not reaped",
			h.PID,
		)
	}
}
