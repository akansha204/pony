package agent

import (
	"strings"
	"testing"
	"time"
)

func TestOutputBufferKeepsWrittenBytes(t *testing.T) {
	o := newOutputBuffer(1024)

	o.write([]byte("hello "))
	o.write([]byte("world"))

	got := make([]byte, 64)
	n, _, err := o.wait(got, 0)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if string(got[:n]) != "hello world" {
		t.Errorf("read %q, want %q", got[:n], "hello world")
	}
}

func TestOutputBufferWaitBlocksUntilWrite(t *testing.T) {
	o := newOutputBuffer(1024)

	go func() {
		time.Sleep(50 * time.Millisecond)
		o.write([]byte("late"))
	}()

	got := make([]byte, 16)
	n, _, err := o.wait(got, 2*time.Second)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if string(got[:n]) != "late" {
		t.Errorf("read %q, want %q", got[:n], "late")
	}
}

func TestOutputBufferTimeoutReturnsNothingRatherThanBlocking(t *testing.T) {
	o := newOutputBuffer(1024)

	start := time.Now()
	n, drained, err := o.wait(make([]byte, 16), 100*time.Millisecond)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if n != 0 {
		t.Errorf("read %d bytes from an empty buffer, want 0", n)
	}
	if drained {
		t.Error("a timed-out wait reported the buffer drained; only close can do that")
	}
	if elapsed < 100*time.Millisecond {
		t.Errorf("returned after %v, want to wait the full 100ms", elapsed)
	}
}

func TestOutputBufferCloseReleasesABlockedReader(t *testing.T) {
	o := newOutputBuffer(1024)

	done := make(chan bool, 1)
	go func() {
		_, drained, err := o.wait(make([]byte, 16), 0)
		if err != nil {
			t.Errorf("wait: %v", err)
		}
		done <- drained
	}()

	time.Sleep(50 * time.Millisecond)
	o.close()

	select {
	case drained := <-done:
		if !drained {
			t.Error("reader woke on close but did not report drained")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("close did not release the blocked reader")
	}
}

func TestOutputBufferCloseIsIdempotent(t *testing.T) {
	o := newOutputBuffer(1024)
	o.close()
	o.close()

	_, drained, err := o.wait(make([]byte, 16), 0)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if !drained {
		t.Error("a closed and empty buffer did not report drained")
	}
}

func TestOutputBufferDropsOldestWhenFull(t *testing.T) {
	const limit = 256
	o := newOutputBuffer(limit)

	o.write([]byte(strings.Repeat("a", limit)))
	o.write([]byte(strings.Repeat("b", limit)))

	got := make([]byte, limit*2)
	n, _, err := o.wait(got, 0)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}

	if n != limit {
		t.Fatalf("retained %d bytes, want the limit of %d", n, limit)
	}
	if string(got[:n]) != strings.Repeat("b", limit) {
		t.Error("overflow did not discard the oldest bytes; the newest must survive")
	}
}

func TestOutputBufferStaysBoundedUnderAFlood(t *testing.T) {
	const limit = 1024
	o := newOutputBuffer(limit)

	for range 2000 {
		o.write([]byte("0123456789"))
	}

	o.mu.Lock()
	retained := len(o.buf)
	capacity := cap(o.buf)
	o.mu.Unlock()

	if retained > limit {
		t.Errorf("retained %d bytes, want at most %d", retained, limit)
	}
	if capacity > limit*2 {
		t.Errorf("grew to a capacity of %d; a bounded buffer must not keep creeping", capacity)
	}
}

func TestOutputBufferKeepsTailWrittenAfterClose(t *testing.T) {
	o := newOutputBuffer(1024)
	o.close()
	o.write([]byte("last words"))

	got := make([]byte, 64)
	n, drained, err := o.wait(got, 0)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if string(got[:n]) != "last words" {
		t.Errorf("read %q, want %q", got[:n], "last words")
	}
	if !drained {
		t.Error("buffer did not report drained after the tail was read")
	}
}

func TestOutputBufferConcurrentWritersAndReaders(t *testing.T) {
	o := newOutputBuffer(4096)

	const writers, perWriter = 8, 50

	written := make(chan struct{})
	for range writers {
		go func() {
			for range perWriter {
				o.write([]byte("data"))
			}
			written <- struct{}{}
		}()
	}

	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		buf := make([]byte, 64)
		for {
			n, drained, err := o.wait(buf, 50*time.Millisecond)
			if err != nil {
				t.Errorf("wait: %v", err)
				return
			}
			if drained {
				return
			}
			_ = n
		}
	}()

	for range writers {
		<-written
	}
	o.close()

	select {
	case <-readerDone:
	case <-time.After(5 * time.Second):
		t.Fatal("close did not release the concurrent readers")
	}

	o.mu.Lock()
	retained := len(o.buf)
	o.mu.Unlock()

	if retained > 4096 {
		t.Errorf("retained %d bytes, want at most 4096", retained)
	}
}
