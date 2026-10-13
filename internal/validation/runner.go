package validation

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

type Runner struct{}

const (
	validationWaitDelay      = 2 * time.Second
	terminationWait          = 3 * time.Second
	maxValidationStreamBytes = 1 << 20
)

type boundedCapture struct {
	buf     bytes.Buffer
	limit   int
	dropped int64
}

func (c *boundedCapture) Write(p []byte) (int, error) {
	room := c.limit - c.buf.Len()
	if room > len(p) {
		room = len(p)
	}
	if room > 0 {
		_, _ = c.buf.Write(p[:room])
	}
	c.dropped += int64(len(p) - room)
	return len(p), nil
}

func NewRunner() *Runner {
	return &Runner{}
}

func (r *Runner) Run(cwd string, step Step) (Result, error) {
	return r.RunContext(context.Background(), cwd, step)
}

func (r *Runner) RunContext(ctx context.Context, cwd string, step Step) (Result, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	result := Result{Step: cloneStep(step), ExitCode: -1}
	if err := step.Validate(); err != nil {
		return result, err
	}
	if strings.TrimSpace(cwd) == "" {
		return result, fmt.Errorf("validation working directory must not be empty")
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}

	cmd := exec.Command(step.Command, step.Args...)
	cmd.Dir = cwd
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// A descendant may inherit stdout/stderr and keep os/exec's copy
	// goroutines open after the command exits. Bound that wait as well.
	cmd.WaitDelay = validationWaitDelay
	stdout := &boundedCapture{limit: maxValidationStreamBytes}
	stderr := &boundedCapture{limit: maxValidationStreamBytes}
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	started := time.Now()
	if err := cmd.Start(); err != nil {
		result.Duration = time.Since(started)
		return result, fmt.Errorf("start validation command %q: %w", step.Command, err)
	}

	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()

	timer := time.NewTimer(step.Timeout)
	defer timer.Stop()
	select {
	case err := <-done:
		result.Duration = time.Since(started)
		setCapturedOutput(&result, stdout, stderr)
		if err == nil {
			result.ExitCode = 0
			return result, nil
		}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			result.ExitCode = exitErr.ExitCode()
			return result, nil
		}
		return result, fmt.Errorf("wait for validation command %q: %w", step.Command, err)

	case <-timer.C:
		result.TimedOut = true
		return terminateGroup(cmd, done, started, stdout, stderr, result)
	case <-ctx.Done():
		result, err := terminateGroup(cmd, done, started, stdout, stderr, result)
		if err != nil {
			return result, errors.Join(ctx.Err(), err)
		}
		return result, ctx.Err()
	}
}

func terminateGroup(cmd *exec.Cmd, done <-chan error, started time.Time, stdout, stderr *boundedCapture, result Result) (Result, error) {
	killErr := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	var waitErr error
	select {
	case waitErr = <-done:
		result.Duration = time.Since(started)
		setCapturedOutput(&result, stdout, stderr)
	case <-time.After(terminationWait):
		// Do not read stdout/stderr here: cmd.Wait may still have copy
		// goroutines writing to them.
		result.Duration = time.Since(started)
		return result, fmt.Errorf("validation command %q did not settle after SIGKILL within %s", result.Step.Command, terminationWait)
	}
	if killErr != nil && !errors.Is(killErr, syscall.ESRCH) {
		return result, fmt.Errorf("kill validation process group: %w", killErr)
	}
	if waitErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(waitErr, &exitErr) {
			return result, fmt.Errorf("wait for terminated validation command %q: %w", result.Step.Command, waitErr)
		}
	}
	return result, nil
}

func setCapturedOutput(result *Result, stdout, stderr *boundedCapture) {
	result.Stdout = stdout.buf.String()
	result.Stderr = stderr.buf.String()
	result.StdoutDroppedBytes = stdout.dropped
	result.StderrDroppedBytes = stderr.dropped
}

func cloneStep(step Step) Step {
	step.Args = append([]string(nil), step.Args...)
	return step
}
