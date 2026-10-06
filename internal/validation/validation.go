package validation

import (
	"fmt"
	"strings"
	"time"
)

type Step struct {
	Command string
	Args    []string
	Timeout time.Duration
}

type Result struct {
	Step     Step
	ExitCode int
	Stdout   string
	Stderr   string
	Duration time.Duration
	TimedOut bool
}

func (s Step) Validate() error {
	if strings.TrimSpace(s.Command) == "" {
		return fmt.Errorf("validation command must not be empty")
	}
	if s.Timeout <= 0 {
		return fmt.Errorf("validation timeout must be greater than zero")
	}
	return nil
}

func (r Result) Succeeded() bool {
	return !r.TimedOut && r.ExitCode == 0
}
