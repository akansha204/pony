package agent

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/akansha204/pony/internal/driver"
)

type Manager struct {
	mu          sync.Mutex
	agents      map[AgentID]*agent
	driver      driver.Driver
	nextSession uint64
}

const stopSettle = 2 * time.Second

func NewManager(d driver.Driver) *Manager {
	return &Manager{agents: make(map[AgentID]*agent), driver: d}
}

func (m *Manager) nextSessionIDLocked() SessionID {
	m.nextSession++
	return SessionID(fmt.Sprintf("sess-%d", m.nextSession))
}

func validateSpec(spec AgentSpec) error {
	if strings.TrimSpace(string(spec.ID)) == "" {
		return fmt.Errorf("agent id must not be empty")
	}

	if strings.TrimSpace(spec.Command) == "" {
		return fmt.Errorf("agent %q command must not be empty", spec.ID)
	}

	return nil
}

func (m *Manager) Start(spec AgentSpec) (SessionSnapshot, error) {
	if err := validateSpec(spec); err != nil {
		return SessionSnapshot{}, err
	}

	spec = cloneSpec(spec)

	m.mu.Lock()
	defer m.mu.Unlock()

	a := m.agents[spec.ID]
	if a != nil {
		if s := a.session; s != nil &&
			(s.State == StateRunning ||
				s.State == StateStarting ||
				s.State == StateStopping) {
			return SessionSnapshot{}, fmt.Errorf(
				"agent %q is already %s",
				spec.ID,
				s.State,
			)
		}
	}

	sessionID := SessionID("")
	var gen uint64

	if a == nil {
		sessionID = m.nextSessionIDLocked()
	} else {
		sessionID = a.sessionID
		if a.session != nil {
			gen = a.session.Generation
		}
	}

	s := &session{
		ID:         sessionID,
		Generation: gen + 1,
		State:      StateStarting,
	}

	h, err := m.driver.Start(context.Background(), driver.Spec{
		Path: spec.Command,
		Args: spec.Args,
		Cwd:  spec.Cwd,
		Env:  spec.Env,
	})
	if err != nil {
		return SessionSnapshot{}, fmt.Errorf("start agent %q: %w", spec.ID, err)
	}

	s.PID = h.PID
	s.h = h
	s.out = newOutputBuffer(defaultOutputLimit)
	s.StartedAt = time.Now()
	s.done = make(chan struct{})
	s.State = StateRunning

	if a == nil {
		a = &agent{sessionID: sessionID}
		m.agents[spec.ID] = a
	}

	a.spec = spec
	a.session = s

	go m.pump(a, s, h)
	go m.monitor(a, s)

	return snapshotOf(a), nil
}

func (m *Manager) Stop(id AgentID) error {
	m.mu.Lock()
	a := m.agents[id]
	if a == nil {
		m.mu.Unlock()
		return fmt.Errorf("no agent %q", id)
	}
	s := a.session
	if s == nil {
		m.mu.Unlock()
		return nil
	}

	switch s.State {
	case StateStopped, StateCrashed:
		m.mu.Unlock()
		return nil

	case StateRunning:
		s.stopReq = true
		s.State = StateStopping

	case StateStopping:
		// Retry/continue the same stop instead of claiming success.

	case StateStarting:
		m.mu.Unlock()
		return fmt.Errorf("agent %q is still starting", id)

	default:
		m.mu.Unlock()
		return fmt.Errorf("agent %q is in unexpected state %q", id, s.State)
	}

	sd := s.done
	h := s.h
	m.mu.Unlock()

	if h == nil {
		return fmt.Errorf("stop agent %q: stopping session has no driver handle", id)
	}

	if err := m.driver.Stop(context.Background(), h); err != nil {
		return fmt.Errorf("stop agent %q: %w", id, err)
	}

	select {
	case <-sd:
		return nil
	case <-time.After(stopSettle):
		return fmt.Errorf("stop agent %q: timed out waiting for process to settle", id)
	}
}

func (m *Manager) Restart(id AgentID) error {
	m.mu.Lock()
	a := m.agents[id]
	if a == nil {
		m.mu.Unlock()
		return fmt.Errorf("no agent %q", id)
	}
	spec := cloneSpec(a.spec)
	m.mu.Unlock()

	if err := m.Stop(id); err != nil {
		return err
	}

	_, err := m.Start(spec)
	return err
}

func (m *Manager) Get(id AgentID) (SessionSnapshot, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()

	a := m.agents[id]
	if a == nil {
		return SessionSnapshot{}, false
	}
	return snapshotOf(a), true
}

func (m *Manager) sessionOutput(id AgentID) *outputBuffer {
	a := m.agents[id]
	if a == nil {
		return nil
	}
	s := a.session
	if s == nil || s.out == nil {
		return nil
	}
	return s.out
}

func (m *Manager) runningHandle(id AgentID) *driver.Handle {
	a := m.agents[id]
	if a == nil {
		return nil
	}
	s := a.session
	if s == nil || s.State != StateRunning {
		return nil
	}
	return s.h
}

func (m *Manager) Write(id AgentID, data []byte) (int, error) {
	m.mu.Lock()
	h := m.runningHandle(id)
	m.mu.Unlock()
	if h == nil {
		return 0, fmt.Errorf("agent %q has no running session", id)
	}
	n, err := m.driver.Write(h, data)
	if err != nil {
		return n, fmt.Errorf("write to agent %q: %w", id, err)
	}
	return n, nil
}

func (m *Manager) Read(id AgentID, p []byte) (int, error) {
	return m.read(id, p, 0)
}

func (m *Manager) ReadTimeout(id AgentID, p []byte, timeout time.Duration) (int, error) {
	return m.read(id, p, timeout)
}

func (m *Manager) read(id AgentID, p []byte, timeout time.Duration) (int, error) {
	m.mu.Lock()
	out := m.sessionOutput(id)
	m.mu.Unlock()
	if out == nil {
		return 0, fmt.Errorf("agent %q has no running session", id)
	}

	n, drained, err := out.wait(p, timeout)
	if err != nil {
		return n, fmt.Errorf("read from agent %q: %w", id, err)
	}
	if n == 0 && drained {
		return 0, io.EOF
	}
	return n, nil
}

func (m *Manager) Resize(id AgentID, rows, cols uint16) error {
	if rows == 0 || cols == 0 {
		return fmt.Errorf("resize agent %q: rows and cols must be > 0", id)
	}

	m.mu.Lock()
	h := m.runningHandle(id)
	m.mu.Unlock()

	if h == nil {
		return fmt.Errorf("agent %q has no running session", id)
	}

	if err := m.driver.Resize(h, rows, cols); err != nil {
		return fmt.Errorf("resize agent %q: %w", id, err)
	}

	return nil
}

func (m *Manager) Snapshots() []SessionSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]SessionSnapshot, 0, len(m.agents))
	for _, a := range m.agents {
		out = append(out, snapshotOf(a))
	}
	return out
}

func snapshotOf(a *agent) SessionSnapshot {
	s := a.session
	if s == nil {
		return SessionSnapshot{
			AgentID:   a.spec.ID,
			SessionID: a.sessionID,
			State:     StateIdle,
		}
	}

	return SessionSnapshot{
		AgentID:    a.spec.ID,
		SessionID:  s.ID,
		Generation: s.Generation,
		State:      s.State,
		PID:        s.PID,
		StartedAt:  s.StartedAt,
		ExitedAt:   s.ExitedAt,
	}
}

func (m *Manager) pump(a *agent, s *session, h *driver.Handle) {
	buf := make([]byte, 4096)
	for {
		n, err := m.driver.Read(h, buf)
		if n > 0 {
			s.out.write(buf[:n])
		}
		if err != nil {
			return
		}
	}
}

func (m *Manager) monitor(a *agent, s *session) {
	res := m.driver.Wait(s.h)
	m.finish(a, s, res)
}

func (m *Manager) finish(a *agent, s *session, res driver.ExitResult) {
	m.mu.Lock()
	defer m.mu.Unlock()

	if a.session != s {
		return
	}

	switch {
	case res.Err == nil:
		s.State = StateStopped
	case s.stopReq && isTerminationSignal(res.Signal):
		s.State = StateStopped
	default:
		s.State = StateCrashed
	}
	s.ExitedAt = time.Now()
	s.PID = 0
	s.h = nil

	if s.out != nil {
		s.out.close()
	}

	close(s.done)
}

// isTerminationSignal reports whether a process died from the signals Stop
// delivers. A crash that merely overlaps with a stop request keeps its crash
// classification instead of being masked as a clean stop.
func isTerminationSignal(sig syscall.Signal) bool {
	return sig == syscall.SIGTERM || sig == syscall.SIGKILL
}
