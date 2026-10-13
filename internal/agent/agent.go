package agent

import (
	"time"

	"github.com/akansha204/pony/internal/driver"
)

type AgentID string

type SessionID string

type RuntimeState string

// AgentState is a best-effort description of what an agent appears to be doing.
// It is independent from RuntimeState, which remains authoritative for process lifecycle.
type AgentState string

const (
	AgentStateUnknown AgentState = "unknown"
	AgentStateIdle    AgentState = "idle"
	AgentStateWorking AgentState = "working"
	AgentStateBlocked AgentState = "blocked"
	AgentStateDone    AgentState = "done"
)

const (
	StateIdle     RuntimeState = "idle"
	StateStarting RuntimeState = "starting"
	StateRunning  RuntimeState = "running"
	StateStopping RuntimeState = "stopping"
	StateCrashed  RuntimeState = "crashed"
	StateStopped  RuntimeState = "stopped"
)

type AgentSpec struct {
	ID      AgentID
	Command string
	Args    []string
	Cwd     string
	Env     []string
}

// cloneSpec copies the spec and its slices, so the stored spec never aliases
// the caller's backing arrays.
func cloneSpec(spec AgentSpec) AgentSpec {
	out := spec
	out.Args = append([]string(nil), spec.Args...)
	out.Env = append([]string(nil), spec.Env...)
	return out
}

type session struct {
	ID         SessionID
	Generation uint64
	State      RuntimeState
	AgentState AgentState
	StartedAt  time.Time
	ExitedAt   time.Time

	PID     int
	h       *driver.Handle
	done    chan struct{}
	stopReq bool
	out     *outputBuffer
}

type agent struct {
	spec      AgentSpec
	sessionID SessionID
	session   *session
}

type SessionSnapshot struct {
	AgentID    AgentID
	SessionID  SessionID
	Generation uint64
	State      RuntimeState
	AgentState AgentState
	PID        int
	StartedAt  time.Time
	ExitedAt   time.Time
}
