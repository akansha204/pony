// Package screen maintains a bounded, read-only model of terminal output.
// It is intended to observe a PTY byte stream that already has a single reader.
package screen

import (
	"strings"
	"sync"
	"sync/atomic"
	"unicode/utf8"
)

const (
	defaultRows       = 24
	defaultColumns    = 80
	defaultScrollback = 256
	maxDimension      = 500
	maxScrollback     = 4096
)

type buffer struct {
	grid        [][]rune
	row, col    int
	savedRow    int
	savedCol    int
	wrapPending bool
	lines       []string
}

// Snapshot is a point-in-time copy of the visible screen and its bounded
// scrollback. Reliable is false if the observer queue dropped terminal bytes.
type Snapshot struct {
	Rows         int
	Columns      int
	CursorRow    int
	CursorColumn int
	Visible      []string
	Scrollback   []string
	Text         string
	DroppedBytes uint64
	Reliable     bool
}

// Model parses a useful subset of terminal control sequences into a bounded grid.
type Model struct {
	mu            sync.Mutex
	rows          int
	cols          int
	maxScrollback int
	main          buffer
	alt           buffer
	useAlt        bool
	state         byte
	sequence      []byte
	oscEsc        bool
	pending       []byte
	dropped       atomic.Uint64
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
	}
}

func newBuffer(rows, columns int) buffer {
	b := buffer{grid: make([][]rune, rows)}
	for i := range b.grid {
		b.grid[i] = blankLine(columns)
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
		if m.state == 'e' {
			i++
			m.state = 0
			switch c {
			case '[':
				m.state = 'c'
			case ']':
				m.state = 'o'
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
}

func resizeBuffer(old buffer, rows, columns int) buffer {
	b := newBuffer(rows, columns)
	for y := 0; y < min(rows, len(old.grid)); y++ {
		copy(b.grid[y], old.grid[y])
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
	for i, line := range b.grid {
		visible[i] = strings.TrimRight(string(line), " ")
	}
	back := append([]string(nil), m.main.lines...)
	return Snapshot{
		Rows: m.rows, Columns: m.cols, CursorRow: b.row, CursorColumn: b.col,
		Visible: visible, Scrollback: back, Text: strings.Join(visible, "\n"),
		DroppedBytes: dropped, Reliable: dropped == 0,
	}
}

func (m *Model) putRune(r rune) {
	b := m.active()
	if b.wrapPending {
		b.col = 0
		m.lineFeed()
	}
	b = m.active()
	b.grid[b.row][b.col] = r
	if b.col == m.cols-1 {
		b.wrapPending = true
	} else {
		b.col++
	}
}

func (m *Model) lineFeed() {
	b := m.active()
	b.wrapPending = false
	if b.row < m.rows-1 {
		b.row++
		return
	}
	removed := b.grid[0]
	copy(b.grid, b.grid[1:])
	b.grid[m.rows-1] = blankLine(m.cols)
	if !m.useAlt && m.maxScrollback > 0 {
		b.lines = append(b.lines, strings.TrimRight(string(removed), " "))
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
		b.row = min(m.rows-1, value(0, 1)-1)
		b.col = min(m.cols-1, value(1, 1)-1)
		b.wrapPending = false
	case 'J':
		m.eraseDisplay(b, value(0, 0))
	case 'K':
		m.eraseLine(b, value(0, 0))
	case 's':
		b.savedRow, b.savedCol = b.row, b.col
	case 'u':
		b.row, b.col = b.savedRow, b.savedCol
		b.wrapPending = false
	case 'S':
		for i := 0; i < value(0, 1); i++ {
			m.scrollUp(b)
		}
	case 'T':
		for i := 0; i < value(0, 1); i++ {
			m.scrollDown(b)
		}
	case 'h', 'l':
		if private {
			for _, mode := range params {
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
}

func (m *Model) eraseDisplay(b *buffer, mode int) {
	switch mode {
	case 0:
		m.eraseLine(b, 0)
		for y := b.row + 1; y < m.rows; y++ {
			fillBlank(b.grid[y])
		}
	case 1:
		for y := 0; y < b.row; y++ {
			fillBlank(b.grid[y])
		}
		m.eraseLine(b, 1)
	case 2:
		for y := range b.grid {
			fillBlank(b.grid[y])
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

func (m *Model) scrollUp(b *buffer) {
	removed := b.grid[0]
	copy(b.grid, b.grid[1:])
	b.grid[m.rows-1] = blankLine(m.cols)
	if b == &m.main && m.maxScrollback > 0 {
		b.lines = append(b.lines, strings.TrimRight(string(removed), " "))
		if extra := len(b.lines) - m.maxScrollback; extra > 0 {
			copy(b.lines, b.lines[extra:])
			b.lines = b.lines[:m.maxScrollback]
		}
	}
}

func (m *Model) scrollDown(b *buffer) {
	copy(b.grid[1:], b.grid[:m.rows-1])
	b.grid[0] = blankLine(m.cols)
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
	m.state = 0
	m.sequence = nil
	m.pending = nil
}
