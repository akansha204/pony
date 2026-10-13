package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/akansha204/pony/internal/detect"
	"github.com/akansha204/pony/internal/driver"
	"github.com/akansha204/pony/internal/screen"
)

type Manager struct {
	mu          sync.Mutex
	agents      map[AgentID]*agent
	driver      driver.Driver
	profiles    *detect.ProfileStore
	nextSession uint64
}

const stopSettle = 2 * time.Second
const identityRefreshInterval = 500 * time.Millisecond
const stateRefreshInterval = 500 * time.Millisecond
const defaultPromptWaitTimeout = 2 * time.Minute

type foregroundGroupReader interface {
	ForegroundProcessGroup(*driver.Handle) (int, error)
}

type foregroundCheck struct {
	index     int
	owner     *agent
	session   *session
	handle    *driver.Handle
	sessionID int
	model     *screen.Model
	kind      AgentKind
	identity  bool
}

func NewManager(d driver.Driver) *Manager {
	return &Manager{agents: make(map[AgentID]*agent), driver: d, profiles: detect.NewProfileStore(defaultProfileDir())}
}

func defaultProfileDir() string {
	if dir := strings.TrimSpace(os.Getenv("PONY_AGENT_PROFILES")); dir != "" {
		return dir
	}
	starts := []string{}
	if cwd, err := os.Getwd(); err == nil {
		starts = append(starts, cwd)
	}
	if executable, err := os.Executable(); err == nil {
		starts = append(starts, filepath.Dir(executable))
	}
	for _, start := range starts {
		for dir := start; ; dir = filepath.Dir(dir) {
			candidate := filepath.Join(dir, "profiles", "agents")
			if info, err := os.Stat(candidate); err == nil && info.IsDir() {
				return candidate
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
		}
	}
	return filepath.Join("profiles", "agents")
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
		AgentState: AgentStateUnknown,
		agentKind:  AgentKindUnknown,
		screen:     screen.New(24, 80, 256),
		screenFeed: make(chan []byte, 64),
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
	go observeScreen(s)
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

// OutputDroppedBytes reports how many older terminal-output bytes were evicted.
func (m *Manager) OutputDroppedBytes(id AgentID) uint64 {
	m.mu.Lock()
	out := m.sessionOutput(id)
	m.mu.Unlock()
	if out == nil {
		return 0
	}
	return out.droppedBytes()
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
	return m.writeToSession(id, nil, data)
}

func (m *Manager) writeToSession(id AgentID, expected *session, data []byte) (int, error) {
	m.mu.Lock()
	a := m.agents[id]
	if a == nil || a.session == nil || a.session.State != StateRunning || (expected != nil && a.session != expected) {
		m.mu.Unlock()
		return 0, fmt.Errorf("agent %q has no running session", id)
	}
	h := a.session.h
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

// Prompt submits one line of input to a running session. If the terminal has
// bracketed paste enabled, the text is wrapped in its paste markers before a
// separate Enter is sent. A requested state wait observes Working before it
// accepts the requested settled state, so an old idle screen cannot satisfy it.
func (m *Manager) Prompt(ctx context.Context, id AgentID, text string, waitFor AgentState, timeout time.Duration) (AgentState, error) {
	if strings.TrimSpace(text) == "" {
		return AgentStateUnknown, fmt.Errorf("prompt text must not be empty")
	}
	if waitFor != "" && waitFor != AgentStateIdle && waitFor != AgentStateBlocked && waitFor != AgentStateDone {
		return AgentStateUnknown, fmt.Errorf("prompt wait state must be idle, blocked, or done")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	m.mu.Lock()
	a := m.agents[id]
	if a == nil || a.session == nil || a.session.State != StateRunning || a.session.h == nil {
		m.mu.Unlock()
		return AgentStateUnknown, fmt.Errorf("agent %q has no running session", id)
	}
	s := a.session
	screenModel := s.screen
	m.mu.Unlock()

	payload := []byte(text)
	if screenModel != nil && screenModel.Snapshot().BracketedPaste {
		payload = append([]byte("\x1b[200~"), payload...)
		payload = append(payload, []byte("\x1b[201~")...)
	}
	if n, err := m.writeToSession(id, s, payload); err != nil {
		return AgentStateUnknown, fmt.Errorf("send prompt to agent %q: %w", id, err)
	} else if n != len(payload) {
		return AgentStateUnknown, fmt.Errorf("send prompt to agent %q: short write (%d of %d bytes)", id, n, len(payload))
	}
	if n, err := m.writeToSession(id, s, []byte("\n")); err != nil {
		return AgentStateUnknown, fmt.Errorf("submit prompt to agent %q: %w", id, err)
	} else if n != 1 {
		return AgentStateUnknown, fmt.Errorf("submit prompt to agent %q: short write (%d of 1 byte)", id, n)
	}

	if waitFor == "" {
		return AgentStateUnknown, nil
	}
	m.mu.Lock()
	if a.session == s {
		s.stateCheckedAt = time.Time{}
	}
	m.mu.Unlock()

	if timeout <= 0 {
		timeout = defaultPromptWaitTimeout
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	sawWorking := false
	lastState := AgentStateUnknown
	for {
		m.mu.Lock()
		current := m.agents[id]
		sameSession := current != nil && current.session == s
		m.mu.Unlock()
		if !sameSession {
			return lastState, fmt.Errorf("agent %q session changed while waiting for state %q", id, waitFor)
		}
		for _, snapshot := range m.Snapshots() {
			if snapshot.AgentID != id {
				continue
			}
			lastState = snapshot.AgentState
			if snapshot.State != StateRunning {
				return lastState, fmt.Errorf("agent %q exited while waiting for state %q", id, waitFor)
			}
			if snapshot.AgentState == AgentStateWorking {
				sawWorking = true
			}
			if sawWorking && snapshot.AgentState == waitFor {
				return snapshot.AgentState, nil
			}
		}
		select {
		case <-waitCtx.Done():
			if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
				return lastState, fmt.Errorf("timed out waiting for agent %q to reach %q (last state %q)", id, waitFor, lastState)
			}
			return lastState, fmt.Errorf("waiting for agent %q to reach %q: %w", id, waitFor, waitCtx.Err())
		case <-ticker.C:
		}
	}
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
	var model *screen.Model
	if a := m.agents[id]; a != nil && a.session != nil {
		model = a.session.screen
	}
	m.mu.Unlock()

	if h == nil {
		return fmt.Errorf("agent %q has no running session", id)
	}

	if err := m.driver.Resize(h, rows, cols); err != nil {
		return fmt.Errorf("resize agent %q: %w", id, err)
	}
	if model != nil {
		model.Resize(int(rows), int(cols))
	}

	return nil
}

// ScreenSnapshot returns the latest asynchronously updated terminal screen.
// The PTY remains consumed only by the session pump; this is a passive copy.
func (m *Manager) ScreenSnapshot(id AgentID) (screen.Snapshot, error) {
	m.mu.Lock()
	a := m.agents[id]
	if a == nil || a.session == nil || a.session.screen == nil {
		m.mu.Unlock()
		return screen.Snapshot{}, fmt.Errorf("agent %q has no screen snapshot", id)
	}
	model := a.session.screen
	m.mu.Unlock()
	return model.Snapshot(), nil
}

func (m *Manager) Snapshots() []SessionSnapshot {
	m.mu.Lock()
	out := make([]SessionSnapshot, 0, len(m.agents))
	checks := make([]foregroundCheck, 0, len(m.agents))
	reader, canInspect := m.driver.(foregroundGroupReader)
	now := time.Now()
	for _, a := range m.agents {
		snapshot := snapshotOf(a)
		index := len(out)
		out = append(out, snapshot)
		s := a.session
		if s != nil && s.State == StateRunning && s.screen != nil && now.Sub(s.stateCheckedAt) >= stateRefreshInterval {
			s.stateCheckedAt = now
			check := foregroundCheck{index: index, owner: a, session: s, model: s.screen, kind: s.agentKind}
			if canInspect && s.h != nil && now.Sub(s.identityCheckedAt) >= identityRefreshInterval {
				s.identityCheckedAt = now
				check.handle = s.h
				check.sessionID = s.h.PID
				check.identity = true
			}
			checks = append(checks, check)
		}
	}
	m.mu.Unlock()

	for _, check := range checks {
		identity := foregroundIdentity{Kind: check.kind}
		if check.identity {
			identity.Kind = AgentKindUnknown
			if pgid, err := reader.ForegroundProcessGroup(check.handle); err == nil {
				if found, err := lookupForegroundIdentity("/proc", check.sessionID, pgid); err == nil {
					identity = found
				}
			}
		}
		state := detect.Unknown
		if identity.Kind != AgentKindUnknown {
			state = m.profiles.Classify(string(identity.Kind), check.model.Snapshot())
		}
		m.mu.Lock()
		if check.owner.session == check.session && check.session.State == StateRunning {
			if check.identity {
				check.session.foregroundPID = identity.PID
				check.session.foregroundProcess = identity.Process
				check.session.agentKind = identity.Kind
			}
			check.session.AgentState = AgentState(state)
			out[check.index] = snapshotOf(check.owner)
		}
		m.mu.Unlock()
	}
	return out
}

func snapshotOf(a *agent) SessionSnapshot {
	s := a.session
	if s == nil {
		return SessionSnapshot{
			AgentID:    a.spec.ID,
			SessionID:  a.sessionID,
			State:      StateIdle,
			AgentState: AgentStateUnknown,
			AgentKind:  AgentKindUnknown,
		}
	}

	return SessionSnapshot{
		AgentID:           a.spec.ID,
		SessionID:         s.ID,
		Generation:        s.Generation,
		State:             s.State,
		AgentState:        s.AgentState,
		AgentKind:         s.agentKind,
		ForegroundPID:     s.foregroundPID,
		ForegroundProcess: s.foregroundProcess,
		PID:               s.PID,
		StartedAt:         s.StartedAt,
		ExitedAt:          s.ExitedAt,
	}
}

func (m *Manager) pump(a *agent, s *session, h *driver.Handle) {
	defer s.out.close()
	defer close(s.screenFeed)

	buf := make([]byte, 4096)
	for {
		n, err := m.driver.Read(h, buf)
		if n > 0 {
			s.out.write(buf[:n])
			chunk := append([]byte(nil), buf[:n]...)
			select {
			case s.screenFeed <- chunk:
			default:
				s.screen.MarkDropped(uint64(len(chunk)))
			}
		}
		if err != nil {
			return
		}
	}
}

func observeScreen(s *session) {
	for chunk := range s.screenFeed {
		s.screen.Feed(chunk)
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

	close(s.done)
}

// isTerminationSignal reports whether a process died from the signals Stop
// delivers. A crash that merely overlaps with a stop request keeps its crash
// classification instead of being masked as a clean stop.
func isTerminationSignal(sig syscall.Signal) bool {
	return sig == syscall.SIGTERM || sig == syscall.SIGKILL
}
