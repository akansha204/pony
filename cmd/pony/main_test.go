package main

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func tempFileWith(t *testing.T, content string) *os.File {
	t.Helper()

	path := filepath.Join(t.TempDir(), "input")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = f.Close() })

	return f
}

func TestLineReaderReturnsLinesWithoutNewlines(t *testing.T) {
	r := newLineReader(tempFileWith(t, "one\ntwo\n"))

	for _, want := range []string{"one", "two"} {
		got, err := r.readLine()
		if err != nil {
			t.Fatalf("readLine: %v", err)
		}
		if got != want {
			t.Errorf("read %q, want %q", got, want)
		}
	}

	if _, err := r.readLine(); err != io.EOF {
		t.Errorf("at end of input err = %v, want io.EOF", err)
	}
}

func TestLineReaderLeavesNothingBufferedAhead(t *testing.T) {
	f := tempFileWith(t, "one\ntwo\n")
	r := newLineReader(f)

	got, err := r.readLine()
	if err != nil {
		t.Fatalf("readLine: %v", err)
	}
	if got != "one" {
		t.Fatalf("read %q, want %q", got, "one")
	}

	offset, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		t.Fatalf("Seek: %v", err)
	}
	if offset != int64(len("one\n")) {
		t.Errorf("file advanced to %d, want %d; the line reader read ahead", offset, len("one\n"))
	}
}

func TestLineReaderReturnsAFinalLineWithNoNewline(t *testing.T) {
	r := newLineReader(tempFileWith(t, "one\ntrailing"))

	if _, err := r.readLine(); err != nil {
		t.Fatalf("readLine: %v", err)
	}

	got, err := r.readLine()
	if got != "trailing" {
		t.Errorf("read %q, want %q", got, "trailing")
	}
	if err != io.EOF {
		t.Errorf("err = %v, want io.EOF alongside the final line", err)
	}
}

func TestLineReaderOnEmptyInput(t *testing.T) {
	r := newLineReader(tempFileWith(t, ""))

	if got, err := r.readLine(); err != io.EOF {
		t.Errorf("read %q, err %v; want io.EOF", got, err)
	}
}

func TestLineReaderHandlesBlankLines(t *testing.T) {
	r := newLineReader(tempFileWith(t, "\n\none\n"))

	for i, want := range []string{"", "", "one"} {
		got, err := r.readLine()
		if err != nil {
			t.Fatalf("line %d: %v", i, err)
		}
		if got != want {
			t.Errorf("line %d read %q, want %q", i, got, want)
		}
	}
}

func TestLineReaderGrowsPastItsInitialCapacity(t *testing.T) {
	long := strings.Repeat("x", 4096)
	r := newLineReader(tempFileWith(t, long+"\n"))

	got, err := r.readLine()
	if err != nil {
		t.Fatalf("readLine: %v", err)
	}
	if got != long {
		t.Errorf("read %d bytes, want %d; the line was truncated", len(got), len(long))
	}
}

func TestLineReaderHandlesCarriageReturns(t *testing.T) {
	r := newLineReader(tempFileWith(t, "one\r\ntwo\r\n"))

	got, err := r.readLine()
	if err != nil {
		t.Fatalf("readLine: %v", err)
	}
	if got != "one\r" {
		t.Errorf("read %q, want %q; the CR should survive for the lexer", got, "one\r")
	}
}

func TestLineReaderFromAPipe(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe: %v", err)
	}
	t.Cleanup(func() {
		_ = pr.Close()
		_ = pw.Close()
	})

	go func() {
		_, _ = pw.Write([]byte("first\n"))
		_ = pw.Close()
	}()

	r := newLineReader(pr)
	got, err := r.readLine()
	if err != nil {
		t.Fatalf("readLine: %v", err)
	}
	if got != "first" {
		t.Errorf("read %q, want %q", got, "first")
	}
}

func TestLineReaderHandsFollowingInputToTheNextReader(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe: %v", err)
	}
	t.Cleanup(func() {
		_ = pr.Close()
		_ = pw.Close()
	})

	go func() {
		_, _ = pw.Write([]byte("attach shell\ntyped while attaching"))
	}()

	r := newLineReader(pr)
	line, err := r.readLine()
	if err != nil {
		t.Fatalf("readLine: %v", err)
	}
	if line != "attach shell" {
		t.Fatalf("read %q, want %q", line, "attach shell")
	}

	buf := make([]byte, len("typed while attaching"))
	if _, err := io.ReadFull(pr, buf); err != nil {
		t.Fatalf("reading what followed: %v", err)
	}
	if string(buf) != "typed while attaching" {
		t.Errorf("next reader got %q, want %q", buf, "typed while attaching")
	}
}
