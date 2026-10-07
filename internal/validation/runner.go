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
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

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
		result.Stdout = stdout.String()
		result.Stderr = stderr.String()
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
		return terminateGroup(cmd, done, started, &stdout, &stderr, result)
	case <-ctx.Done():
		result, err := terminateGroup(cmd, done, started, &stdout, &stderr, result)
		if err != nil {
			return result, errors.Join(ctx.Err(), err)
		}
		return result, ctx.Err()
	}
}

func terminateGroup(cmd *exec.Cmd, done <-chan error, started time.Time, stdout, stderr *bytes.Buffer, result Result) (Result, error) {
	killErr := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	waitErr := <-done
	result.Duration = time.Since(started)
	result.Stdout = stdout.String()
	result.Stderr = stderr.String()
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

func cloneStep(step Step) Step {
	step.Args = append([]string(nil), step.Args...)
	return step
}
