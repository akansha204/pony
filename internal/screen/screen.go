// Package screen maintains a bounded, read-only model of terminal output.
// It is intended to observe a PTY byte stream that already has a single reader.
package screen

import (
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"

	"github.com/mattn/go-runewidth"
)

const (
	defaultRows       = 24
	defaultColumns    = 80
	defaultScrollback = 256
	maxDimension      = 500
	maxScrollback     = 4096
)

// MaxDimension is the largest terminal side the bounded screen model stores.
const MaxDimension = maxDimension

type buffer struct {
	grid        [][]rune
	styles      [][]string
	row, col    int
	savedRow    int
	savedCol    int
	wrapPending bool
	lines       []string
}

// Snapshot is a point-in-time copy of the visible screen and its bounded
// scrollback. Reliable is false if the observer queue dropped terminal bytes.
type Snapshot struct {
	Rows            int
	Columns         int
	CursorRow       int
	CursorColumn    int
	Visible         []string
	StyledVisible   []string
	Scrollback      []string
	Text            string
	DroppedBytes    uint64
	Reliable        bool
	BracketedPaste  bool
	AlternateScreen bool
	MouseModes      []int
	FocusReporting  bool
	CursorVisible   bool
	ScrollTop       int
	ScrollBottom    int
	OriginMode      bool
	CurrentStyle    string
}

// Model parses a useful subset of terminal control sequences into a bounded grid.
type Model struct {
	mu             sync.Mutex
	rows           int
	cols           int
	maxScrollback  int
	main           buffer
	alt            buffer
	useAlt         bool
	bracketedPaste bool
	mouseModes     map[int]bool
	focusReporting bool
	cursorVisible  bool
	scrollTop      int
	scrollBottom   int
	originMode     bool
	currentStyle   string
	state          byte
	sequence       []byte
	oscEsc         bool
	pending        []byte
	dropped        atomic.Uint64
}

// New creates a screen with fixed dimensions and a bounded main-screen scrollback.
func New(rows, columns, scrollback int) *Model {
	if rows < 1 {
		rows = defaultRows
	}
	if columns < 1 {
		columns = defaultColumns
	}
	if rows > maxDimension {
		rows = maxDimension
	}
	if columns > maxDimension {
		columns = maxDimension
	}
	if scrollback < 0 {
		scrollback = 0
	}
	if scrollback > maxScrollback {
		scrollback = maxScrollback
	}
	return &Model{
		rows: rows, cols: columns, maxScrollback: scrollback,
		main: newBuffer(rows, columns), alt: newBuffer(rows, columns),
		mouseModes: make(map[int]bool), cursorVisible: true, scrollBottom: rows - 1,
	}
}

func newBuffer(rows, columns int) buffer {
	b := buffer{grid: make([][]rune, rows), styles: make([][]string, rows)}
	for i := range b.grid {
		b.grid[i] = blankLine(columns)
		b.styles[i] = make([]string, columns)
	}
	return b
}

func blankLine(columns int) []rune {
	line := make([]rune, columns)
	for i := range line {
		line[i] = ' '
	}
	return line
}

func (m *Model) active() *buffer {
	if m.useAlt {
		return &m.alt
	}
	return &m.main
}

// Feed adds bytes from the sole PTY reader. Calls may contain partial escape
// sequences or UTF-8 runes; both are retained until the next call.
func (m *Model) Feed(data []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	input := make([]byte, 0, len(m.pending)+len(data))
	input = append(input, m.pending...)
	input = append(input, data...)
	m.pending = m.pending[:0]
	for i := 0; i < len(input); {
		c := input[i]
		if m.state == 'c' {
			i++
			if c >= 0x40 && c <= 0x7e {
				m.handleCSI(c, m.sequence)
				m.sequence = m.sequence[:0]
				m.state = 0
			} else if len(m.sequence) < 128 {
				m.sequence = append(m.sequence, c)
			} else {
				m.sequence = m.sequence[:0]
				m.state = 0
			}
			continue
		}
		if m.state == 'o' { // OSC: discard through BEL or ST (ESC \\).
			i++
			if c == 0x07 || (m.oscEsc && c == '\\') {
				m.state = 0
			}
			m.oscEsc = c == 0x1b
			continue
		}
		if m.state == 'd' { // DCS, SOS, PM, APC: discard through ST only.
			i++
			if m.oscEsc && c == '\\' {
				m.state = 0
			}
			m.oscEsc = c == 0x1b
			continue
		}
		if m.state == 'e' {
			i++
			m.state = 0
			switch c {
			case '[':
				m.state = 'c'
			case ']':
				m.state = 'o'
			case 'P', 'X', '^', '_':
				m.state = 'd'
			case '7':
				b := m.active()
				b.savedRow, b.savedCol = b.row, b.col
			case '8':
				b := m.active()
				b.row, b.col = b.savedRow, b.savedCol
				b.wrapPending = false
			case 'c':
				m.resetLocked()
			}
			continue
		}
		if c == 0x1b {
			m.state = 'e'
			i++
			continue
		}
		if c >= 0x80 {
			if !utf8.FullRune(input[i:]) {
				m.pending = append(m.pending, input[i:]...)
				break
			}
			r, size := utf8.DecodeRune(input[i:])
			if r == utf8.RuneError && size == 1 {
				i++
				continue
			}
			m.putRune(r)
			i += size
			continue
		}
		i++
		switch c {
		case '\r':
			b := m.active()
			b.col = 0
			b.wrapPending = false
		case '\n', '\v', '\f':
			m.lineFeed()
		case '\b':
			b := m.active()
			if b.col > 0 {
				b.col--
			}
			b.wrapPending = false
		case '\t':
			b := m.active()
			b.col = min(m.cols-1, ((b.col/8)+1)*8)
			b.wrapPending = false
		case 0x07, 0x00: // Bell and NUL do not alter the visible screen.
		default:
			if c >= 0x20 && c != 0x7f {
				m.putRune(rune(c))
			}
		}
	}
}

// MarkDropped records observer bytes lost when its bounded handoff queue is full.
func (m *Model) MarkDropped(n uint64) {
	if n == 0 {
		return
	}
	m.dropped.Add(n)
}

// Resize changes the modeled terminal dimensions while preserving visible cells.
func (m *Model) Resize(rows, columns int) {
	if rows < 1 || columns < 1 {
		return
	}
	if rows > maxDimension {
		rows = maxDimension
	}
	if columns > maxDimension {
		columns = maxDimension
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.main = resizeBuffer(m.main, rows, columns)
	m.alt = resizeBuffer(m.alt, rows, columns)
	m.rows, m.cols = rows, columns
	m.scrollTop = min(m.scrollTop, rows-1)
	m.scrollBottom = min(m.scrollBottom, rows-1)
	if m.scrollBottom <= m.scrollTop {
		m.scrollTop, m.scrollBottom = 0, rows-1
	}
}

func resizeBuffer(old buffer, rows, columns int) buffer {
	b := newBuffer(rows, columns)
	for y := 0; y < min(rows, len(old.grid)); y++ {
		copy(b.grid[y], old.grid[y])
		copy(b.styles[y], old.styles[y])
	}
	b.row = min(old.row, rows-1)
	b.col = min(old.col, columns-1)
	b.savedRow = min(old.savedRow, rows-1)
	b.savedCol = min(old.savedCol, columns-1)
	b.wrapPending = old.wrapPending && b.col == columns-1
	b.lines = old.lines
	return b
}

// Snapshot returns copies of the visible grid and scrollback. Text contains
// the visible rows joined by newlines, with trailing spaces removed per row.
func (m *Model) Snapshot() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	b := m.active()
	dropped := m.dropped.Load()
	visible := make([]string, len(b.grid))
	styled := make([]string, len(b.grid))
	for i, line := range b.grid {
		visible[i] = plainLine(line)
		styled[i] = styledLine(line, b.styles[i])
	}
	back := append([]string(nil), b.lines...)
	mouseModes := make([]int, 0, len(m.mouseModes))
	for mode, enabled := range m.mouseModes {
		if enabled {
			mouseModes = append(mouseModes, mode)
		}
	}
	sort.Ints(mouseModes)
	return Snapshot{
		Rows: m.rows, Columns: m.cols, CursorRow: b.row, CursorColumn: b.col,
		Visible: visible, StyledVisible: styled, Scrollback: back, Text: strings.Join(visible, "\n"),
		DroppedBytes: dropped, Reliable: dropped == 0, BracketedPaste: m.bracketedPaste,
		AlternateScreen: m.useAlt, MouseModes: mouseModes, FocusReporting: m.focusReporting,
		CursorVisible: m.cursorVisible, ScrollTop: m.scrollTop, ScrollBottom: m.scrollBottom,
		OriginMode: m.originMode, CurrentStyle: m.currentStyle,
	}
}

func plainLine(line []rune) string {
	var b strings.Builder
	for _, r := range line {
		if r != 0 {
			b.WriteRune(r)
		}
	}
	return strings.TrimRight(b.String(), " ")
}

func styledLine(line []rune, styles []string) string {
	var b strings.Builder
	current := ""
	for i, r := range line {
		if r == 0 {
			continue
		}
		if styles[i] != current {
			b.WriteString("\x1b[0m")
			b.WriteString(styles[i])
			current = styles[i]
		}
		b.WriteRune(r)
	}
	if current != "" {
		b.WriteString("\x1b[0m")
	}
	return b.String()
}

// InputModes reports whether the agent currently requests mouse tracking and
// focus reporting. It avoids copying the screen grid for input routing.
func (m *Model) InputModes() (mouse, focus bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, mode := range []int{9, 1000, 1002, 1003} {
		if m.mouseModes[mode] {
			mouse = true
			break
		}
	}
	return mouse, m.focusReporting
}

func (m *Model) putRune(r rune) {
	b := m.active()
	width := runewidth.RuneWidth(r)
	if width < 1 {
		width = 1
	}
	if width > m.cols {
		width = 1
	}
	if width == 2 && b.col == m.cols-1 {
		b.wrapPending = true
	}
	if b.wrapPending {
		b.col = 0
		m.lineFeed()
	}
	b = m.active()
	b.grid[b.row][b.col] = r
	b.styles[b.row][b.col] = m.currentStyle
	if width == 2 {
		b.grid[b.row][b.col+1] = 0
		b.styles[b.row][b.col+1] = m.currentStyle
	}
	if b.col+width >= m.cols {
		b.wrapPending = true
	} else {
		b.col += width
	}
}

func (m *Model) lineFeed() {
	b := m.active()
	b.wrapPending = false
	if b.row < m.scrollBottom || b.row > m.scrollBottom {
		b.row++
		b.row = min(b.row, m.rows-1)
		return
	}
	removed := b.grid[m.scrollTop]
	copy(b.grid[m.scrollTop:m.scrollBottom], b.grid[m.scrollTop+1:m.scrollBottom+1])
	copy(b.styles[m.scrollTop:m.scrollBottom], b.styles[m.scrollTop+1:m.scrollBottom+1])
	b.grid[m.scrollBottom] = blankLine(m.cols)
	b.styles[m.scrollBottom] = make([]string, m.cols)
	if m.maxScrollback > 0 {
		b.lines = append(b.lines, plainLine(removed))
		if extra := len(b.lines) - m.maxScrollback; extra > 0 {
			copy(b.lines, b.lines[extra:])
			b.lines = b.lines[:m.maxScrollback]
		}
	}
}

func (m *Model) handleCSI(final byte, raw []byte) {
	private := false
	if len(raw) > 0 && raw[0] == '?' {
		private = true
		raw = raw[1:]
	}
	params := parseParams(raw)
	value := func(i, fallback int) int {
		if i >= len(params) || params[i] == 0 {
			return fallback
		}
		return params[i]
	}
	b := m.active()
	switch final {
	case 'A':
		b.row = max(0, b.row-value(0, 1))
		b.wrapPending = false
	case 'B':
		b.row = min(m.rows-1, b.row+value(0, 1))
		b.wrapPending = false
	case 'C':
		b.col = min(m.cols-1, b.col+value(0, 1))
		b.wrapPending = false
	case 'D':
		b.col = max(0, b.col-value(0, 1))
		b.wrapPending = false
	case 'E':
		b.row = min(m.rows-1, b.row+value(0, 1))
		b.col = 0
		b.wrapPending = false
	case 'F':
		b.row = max(0, b.row-value(0, 1))
		b.col = 0
		b.wrapPending = false
	case 'G':
		b.col = min(m.cols-1, value(0, 1)-1)
		b.wrapPending = false
	case 'd':
		b.row = min(m.rows-1, value(0, 1)-1)
		b.wrapPending = false
	case 'H', 'f':
		row := value(0, 1) - 1
		if m.originMode {
			row += m.scrollTop
			row = min(row, m.scrollBottom)
		}
		b.row = min(m.rows-1, row)
		b.col = min(m.cols-1, value(1, 1)-1)
		b.wrapPending = false
	case 'J':
		m.eraseDisplay(b, value(0, 0))
	case 'K':
		m.eraseLine(b, value(0, 0))
	case 's':
		b.savedRow, b.savedCol = b.row, b.col
	case 'u':
		// Only the parameterless cursor-restore sequence is DECRC's CSI form.
		// Kitty keyboard controls also end in 'u' and must not move the cursor.
		if !private && len(raw) == 0 {
			b.row, b.col = b.savedRow, b.savedCol
			b.wrapPending = false
		}
	case 'S':
		for i := 0; i < value(0, 1); i++ {
			m.scrollUp(b)
		}
	case 'T':
		for i := 0; i < value(0, 1); i++ {
			m.scrollDown(b)
		}
	case 'm':
		if len(raw) == 0 || (len(params) > 0 && params[0] == 0) {
			m.currentStyle = ""
		}
		if len(raw) > 0 && string(raw) != "0" && len(m.currentStyle)+len(raw) < 256 {
			m.currentStyle += "\x1b[" + string(raw) + "m"
		}
	case 'r':
		top, bottom := value(0, 1)-1, value(1, m.rows)-1
		if top >= 0 && bottom < m.rows && top < bottom {
			m.scrollTop, m.scrollBottom = top, bottom
			b.row, b.col = 0, 0
			if m.originMode {
				b.row = top
			}
			b.wrapPending = false
		}
	case 'h', 'l':
		if private {
			for _, mode := range params {
				if mode == 2004 {
					m.bracketedPaste = final == 'h'
				}
				if mode == 9 || mode == 1000 || mode == 1002 || mode == 1003 || mode == 1005 || mode == 1006 || mode == 1015 || mode == 1016 {
					m.mouseModes[mode] = final == 'h'
				}
				if mode == 1004 {
					m.focusReporting = final == 'h'
				}
				if mode == 25 {
					m.cursorVisible = final == 'h'
				}
				if mode == 6 {
					m.originMode = final == 'h'
					b.row, b.col = 0, 0
					if m.originMode {
						b.row = m.scrollTop
					}
					b.wrapPending = false
				}
				if mode == 1049 || mode == 47 || mode == 1047 {
					if final == 'h' {
						m.enterAlt()
					} else {
						m.leaveAlt()
					}
				}
			}
		}
	}
}

func parseParams(raw []byte) []int {
	if len(raw) == 0 {
		return nil
	}
	parts := strings.Split(string(raw), ";")
	out := make([]int, len(parts))
	for i, part := range parts {
		n := 0
		for _, r := range part {
			if r < '0' || r > '9' {
				n = 0
				break
			}
			n = n*10 + int(r-'0')
		}
		out[i] = n
	}
	return out
}

func (m *Model) eraseLine(b *buffer, mode int) {
	start, end := 0, m.cols
	switch mode {
	case 0:
		start = b.col
	case 1:
		end = b.col + 1
	case 2:
	default:
		return
	}
	fillBlank(b.grid[b.row][start:end])
	fillStyle(b.styles[b.row][start:end], m.currentStyle)
}

func (m *Model) eraseDisplay(b *buffer, mode int) {
	switch mode {
	case 0:
		m.eraseLine(b, 0)
		for y := b.row + 1; y < m.rows; y++ {
			fillBlank(b.grid[y])
			fillStyle(b.styles[y], m.currentStyle)
		}
	case 1:
		for y := 0; y < b.row; y++ {
			fillBlank(b.grid[y])
			fillStyle(b.styles[y], m.currentStyle)
		}
		m.eraseLine(b, 1)
	case 2:
		for y := range b.grid {
			fillBlank(b.grid[y])
			fillStyle(b.styles[y], m.currentStyle)
		}
	case 3:
		for y := range b.lines {
			b.lines[y] = ""
		}
		b.lines = nil
	}
}

func fillBlank(line []rune) {
	for i := range line {
		line[i] = ' '
	}
}

func fillStyle(line []string, style string) {
	for i := range line {
		line[i] = style
	}
}

func (m *Model) scrollUp(b *buffer) {
	removed := b.grid[m.scrollTop]
	copy(b.grid[m.scrollTop:m.scrollBottom], b.grid[m.scrollTop+1:m.scrollBottom+1])
	copy(b.styles[m.scrollTop:m.scrollBottom], b.styles[m.scrollTop+1:m.scrollBottom+1])
	b.grid[m.scrollBottom] = blankLine(m.cols)
	b.styles[m.scrollBottom] = make([]string, m.cols)
	if m.maxScrollback > 0 {
		b.lines = append(b.lines, plainLine(removed))
		if extra := len(b.lines) - m.maxScrollback; extra > 0 {
			copy(b.lines, b.lines[extra:])
			b.lines = b.lines[:m.maxScrollback]
		}
	}
}

func (m *Model) scrollDown(b *buffer) {
	copy(b.grid[m.scrollTop+1:m.scrollBottom+1], b.grid[m.scrollTop:m.scrollBottom])
	copy(b.styles[m.scrollTop+1:m.scrollBottom+1], b.styles[m.scrollTop:m.scrollBottom])
	b.grid[m.scrollTop] = blankLine(m.cols)
	b.styles[m.scrollTop] = make([]string, m.cols)
}

func (m *Model) enterAlt() {
	if m.useAlt {
		return
	}
	m.useAlt = true
	m.alt = newBuffer(m.rows, m.cols)
}

func (m *Model) leaveAlt() {
	if m.useAlt {
		m.useAlt = false
		m.alt = newBuffer(m.rows, m.cols)
	}
}

func (m *Model) resetLocked() {
	m.main = newBuffer(m.rows, m.cols)
	m.alt = newBuffer(m.rows, m.cols)
	m.useAlt = false
	m.bracketedPaste = false
	m.mouseModes = make(map[int]bool)
	m.focusReporting = false
	m.cursorVisible = true
	m.scrollTop, m.scrollBottom = 0, m.rows-1
	m.originMode = false
	m.currentStyle = ""
	m.state = 0
	m.sequence = nil
	m.pending = nil
}
