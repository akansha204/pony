package event

import "time"

type Type string

const (
	TaskCreated       Type = "task_created"
	WorkspaceCreated  Type = "workspace_created"
	SessionStarted    Type = "session_started"
	RuntimeStarted    Type = "runtime_started"
	RuntimeExited     Type = "runtime_exited"
	RuntimeCrashed    Type = "runtime_crashed"
	ValidationStarted Type = "validation_started"
	ValidationPassed  Type = "validation_passed"
	ValidationFailed  Type = "validation_failed"
	TaskCompleted     Type = "task_completed"
	TaskFailed        Type = "task_failed"
)

type Event struct {
	Sequence   uint64
	Time       time.Time
	TaskID     string
	SessionID  string
	Generation uint64
	Type       Type
	Message    string
}
