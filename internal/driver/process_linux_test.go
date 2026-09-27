//go:build linux

package driver

import (
	"context"
	"os"
	"testing"
)

func fdCount(t *testing.T) int {
	t.Helper()

	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatalf("ReadDir /proc/self/fd: %v", err)
	}

	return len(entries)
}

func TestFailedStartDoesNotLeakPipes(t *testing.T) {
	d := NewProcessDriver()
	before := fdCount(t)

	for i := 0; i < 100; i++ {
		h, err := d.Start(
			context.Background(),
			Spec{Path: "/definitely/not/a/real/pony-command"},
		)

		if err == nil {
			if h != nil {
				t.Fatalf("unexpected successful start: %+v", h)
			}
			t.Fatal("expected failed Start")
		}
	}

	after := fdCount(t)

	if after > before+5 {
		t.Fatalf(
			"failed starts leaked descriptors: before=%d after=%d",
			before,
			after,
		)
	}
}
