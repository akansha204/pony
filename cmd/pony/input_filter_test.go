package main

import (
	"bytes"
	"testing"
)

func collectFilter(f *attachInputFilter, input []byte, mouse, focus bool) []byte {
	var out []byte
	for _, b := range input {
		out = append(out, f.feed(b, mouse, focus)...)
	}
	return out
}

func TestAttachInputFilterMouseModes(t *testing.T) {
	report := []byte("\x1b[<35;12;14M")
	for _, tc := range []struct {
		name  string
		allow bool
		want  []byte
	}{
		{"unrequested SGR report", false, nil},
		{"requested SGR report", true, report},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var f attachInputFilter
			var got []byte
			for _, b := range report {
				got = append(got, f.feed(b, tc.allow, false)...)
			}
			if !bytes.Equal(got, tc.want) {
				t.Fatalf("output %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAttachInputFilterSplitReportsAndKeyboard(t *testing.T) {
	var f attachInputFilter
	got := collectFilter(&f, []byte("\x1b[<35;12;14"), false, false)
	if len(got) != 0 {
		t.Fatalf("partial mouse report forwarded: %q", got)
	}
	got = append(got, collectFilter(&f, []byte("Mleft\x1b[A"), false, false)...)
	if string(got) != "left\x1b[A" {
		t.Fatalf("output %q, want keyboard bytes and arrow only", got)
	}
}

func TestAttachInputFilterX10FocusAndEscape(t *testing.T) {
	var f attachInputFilter
	got := collectFilter(&f, []byte("\x1b[Mabc\x1b[I\x1b[O"), false, false)
	if len(got) != 0 {
		t.Fatalf("unrequested reports forwarded: %q", got)
	}
	got = collectFilter(&f, []byte("\x1b[I\x1b[O"), false, true)
	if string(got) != "\x1b[I\x1b[O" {
		t.Fatalf("requested focus reports output %q", got)
	}
	_ = collectFilter(&f, []byte{0x1b}, false, false)
	if got := f.flush(); !bytes.Equal(got, []byte{0x1b}) {
		t.Fatalf("Escape flush = %q", got)
	}
}

func TestAttachInputFilterRecognizesURXVTMouseEncoding(t *testing.T) {
	report := []byte("\x1b[35;12;14M")
	var f attachInputFilter
	if got := collectFilter(&f, report, false, false); len(got) != 0 {
		t.Fatalf("unrequested urxvt mouse report forwarded: %q", got)
	}
	if got := collectFilter(&f, report, true, false); !bytes.Equal(got, report) {
		t.Fatalf("requested urxvt mouse report = %q, want %q", got, report)
	}
}

func TestAttachInputFilterPreservesRequestedX10Mouse(t *testing.T) {
	input := []byte("\x1b[Mabc")
	var f attachInputFilter
	var got []byte
	for i, b := range input {
		part := f.feed(b, true, false)
		if i < len(input)-1 && len(part) != 0 {
			t.Fatalf("requested X10 prefix forwarded early at byte %d: %q", i, part)
		}
		got = append(got, part...)
	}
	if !bytes.Equal(got, input) {
		t.Fatalf("requested X10 mouse input = %q, want %q", got, input)
	}
}

func TestAttachInputFilterBuffersSS3KeysUntilFinalByte(t *testing.T) {
	for _, input := range [][]byte{[]byte("\x1bOA"), []byte("\x1bOP")} {
		t.Run(string(input[2:]), func(t *testing.T) {
			var f attachInputFilter
			for i, b := range input {
				part := f.feed(b, false, false)
				if i < len(input)-1 && len(part) != 0 {
					t.Fatalf("SS3 prefix forwarded early at byte %d: %q", i, part)
				}
				if i == len(input)-1 && !bytes.Equal(part, input) {
					t.Fatalf("SS3 final output %q, want %q", part, input)
				}
			}
		})
	}
}

func TestAttachInputFilterDoesNotFilterBracketedPaste(t *testing.T) {
	input := []byte("\x1b[200~text \x1b[<35;12;14M\x1b[201~")
	var f attachInputFilter
	got := collectFilter(&f, input, false, false)
	if !bytes.Equal(got, input) {
		t.Fatalf("paste output %q, want exact input %q", got, input)
	}
}
