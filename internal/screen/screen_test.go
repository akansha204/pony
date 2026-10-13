package screen

import (
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
