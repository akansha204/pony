package main

import (
	"fmt"
	"strings"
	"unicode"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/akansha204/pony/internal/agent"
	"github.com/akansha204/pony/internal/task"
)

var (
	accentStyle    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#001C24")).Background(lipgloss.Color("#62D6E8")).Padding(0, 1)
	wordmarkStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#62D6E8"))
	mutedStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("#8B98A5"))
	selectedStyle  = lipgloss.NewStyle().Reverse(true).Bold(true)
	panelStyle     = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("#526675")).Padding(0, 1)
	errorStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("#FF6B6B"))
	infoStyle      = lipgloss.NewStyle().Foreground(lipgloss.Color("#62C7E8"))
	successStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("#62D6A7"))
	attentionStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("#FF6B6B")).Bold(true)
)

const ponyWordmark = ` ____   ___  _   _ __   __
|  _ \ / _ \| \ | |\ \ / /
| |_) | | | |  \| | \ V /
|  __/| |_| | |\  |  | |
|_|    \___/|_| \_|  |_|`

func renderTUI(m tuiModel) string {
	width := m.width
	if width <= 0 {
		width = 80
	}
	contentWidth := max(1, width-2)
	compact := width < 42 || (m.height > 0 && m.height < 12)
	showWordmark := width >= 50 && (m.height == 0 || m.height >= 18)
	var b strings.Builder
	if showWordmark {
		b.WriteString(wordmarkStyle.Render(ponyWordmark) + "\n")
	}
	fmt.Fprintf(&b, "%s  %s\n\n", accentStyle.Render("PONY"), mutedStyle.Render(fmt.Sprintf("tasks  %d", len(m.tasks))))
	if m.updateNotice != "" {
		b.WriteString(successStyle.Render(ansi.Truncate(m.updateNotice, contentWidth, "…")) + "\n\n")
	}
	if len(m.tasks) == 0 {
		b.WriteString("No tasks in this session. Press : for a command, or ? for help.\n")
	} else {
		start, limit := visibleTasks(m, compact, showWordmark)
		for i := start; i < len(m.tasks) && i < start+limit; i++ {
			snapshot := m.tasks[i]
			line := fmt.Sprintf("  %-14s task=%-12s", snapshot.ID, snapshot.State)
			if session, ok := agentSessionForTask(m.agents, snapshot.ID); ok {
				line += " agent_state=" + string(session.AgentState)
			} else {
				line += " agent_state=unknown"
			}
			line = ansi.Truncate(line, contentWidth, "…")
			if i == m.selected {
				line = selectedStyle.Render(line)
			} else {
				line = styleStatus(line, snapshot.State)
			}
			b.WriteString(line + "\n")
		}
		if !compact {
			selected := m.tasks[m.selected]
			details := []string{
				"Task: " + string(selected.ID),
				"Goal: " + safeText(selected.Goal),
				"Repo: " + safeText(selected.Repository),
				"Workspace: " + safeText(selected.WorkspacePath),
			}
			for i := range details {
				details[i] = ansi.Truncate(details[i], max(8, contentWidth-4), "…")
			}
			b.WriteString("\n" + panelStyle.Render(strings.Join(details, "\n")) + "\n")
		}
	}
	if m.message != "" {
		style := infoStyle
		if isErrorMessage(m.message) {
			style = errorStyle
		} else if isSuccessMessage(m.message) {
			style = successStyle
		}
		b.WriteString("\n" + style.Render(ansi.Truncate(safeText(m.message), contentWidth, "…")) + "\n")
	}
	if m.editing {
		b.WriteString("\n" + accentStyle.Render(":") + " " + ansi.Truncate(safeText(m.command), max(1, contentWidth-4), "…") + "\n")
	} else {
		b.WriteString("\n" + mutedStyle.Render(ansi.Truncate(": commands  •  ? help  •  u update  •  p sessions  •  v validate  •  j/k select  •  s stop  •  R restart  •  c clean  •  r refresh  •  q quit", contentWidth, "…")) + "\n")
	}
	return b.String()
}

func renderTUIHelp(m tuiModel) string {
	width := m.width
	if width <= 0 {
		width = 80
	}
	contentWidth := max(1, width-2)
	lines := []string{
		"PONY HELP",
		"",
		"Start a task",
		"  Press : and enter a run command, for example:",
		`  run --id demo --goal "Plan a Go app" --command codex`,
		"  Pony uses the current Git repository by default and creates a worktree.",
		"  Add --repo PATH to choose another repo, or --validate COMMAND to run checks.",
		"  codex, opencode, claude, and aider receive the goal at launch.",
		"  Other commands need one whole --arg '{goal}' at the goal argument position.",
		"  running means the process started; inspect status/read/attach for progress.",
		"",
		"Agent session commands (enter them after pressing :)",
		"  start ID CMD [args...]     Start a separate agent process",
		"  send ID TEXT              Submit one line; return immediately",
		"  prompt ID TEXT            Submit a request; optionally wait for agent_state",
		"  prompt ID --wait STATE TEXT  STATE: idle, blocked, or done (2m timeout)",
		"  send and prompt both press Enter; prompt --wait waits for the inferred state",
		"  read ID                   Open readable agent output and scrollback",
		"  attach ID                 Use the agent in this terminal; Ctrl+\\ returns here",
		"  resize ID ROWS COLS       Change the agent terminal size",
		"  status                    Show agent process status (same as p)",
		"  stop ID / restart ID      Stop or restart a task or agent",
		`  Example: start demo codex -- "Fix the bug"`,
		"",
		"Task list keys",
		"  j / ↓       Select the next task",
		"  k / ↑       Select the previous task",
		"  s           Stop the selected task",
		"  R           Restart it in its worktree",
		"  c           Clean a stopped/completed task's clean worktree",
		"  v           Validate a stopped task with configured checks",
		"  p           View agent session process status",
		"  r           Refresh the task list",
		"  q           Quit Pony (running agents are stopped)",
		"  u           Install the latest tagged release when an update is available",
		"",
		"agent_state is inferred from visible terminal text; unknown means Pony could not tell.",
		"read shows a snapshot. Check agent_state first; use prompt --wait done to wait, or read again later.",
		"idle = ready for input; blocked = waiting for you; done = a turn appears complete.",
		"checks_passed means configured commands exited successfully; it does not prove the goal was met.",
		"The same task controls are available as : stop ID, : restart ID, : clean ID, and : validate ID.",
		"",
		"Press ?, Esc, Enter, or q to return.",
	}
	var b strings.Builder
	for _, line := range lines {
		b.WriteString(ansi.Truncate(line, contentWidth, "…") + "\n")
	}
	return panelStyle.Render(b.String())
}

func renderSessionStatus(m tuiModel) string {
	width := m.width
	if width <= 0 {
		width = 80
	}
	contentWidth := max(1, width-6)
	var b strings.Builder
	b.WriteString("PONY SESSION STATUS\n\n")
	if len(m.agents) == 0 {
		b.WriteString("No agent sessions have been started in this Pony process.\n")
	} else {
		for _, session := range m.agents {
			attention := session.AgentState == agent.AgentStateBlocked || session.AgentState == agent.AgentStateDone
			marker := "  "
			if attention {
				marker = "!!"
			}
			line := fmt.Sprintf("%s %-20s pid=%-7d runtime=%s agent=%s agent_state=%s fgpid=%d process=%s", marker, session.AgentID, session.PID, session.State, session.AgentKind, session.AgentState, session.ForegroundPID, session.ForegroundProcess)
			if attention {
				line += "  NEEDS ATTENTION"
			}
			line = ansi.Truncate(line, contentWidth, "…")
			if attention {
				line = attentionStyle.Render(line)
			}
			b.WriteString(line + "\n")
		}
	}
	b.WriteString("\nRuntime is process status; agent_state is inferred from terminal text. Press p, Esc, Enter, or q to return.\n")
	return panelStyle.Render(b.String())
}

func renderAgentOutput(m tuiModel) string {
	width := m.width
	if width <= 0 {
		width = 80
	}
	contentWidth := max(1, width-6)
	lines := agentOutputLinesAtWidth(m.agentOutput, contentWidth)
	pageSize := agentOutputPageSize(m)
	maxScroll := max(0, len(lines)-pageSize)
	start := min(max(0, m.outputScroll), maxScroll)
	end := min(start+pageSize, len(lines))
	var b strings.Builder
	b.WriteString("PONY AGENT OUTPUT — " + string(m.agentOutputID) + "\n")
	state := m.agentOutputState
	b.WriteString(mutedStyle.Render("agent_state="+string(state)+"; snapshot may change while the agent is working") + "\n")
	if m.message != "" {
		b.WriteString(mutedStyle.Render(ansi.Truncate(safeText(m.message), contentWidth, "…")) + "\n")
	}
	b.WriteString("\n")
	if len(lines) == 0 {
		b.WriteString("No visible screen text.\n")
	} else {
		b.WriteString(panelStyle.Width(contentWidth).Render(strings.Join(lines[start:end], "\n")) + "\n")
		fmt.Fprintf(&b, "Lines %d–%d of %d\n", start+1, end, len(lines))
	}
	fmt.Fprintf(&b, "\n%s\n", mutedStyle.Render("↑/↓ or j/k scroll • PgUp/PgDn page • Home/End jump • Esc close"))
	return b.String()
}

func agentOutputLines(m tuiModel) []string {
	width := m.width
	if width <= 0 {
		width = 80
	}
	return agentOutputLinesAtWidth(m.agentOutput, max(1, width-6))
}

func agentOutputPageSize(m tuiModel) int {
	height := m.height
	if height <= 0 {
		height = 24
	}
	return max(1, height-7)
}

func agentOutputLinesAtWidth(output string, width int) []string {
	if output == "" {
		return nil
	}
	var lines []string
	for _, line := range strings.Split(strings.TrimRight(output, "\n"), "\n") {
		line = safeText(line)
		wrapped := strings.Split(ansi.Wrap(line, width, " "), "\n")
		lines = append(lines, wrapped...)
	}
	return lines
}

func isSuccessMessage(message string) bool {
	for _, prefix := range []string{"started ", "sent ", "prompt sent to ", "stopped ", "resized ", "restarted ", "stop ", "restart ", "clean ", "validate "} {
		if strings.HasPrefix(message, prefix) {
			return true
		}
	}
	return false
}

func isErrorMessage(message string) bool {
	for _, prefix := range []string{"run:", "start:", "send:", "prompt:", "read:", "resize:", "update:", "input:", "usage:", "unknown command"} {
		if strings.HasPrefix(message, prefix) {
			return true
		}
	}
	for _, prefix := range []string{"started ", "stop ", "restart ", "clean "} {
		if strings.HasPrefix(message, prefix) && strings.Contains(message, ": ") {
			return true
		}
	}
	if strings.HasPrefix(message, "read ") && strings.Contains(message, ": ") {
		return true
	}
	if strings.HasPrefix(message, "prompt ") && strings.Contains(message, ": ") {
		return true
	}
	if strings.HasPrefix(message, "validate ") && strings.Contains(message, ": ") {
		_, detail, ok := strings.Cut(message, ": ")
		if !ok {
			return false
		}
		for _, state := range []string{"pending", "running", "validating", "completed", "checks_passed", "failed", "stopped"} {
			if strings.HasPrefix(detail, state) {
				return state == "failed"
			}
		}
		return true
	}
	return strings.HasPrefix(message, "update check failed")
}

func agentSessionForTask(sessions []agent.SessionSnapshot, id task.TaskID) (agent.SessionSnapshot, bool) {
	for _, session := range sessions {
		if string(session.AgentID) == string(id) {
			return session, true
		}
	}
	return agent.SessionSnapshot{}, false
}

func visibleTasks(m tuiModel, compact, showWordmark bool) (int, int) {
	limit := len(m.tasks)
	if m.height > 0 {
		reserve := 4
		if !compact {
			reserve = 12
		}
		if showWordmark {
			reserve += 5
		}
		if m.message != "" {
			reserve += 2
		}
		limit = max(1, min(limit, m.height-reserve))
	}
	start := 0
	if m.selected >= limit {
		start = m.selected - limit + 1
	}
	return start, limit
}

func styleStatus(line string, state task.State) string {
	var color string
	switch state {
	case task.StateRunning, task.StateChecksPassed:
		color = "#62D6A7"
	case task.StateCompleted:
		color = "#62D6A7"
	case task.StateValidating, task.StatePending:
		color = "#E8BD62"
	case task.StateFailed:
		color = "#FF6B6B"
	default:
		color = "#9AA8B4"
	}
	return lipgloss.NewStyle().Foreground(lipgloss.Color(color)).Render(line)
}

func safeText(s string) string {
	s = ansi.Strip(s)
	return strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, s)
}
