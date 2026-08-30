package provider

import (
	"bytes"
	"errors"
	"io"
	"net"
	"strconv"
	"testing"
	"time"
)

// fakeConn is a minimal net.Conn that captures everything written to it.
type fakeConn struct {
	mu     bytesBuffer
	closed bool
}

// bytesBuffer exists so fakeConn stays a net.Conn without importing sync
// into a hot test path; scrubber writes in tests are single-goroutine.
type bytesBuffer = bytes.Buffer

func (c *fakeConn) Read(p []byte) (int, error)       { return 0, io.EOF }
func (c *fakeConn) Write(p []byte) (int, error)      { return c.mu.Write(p) }
func (c *fakeConn) Close() error                     { c.closed = true; return nil }
func (c *fakeConn) LocalAddr() net.Addr              { return nil }
func (c *fakeConn) RemoteAddr() net.Addr             { return nil }
func (c *fakeConn) SetDeadline(time.Time) error      { return nil }
func (c *fakeConn) SetReadDeadline(time.Time) error  { return nil }
func (c *fakeConn) SetWriteDeadline(time.Time) error { return nil }

// written returns everything the scrubber forwarded to the target.
func (c *fakeConn) written() []byte { return c.mu.Bytes() }

// writeAll is a test helper honouring the io.Writer contract.
func writeAll(w io.Writer, data string) error {
	n, err := w.Write([]byte(data))
	if n < len(data) && err == nil {
		err = errors.New("short write")
	}
	return err
}

// TestScrubConn_StripsProxyHeaders is the basic case: a GET with proxy
// headers arrives in one write; they must be removed.
func TestScrubConn_StripsProxyHeaders(t *testing.T) {
	inner := &fakeConn{}
	s := NewScrubConn(inner)

	req := "GET /index.html HTTP/1.1\r\nHost: example.com\r\nX-Forwarded-For: 1.2.3.4\r\nVia: proxy1\r\nUser-Agent: test\r\n\r\n"
	if err := writeAll(s, req); err != nil {
		t.Fatalf("write: %v", err)
	}

	got := string(inner.written())
	want := "GET /index.html HTTP/1.1\r\nHost: example.com\r\nUser-Agent: test\r\n\r\n"
	if got != want {
		t.Fatalf("scrubbed output:\n%q\nwant:\n%q", got, want)
	}
}

// TestScrubConn_PostBodyWithHeaderLikeLinesIsNotCorrupted covers the main
// corruption bug: a POST body that contains lines starting with proxy header
// names ("Via: ...") must pass through untouched. The scrubber knows the body
// length from Content-Length, so it must skip exactly those bytes before
// looking for the next request.
func TestScrubConn_PostBodyWithHeaderLikeLinesIsNotCorrupted(t *testing.T) {
	inner := &fakeConn{}
	s := NewScrubConn(inner)

	body := "2024-01-01 request log\nVia: internal-proxy (spoofed line in body)\nX-Forwarded-For: 9.9.9.9\nend\n"
	req := "POST /upload HTTP/1.1\r\nHost: example.com\r\nContent-Type: text/plain\r\nContent-Length: " +
		itoa(len(body)) + "\r\nX-Real-IP: 1.1.1.1\r\n\r\n" + body

	if err := writeAll(s, req); err != nil {
		t.Fatalf("write: %v", err)
	}

	got := string(inner.written())
	want := "POST /upload HTTP/1.1\r\nHost: example.com\r\nContent-Type: text/plain\r\nContent-Length: " +
		itoa(len(body)) + "\r\n\r\n" + body
	if got != want {
		t.Fatalf("scrubbed output:\n%q\nwant:\n%q", got, want)
	}
}

// TestScrubConn_KeepAliveSecondRequestIsScrubbed verifies the scrubber
// resynchronizes after a Content-Length body: a second request on the same
// keep-alive connection must be scrubbed too.
func TestScrubConn_KeepAliveSecondRequestIsScrubbed(t *testing.T) {
	inner := &fakeConn{}
	s := NewScrubConn(inner)

	body := "hello"
	first := "POST /a HTTP/1.1\r\nHost: example.com\r\nContent-Length: 5\r\nForwarded: for=1.2.3.4\r\n\r\n" + body
	second := "GET /b HTTP/1.1\r\nHost: example.com\r\nProxy-Authorization: Basic xyz\r\n\r\n"

	// Deliver in two writes, as the tunnel would.
	if err := writeAll(s, first); err != nil {
		t.Fatalf("write first: %v", err)
	}
	if err := writeAll(s, second); err != nil {
		t.Fatalf("write second: %v", err)
	}

	got := string(inner.written())
	want := "POST /a HTTP/1.1\r\nHost: example.com\r\nContent-Length: 5\r\n\r\n" + body +
		"GET /b HTTP/1.1\r\nHost: example.com\r\n\r\n"
	if got != want {
		t.Fatalf("scrubbed output:\n%q\nwant:\n%q", got, want)
	}
}

// TestScrubConn_BodyStartingWithHTTPMethodIsNotCorrupted is the second
// corruption case: a body whose first bytes look like an HTTP method. With a
// known Content-Length the scrubber must not mistake the body for a new
// request.
func TestScrubConn_BodyStartingWithHTTPMethodIsNotCorrupted(t *testing.T) {
	inner := &fakeConn{}
	s := NewScrubConn(inner)

	// A POSTed HTTP request capture: the body starts with "GET ".
	body := "GET / HTTP/1.1\r\nHost: captured\r\nVia: should-stay-in-body\r\n\r\n"
	req := "POST /capture HTTP/1.1\r\nHost: example.com\r\nContent-Length: " +
		itoa(len(body)) + "\r\n\r\n" + body

	if err := writeAll(s, req); err != nil {
		t.Fatalf("write: %v", err)
	}

	got := string(inner.written())
	want := "POST /capture HTTP/1.1\r\nHost: example.com\r\nContent-Length: " +
		itoa(len(body)) + "\r\n\r\n" + body
	if got != want {
		t.Fatalf("scrubbed output:\n%q\nwant:\n%q", got, want)
	}
}

// TestScrubConn_ChunkedBodyPassesThrough verifies that an untrackable body
// (Transfer-Encoding: chunked) switches the connection to pass-through
// instead of guessing: nothing is lost, headers of the first request are
// still scrubbed.
func TestScrubConn_ChunkedBodyPassesThrough(t *testing.T) {
	inner := &fakeConn{}
	s := NewScrubConn(inner)

	req := "POST /chunked HTTP/1.1\r\nHost: example.com\r\nTransfer-Encoding: chunked\r\nVia: proxy1\r\n\r\n5\r\nhello\r\n0\r\n\r\n"
	if err := writeAll(s, req); err != nil {
		t.Fatalf("write: %v", err)
	}

	got := string(inner.written())
	want := "POST /chunked HTTP/1.1\r\nHost: example.com\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhello\r\n0\r\n\r\n"
	if got != want {
		t.Fatalf("scrubbed output:\n%q\nwant:\n%q", got, want)
	}
}

// TestScrubConn_TLSPassesThrough: a TLS ClientHello (starts with 0x16) must
// not be treated as HTTP.
func TestScrubConn_TLSPassesThrough(t *testing.T) {
	inner := &fakeConn{}
	s := NewScrubConn(inner)

	// Minimal ClientHello-shaped prefix.
	hello := []byte{0x16, 0x03, 0x01, 0x02, 0x00, 'V', 'i', 'a', ':', 'x'}
	if _, err := s.Write(hello); err != nil {
		t.Fatalf("write: %v", err)
	}

	if !bytes.Equal(inner.written(), hello) {
		t.Fatalf("TLS bytes were modified: got %x, want %x", inner.written(), hello)
	}
}

// TestScrubConn_HostileHeadersSplitAcrossWrites verifies headers spanning
// multiple writes are still buffered and scrubbed once complete.
func TestScrubConn_HostileHeadersSplitAcrossWrites(t *testing.T) {
	inner := &fakeConn{}
	s := NewScrubConn(inner)

	full := "GET /x HTTP/1.1\r\nHost: example.com\r\nVia: split\r\n\r\n"
	// Write one byte at a time — the worst case for buffering.
	for i := 0; i < len(full); i++ {
		if _, err := s.Write([]byte{full[i]}); err != nil {
			t.Fatalf("write byte %d: %v", i, err)
		}
	}

	got := string(inner.written())
	want := "GET /x HTTP/1.1\r\nHost: example.com\r\n\r\n"
	if got != want {
		t.Fatalf("scrubbed output:\n%q\nwant:\n%q", got, want)
	}
}

// TestScrubConn_BoundarySplitMidBody verifies a write boundary landing in
// the middle of a Content-Length body does not lose or duplicate bytes.
func TestScrubConn_BoundarySplitMidBody(t *testing.T) {
	inner := &fakeConn{}
	s := NewScrubConn(inner)

	body := "0123456789"
	req := "POST /b HTTP/1.1\r\nHost: example.com\r\nContent-Length: 10\r\nX-Proxy-ID: 7\r\n\r\n" + body

	// Headers + 3 body bytes in the first write, the rest in the second.
	split := len(req) - len(body) + 3
	if err := writeAll(s, req[:split]); err != nil {
		t.Fatalf("write part 1: %v", err)
	}
	if err := writeAll(s, req[split:]); err != nil {
		t.Fatalf("write part 2: %v", err)
	}

	got := string(inner.written())
	want := "POST /b HTTP/1.1\r\nHost: example.com\r\nContent-Length: 10\r\n\r\n" + body
	if got != want {
		t.Fatalf("scrubbed output:\n%q\nwant:\n%q", got, want)
	}
}

// TestScrubConn_PipelinedRequestsInOneWrite verifies two requests in a single
// write are both scrubbed.
func TestScrubConn_PipelinedRequestsInOneWrite(t *testing.T) {
	inner := &fakeConn{}
	s := NewScrubConn(inner)

	body := "ab"
	full := "POST /1 HTTP/1.1\r\nHost: e\r\nContent-Length: 2\r\nVia: a\r\n\r\n" + body +
		"GET /2 HTTP/1.1\r\nHost: e\r\nVia: b\r\n\r\n"

	if err := writeAll(s, full); err != nil {
		t.Fatalf("write: %v", err)
	}

	got := string(inner.written())
	want := "POST /1 HTTP/1.1\r\nHost: e\r\nContent-Length: 2\r\n\r\n" + body +
		"GET /2 HTTP/1.1\r\nHost: e\r\n\r\n"
	if got != want {
		t.Fatalf("scrubbed output:\n%q\nwant:\n%q", got, want)
	}
}

// TestScrubConn_ContentLengthZeroStaysAtBoundary: a zero-length body means
// the next bytes are the next request, not body data.
func TestScrubConn_ContentLengthZeroStaysAtBoundary(t *testing.T) {
	inner := &fakeConn{}
	s := NewScrubConn(inner)

	full := "PUT /z HTTP/1.1\r\nHost: e\r\nContent-Length: 0\r\nVia: a\r\n\r\n" +
		"GET /after HTTP/1.1\r\nHost: e\r\nVia: b\r\n\r\n"

	if err := writeAll(s, full); err != nil {
		t.Fatalf("write: %v", err)
	}

	got := string(inner.written())
	want := "PUT /z HTTP/1.1\r\nHost: e\r\nContent-Length: 0\r\n\r\n" +
		"GET /after HTTP/1.1\r\nHost: e\r\n\r\n"
	if got != want {
		t.Fatalf("scrubbed output:\n%q\nwant:\n%q", got, want)
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
