package transport

import (
	"io"
	"sync"
	"time"
)

// BufferedPipe is a thread-safe in-memory pipe for passing data between
// an HTTP handler goroutine and a yamux session goroutine.
//
// Write() appends data and wakes blocked readers.
// Read() blocks until data is available or the pipe is closed.
// ReadAvailable() is like Read() but returns what's available after a timeout.
type BufferedPipe struct {
	mu     sync.Mutex
	cond   *sync.Cond
	buf    []byte
	closed bool
}

// NewBufferedPipe creates a new BufferedPipe ready for use.
func NewBufferedPipe() *BufferedPipe {
	p := &BufferedPipe{}
	p.cond = sync.NewCond(&p.mu)
	return p
}

// Write appends data to the buffer and signals waiting readers.
// Returns an error if the pipe is closed.
func (p *BufferedPipe) Write(data []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.closed {
		return 0, io.ErrClosedPipe
	}

	p.buf = append(p.buf, data...)
	p.cond.Signal()
	return len(data), nil
}

// Read blocks until data is available, then copies into dst.
// Returns io.EOF if the pipe is closed and empty.
func (p *BufferedPipe) Read(dst []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for len(p.buf) == 0 {
		if p.closed {
			return 0, io.EOF
		}
		p.cond.Wait()
	}

	n := copy(dst, p.buf)
	p.buf = p.buf[n:]
	return n, nil
}

// ReadAvailable reads whatever data is currently available without blocking.
// If no data is available, waits up to timeout. Returns (0, nil) if timeout
// expires with no data (caller should send an empty response).
// Returns io.EOF if the pipe is closed and empty.
func (p *BufferedPipe) ReadAvailable(dst []byte, timeout time.Duration) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	// If data is already available, return it immediately.
	if len(p.buf) > 0 {
		n := copy(dst, p.buf)
		p.buf = p.buf[n:]
		return n, nil
	}

	// If closed and empty, return EOF.
	if p.closed {
		return 0, io.EOF
	}

	// No data available — wait up to timeout using a timer goroutine.
	timedOut := false
	timer := time.AfterFunc(timeout, func() {
		p.mu.Lock()
		timedOut = true
		p.cond.Broadcast()
		p.mu.Unlock()
	})
	defer timer.Stop()

	for len(p.buf) == 0 && !p.closed && !timedOut {
		p.cond.Wait()
	}

	// Check what woke us up.
	if len(p.buf) > 0 {
		n := copy(dst, p.buf)
		p.buf = p.buf[n:]
		return n, nil
	}

	if p.closed {
		return 0, io.EOF
	}

	// Timeout expired with no data.
	return 0, nil
}

// Close closes the pipe, unblocking any waiting readers.
func (p *BufferedPipe) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.closed = true
	p.cond.Broadcast()
	return nil
}
