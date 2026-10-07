package main

import (
	"time"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"

	"github.com/akansha204/pony/internal/shell_lexer"
	"github.com/akansha204/pony/internal/task"
)

type refreshMsg struct{}

type tuiModel struct {
	list     func() []task.Snapshot
	run      func(runOptions) error
	tasks    []task.Snapshot
	selected int
	width    int
	height   int
	editing  bool
	command  string
	message  string
}

func newTUIModel(a *app) tuiModel {
	return tuiModel{
		list: a.tasks.List,
		run: func(opts runOptions) error {
			_, err := a.run(opts)
			return err
		},
		tasks: a.tasks.List(),
	}
}

func (m tuiModel) Init() tea.Cmd {
	return scheduleRefresh()
}

func scheduleRefresh() tea.Cmd {
	return tea.Tick(250*time.Millisecond, func(time.Time) tea.Msg { return refreshMsg{} })
}

func (m tuiModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.KeyPressMsg:
		if m.editing {
			switch msg.String() {
			case "enter":
				m.editing = false
				m.submit()
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
		}
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case refreshMsg:
		m.reload()
		return m, scheduleRefresh()
	}
	return m, nil
}

func (m *tuiModel) submit() {
	fields, err := shell_lexer.Fields(m.command)
	if err != nil {
		m.message = "input: " + err.Error()
	} else if len(fields) == 0 {
		m.message = ""
	} else if fields[0] != "run" {
		m.message = "only run is available here; use pony --cli for other commands"
	} else {
		var opts runOptions
		opts, err = parseRun(fields[1:])
		if err == nil {
			err = m.run(opts)
		}
		if err != nil {
			m.message = "run: " + err.Error()
		} else {
			m.message = "started " + string(opts.id)
			m.reload()
		}
	}
	m.command = ""
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
	v := tea.NewView(renderTUI(m))
	v.AltScreen = true
	return v
}

func runTUI(a *app) error {
	_, err := tea.NewProgram(newTUIModel(a)).Run()
	return err
}
