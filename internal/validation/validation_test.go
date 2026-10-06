package validation

import (
	"testing"
	"time"
)

func TestStepValidate(t *testing.T) {
	tests := []struct {
		name string
		step Step
		ok   bool
	}{
		{"valid", Step{Command: "go", Args: []string{"test", "./..."}, Timeout: time.Minute}, true},
		{"empty command", Step{Timeout: time.Minute}, false},
		{"blank command", Step{Command: "  ", Timeout: time.Minute}, false},
		{"zero timeout", Step{Command: "go"}, false},
		{"negative timeout", Step{Command: "go", Timeout: -time.Second}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.step.Validate()
			if tt.ok && err != nil {
				t.Fatalf("Validate: %v", err)
			}
			if !tt.ok && err == nil {
				t.Fatalf("Validate accepted %+v", tt.step)
			}
		})
	}
}

func TestResultSucceeded(t *testing.T) {
	tests := []struct {
		name   string
		result Result
		want   bool
	}{
		{"zero exit", Result{ExitCode: 0}, true},
		{"nonzero exit", Result{ExitCode: 1}, false},
		{"timeout", Result{ExitCode: 0, TimedOut: true}, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.result.Succeeded(); got != tt.want {
				t.Fatalf("Succeeded = %v, want %v", got, tt.want)
			}
		})
	}
}
