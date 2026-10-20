package driver

import (
	"context"
	"fmt"
	"os/exec"
	"syscall"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
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
	// Fd may switch Go's pollable file into blocking mode. Capture it once,
	// then use only raw nonblocking syscalls for the PTY for its full lifetime.
	masterFD := int(master.Fd())
	if err := unix.SetNonblock(masterFD, true); err != nil {
		master.Close()
		slave.Close()
		return nil, fmt.Errorf("set PTY master nonblocking: %w", err)
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
		PID:      cmd.Process.Pid,
		done:     make(chan struct{}),
		closing:  make(chan struct{}),
		proc:     cmd.Process,
		cmd:      cmd,
		master:   master,
		masterFD: masterFD,
	}, nil
}

func (d *PTYDriver) Write(h *Handle, data []byte) (int, error) {
	return d.writePTY(h, data, nil)
}

// WriteInterruptible sends terminal protocol responses and also stops when
// either its session reader or the PTY process closes.
func (d *PTYDriver) WriteInterruptible(h *Handle, data []byte, cancel <-chan struct{}) (int, error) {
	return d.writePTY(h, data, cancel)
}

func (d *PTYDriver) writePTY(h *Handle, data []byte, cancel <-chan struct{}) (int, error) {
	h.writeMu.Lock()
	defer h.writeMu.Unlock()

	h.stateMu.RLock()
	fd := h.masterFD
	if h.master == nil || fd < 0 {
		h.stateMu.RUnlock()
		return 0, fmt.Errorf("%w: process %d is not on a terminal", ErrClosed, h.PID)
	}
	defer h.stateMu.RUnlock()

	written := 0
	for written < len(data) {
		select {
		case <-cancel:
			return written, fmt.Errorf("%w: terminal response canceled", ErrClosed)
		case <-h.closing:
			return written, fmt.Errorf("%w: process %d output closed", ErrClosed, h.PID)
		default:
		}
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLOUT | unix.POLLERR | unix.POLLHUP}}
		n, err := unix.Poll(fds, 50)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return written, err
		}
		if n == 0 {
			continue
		}
		if fds[0].Revents&(unix.POLLERR|unix.POLLHUP|unix.POLLNVAL) != 0 {
			return written, fmt.Errorf("%w: process %d terminal is closed", ErrClosed, h.PID)
		}
		if fds[0].Revents&unix.POLLOUT == 0 {
			continue
		}
		count, err := unix.Write(fd, data[written:])
		if err == unix.EINTR || err == unix.EAGAIN || err == unix.EWOULDBLOCK {
			continue
		}
		if err != nil {
			return written, err
		}
		if count == 0 {
			continue
		}
		written += count
	}
	return written, nil
}

func (d *PTYDriver) Read(h *Handle, p []byte) (int, error) {
	h.readMu.Lock()
	defer h.readMu.Unlock()

	// readMu is separate from writeMu, so a read and a write stay full-duplex.
	h.stateMu.RLock()
	fd := h.masterFD
	h.stateMu.RUnlock()
	if fd < 0 {
		return 0, fmt.Errorf("%w: process %d is not on a terminal", ErrClosed, h.PID)
	}
	return readPTYUntilDone(h, fd, p)
}

func readPTYUntilDone(h *Handle, fd int, p []byte) (int, error) {
	return readPTYTimeout(h, fd, p, -1)
}

func readPTYTimeout(h *Handle, fd int, p []byte, timeout time.Duration) (int, error) {
	deadline := time.Time{}
	if timeout >= 0 {
		deadline = time.Now().Add(timeout)
	}
	for {
		select {
		case <-h.closing:
			return 0, fmt.Errorf("%w: process %d output closed", ErrClosed, h.PID)
		default:
		}
		pollMS := 50
		if timeout >= 0 {
			remaining := time.Until(deadline)
			if remaining <= 0 && timeout > 0 {
				return 0, nil
			}
			if timeout == 0 {
				pollMS = 0
			} else {
				pollMS = min(50, pollTimeoutMillis(remaining))
			}
		}
		fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN | unix.POLLHUP | unix.POLLERR}}
		n, err := unix.Poll(fds, pollMS)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return 0, err
		}
		if n == 0 {
			if timeout == 0 || (timeout > 0 && !time.Now().Before(deadline)) {
				return 0, nil
			}
			continue
		}
		if fds[0].Revents&unix.POLLNVAL != 0 {
			return 0, fmt.Errorf("%w: process %d output fd is invalid", ErrClosed, h.PID)
		}
		if fds[0].Revents&(unix.POLLIN|unix.POLLHUP|unix.POLLERR) != 0 {
			n, err := unix.Read(fd, p)
			if err == unix.EINTR || err == unix.EAGAIN || err == unix.EWOULDBLOCK {
				continue
			}
			return n, err
		}
	}
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

	if h.master == nil || h.masterFD < 0 {
		return fmt.Errorf("%w: process %d has no PTY", ErrClosed, h.PID)
	}

	return unix.IoctlSetWinsize(h.masterFD, unix.TIOCSWINSZ, &unix.Winsize{Row: rows, Col: cols})
}

// ForegroundProcessGroup reports the PTY's current foreground process group.
// The lock keeps Wait from closing and reusing the master fd during ioctl.
func (d *PTYDriver) ForegroundProcessGroup(h *Handle) (int, error) {
	h.stateMu.RLock()
	defer h.stateMu.RUnlock()
	if h.master == nil || h.masterFD < 0 {
		return 0, fmt.Errorf("%w: process %d has no PTY", ErrClosed, h.PID)
	}
	pgid, err := unix.IoctlGetInt(h.masterFD, unix.TIOCGPGRP)
	if err != nil {
		return 0, fmt.Errorf("read PTY foreground process group: %w", err)
	}
	if pgid <= 0 {
		return 0, fmt.Errorf("PTY returned invalid foreground process group %d", pgid)
	}
	return pgid, nil
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
