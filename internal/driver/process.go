package driver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	defaultGrace = 3 * time.Second
	killTimeout  = 2 * time.Second
)

type Spec struct {
	Path string
	Args []string
	Cwd  string
	Env  []string
}

// ExitResult describes how a process actually died, independent of why the
// caller might have wanted it to die. For a normal exit Err is nil, ExitCode
// is the status and Signal is -1; for a signal death Err is non-nil, Signal is
// the fatal signal and ExitCode is -1.
type ExitResult struct {
	Err      error
	ExitCode int
	Signal   syscall.Signal
}

var (
	ErrClosed      = errors.New("driver handle is closed")
	ErrUnsupported = errors.New("operation unsupported by driver")
)

// mergedEnv keeps the host environment and layers the overrides on top, so
// a non-nil spec.Env augments rather than replaces PATH, HOME, and friends.
func mergedEnv(overrides []string) []string {
	if len(overrides) == 0 {
		return nil
	}

	env := append([]string(nil), os.Environ()...)
	env = append(env, overrides...)
	return env
}

type Handle struct {
	PID int

	// stateMu guards the io fields against Wait's teardown; readMu and
	// writeMu keep one read running alongside one write (full-duplex).
	stateMu sync.RWMutex
	readMu  sync.Mutex
	writeMu sync.Mutex

	done     chan struct{} //closed by Wait when the process dies
	closing  chan struct{} // closed by Wait before it tears down output
	proc     *os.Process
	cmd      *exec.Cmd
	stdin    io.WriteCloser // optional; Write feeds the process here
	stdout   io.ReadCloser  // optional; Read drains the process here
	master   *os.File       // optional; the PTY master when on a terminal
	masterFD int            // cached nonblocking fd, valid only while master != nil
}

type Driver interface {
	Start(ctx context.Context, spec Spec) (*Handle, error)
	Write(h *Handle, data []byte) (int, error)
	Read(h *Handle, p []byte) (int, error)
	ReadTimeout(h *Handle, p []byte, timeout time.Duration) (int, error)
	Resize(h *Handle, rows, cols uint16) error
	Stop(ctx context.Context, h *Handle) error
	Wait(h *Handle) ExitResult
}

// InterruptibleWriter is implemented by drivers that can cancel protocol
// replies when a session ends. It is intended for small terminal responses
// sent from a background writer, not for regular user input.
type InterruptibleWriter interface {
	WriteInterruptible(h *Handle, data []byte, cancel <-chan struct{}) (int, error)
}

type ProcessDriver struct {
	Grace time.Duration
}

func NewProcessDriver() *ProcessDriver {
	return &ProcessDriver{Grace: defaultGrace}
}

func (d *ProcessDriver) Start(ctx context.Context, spec Spec) (*Handle, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	cmd := exec.CommandContext(ctx, spec.Path, spec.Args...)
	cmd.Dir = spec.Cwd
	cmd.Env = mergedEnv(spec.Env)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, stdoutWriter, err := os.Pipe()
	if err != nil {
		stdin.Close()
		return nil, err
	}
	cmd.Stdout = stdoutWriter
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stdoutWriter.Close()
		return nil, err
	}
	_ = stdoutWriter.Close()

	return &Handle{
		PID:     cmd.Process.Pid,
		done:    make(chan struct{}),
		closing: make(chan struct{}),
		proc:    cmd.Process,
		cmd:     cmd,
		stdin:   stdin,
		stdout:  stdout,
	}, nil
}

func (d *ProcessDriver) Write(h *Handle, data []byte) (int, error) {
	h.writeMu.Lock()
	defer h.writeMu.Unlock()

	// Held across the write so Wait cannot close the file mid-write.
	h.stateMu.RLock()
	defer h.stateMu.RUnlock()

	if h.stdin == nil {
		return 0, fmt.Errorf("%w: process %d has no stdin", ErrClosed, h.PID)
	}
	return h.stdin.Write(data)
}

func (d *ProcessDriver) Read(h *Handle, p []byte) (int, error) {
	h.readMu.Lock()
	defer h.readMu.Unlock()

	h.stateMu.RLock()
	f := h.stdout
	h.stateMu.RUnlock()
	if f == nil {
		return 0, fmt.Errorf("%w: process %d has no stdout", ErrClosed, h.PID)
	}
	file, ok := f.(*os.File)
	if !ok {
		return 0, fmt.Errorf("%w: process %d stdout is not pollable", ErrUnsupported, h.PID)
	}
	select {
	case <-h.closing:
		return 0, fmt.Errorf("%w: process %d output closed", ErrClosed, h.PID)
	default:
	}

	// Closing an fd from another goroutine does not reliably interrupt a
	// blocking read on Linux. Poll briefly so Wait can signal teardown through
	// done, even when a descendant still holds the pipe's write end open.
	return readUntilDone(h, file, p)
}

func readUntilDone(h *Handle, f *os.File, p []byte) (int, error) {
	for {
		select {
		case <-h.closing:
			return 0, fmt.Errorf("%w: process %d output closed", ErrClosed, h.PID)
		default:
		}

		fds := []unix.PollFd{{Fd: int32(f.Fd()), Events: unix.POLLIN | unix.POLLHUP | unix.POLLERR}}
		n, err := unix.Poll(fds, 50)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return 0, err
		}
		if n == 0 {
			continue
		}
		if fds[0].Revents&unix.POLLNVAL != 0 {
			return 0, fmt.Errorf("%w: process %d output fd is invalid", ErrClosed, h.PID)
		}
		if fds[0].Revents&(unix.POLLIN|unix.POLLHUP|unix.POLLERR) != 0 {
			return f.Read(p)
		}
	}
}

func (d *ProcessDriver) ReadTimeout(h *Handle, p []byte, timeout time.Duration) (int, error) {
	if timeout < 0 {
		return 0, fmt.Errorf("timeout must be >= 0")
	}

	h.readMu.Lock()
	defer h.readMu.Unlock()

	h.stateMu.RLock()
	if h.master != nil {
		fd := h.masterFD
		h.stateMu.RUnlock()
		if fd < 0 {
			return 0, fmt.Errorf("%w: process %d has no PTY", ErrClosed, h.PID)
		}
		return readPTYTimeout(h, fd, p, timeout)
	}
	f, _ := h.stdout.(*os.File)
	h.stateMu.RUnlock()
	if f == nil {
		return 0, fmt.Errorf("%w: process %d has no pollable output", ErrClosed, h.PID)
	}

	// poll(2) watches a single fd by number, so there is no FD_SETSIZE
	// (1024) ceiling like select(2). POLLHUP|POLLERR are watched so a dying
	// process ends the wait and the read returns its final EOF/EIO.
	deadline := time.Now().Add(timeout)
	for {
		select {
		case <-h.closing:
			return 0, fmt.Errorf("%w: process %d output closed", ErrClosed, h.PID)
		default:
		}

		remaining := time.Until(deadline)
		if timeout == 0 {
			remaining = 0
		} else if remaining <= 0 {
			return 0, nil
		}

		fds := []unix.PollFd{{
			Fd:     int32(f.Fd()),
			Events: unix.POLLIN | unix.POLLHUP | unix.POLLERR,
		}}
		n, err := unix.Poll(fds, pollTimeoutMillis(remaining))
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return 0, err
		}
		if n == 0 {
			return 0, nil
		}
		if revents := fds[0].Revents; revents&unix.POLLNVAL != 0 {
			return 0, fmt.Errorf("%w: process %d output fd is invalid", ErrClosed, h.PID)
		}
		if revents := fds[0].Revents; revents&(unix.POLLIN|unix.POLLHUP|unix.POLLERR) != 0 {
			return f.Read(p)
		}
	}
}

func pollTimeoutMillis(d time.Duration) int {
	if d <= 0 {
		return 0
	}
	ms := (d + time.Millisecond - 1) / time.Millisecond
	const maxPollMillis = int64(1<<31 - 1)
	if int64(ms) > maxPollMillis {
		return int(maxPollMillis)
	}
	return int(ms)
}

// Resize is unsupported without a terminal; PTYDriver overrides it.
func (d *ProcessDriver) Resize(_ *Handle, _, _ uint16) error {
	return ErrUnsupported
}

func classifyExit(err error) ExitResult {
	res := ExitResult{
		Err:      err,
		ExitCode: 0,
		Signal:   -1,
	}

	if err == nil {
		return res
	}

	res.ExitCode = -1

	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) {
		return res
	}

	ws, ok := exitErr.Sys().(syscall.WaitStatus)
	if !ok {
		return res
	}

	if ws.Exited() {
		res.ExitCode = ws.ExitStatus()
	}

	if ws.Signaled() {
		res.Signal = ws.Signal()
	}

	return res
}

func (d *ProcessDriver) Wait(h *Handle) ExitResult {
	h.stateMu.RLock()
	cmd := h.cmd
	h.stateMu.RUnlock()

	if cmd == nil {
		return ExitResult{
			Err:      ErrClosed,
			ExitCode: -1,
			Signal:   -1,
		}
	}

	err := cmd.Wait()
	res := classifyExit(err)
	close(h.closing)

	// A blocking read polls h.closing and exits promptly. Wait for it before
	// closing the descriptor so the fd cannot be reused under an active poll.
	h.readMu.Lock()
	defer h.readMu.Unlock()

	h.stateMu.Lock()
	stdin := h.stdin
	stdout := h.stdout
	master := h.master

	h.proc = nil
	h.cmd = nil
	h.stdin = nil
	h.stdout = nil
	h.master = nil
	h.masterFD = -1
	h.stateMu.Unlock()

	if stdin != nil {
		_ = stdin.Close()
	}
	if stdout != nil {
		_ = stdout.Close()
	}
	if master != nil {
		_ = master.Close()
	}

	close(h.done)

	return res
}

// Stop escalates SIGTERM to SIGKILL against the whole process group and only
// succeeds after every live member has exited and the leader has been reaped.
func (d *ProcessDriver) Stop(ctx context.Context, h *Handle) error {
	if ctx == nil {
		ctx = context.Background()
	}

	grace := d.Grace
	if grace <= 0 {
		grace = defaultGrace
	}

	pgid := -h.PID
	if err := syscall.Kill(pgid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("signal process group: %w", err)
	}

	empty, err := waitProcessGroupEmpty(ctx.Done(), h.PID, grace)
	if err != nil {
		return fmt.Errorf("wait for process group after SIGTERM: %w", err)
	}
	if !empty {
		if err := syscall.Kill(pgid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("kill process group: %w", err)
		}
		empty, err = waitProcessGroupEmpty(nil, h.PID, killTimeout)
		if err != nil {
			return fmt.Errorf("wait for process group after SIGKILL: %w", err)
		}
		if !empty {
			return fmt.Errorf("process group %d did not terminate after SIGKILL", h.PID)
		}
	}

	select {
	case <-h.done:
		return nil
	case <-time.After(killTimeout):
		return fmt.Errorf("process %d was not reaped after its process group exited", h.PID)
	}
}
