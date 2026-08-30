package provider

import (
	"bytes"
	"net"
	"strconv"
	"strings"
)

// proxyHeaders lists HTTP headers that reveal proxy usage.
var proxyHeaders = []string{
	"X-Forwarded-For",
	"X-Real-IP",
	"Via",
	"Forwarded",
	"Proxy-Connection",
	"Proxy-Authorization",
	"X-Proxy-ID",
}

// httpMethods lists HTTP method prefixes used to detect plain HTTP traffic.
var httpMethods = []string{
	"GET ", "POST ", "PUT ", "DELETE ", "HEAD ", "OPTIONS ", "PATCH ", "CONNECT ",
}

// maxHeaderBytes bounds how many bytes may be buffered while waiting for a
// request's headers to complete. Legitimate request headers (even with large
// cookie blocks) fit well under this; it exists so a misdetected binary
// stream that merely *starts* with an HTTP method cannot grow the buffer
// without limit. Once crossed, the connection switches to pass-through.
const maxHeaderBytes = 64 << 10

// ScrubConn wraps a net.Conn and scrubs proxy headers from HTTP requests.
// For TLS or non-HTTP traffic, it passes through unchanged.
//
// It is a small request-boundary state machine, because a keep-alive
// connection carries many requests and the scrubber must resynchronize after
// each one:
//
//   - atBoundary: the next bytes start a new request. Bytes are inspected:
//     a TLS ClientHello or any non-HTTP start switches the connection to
//     pass-through; otherwise headers are buffered until "\r\n\r\n" arrives,
//     proxy headers are stripped, and the body is accounted for via
//     Content-Length (skipped exactly) or, when the length is unknown
//     (chunked, connection close, malformed), the connection switches to
//     pass-through so body bytes are never mistaken for headers.
//   - skipping body: exactly the declared Content-Length bytes are passed
//     through untouched, then the scrubber is back at a request boundary.
//   - passThrough: everything goes straight to the underlying conn.
//
// This matters because a naive "scrub the first request, then stop" scrubber
// corrupts POST bodies that happen to contain lines starting with a proxy
// header name (e.g. "Via: ..." inside an uploaded log dump), and a naive
// "treat the rest of each write as the next request" scrubber corrupts
// bodies that merely *start* with an HTTP method.
type ScrubConn struct {
	net.Conn
	passThrough bool   // once true, every write goes straight to Conn
	skipBody    int    // body bytes of the current request still to pass through
	buf         []byte // buffered bytes of the current request's headers
}

// NewScrubConn wraps conn with HTTP proxy header scrubbing.
func NewScrubConn(conn net.Conn) *ScrubConn {
	return &ScrubConn{Conn: conn}
}

// Write implements io.Writer with the contract io.Copy relies on: it accepts
// all of p (returning len(p), nil) unless the underlying conn errors.
func (s *ScrubConn) Write(p []byte) (int, error) {
	if s.passThrough {
		return s.Conn.Write(p)
	}
	if len(p) == 0 {
		return 0, nil
	}

	// Skip exactly the declared body of the previous request, then fall back
	// to boundary inspection for whatever follows in the same write.
	if s.skipBody > 0 {
		n := min(len(p), s.skipBody)
		if _, err := s.writeAll(p[:n]); err != nil {
			return n, err
		}
		s.skipBody -= n
		p = p[n:]
		if len(p) == 0 {
			return n, nil
		}
	}

	// Already accumulating this request's headers from earlier writes: keep
	// appending. Re-running the boundary heuristic here would be wrong — the
	// next byte of "GET " (e.g. "E") is not itself a method prefix.
	if len(s.buf) > 0 {
		return s.absorbHeaderBytes(p)
	}

	// At a request boundary: is this the start of another HTTP request?
	if !looksLikeHTTPRequest(p) {
		// TLS ClientHello (0x16...), other binary, or a body whose declared
		// length we could not track. Whatever this is, the rest of the
		// connection is not scrub-able request headers.
		s.passThrough = true
		return s.Conn.Write(p)
	}

	return s.absorbHeaderBytes(p)
}

// absorbHeaderBytes appends p to the header buffer and, once the header
// block is complete, scrubs and forwards it. On success it returns
// (len(p), nil): every byte of the call was either written to the target or
// is held in the buffer, which is the contract io.Copy relies on.
func (s *ScrubConn) absorbHeaderBytes(p []byte) (int, error) {
	s.buf = append(s.buf, p...)

	headerEnd := bytes.Index(s.buf, []byte("\r\n\r\n"))
	if headerEnd < 0 {
		// Headers not complete yet; buffer and wait for the next write.
		if len(s.buf) > maxHeaderBytes {
			// Not really HTTP after all (no header end in sight). Flush what
			// we held and stop scrubbing this connection.
			if _, err := s.writeAll(s.buf); err != nil {
				return 0, err
			}
			s.buf = s.buf[:0]
			s.passThrough = true
			return len(p), nil
		}
		return len(p), nil
	}

	// Complete headers in hand. Scrub and forward them, then account for the
	// body so the next request can be found (or the connection can pass
	// through) instead of guessing.
	headerEnd += 4 // include the "\r\n\r\n"
	headerSection := s.buf[:headerEnd]
	remainder := s.buf[headerEnd:]
	s.buf = s.buf[:0]

	if _, err := s.writeAll(scrubProxyHeaders(headerSection)); err != nil {
		return 0, err
	}

	s.setBodyAccounting(headerSection)

	// Any bytes after the headers were part of the same write: run them
	// through the same state machine so a pipelined next request already
	// sitting in the buffer is scrubbed too.
	if len(remainder) > 0 {
		if _, err := s.Write(remainder); err != nil {
			return 0, err
		}
	}

	return len(p), nil
}

// setBodyAccounting decides what follows the just-written headers.
func (s *ScrubConn) setBodyAccounting(header []byte) {
	skip, mode := bodyPlan(header)
	switch mode {
	case bodyLength:
		s.skipBody = skip // 0 (Content-Length: 0) just means: still at a boundary
	case bodyPassThrough:
		// Chunked / connection-close: the body length is not trackable without
		// a full chunk decoder, so stop scrubbing this connection rather than
		// risk eating body bytes as headers.
		s.passThrough = true
	}
	// bodyNone: no body expected (GET/HEAD/...); the next bytes are the start
	// of the next request, so stay at a boundary.
}

// writeAll writes all of p to the underlying conn, absorbing the (rare)
// short writes net.Conn can report.
func (s *ScrubConn) writeAll(p []byte) (int, error) {
	total := 0
	for total < len(p) {
		n, err := s.Conn.Write(p[total:])
		total += n
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// Body plan modes for what follows a request's headers.
const (
	bodyNone        = iota // no body expected; next bytes start the next request
	bodyLength             // exactly skip body bytes follow
	bodyPassThrough        // a body exists but its length is not trackable
)

// bodyPlan inspects the request headers and decides how to account for the
// body that follows them (see bodyLength's callers for why guessing is bad).
func bodyPlan(header []byte) (skip int, mode int) {
	var contentLength int = -1
	var chunked, connClose bool

	rest := header
	for len(rest) > 0 {
		idx := bytes.IndexByte(rest, '\n')
		if idx < 0 {
			break
		}
		line := bytes.TrimRight(rest[:idx], "\r")
		rest = rest[idx+1:]

		ci := bytes.IndexByte(line, ':')
		if ci <= 0 {
			continue // request line or malformed
		}
		name := line[:ci]
		value := bytes.TrimSpace(line[ci+1:])

		switch {
		case bytes.EqualFold(name, []byte("content-length")):
			if n, err := strconv.Atoi(string(value)); err == nil && n >= 0 {
				contentLength = n
			}
		case bytes.EqualFold(name, []byte("transfer-encoding")):
			chunked = true
		case bytes.EqualFold(name, []byte("connection")):
			connClose = strings.Contains(strings.ToLower(string(value)), "close")
		}
	}

	switch {
	case connClose || chunked:
		return 0, bodyPassThrough
	case contentLength >= 0:
		return contentLength, bodyLength
	default:
		return 0, bodyNone
	}
}

// looksLikeHTTPRequest reports whether p could start an HTTP request: either
// p begins with a complete method token ("GET ") or p itself is a prefix of
// one (the first write of a connection can carry just "G" or "GET"). In the
// second case the caller buffers p and waits for the rest; if the next bytes
// complete a method the request is scrubbed, if not the maxHeaderBytes guard
// flushes the buffer and switches the connection to pass-through.
func looksLikeHTTPRequest(p []byte) bool {
	for _, method := range httpMethods {
		if len(p) < len(method) {
			if bytes.Equal(p, []byte(method)[:len(p)]) {
				return true
			}
			continue
		}
		if bytes.HasPrefix(p, []byte(method)) {
			return true
		}
	}
	return false
}

// scrubProxyHeaders removes proxy-identifying headers from raw HTTP header bytes.
// The input includes the trailing \r\n\r\n.
func scrubProxyHeaders(headerBytes []byte) []byte {
	// Split into lines (each ending with \r\n).
	raw := string(headerBytes)
	lines := strings.Split(raw, "\r\n")

	var result []string
	for _, line := range lines {
		if shouldRemoveLine(line) {
			continue
		}
		result = append(result, line)
	}

	return []byte(strings.Join(result, "\r\n"))
}

// shouldRemoveLine checks if a header line matches a proxy header to remove.
func shouldRemoveLine(line string) bool {
	for _, h := range proxyHeaders {
		prefix := h + ":"
		if len(line) >= len(prefix) && strings.EqualFold(line[:len(prefix)], prefix) {
			return true
		}
	}
	return false
}
