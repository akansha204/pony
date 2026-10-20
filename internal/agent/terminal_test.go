package agent

import (
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/akansha204/pony/internal/screen"
)

func TestTerminalProtocolAnswersSplitQueriesAtTheirCursorPosition(t *testing.T) {
	p := newTerminalProtocol(4, 20)
	first, replies := p.Process([]byte("abc\x1b["))
	if string(first) != "abc" || len(replies) != 0 {
		t.Fatalf("first Process = (%q, %q), want ordinary text and no early reply", first, replies)
	}
	second, replies := p.Process([]byte("6nZ\x1b[?6n\x1b[5n\x1b[c\x1b[0c"))
	if string(second) != "Z" {
		t.Fatalf("filtered output = %q, want only following text", second)
	}
	want := [][]byte{[]byte("\x1b[1;4R"), []byte("\x1b[?1;5R"), []byte("\x1b[0n"), []byte("\x1b[?1;2c"), []byte("\x1b[?1;2c")}
	if !reflect.DeepEqual(replies, want) {
		t.Fatalf("replies = %q, want %q", replies, want)
	}
}

func TestTerminalProtocolPassesUnknownAndOpaqueSequencesLosslessly(t *testing.T) {
	p := newTerminalProtocol(4, 20)
	input := []byte("hello ☃\x1b]title \x1b[6n\a\x1bPdata\a\x1b[5n\x1b\\\x1bXdata\x1b[6n\x1b\\\x1b[?25l!")
	got, replies := p.Process(input)
	if string(got) != string(input) || len(replies) != 0 {
		t.Fatalf("Process = (%q, %q), want input unchanged and no embedded query replies", got, replies)
	}
}

func TestTerminalProtocolDoesNotParseQueriesInsideControlStrings(t *testing.T) {
	p := newTerminalProtocol(4, 20)
	got, replies := p.Process([]byte("a\x1bPopaque\a\x1b[6n\x1b\\\x1b[6n"))
	if string(got) != "a\x1bPopaque\a\x1b[6n\x1b\\" || !reflect.DeepEqual(replies, [][]byte{[]byte("\x1b[1;2R")}) {
		t.Fatalf("Process = (%q, %q), want opaque DCS preserved and only real CPR answered", got, replies)
	}
}

func TestTerminalProtocolConsumesKittyControlsWithoutAdvertisingSupport(t *testing.T) {
	p := newTerminalProtocol(4, 20)
	got, replies := p.Process([]byte("a\x1b[>7u\x1b[?u\x1b[<u\x1b[=1u\x1b[>1:2u"))
	if string(got) != "a\x1b[>1:2u" || len(replies) != 0 {
		t.Fatalf("Process = (%q, %q), want unknown sequence preserved and no Kitty capability reply", got, replies)
	}
}

func TestTerminalProtocolAnswersFragmentedOSCColorQueriesOnly(t *testing.T) {
	p := newTerminalProtocol(4, 20)
	first, replies := p.Process([]byte("title\x1b]10;?\x1b"))
	if string(first) != "title" || len(replies) != 0 {
		t.Fatalf("first Process = (%q, %q), want title and buffered partial query", first, replies)
	}
	second, replies := p.Process([]byte("\\\x1b]11;?\a\x1b]0;window title\a\x1b]10;rgb:1111/2222/3333\a"))
	wantOutput := "\x1b]0;window title\a\x1b]10;rgb:1111/2222/3333\a"
	if string(second) != wantOutput {
		t.Fatalf("filtered output = %q, want ordinary OSC setters unchanged", second)
	}
	wantReplies := [][]byte{[]byte("\x1b]10;rgb:ffff/ffff/ffff\x1b\\"), []byte("\x1b]11;rgb:0000/0000/0000\x1b\\")}
	if !reflect.DeepEqual(replies, wantReplies) {
		t.Fatalf("OSC replies = %q, want %q", replies, wantReplies)
	}
}

func TestPumpAnswersQueriesAndDoesNotExposeThemToRead(t *testing.T) {
	d := newPromptTestDriver()
	m := promptManager(t, d)
	d.output <- []byte("before\x1b[6n\x1b[cafter")
	buf := make([]byte, 64)
	n, err := m.ReadTimeout("test", buf, time.Second)
	if err != nil && err != io.EOF {
		t.Fatalf("ReadTimeout: %v", err)
	}
	if got := string(buf[:n]); got != "beforeafter" {
		t.Fatalf("read = %q, want handled queries removed", got)
	}
	deadline := time.Now().Add(time.Second)
	var writes [][]byte
	for time.Now().Before(deadline) {
		writes = d.recordedWrites()
		if len(writes) >= 2 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if len(writes) != 2 || string(writes[0]) != "\x1b[1;7R" || string(writes[1]) != "\x1b[?1;2c" {
		t.Fatalf("query responses = %q, want one CPR and one DA response", writes)
	}
}

func TestPTYQueryFloodDoesNotBlockOutputAttachOrStop(t *testing.T) {
	m := pumpPTY(t)
	program := `stty raw -echo; i=0; while [ $i -lt 20000 ]; do printf '\033]10;?\007'; i=$((i+1)); done; printf 'QUERY_FLOOD_DONE\n'; sleep 5`
	id := AgentID("query-flood")
	if _, err := m.Start(AgentSpec{ID: id, Command: "sh", Args: []string{"-c", program}}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = m.Stop(id) })

	deadline := time.Now().Add(4 * time.Second)
	buf := make([]byte, 4096)
	var output string
	for time.Now().Before(deadline) {
		n, err := m.ReadTimeout(id, buf, 100*time.Millisecond)
		output += string(buf[:n])
		if strings.Contains(output, "QUERY_FLOOD_DONE") {
			break
		}
		if err != nil && err != io.EOF {
			t.Fatalf("ReadTimeout: %v", err)
		}
	}
	if !strings.Contains(output, "QUERY_FLOOD_DONE") {
		t.Fatal("output pump did not reach marker after query flood")
	}
	attachDone := make(chan error, 1)
	go func() {
		_, err := m.PrepareAttach(id)
		attachDone <- err
	}()
	select {
	case err := <-attachDone:
		if err != nil {
			t.Fatalf("PrepareAttach: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("PrepareAttach stalled during query flood")
	}
	stopDone := make(chan error, 1)
	go func() { stopDone <- m.Stop(id) }()
	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatalf("Stop: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Stop stalled after query flood")
	}
}

func TestTerminalProtocolResizeAndFlushPartialInput(t *testing.T) {
	p := newTerminalProtocol(2, 4)
	p.Process([]byte("long\n\x1b["))
	p.Resize(4, 20)
	if got := string(p.Flush()); got != "\x1b[" {
		t.Fatalf("Flush = %q, want incomplete escape bytes preserved", got)
	}
	if _, replies := p.Process([]byte("\x1b[6n")); len(replies) != 1 || string(replies[0]) != "\x1b[2;4R" {
		t.Fatalf("resized cursor reply = %q, want resized screen coordinates", replies)
	}
}

func TestPrepareAttachOverlaysLosslessProtocolModesAtStreamBoundary(t *testing.T) {
	s := &session{
		State:      StateRunning,
		out:        newOutputBuffer(defaultOutputLimit),
		screen:     screen.New(4, 20, 8),
		protocol:   newTerminalProtocol(4, 20),
		screenFeed: make(chan screenEvent, 1),
		screenDone: make(chan struct{}),
	}
	_, _ = s.protocol.Process([]byte("\x1b[?1002h\x1b[?1004h\x1b[?2004h\x1b[?1049h"))
	// Simulate a stale asynchronous observer event that says these modes are off.
	s.screenFeed <- screenEvent{data: []byte("\x1b[?1002l\x1b[?1004l\x1b[?2004l\x1b[?1049l")}
	go observeScreen(s)
	m := NewManager(newPromptTestDriver())
	m.agents["attach-modes"] = &agent{session: s}

	snapshot, err := m.PrepareAttach("attach-modes")
	if err != nil {
		t.Fatalf("PrepareAttach: %v", err)
	}
	if !snapshot.BracketedPaste || !snapshot.AlternateScreen || !snapshot.FocusReporting || !reflect.DeepEqual(snapshot.MouseModes, []int{1002}) {
		t.Fatalf("attached terminal modes = paste:%v alt:%v focus:%v mouse:%v, want all enabled", snapshot.BracketedPaste, snapshot.AlternateScreen, snapshot.FocusReporting, snapshot.MouseModes)
	}
	s.streamMu.Lock()
	s.screenFeedClosed = true
	close(s.screenFeed)
	s.streamMu.Unlock()
	<-s.screenDone
}
