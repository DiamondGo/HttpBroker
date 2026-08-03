package broker

import (
	"bytes"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// mockConn is a minimal net.Conn whose Read is driven by an injected
// io.Reader and whose Write captures bytes into a buffer. Once Close is
// called, further Writes to *this* connection fail — but Reads keep
// draining whatever the injected reader still has, matching how a
// yamux stream behaves after a local half-close (Close only forbids
// further local writes; it doesn't cut short in-progress local reads).
type mockConn struct {
	mu     sync.Mutex
	reader io.Reader
	buf    bytes.Buffer
	closed bool
}

func (c *mockConn) Read(p []byte) (int, error) { return c.reader.Read(p) }

func (c *mockConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0, errors.New("mockConn: write on closed connection")
	}
	return c.buf.Write(p)
}

func (c *mockConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	return nil
}

func (c *mockConn) written() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.buf.Bytes()...)
}

func (c *mockConn) LocalAddr() net.Addr              { return nil }
func (c *mockConn) RemoteAddr() net.Addr             { return nil }
func (c *mockConn) SetDeadline(time.Time) error      { return nil }
func (c *mockConn) SetReadDeadline(time.Time) error  { return nil }
func (c *mockConn) SetWriteDeadline(time.Time) error { return nil }

// slowReader hands out pre-built chunks one at a time with a delay between
// each, simulating a peer that is still streaming a large/slow response.
type slowReader struct {
	chunks [][]byte
	delay  time.Duration
}

func (r *slowReader) Read(p []byte) (int, error) {
	if len(r.chunks) == 0 {
		return 0, io.EOF
	}
	time.Sleep(r.delay)
	chunk := r.chunks[0]
	r.chunks = r.chunks[1:]
	return copy(p, chunk), nil
}

// TestBridgeConnsDoesNotTruncateSlowDirectionOnFastHalfClose reproduces the
// scenario where one side (the consumer) finishes sending almost instantly
// and its half of the copy completes long before the other side (the
// provider) is done streaming its response back. bridgeConns must not
// truncate the still-in-progress direction: each goroutine may only close
// the connection it writes to, never the connection the other goroutine
// is still writing into.
func TestBridgeConnsDoesNotTruncateSlowDirectionOnFastHalfClose(t *testing.T) {
	const chunkSize = 4096
	const numChunks = 10

	var want []byte
	chunks := make([][]byte, numChunks)
	for i := range chunks {
		chunk := bytes.Repeat([]byte{byte('A' + i)}, chunkSize)
		chunks[i] = chunk
		want = append(want, chunk...)
	}

	// consumer "finishes sending" almost instantly (short request, fast EOF).
	consumer := &mockConn{reader: strings.NewReader("request")}
	// provider streams a slow, multi-chunk response back.
	provider := &mockConn{reader: &slowReader{chunks: chunks, delay: 5 * time.Millisecond}}

	done := make(chan struct{})
	go func() {
		bridgeConns(provider, consumer)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("bridgeConns did not return in time — a direction is likely deadlocked")
	}

	got := consumer.written()
	if !bytes.Equal(got, want) {
		t.Fatalf("consumer received truncated/corrupted data: got %d bytes, want %d bytes", len(got), len(want))
	}
}

// TestBridgeConnsCopiesBothDirections is a basic sanity check that
// bridgeConns relays data both ways and returns once both sides finish.
func TestBridgeConnsCopiesBothDirections(t *testing.T) {
	a := &mockConn{reader: strings.NewReader("hello from a")}
	b := &mockConn{reader: strings.NewReader("hello from b")}

	done := make(chan struct{})
	go func() {
		bridgeConns(a, b)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("bridgeConns did not return in time")
	}

	if got := a.written(); string(got) != "hello from b" {
		t.Errorf("a received %q, want %q", got, "hello from b")
	}
	if got := b.written(); string(got) != "hello from a" {
		t.Errorf("b received %q, want %q", got, "hello from a")
	}
}
