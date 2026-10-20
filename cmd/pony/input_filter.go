package main

import (
	"bytes"
	"strconv"
	"strings"
	"time"
)

// attachInputFilter holds only ambiguous terminal reports until they can be
// classified. Keyboard input is otherwise returned byte-for-byte.
type attachInputFilter struct {
	pending []byte
	paste   bool
	x10     int
}

func (f *attachInputFilter) feed(b byte, allowMouse, allowFocus bool) []byte {
	if f.paste {
		f.pending = append(f.pending, b)
		if bytes.HasSuffix(f.pending, []byte("\x1b[201~")) {
			out := append([]byte(nil), f.pending...)
			f.pending = nil
			f.paste = false
			return out
		}
		// Keep only a possible suffix prefix for the paste terminator.
		if len(f.pending) > len("\x1b[201~") {
			n := len(f.pending) - len("\x1b[201~")
			out := append([]byte(nil), f.pending[:n]...)
			f.pending = append([]byte(nil), f.pending[n:]...)
			return out
		}
		return nil
	}

	if f.x10 > 0 {
		f.pending = append(f.pending, b)
		f.x10--
		if f.x10 == 0 {
			out := append([]byte(nil), f.pending...)
			f.pending = nil
			if allowMouse {
				return out
			}
			return nil
		}
		return nil
	}

	if len(f.pending) == 0 {
		if b == 0x1b {
			f.pending = []byte{b}
			return nil
		}
		return []byte{b}
	}
	f.pending = append(f.pending, b)
	if len(f.pending) == 2 {
		if b == 'O' { // SS3 arrows and function keys have a final third byte.
			return nil
		}
		if b != '[' {
			return f.releasePending()
		}
		return nil
	}
	if len(f.pending) == 3 {
		if f.pending[1] == 'O' {
			if b >= 0x40 && b <= 0x7e {
				return f.releasePending()
			}
			return nil
		}
		switch b {
		case '<': // SGR mouse report; wait for its final M/m.
			return nil
		case 'M': // Legacy X10 report has three bytes after CSI M.
			f.x10 = 3
			return nil
		case 'I', 'O': // Focus in/out reports.
			out := append([]byte(nil), f.pending...)
			f.pending = nil
			if allowFocus {
				return out
			}
			return nil
		default:
			// A CSI final byte can complete a short keyboard sequence here;
			// parameter and intermediate bytes need to stay buffered.
			if b >= 0x40 && b <= 0x7e {
				return f.releasePending()
			}
			return nil
		}
	}

	if len(f.pending) > 3 && f.pending[2] == '<' {
		if b >= 0x40 && b <= 0x7e {
			seq := append([]byte(nil), f.pending...)
			f.pending = nil
			if isSGRMouse(seq) && !allowMouse {
				return nil
			}
			return seq
		}
		if b < 0x20 || b > 0x3f || len(f.pending) > 64 {
			return f.releasePending()
		}
	}
	if len(f.pending) > 128 {
		return f.releasePending()
	}
	// Recognize bracketed paste start as a complete CSI sequence.
	if f.pending[2] != '<' && b >= 0x40 && b <= 0x7e {
		seq := append([]byte(nil), f.pending...)
		f.pending = nil
		if string(seq) == "\x1b[200~" {
			f.paste = true
		} else if isURXVTMouse(seq) && !allowMouse {
			return nil
		}
		return seq
	}
	return nil
}

func isURXVTMouse(seq []byte) bool {
	if len(seq) < 7 || seq[0] != 0x1b || seq[1] != '[' || seq[len(seq)-1] != 'M' {
		return false
	}
	parts := strings.Split(string(seq[2:len(seq)-1]), ";")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		if _, err := strconv.Atoi(part); err != nil {
			return false
		}
	}
	return true
}

func (f *attachInputFilter) releasePending() []byte {
	out := append([]byte(nil), f.pending...)
	f.pending = nil
	f.x10 = 0
	return out
}

// flush releases an incomplete sequence after a short inter-byte timeout. A
// lone Escape is delayed by only one poll interval, so Escape remains usable.
func (f *attachInputFilter) flush() []byte {
	if len(f.pending) == 0 {
		return nil
	}
	if f.paste {
		return nil
	}
	return f.releasePending()
}

func (f *attachInputFilter) flushAfter() time.Duration {
	if len(f.pending) == 1 && f.pending[0] == 0x1b {
		return 35 * time.Millisecond
	}
	return 250 * time.Millisecond
}

func isSGRMouse(seq []byte) bool {
	if len(seq) < 7 || seq[0] != 0x1b || seq[1] != '[' || seq[2] != '<' {
		return false
	}
	if seq[len(seq)-1] != 'M' && seq[len(seq)-1] != 'm' {
		return false
	}
	parts := strings.Split(string(seq[3:len(seq)-1]), ";")
	if len(parts) != 3 {
		return false
	}
	for _, part := range parts {
		if part == "" {
			return false
		}
		if _, err := strconv.Atoi(part); err != nil {
			return false
		}
	}
	return true
}
