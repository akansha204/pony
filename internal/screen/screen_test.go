package screen

import (
	"reflect"
	"strings"
	"testing"
)

func TestFeedBuildsVisibleScreenAcrossChunks(t *testing.T) {
	m := New(3, 10, 4)
	m.Feed([]byte("hello\r\nwor"))
	m.Feed([]byte("ld\x1b[2;3H!"))
	s := m.Snapshot()
	if s.Rows != 3 || s.Columns != 10 || s.CursorRow != 1 || s.CursorColumn != 3 {
		t.Fatalf("screen metadata = %+v", s)
	}
	if s.Visible[0] != "hello" || s.Visible[1] != "wo!ld" {
		t.Fatalf("visible rows = %#v", s.Visible)
	}
}

func TestSnapshotKeepsStyleWideCellsAndControlState(t *testing.T) {
	m := New(5, 12, 0)
	m.Feed([]byte("\x1b[31m赤A\x1b[0m\x1b[3;5r\x1b[?6h\x1b[?25l\x1b[2;1H\x1b[1mB"))
	s := m.Snapshot()
	if s.Visible[0] != "赤A" || !strings.Contains(s.StyledVisible[0], "\x1b[31m赤A") {
		t.Fatalf("styled wide text = %q / %q", s.Visible[0], s.StyledVisible[0])
	}
	if s.ScrollTop != 2 || s.ScrollBottom != 4 || !s.OriginMode || s.CursorVisible || s.CursorRow != 3 || s.CursorColumn != 1 || s.CurrentStyle != "\x1b[1m" {
		t.Fatalf("control state = %+v", s)
	}
	if s.Visible[3] != "B" {
		t.Fatalf("origin-relative cursor row = %#v", s.Visible)
	}
}

func TestAlternateScreenDoesNotReplaceMainScreen(t *testing.T) {
	m := New(2, 12, 4)
	m.Feed([]byte("main screen\r\n"))
	m.Feed([]byte("\x1b[?1049h\x1b[2J\x1b[Halternate"))
	if got := m.Snapshot().Visible[0]; got != "alternate" {
		t.Fatalf("alternate screen row = %q", got)
	}
	m.Feed([]byte("\x1b[?1049l"))
	if got := m.Snapshot().Visible[0]; got != "main screen" {
		t.Fatalf("restored main screen row = %q", got)
	}
}

func TestScrollbackStaysBounded(t *testing.T) {
	m := New(2, 8, 2)
	m.Feed([]byte("a\r\nb\r\nc\r\nd\r\ne"))
	s := m.Snapshot()
	if len(s.Scrollback) != 2 || strings.Join(s.Scrollback, ",") != "b,c" {
		t.Fatalf("scrollback = %#v, want [b c]", s.Scrollback)
	}
	if s.Visible[0] != "d" || s.Visible[1] != "e" {
		t.Fatalf("visible rows = %#v", s.Visible)
	}
}

func TestDroppedObserverInputMarksSnapshotIncomplete(t *testing.T) {
	m := New(2, 8, 2)
	m.Feed([]byte("known"))
	m.MarkDropped(17)
	s := m.Snapshot()
	if s.Reliable || s.DroppedBytes != 17 || !strings.Contains(s.Text, "known") {
		t.Fatalf("snapshot after dropped input = %+v", s)
	}
}

func TestBracketedPasteModeTracksTerminalControls(t *testing.T) {
	m := New(2, 12, 4)
	m.Feed([]byte("\x1b[?2004h"))
	if !m.Snapshot().BracketedPaste {
		t.Fatal("bracketed paste mode should be enabled")
	}
	m.Feed([]byte("\x1b[?2004l"))
	if m.Snapshot().BracketedPaste {
		t.Fatal("bracketed paste mode should be disabled")
	}
}

func TestInteractiveTerminalModesAreIncludedInSnapshot(t *testing.T) {
	m := New(2, 12, 4)
	m.Feed([]byte("\x1b[?1000h\x1b[?1005h\x1b[?1006h\x1b[?1015h\x1b[?1016h\x1b[?1004h\x1b[?2004h"))
	s := m.Snapshot()
	if !reflect.DeepEqual(s.MouseModes, []int{1000, 1005, 1006, 1015, 1016}) || !s.FocusReporting || !s.BracketedPaste {
		t.Fatalf("interactive terminal modes = %+v", s)
	}
	m.Feed([]byte("\x1b[?1000l\x1b[?1005l\x1b[?1006l\x1b[?1015l\x1b[?1016l\x1b[?1004l\x1b[?2004l"))
	s = m.Snapshot()
	if len(s.MouseModes) != 0 || s.FocusReporting || s.BracketedPaste {
		t.Fatalf("disabled interactive terminal modes = %+v", s)
	}
}

func TestInputModesSeparatesMouseTrackingFromEncoding(t *testing.T) {
	m := New(2, 12, 4)
	m.Feed([]byte("\x1b[?1006h\x1b[?1015h\x1b[?1004h"))
	mouse, focus := m.InputModes()
	if mouse || !focus {
		t.Fatalf("InputModes after encoding-only modes = (%v,%v), want (false,true)", mouse, focus)
	}
	m.Feed([]byte("\x1b[?1002h"))
	mouse, focus = m.InputModes()
	if !mouse || !focus {
		t.Fatalf("InputModes after tracking mode = (%v,%v), want (true,true)", mouse, focus)
	}
}

func TestKittyKeyboardSequencesDoNotRestoreCursor(t *testing.T) {
	m := New(3, 12, 4)
	m.Feed([]byte("\x1b[2;5H\x1b[s\x1b[1;1H\x1b[>7u\x1b[?u\x1b[<u\x1b[=1u"))
	s := m.Snapshot()
	if s.CursorRow != 0 || s.CursorColumn != 0 {
		t.Fatalf("cursor after Kitty sequences = (%d,%d), want unchanged at origin", s.CursorRow, s.CursorColumn)
	}
	m.Feed([]byte("\x1b[u"))
	s = m.Snapshot()
	if s.CursorRow != 1 || s.CursorColumn != 4 {
		t.Fatalf("cursor after plain CSI u = (%d,%d), want saved position (1,4)", s.CursorRow, s.CursorColumn)
	}
}

func TestResizeAndSplitUTF8(t *testing.T) {
	m := New(2, 4, 2)
	m.Feed([]byte("hi ☃"[:4]))
	m.Feed([]byte("☃"[1:]))
	m.Resize(3, 8)
	s := m.Snapshot()
	if s.Rows != 3 || s.Columns != 8 || s.Visible[0] != "hi ☃" {
		t.Fatalf("resized screen = %+v", s)
	}
}
