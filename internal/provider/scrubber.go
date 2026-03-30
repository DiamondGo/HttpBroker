package provider

import (
	"bytes"
	"net"
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

// ScrubConn wraps a net.Conn and scrubs proxy headers from HTTP requests.
// For TLS or non-HTTP traffic, it passes through unchanged.
type ScrubConn struct {
	net.Conn
	scrubDone   bool   // true once we've inspected/modified the first request
	buf         []byte // buffered bytes for inspection
	passThrough bool   // true if traffic is TLS/binary — skip scrubbing
}

// NewScrubConn wraps conn with HTTP proxy header scrubbing.
func NewScrubConn(conn net.Conn) *ScrubConn {
	return &ScrubConn{
		Conn: conn,
	}
}

// Write inspects data on first call. If HTTP, scrubs proxy headers.
// After the first HTTP request headers are processed, passes through directly.
func (s *ScrubConn) Write(p []byte) (int, error) {
	// Fast path: already decided to pass through or already scrubbed.
	if s.passThrough || s.scrubDone {
		return s.Conn.Write(p)
	}

	// First call: inspect the data.
	if len(p) == 0 {
		return 0, nil
	}

	// TLS ClientHello starts with 0x16.
	if p[0] == 0x16 {
		s.passThrough = true
		return s.Conn.Write(p)
	}

	// Check if it starts with an HTTP method.
	if !isHTTPMethod(p) {
		s.passThrough = true
		return s.Conn.Write(p)
	}

	// It looks like HTTP. Buffer the data and look for end of headers.
	s.buf = append(s.buf, p...)

	headerEnd := bytes.Index(s.buf, []byte("\r\n\r\n"))
	if headerEnd < 0 {
		// Headers not complete yet. For simplicity (best-effort), if headers
		// span multiple writes we just flush what we have and pass through.
		s.scrubDone = true
		n, err := s.Conn.Write(s.buf)
		s.buf = nil
		// Return original p length to caller since we accepted all of p.
		if err != nil {
			return 0, err
		}
		_ = n
		return len(p), nil
	}

	// We have complete headers. Scrub proxy headers and write.
	headerEnd += 4 // include the "\r\n\r\n"
	headerSection := s.buf[:headerEnd]
	remainder := s.buf[headerEnd:]

	scrubbed := scrubProxyHeaders(headerSection)

	// Reset scrubber state so the next HTTP request on this keep-alive
	// connection will also be scrubbed.
	s.scrubDone = false
	s.buf = nil

	// Write scrubbed headers.
	if _, err := s.Conn.Write(scrubbed); err != nil {
		return 0, err
	}

	// Write any body data that was buffered after headers.
	// Process remainder through Write() so subsequent request headers
	// in the same buffer are also scrubbed.
	if len(remainder) > 0 {
		if _, err := s.Write(remainder); err != nil {
			return 0, err
		}
	}

	return len(p), nil
}

// isHTTPMethod checks if p starts with a known HTTP method.
func isHTTPMethod(p []byte) bool {
	for _, method := range httpMethods {
		if len(p) >= len(method) && string(p[:len(method)]) == method {
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
