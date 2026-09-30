package terminal

import (
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

// openTTY returns a terminal pair. Assertions run against the slave, the
// side Pony would bridge from.
func openTTY(t *testing.T) (master, slave *os.File) {
	t.Helper()

	master, slave, err := pty.Open()
	if err != nil {
		t.Fatalf("open pty: %v", err)
	}
	t.Cleanup(func() {
		_ = slave.Close()
		_ = master.Close()
	})
	return master, slave
}

func termiosOf(t *testing.T, f *os.File) *unix.Termios {
	t.Helper()

	tio, err := unix.IoctlGetTermios(int(f.Fd()), unix.TCGETS)
	if err != nil {
		t.Fatalf("get termios: %v", err)
	}
	return tio
}

func TestMakeRawClearsLocalLineControl(t *testing.T) {
	_, slave := openTTY(t)

	prior, err := MakeRaw(slave.Fd())
	if err != nil {
		t.Fatalf("MakeRaw: %v", err)
	}
	t.Cleanup(func() { _ = Restore(slave.Fd(), prior) })

	tio := termiosOf(t, slave)

	// ICANON would hold input until a newline, ECHO would print what the
	// user types, ISIG would turn Ctrl+C into SIGINT here instead of a
	// byte for the agent.
	cleared := map[string]bool{
		"ICANON": tio.Lflag&unix.ICANON != 0,
		"ECHO":   tio.Lflag&unix.ECHO != 0,
		"ISIG":   tio.Lflag&unix.ISIG != 0,
		"IEXTEN": tio.Lflag&unix.IEXTEN != 0,
	}
	for name, stillSet := range cleared {
		if stillSet {
			t.Errorf("raw mode left %s set", name)
		}
	}

	// A byte at a time, indefinitely, rather than timing out on zero.
	if tio.Cc[unix.VMIN] != 1 {
		t.Errorf("VMIN = %d, want 1", tio.Cc[unix.VMIN])
	}
	if tio.Cc[unix.VTIME] != 0 {
		t.Errorf("VTIME = %d, want 0", tio.Cc[unix.VTIME])
	}

	// Output is still cooked, so newlines still translate.
	if tio.Oflag&unix.OPOST == 0 {
		t.Error("raw mode cleared OPOST; output newlines would no longer return the carriage")
	}
}

func TestMakeRawLeavesOutputProcessingAlone(t *testing.T) {
	_, slave := openTTY(t)

	before := termiosOf(t, slave)

	prior, err := MakeRaw(slave.Fd())
	if err != nil {
		t.Fatalf("MakeRaw: %v", err)
	}
	t.Cleanup(func() { _ = Restore(slave.Fd(), prior) })

	// With OPOST cleared, a bare \n steps down a line without returning
	// to column 0, so every line lands one further right than the last.
	if got := termiosOf(t, slave).Oflag; got != before.Oflag {
		t.Errorf("output flags changed: %#x, want %#x", got, before.Oflag)
	}

	// Input, by contrast, is genuinely raw.
	if termiosOf(t, slave).Iflag == before.Iflag &&
		termiosOf(t, slave).Lflag == before.Lflag {
		t.Error("input flags were left cooked; this is not raw mode")
	}
}

func TestRestoreReturnsTheOriginalState(t *testing.T) {
	_, slave := openTTY(t)

	before := termiosOf(t, slave)

	prior, err := MakeRaw(slave.Fd())
	if err != nil {
		t.Fatalf("MakeRaw: %v", err)
	}
	if reflect.DeepEqual(before, termiosOf(t, slave)) {
		t.Fatal("MakeRaw did not change the terminal; the test would prove nothing")
	}

	if err := Restore(slave.Fd(), prior); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	after := termiosOf(t, slave)
	if !reflect.DeepEqual(before, after) {
		t.Errorf("restore did not reproduce the original termios\nbefore: %+v\nafter:  %+v", before, after)
	}
}

func TestRestoreIsIdempotent(t *testing.T) {
	_, slave := openTTY(t)

	before := termiosOf(t, slave)

	prior, err := MakeRaw(slave.Fd())
	if err != nil {
		t.Fatalf("MakeRaw: %v", err)
	}
	t.Cleanup(func() { _ = Restore(slave.Fd(), prior) })

	for i := range 3 {
		if err := Restore(slave.Fd(), prior); err != nil {
			t.Fatalf("Restore call %d: %v", i+1, err)
		}
		if !reflect.DeepEqual(before, termiosOf(t, slave)) {
			t.Fatalf("Restore call %d changed the terminal away from the original", i+1)
		}
	}
}

func TestRestoreSurvivesAnIntermediateMakeRaw(t *testing.T) {
	_, slave := openTTY(t)

	before := termiosOf(t, slave)

	first, err := MakeRaw(slave.Fd())
	if err != nil {
		t.Fatalf("first MakeRaw: %v", err)
	}
	// A nested bridge capturing the already-raw state.
	if _, err := MakeRaw(slave.Fd()); err != nil {
		t.Fatalf("second MakeRaw: %v", err)
	}

	// The outermost saved state gets back to normal.
	if err := Restore(slave.Fd(), first); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	if !reflect.DeepEqual(before, termiosOf(t, slave)) {
		t.Error("restore after a nested raw period did not return the original termios")
	}
}

func TestSizeReportsRowsThenColumns(t *testing.T) {
	_, slave := openTTY(t)

	const wantRows, wantCols = 40, 132

	if err := pty.Setsize(slave, &pty.Winsize{Rows: wantRows, Cols: wantCols}); err != nil {
		t.Fatalf("Setsize: %v", err)
	}

	rows, cols, err := Size(slave.Fd())
	if err != nil {
		t.Fatalf("Size: %v", err)
	}

	// Non-square, so a transposed result fails here and nowhere else.
	if rows != wantRows || cols != wantCols {
		t.Errorf("Size = (%d, %d), want (%d, %d)", rows, cols, wantRows, wantCols)
	}
}

func TestNonTerminalDescriptorsAreRejected(t *testing.T) {
	readEnd, writeEnd, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	t.Cleanup(func() {
		_ = readEnd.Close()
		_ = writeEnd.Close()
	})

	regular, err := os.CreateTemp(t.TempDir(), "not-a-tty")
	if err != nil {
		t.Fatalf("temp file: %v", err)
	}
	t.Cleanup(func() { _ = regular.Close() })

	for name, f := range map[string]*os.File{
		"pipe":      readEnd,
		"file":      regular,
		"closed fd": os.NewFile(uintptr(1<<20), "gone"),
	} {
		if _, err := GetState(f.Fd()); err == nil {
			t.Errorf("GetState on a %s succeeded; it should not", name)
		} else if !strings.Contains(err.Error(), "terminal") {
			t.Errorf("GetState on a %s: %v, want an error naming the terminal", name, err)
		}
		if _, _, err := Size(f.Fd()); err == nil {
			t.Errorf("Size on a %s succeeded; it should not", name)
		}
	}
}

func TestRestoreRejectsAMissingState(t *testing.T) {
	_, slave := openTTY(t)

	if err := Restore(slave.Fd(), nil); err == nil {
		t.Error("Restore with a nil state succeeded; it should not")
	}
}
