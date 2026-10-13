package main

import (
	"fmt"
	"strings"
	"unicode"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/akansha204/pony/internal/task"
)

var (
	accentStyle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#001C24")).Background(lipgloss.Color("#62D6E8")).Padding(0, 1)
	wordmarkStyle = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("#62D6E8"))
	mutedStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("#8B98A5"))
	selectedStyle = lipgloss.NewStyle().Reverse(true).Bold(true)
	panelStyle    = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("#526675")).Padding(0, 1)
	errorStyle    = lipgloss.NewStyle().Foreground(lipgloss.Color("#FF6B6B"))
	successStyle  = lipgloss.NewStyle().Foreground(lipgloss.Color("#62D6A7"))
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
	if len(m.tasks) == 0 {
		b.WriteString("No tasks in this session. Press : to run one, or ? for help.\n")
	} else {
		start, limit := visibleTasks(m, compact, showWordmark)
		for i := start; i < len(m.tasks) && i < start+limit; i++ {
			snapshot := m.tasks[i]
			line := fmt.Sprintf("  %-20s %s", snapshot.ID, snapshot.State)
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
		style := errorStyle
		if isSuccessMessage(m.message) {
			style = successStyle
		}
		b.WriteString("\n" + style.Render(ansi.Truncate(safeText(m.message), contentWidth, "…")) + "\n")
	}
	if m.editing {
		b.WriteString("\n" + accentStyle.Render(":") + " " + ansi.Truncate(safeText(m.command), max(1, contentWidth-4), "…") + "\n")
	} else {
		b.WriteString("\n" + mutedStyle.Render(ansi.Truncate(": run  •  ? help  •  j/k select  •  s stop  •  R restart  •  c clean  •  r refresh  •  q quit", contentWidth, "…")) + "\n")
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
		"Start a coding task",
		"  Press : and enter a run command, for example:",
		`  run --id demo --goal "Fix the bug" --repo /path/to/repo --command codex --validate "go test ./..."`,
		"  Pony creates a Git worktree, starts the agent there, and checks it after a clean exit.",
		"",
		"Task list keys",
		"  j / ↓       Select the next task",
		"  k / ↑       Select the previous task",
		"  s           Stop the selected task",
		"  R           Restart it in its worktree",
		"  c           Clean a stopped task's clean worktree",
		"  r           Refresh the task list",
		"  q           Quit Pony (running agents are stopped)",
		"",
		"More commands are available in the CLI: pony --cli, then type help.",
		"The CLI supports validate and agent sessions (start, send, read, attach, resize, status).",
		"",
		"Press ?, Esc, Enter, or q to return.",
	}
	var b strings.Builder
	for _, line := range lines {
		b.WriteString(ansi.Truncate(line, contentWidth, "…") + "\n")
	}
	return panelStyle.Render(b.String())
}

func isSuccessMessage(message string) bool {
	for _, prefix := range []string{"started ", "stop ", "restart ", "clean "} {
		if strings.HasPrefix(message, prefix) {
			return true
		}
	}
	return false
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
	case task.StateRunning, task.StateVerified:
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
