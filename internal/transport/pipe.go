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

// DefaultCoalesceWindow is used by both the read side (ReadAvailable's
// phase 2, below) and the write side (HTTPConn.Write) whenever the caller
// hasn't configured an explicit value. See TunnelConfig.CoalesceWindow and
// TransportConfig.CoalesceWindow.
const DefaultCoalesceWindow = 2 * time.Millisecond

// ReadAvailable reads whatever data is currently available, in two phases.
//
// Phase 1: if the pipe is empty, wait up to timeout for the first byte to
// arrive (this is the actual long-poll wait — unchanged from before).
//
// Phase 2: once at least one byte is available, wait a further, much
// shorter coalesceWindow for more to accumulate (capped by len(dst)),
// instead of flushing immediately. Without this, a poll response goes out
// the instant a single byte lands in the buffer — and since callers
// (HTTPConn's poll loop) immediately re-poll after receiving any data, that
// turns every small trickle of data into its own full round trip, never
// letting dst's full capacity get used even though it's sitting right
// there. This is the same class of fix as HTTPConn.Write's coalescing, but
// for the download direction. Pass coalesceWindow <= 0 to use
// DefaultCoalesceWindow.
//
// Returns (0, nil) if timeout expires with no data at all (caller should
// send an empty response). Returns io.EOF if the pipe is closed and empty.
func (p *BufferedPipe) ReadAvailable(dst []byte, timeout time.Duration, coalesceWindow time.Duration) (int, error) {
	if coalesceWindow <= 0 {
		coalesceWindow = DefaultCoalesceWindow
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if len(p.buf) == 0 {
		if p.closed {
			return 0, io.EOF
		}

		// Phase 1: wait for the first byte (or timeout/close).
		timedOut := false
		timer := time.AfterFunc(timeout, func() {
			p.mu.Lock()
			timedOut = true
			p.cond.Broadcast()
			p.mu.Unlock()
		})
		for len(p.buf) == 0 && !p.closed && !timedOut {
			p.cond.Wait()
		}
		timer.Stop()

		if len(p.buf) == 0 {
			if p.closed {
				return 0, io.EOF
			}
			return 0, nil // pure timeout, no data at all
		}
	}

	// Phase 2: at least one byte is available. Give it a brief window to
	// accumulate more before flushing, unless dst is already full or the
	// pipe closed in the meantime.
	if len(p.buf) < len(dst) && !p.closed {
		timedOut := false
		timer := time.AfterFunc(coalesceWindow, func() {
			p.mu.Lock()
			timedOut = true
			p.cond.Broadcast()
			p.mu.Unlock()
		})
		for len(p.buf) < len(dst) && !p.closed && !timedOut {
			p.cond.Wait()
		}
		timer.Stop()
	}

	n := copy(dst, p.buf)
	p.buf = p.buf[n:]
	return n, nil
}

// Close closes the pipe, unblocking any waiting readers.
func (p *BufferedPipe) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.closed = true
	p.cond.Broadcast()
	return nil
}
