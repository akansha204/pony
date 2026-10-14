package main

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"

	"github.com/akansha204/pony/internal/agent"
	"github.com/akansha204/pony/internal/shell_lexer"
	"github.com/akansha204/pony/internal/task"
	"github.com/akansha204/pony/internal/validation"
)

type refreshMsg struct{}

type validationMsg struct {
	snapshot task.Snapshot
	results  []validation.Result
	err      error
}

type taskActionMsg struct {
	action, id string
	err        error
}

type agentReadMsg struct {
	id         agent.AgentID
	out        string
	dropped    uint64
	agentState agent.AgentState
	err        error
}

type agentPromptMsg struct {
	id      agent.AgentID
	state   agent.AgentState
	waitFor agent.AgentState
	err     error
}

type updateCheckMsg struct {
	version   string
	available bool
	err       error
}

type updateInstallMsg struct {
	version string
	err     error
}

type tuiModel struct {
	list             func() []task.Snapshot
	run              func(runOptions) error
	stop             func(task.TaskID) error
	restart          func(task.TaskID) error
	clean            func(task.TaskID) error
	validate         func(task.TaskID) (task.Snapshot, []validation.Result, error)
	sessions         func() []agent.SessionSnapshot
	viewSessions     func() []agent.SessionSnapshot
	agentsAPI        *agent.Manager
	tasks            []task.Snapshot
	agents           []agent.SessionSnapshot
	selected         int
	width            int
	height           int
	editing          bool
	command          string
	message          string
	showHelp         bool
	showSessions     bool
	showAgentOutput  bool
	attachID         agent.AgentID
	agentOutputID    agent.AgentID
	agentOutput      string
	agentOutputState agent.AgentState
	outputScroll     int
	updateCheckDone  bool
	updateReady      bool
	updateInstalling bool
	updateVer        string
	updateNotice     string
	updateFailed     bool
	actionPending    bool
}

func newTUIModel(a *app) tuiModel {
	return tuiModel{
		list: a.tasks.List,
		run: func(opts runOptions) error {
			_, err := a.run(opts)
			return err
		},
		stop: func(id task.TaskID) error {
			_, err := a.stop(id)
			return err
		},
		restart: func(id task.TaskID) error {
			_, err := a.restart(id)
			return err
		},
		clean:        a.clean,
		validate:     a.validate,
		sessions:     a.agents.Snapshots,
		viewSessions: a.agents.ViewSnapshots,
		agentsAPI:    a.agents,
		tasks:        a.tasks.List(),
		agents:       a.agents.Snapshots(),
	}
}

func (m tuiModel) Init() tea.Cmd {
	return tea.Batch(scheduleRefresh(), checkForUpdate())
}

func checkForUpdate() tea.Cmd {
	return func() tea.Msg {
		version, available, err := updateCheckMessage()
		return updateCheckMsg{version: version, available: available, err: err}
	}
}

func scheduleRefresh() tea.Cmd {
	return tea.Tick(250*time.Millisecond, func(time.Time) tea.Msg { return refreshMsg{} })
}

func (m tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		if m.showHelp {
			switch msg.String() {
			case "?", "esc", "q", "enter":
				m.showHelp = false
			}
			return m, nil
		}
		if m.showSessions {
			switch msg.String() {
			case "p", "esc", "q", "enter":
				m.showSessions = false
			}
			return m, nil
		}
		if m.showAgentOutput {
			switch msg.String() {
			case "esc", "q", "enter":
				m.showAgentOutput = false
			case "up", "k":
				m.outputScroll = max(0, m.outputScroll-1)
			case "down", "j":
				m.outputScroll++
			case "pgup":
				m.outputScroll = max(0, m.outputScroll-agentOutputPageSize(m)-1)
			case "pgdown":
				m.outputScroll += agentOutputPageSize(m) - 1
			case "home", "g":
				m.outputScroll = 0
			case "end", "G":
				m.outputScroll = max(0, len(agentOutputLines(m))-agentOutputPageSize(m))
			}
			return m, nil
		}
		if m.editing {
			switch msg.String() {
			case "enter":
				m.editing = false
				return m, m.submit()
			case "esc":
				m.editing = false
				m.command = ""
			case "backspace":
				if len(m.command) > 0 {
					_, size := utf8.DecodeLastRuneInString(m.command)
					m.command = m.command[:len(m.command)-size]
				}
			default:
				m.command += msg.Key().Text
			}
			return m, nil
		}
		switch msg.String() {
		case "q", "ctrl+c":
			return m, tea.Quit
		case ":":
			m.editing = true
			m.command = ""
			m.message = ""
		case "?":
			m.showHelp = true
		case "p":
			m.showSessions = !m.showSessions
			if m.showSessions && m.viewSessions != nil {
				m.agents = m.viewSessions()
			} else if m.sessions != nil {
				m.agents = m.sessions()
			}
		case "j", "down":
			if m.selected+1 < len(m.tasks) {
				m.selected++
			}
		case "k", "up":
			if m.selected > 0 {
				m.selected--
			}
		case "r":
			m.reload()
			m.message = "refreshed"
		case "s":
			return m, m.applySelected("stop", m.stop)
		case "R":
			return m, m.applySelected("restart", m.restart)
		case "c":
			return m, m.applySelected("clean", m.clean)
		case "v":
			return m, m.validateSelected()
		case "u":
			return m, m.startUpdate()
		}
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case refreshMsg:
		m.reload()
		if m.showSessions && m.viewSessions != nil {
			m.agents = m.viewSessions()
		} else if m.sessions != nil {
			m.agents = m.sessions()
		}
		return m, scheduleRefresh()
	case validationMsg:
		m.reload()
		if msg.err != nil {
			m.message = "validate " + string(msg.snapshot.ID) + ": " + msg.err.Error()
		} else {
			m.message = fmt.Sprintf("validate %s: %s", msg.snapshot.ID, msg.snapshot.State)
			for i, result := range msg.results {
				m.message += fmt.Sprintf("; check %d exit=%d", i+1, result.ExitCode)
				if result.TimedOut {
					m.message += " timed out"
				}
				if result.StdoutDroppedBytes > 0 {
					m.message += fmt.Sprintf("; stdout truncated (%d bytes omitted)", result.StdoutDroppedBytes)
				}
				if result.StderrDroppedBytes > 0 {
					m.message += fmt.Sprintf("; stderr truncated (%d bytes omitted)", result.StderrDroppedBytes)
				}
			}
		}
	case taskActionMsg:
		m.actionPending = false
		if msg.err != nil {
			m.message = msg.action + " " + msg.id + ": " + msg.err.Error()
		} else {
			m.message = msg.action + " " + msg.id
		}
		m.reload()
		m.refreshAgents()
	case agentReadMsg:
		if msg.err != nil {
			m.message = "read " + string(msg.id) + ": " + msg.err.Error()
			m.agentOutput = ""
			m.showAgentOutput = false
		} else if msg.out == "" {
			m.message = fmt.Sprintf("no visible screen text for %s (agent_state=%s; try again later)", msg.id, msg.agentState)
			m.agentOutput = ""
			m.agentOutputState = msg.agentState
			m.showAgentOutput = false
		} else {
			m.message = "latest terminal snapshot; agent_state=" + string(msg.agentState)
			if msg.agentState == agent.AgentStateWorking {
				m.message += "; still working — read again when idle or done"
			}
			if msg.dropped > 0 {
				m.message += fmt.Sprintf("; screen capture may be incomplete (%d bytes dropped)", msg.dropped)
			}
			m.agentOutputID = msg.id
			m.agentOutput = msg.out
			m.agentOutputState = msg.agentState
			m.outputScroll = max(0, len(agentOutputLines(m))-agentOutputPageSize(m))
			m.showAgentOutput = true
		}
	case agentPromptMsg:
		m.actionPending = false
		if msg.err != nil {
			m.message = "prompt " + string(msg.id) + ": " + msg.err.Error()
		} else if msg.waitFor == "" {
			m.message = "prompt sent to " + string(msg.id)
		} else {
			m.message = string(msg.id) + " reached " + string(msg.state)
		}
		m.refreshAgents()
	case updateCheckMsg:
		m.updateCheckDone = true
		m.updateFailed = msg.err != nil
		m.updateReady = msg.err == nil && msg.available
		if msg.err == nil && msg.available {
			m.updateVer = msg.version
			m.updateNotice = "Pony " + msg.version + " is available — press u to update"
		} else if msg.err == nil {
			m.updateNotice = ""
		}
	case updateInstallMsg:
		m.updateInstalling = false
		if msg.err != nil {
			m.message = "update: " + msg.err.Error()
			m.updateNotice = "Pony update failed"
		} else {
			m.updateReady = false
			m.updateNotice = "Pony " + msg.version + " installed — restart Pony to use the update"
		}
	}
	return m, nil
}

func (m *tuiModel) validateSelected() tea.Cmd {
	if len(m.tasks) == 0 {
		m.message = "no task selected"
		return nil
	}
	if m.validate == nil {
		m.message = "validate: not available"
		return nil
	}
	id := m.tasks[m.selected].ID
	m.message = "validating " + string(id) + "..."
	validate := m.validate
	return func() tea.Msg {
		snapshot, results, err := validate(id)
		return validationMsg{snapshot: snapshot, results: results, err: err}
	}
}

func (m *tuiModel) applySelected(action string, fn func(task.TaskID) error) tea.Cmd {
	if m.actionPending {
		m.message = "another action is still running"
		return nil
	}
	if len(m.tasks) == 0 {
		m.message = "no task selected"
		return nil
	}
	id := m.tasks[m.selected].ID
	if fn == nil {
		m.message = action + ": not available"
		return nil
	}
	m.message = action + " " + string(id) + "..."
	m.actionPending = true
	return func() tea.Msg { return taskActionMsg{action: action, id: string(id), err: fn(id)} }
}

func (m *tuiModel) runAction(action, id string, fn func() error) tea.Cmd {
	if m.actionPending {
		m.message = "another action is still running"
		return nil
	}
	m.actionPending = true
	return func() tea.Msg { return taskActionMsg{action: action, id: id, err: fn()} }
}

func (m *tuiModel) submit() tea.Cmd {
	if m.actionPending {
		m.message = "another action is still running"
		m.command = ""
		return nil
	}
	fields, err := shell_lexer.Fields(m.command)
	var cmd tea.Cmd
	if err != nil {
		m.message = "input: " + err.Error()
	} else if len(fields) == 0 {
		m.message = ""
	} else {
		switch fields[0] {
		case "help":
			m.showHelp = true
		case "run":
			var opts runOptions
			opts, err = parseRun(fields[1:])
			if err == nil {
				if m.run == nil {
					err = errors.New("not available")
				} else {
					run := m.run
					id := string(opts.id)
					m.message = "starting " + id + "..."
					cmd = m.runAction("started", id, func() error { return run(opts) })
				}
			}
			if err != nil {
				m.message = "run: " + err.Error()
			}
		case "start":
			if len(fields) < 3 {
				m.message = "usage: start <id> <command> [args...]"
			} else if _, exists := m.taskByID(task.TaskID(fields[1])); exists {
				m.message = "start: " + fields[1] + " is a task ID; use restart " + fields[1]
			} else if m.agentsAPI == nil {
				m.message = "start: not available"
			} else {
				agents := m.agentsAPI
				spec := agent.AgentSpec{ID: agent.AgentID(fields[1]), Command: fields[2], Args: append([]string(nil), fields[3:]...)}
				m.message = "starting " + fields[1] + "..."
				cmd = m.runAction("started", fields[1], func() error { _, err := agents.Start(spec); return err })
			}
		case "send":
			if len(fields) < 3 {
				m.message = "usage: send <id> <text>"
			} else if m.agentsAPI == nil {
				m.message = "send: not available"
			} else {
				text := strings.Join(fields[2:], " ")
				_, err = m.agentsAPI.Prompt(context.Background(), agent.AgentID(fields[1]), text, "", 0)
				if err != nil {
					m.message = "send: " + err.Error()
				} else {
					m.message = "sent " + fields[1] + ": " + text
				}
			}
		case "prompt":
			var opts promptOptions
			opts, err = parsePrompt(fields[1:])
			if err != nil {
				m.message = "prompt: " + err.Error()
			} else if m.agentsAPI == nil {
				m.message = "prompt: not available"
			} else if m.actionPending {
				m.message = "another action is still running"
			} else {
				agents := m.agentsAPI
				m.actionPending = true
				m.message = "prompting " + string(opts.id) + "..."
				cmd = func() tea.Msg {
					state, err := agents.Prompt(context.Background(), opts.id, opts.text, opts.waitFor, 0)
					return agentPromptMsg{id: opts.id, state: state, waitFor: opts.waitFor, err: err}
				}
			}
		case "read":
			if len(fields) != 2 {
				m.message = "usage: read <id>"
			} else if m.agentsAPI == nil {
				m.message = "read: not available"
			} else {
				m.message = "reading " + fields[1] + "..."
				mgr, id := m.agentsAPI, agent.AgentID(fields[1])
				cmd = func() tea.Msg {
					out, dropped, err := readAgentOutput(mgr, id)
					state := agentStateFor(mgr.Snapshots(), id)
					return agentReadMsg{id: id, out: out, dropped: dropped, agentState: state, err: err}
				}
			}
		case "attach":
			if len(fields) != 2 {
				m.message = "usage: attach <id>"
			} else {
				m.attachID = agent.AgentID(fields[1])
				return tea.Quit
			}
		case "resize":
			if len(fields) != 4 {
				m.message = "usage: resize <id> <rows> <cols>"
				break
			}
			rows, rowErr := strconv.Atoi(fields[2])
			cols, colErr := strconv.Atoi(fields[3])
			if rowErr != nil || colErr != nil || rows < 1 || rows > 65535 || cols < 1 || cols > 65535 {
				m.message = "resize: rows and cols must be numbers in the range 1..65535"
			} else if m.agentsAPI == nil {
				m.message = "resize: not available"
			} else if err = m.agentsAPI.Resize(agent.AgentID(fields[1]), uint16(rows), uint16(cols)); err != nil {
				m.message = "resize: " + err.Error()
			} else {
				m.message = fmt.Sprintf("resized %s to %dx%d", fields[1], rows, cols)
			}
		case "status":
			m.showSessions = true
			if m.viewSessions != nil {
				m.agents = m.viewSessions()
			} else {
				m.refreshAgents()
			}
		case "clean":
			if len(fields) != 2 {
				m.message = "usage: clean <task-id>"
			} else if m.clean == nil {
				m.message = "clean: not available"
			} else {
				id := task.TaskID(fields[1])
				clean := m.clean
				m.message = "clean " + fields[1] + "..."
				cmd = m.runAction("clean", fields[1], func() error { return clean(id) })
			}
		case "validate":
			if len(fields) != 2 {
				m.message = "usage: validate <task-id>"
			} else if m.validate == nil {
				m.message = "validate: not available"
			} else {
				id := task.TaskID(fields[1])
				m.message = "validating " + string(id) + "..."
				validate := m.validate
				cmd = func() tea.Msg {
					snapshot, results, err := validate(id)
					return validationMsg{snapshot: snapshot, results: results, err: err}
				}
			}
		case "list":
			m.reload()
			m.message = fmt.Sprintf("%d task(s)", len(m.tasks))
		case "stop", "restart":
			if len(fields) != 2 {
				m.message = "usage: " + fields[0] + " <id>"
				break
			}
			id := fields[1]
			if _, ok := m.taskByID(task.TaskID(id)); ok {
				fn := m.stop
				if fields[0] == "restart" {
					fn = m.restart
				}
				if fn == nil {
					m.message = fields[0] + ": not available"
				} else {
					taskID := task.TaskID(id)
					m.message = fields[0] + " " + id + "..."
					cmd = m.runAction(fields[0], id, func() error { return fn(taskID) })
				}
			} else if m.agentsAPI == nil {
				m.message = fields[0] + ": not available"
			} else {
				agents := m.agentsAPI
				agentID, action := agent.AgentID(id), fields[0]
				m.message = action + " " + id + "..."
				cmd = m.runAction(action, id, func() error {
					if action == "stop" {
						return agents.Stop(agentID)
					}
					return agents.Restart(agentID)
				})
			}
		case "quit", "exit":
			return tea.Quit
		default:
			m.message = "unknown command; type help for usage"
		}
	}
	m.command = ""
	return cmd
}

func (m *tuiModel) refreshAgents() {
	if m.showSessions && m.viewSessions != nil {
		m.agents = m.viewSessions()
	} else if m.sessions != nil {
		m.agents = m.sessions()
	}
}

func (m *tuiModel) startUpdate() tea.Cmd {
	if m.updateInstalling {
		m.message = "Pony update is already installing"
		return nil
	}
	if !m.updateCheckDone {
		m.message = "checking for updates..."
		return checkForUpdate()
	}
	if m.updateFailed {
		m.message = "update check failed; check your internet connection and try again"
		m.updateCheckDone = false
		return checkForUpdate()
	}
	if !m.updateReady {
		m.message = "no newer release is available"
		return nil
	}
	version := m.updateVer
	m.updateInstalling = true
	m.updateNotice = "installing Pony " + version + "..."
	return func() tea.Msg {
		return updateInstallMsg{version: version, err: updatePonyVersion(version)}
	}
}

func readAgentOutput(mgr *agent.Manager, id agent.AgentID) (string, uint64, error) {
	snapshot, err := mgr.ScreenSnapshot(id)
	if err != nil {
		return "", 0, err
	}
	lines := append(snapshot.Scrollback, snapshot.Visible...)
	return strings.TrimRight(strings.Join(lines, "\n"), " \n"), snapshot.DroppedBytes, nil
}

func agentStateFor(sessions []agent.SessionSnapshot, id agent.AgentID) agent.AgentState {
	for _, session := range sessions {
		if session.AgentID == id {
			return session.AgentState
		}
	}
	return agent.AgentStateUnknown
}

func (m tuiModel) taskByID(id task.TaskID) (task.Snapshot, bool) {
	for _, snapshot := range m.tasks {
		if snapshot.ID == id {
			return snapshot, true
		}
	}
	return task.Snapshot{}, false
}

func (m *tuiModel) reload() {
	var selected task.TaskID
	if m.selected < len(m.tasks) {
		selected = m.tasks[m.selected].ID
	}
	m.tasks = m.list()
	for i, snapshot := range m.tasks {
		if snapshot.ID == selected {
			m.selected = i
			return
		}
	}
	if m.selected >= len(m.tasks) {
		m.selected = max(0, len(m.tasks)-1)
	}
}

func (m tuiModel) View() tea.View {
	content := renderTUI(m)
	if m.showHelp {
		content = renderTUIHelp(m)
	} else if m.showSessions {
		content = renderSessionStatus(m)
	} else if m.showAgentOutput {
		content = renderAgentOutput(m)
	}
	v := tea.NewView(content)
	v.AltScreen = true
	return v
}

func runTUI(a *app) (agent.AgentID, error) {
	finalModel, err := tea.NewProgram(newTUIModel(a)).Run()
	if err != nil {
		return "", err
	}
	if model, ok := finalModel.(tuiModel); ok {
		return model.attachID, nil
	}
	return "", nil
}
