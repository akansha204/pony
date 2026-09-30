package terminal

import (
	"fmt"

	"golang.org/x/sys/unix"
	xterm "golang.org/x/term"
)

// maxDimension keeps a bogus size from wrapping into a plausible uint16.
const maxDimension = 1<<16 - 1

type State struct {
	inner *xterm.State
}

// GetState reads the current configuration of the terminal on fd.
func GetState(fd uintptr) (*State, error) {
	st, err := xterm.GetState(int(fd))
	if err != nil {
		return nil, fmt.Errorf("get terminal state: %w", err)
	}
	return &State{inner: st}, nil
}

// MakeRaw makes input on fd raw and returns the prior state, for Restore.
//
// cfmakeraw also clears output post-processing, which is wrong here: Pony
// still writes to a real terminal, so a bare \n would step down a line
// without returning to column 0. Output is left as the user had it, the
// way `ssh -t` does it.
func MakeRaw(fd uintptr) (*State, error) {
	before, err := unix.IoctlGetTermios(int(fd), unix.TCGETS)
	if err != nil {
		return nil, fmt.Errorf("read terminal state: %w", err)
	}
	outputFlags := before.Oflag

	st, err := xterm.MakeRaw(int(fd))
	if err != nil {
		return nil, fmt.Errorf("make terminal raw: %w", err)
	}

	tio, err := unix.IoctlGetTermios(int(fd), unix.TCGETS)
	if err != nil {
		return nil, fmt.Errorf("read terminal state after raw: %w", err)
	}

	tio.Oflag = outputFlags

	if err := unix.IoctlSetTermios(int(fd), unix.TCSETS, tio); err != nil {
		return nil, fmt.Errorf("restore output processing: %w", err)
	}

	return &State{inner: st}, nil
}

// Restore returns fd to a captured state. It rewrites saved settings rather than unsetting anything
func Restore(fd uintptr, s *State) error {
	if s == nil || s.inner == nil {
		return fmt.Errorf("restore terminal: no saved state")
	}
	if err := xterm.Restore(int(fd), s.inner); err != nil {
		return fmt.Errorf("restore terminal state: %w", err)
	}
	return nil
}

// Size reports rows and columns, in that order, to match pty.Winsize and the driver's Resize.
func Size(fd uintptr) (rows, cols uint16, err error) {
	width, height, err := xterm.GetSize(int(fd))
	if err != nil {
		return 0, 0, fmt.Errorf("get terminal size: %w", err)
	}
	if height <= 0 || width <= 0 {
		return 0, 0, fmt.Errorf("terminal reported a zero size (%d rows, %d cols)", height, width)
	}
	if height > maxDimension || width > maxDimension {
		return 0, 0, fmt.Errorf("terminal reported an out-of-range size (%d rows, %d cols)", height, width)
	}
	return uint16(height), uint16(width), nil
}
