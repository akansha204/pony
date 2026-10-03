package agent

import (
	"sync"
	"time"
)

const defaultOutputLimit = 64 << 10

// outputBuffer retains recent output without blocking the agent.
type outputBuffer struct {
	mu     sync.Mutex
	buf    []byte
	limit  int
	closed bool
	notify chan struct{}
}

func newOutputBuffer(limit int) *outputBuffer {
	if limit <= 0 {
		limit = defaultOutputLimit
	}
	return &outputBuffer{
		buf:    make([]byte, 0, limit),
		limit:  limit,
		notify: make(chan struct{}),
	}
}

func (o *outputBuffer) write(data []byte) {
	if len(data) == 0 {
		return
	}

	o.mu.Lock()
	defer o.mu.Unlock()
	if o.closed {
		return
	}

	o.buf = append(o.buf, data...)
	if extra := len(o.buf) - o.limit; extra > 0 {
		copy(o.buf, o.buf[extra:])
		o.buf = o.buf[:o.limit]
	}

	o.signalLocked()
}

func (o *outputBuffer) close() {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.closed {
		return
	}
	o.closed = true
	o.signalLocked()
}

func (o *outputBuffer) signalLocked() {
	close(o.notify)
	o.notify = make(chan struct{})
}

func (o *outputBuffer) wait(p []byte, timeout time.Duration) (n int, drained bool, err error) {
	var timer <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		timer = t.C
	}

	for {
		o.mu.Lock()

		if len(o.buf) > 0 {
			n = copy(p, o.buf)
			o.buf = append(o.buf[:0], o.buf[n:]...)
			drained = o.closed && len(o.buf) == 0
			o.mu.Unlock()
			return n, drained, nil
		}

		if o.closed {
			o.mu.Unlock()
			return 0, true, nil
		}

		notify := o.notify
		o.mu.Unlock()

		select {
		case <-notify:
		case <-timer:
			return 0, false, nil
		}
	}
}
