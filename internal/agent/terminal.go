package agent

import (
	"strconv"

	"github.com/akansha204/pony/internal/screen"
)

// terminalProtocol tracks terminal output synchronously with the PTY pump. It
// answers a small set of legacy terminal queries itself, so an attached host
// terminal never sees or answers those queries a second time. The screen
// observer remains asynchronous and receives only the filtered byte stream.
type terminalProtocol struct {
	model     *screen.Model
	state     byte
	stringEsc bool
	pending   []byte
}

const maxProtocolSequence = 128
const maxOSCQuerySequence = 64

func newTerminalProtocol(rows, cols int) *terminalProtocol {
	return &terminalProtocol{model: screen.New(rows, cols, 0)}
}

// Process returns bytes safe to expose to readers and replies for terminal
// queries encountered in data. Incomplete escape sequences are held between
// calls, allowing sequences split across PTY reads to be handled atomically.
func (p *terminalProtocol) Process(data []byte) (output []byte, replies [][]byte) {
	var segment []byte
	flushSegment := func() {
		if len(segment) != 0 {
			p.model.Feed(segment)
			output = append(output, segment...)
			segment = segment[:0]
		}
	}
	for _, b := range data {
		switch p.state {
		case 0:
			if b == 0x1b {
				p.pending = append(p.pending[:0], b)
				p.state = 'e'
			} else {
				segment = append(segment, b)
			}
		case 'e':
			p.pending = append(p.pending, b)
			switch b {
			case '[':
				p.state = 'c'
			case ']': // Buffer only a short OSC candidate; long strings pass through.
				p.state = 'o'
			case 'P', 'X', '^', '_': // DCS, SOS, PM, and APC end only with ST.
				segment = append(segment, p.pending...)
				p.pending = p.pending[:0]
				p.state = 's'
			default:
				segment = append(segment, p.pending...)
				p.pending = p.pending[:0]
				p.state = 0
			}
		case 'c':
			p.pending = append(p.pending, b)
			if b >= 0x40 && b <= 0x7e {
				flushSegment()
				seq := string(p.pending)
				var cursor *screen.Snapshot
				if seq == "\x1b[6n" || seq == "\x1b[?6n" {
					atQuery := p.model.Snapshot()
					cursor = &atQuery
				}
				if reply, consumed := protocolQueryReply(seq, cursor); consumed {
					if reply != nil {
						replies = append(replies, reply)
					}
				} else {
					p.model.Feed(p.pending)
					output = append(output, p.pending...)
				}
				p.pending = p.pending[:0]
				p.state = 0
			} else if len(p.pending) >= maxProtocolSequence {
				segment = append(segment, p.pending...)
				p.pending = p.pending[:0]
				p.state = 'x' // oversized CSI: pass through while tracking its end.
			}
		case 'x':
			segment = append(segment, b)
			if b >= 0x40 && b <= 0x7e {
				p.state = 0
			}
		case 's':
			segment = append(segment, b)
			if p.stringEsc && b == '\\' {
				p.state = 0
			}
			p.stringEsc = b == 0x1b
		case 'o':
			p.pending = append(p.pending, b)
			terminated := b == 0x07 || (p.stringEsc && b == '\\')
			p.stringEsc = b == 0x1b
			if terminated {
				flushSegment()
				seq := string(p.pending)
				if reply, consumed := oscQueryReply(seq); consumed {
					replies = append(replies, reply)
				} else {
					p.model.Feed(p.pending)
					output = append(output, p.pending...)
				}
				p.pending = p.pending[:0]
				p.state = 0
				p.stringEsc = false
			} else if len(p.pending) >= maxOSCQuerySequence {
				segment = append(segment, p.pending...)
				p.pending = p.pending[:0]
				p.state = 'O'
			}
		case 'O': // OSC too large to be a query; pass through until BEL or ST.
			segment = append(segment, b)
			if b == 0x07 || (p.stringEsc && b == '\\') {
				p.state = 0
			}
			p.stringEsc = b == 0x1b
		}
	}
	flushSegment()
	return output, replies
}

// Flush releases any partial escape sequence at process exit without
// interpreting it as a query.
func (p *terminalProtocol) Flush() []byte {
	if len(p.pending) == 0 {
		return nil
	}
	out := append([]byte(nil), p.pending...)
	p.model.Feed(out)
	p.pending = p.pending[:0]
	p.state = 0
	return out
}

func (p *terminalProtocol) Resize(rows, cols int) { p.model.Resize(rows, cols) }

func (p *terminalProtocol) InputModes() (mouse, focus bool) { return p.model.InputModes() }

func protocolQueryReply(seq string, cursor *screen.Snapshot) ([]byte, bool) {
	switch seq {
	case "\x1b[6n":
		if cursor != nil {
			return []byte("\x1b[" + strconv.Itoa(cursor.CursorRow+1) + ";" + strconv.Itoa(cursor.CursorColumn+1) + "R"), true
		}
	case "\x1b[?6n":
		if cursor != nil {
			return []byte("\x1b[?" + strconv.Itoa(cursor.CursorRow+1) + ";" + strconv.Itoa(cursor.CursorColumn+1) + "R"), true
		}
	case "\x1b[5n":
		return []byte("\x1b[0n"), true
	case "\x1b[c", "\x1b[0c":
		return []byte("\x1b[?1;2c"), true
	case "\x1b[?u":
		// Kitty keyboard protocol is unsupported. Consume its capability query
		// without advertising enhanced keyboard support.
		return nil, true
	}
	if isKittyKeyboardControl(seq) {
		return nil, true // swallow Kitty keyboard mode controls; do not emulate them.
	}
	return nil, false
}

func oscQueryReply(seq string) ([]byte, bool) {
	switch seq {
	case "\x1b]10;?\a", "\x1b]10;?\x1b\\":
		return []byte("\x1b]10;rgb:ffff/ffff/ffff\x1b\\"), true
	case "\x1b]11;?\a", "\x1b]11;?\x1b\\":
		return []byte("\x1b]11;rgb:0000/0000/0000\x1b\\"), true
	default:
		return nil, false
	}
}

func isKittyKeyboardControl(seq string) bool {
	if len(seq) < 4 || seq[len(seq)-1] != 'u' || (seq[2] != '>' && seq[2] != '<' && seq[2] != '=') {
		return false
	}
	for i := 3; i < len(seq)-1; i++ {
		if (seq[i] < '0' || seq[i] > '9') && seq[i] != ';' {
			return false
		}
	}
	return true
}
