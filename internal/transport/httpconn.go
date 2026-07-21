package transport

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// HTTPConn implements io.ReadWriteCloser over HTTP long-polling.
//
// Design:
// 1. A dedicated long-polling connection continuously receives data from the broker.
// 2. Write() buffers data and sends it via temporary connections, coalescing
//    back-to-back writes into as few requests as possible (see writeCoalesceWindow).
// 3. All responses (including acknowledgments of writes) come through the long-polling connection.
//
// This design eliminates the need to wait for the previous poll to complete before sending new data.
//
// Transport failure detection:
// When the poll loop encounters a fatal error (broker gone, session invalid), it closes
// the readPipe AND signals the transportFailedCh channel. Callers can select on
// TransportFailed() to distinguish a broker-level failure from a yamux-level close.
type HTTPConn struct {
	sessionID  string
	pollURL    string // e.g. http://broker:8080/tunnel/{id}/poll
	deleteURL  string // e.g. http://broker:8080/tunnel/{id}
	httpClient *http.Client
	authToken  string // Optional bearer token for authentication

	// Read-side: data received from long-polling connection
	readPipe *BufferedPipe

	// transportFailedCh is closed when the HTTP transport itself fails
	// (network error, 404, 401). This lets callers distinguish a broker-level
	// failure from a yamux session close caused by the remote peer (e.g. the
	// broker closing the consumer's yamux session because the provider left).
	transportFailedCh chan struct{}
	transportFailOnce sync.Once

	closed int32 // atomic: 0=open, 1=closed
	stopCh chan struct{}
	wg     sync.WaitGroup

	// Write-side coalescing: yamux always issues a frame's header and body as
	// two back-to-back Write() calls (and, under load, many frames queue up
	// while a previous send is still in flight). Sending each Write() as its
	// own HTTP request is fine on a near-zero-latency link, but once real
	// network RTT is involved (e.g. through a public reverse proxy) it turns
	// every tiny write into a full round trip and craters throughput. See
	// writeCoalesceWindow (set in NewHTTPConn) for why a short fixed window
	// is used instead of a purely race-based (zero-wait) merge.
	writeMu             sync.Mutex
	writeBuf            []byte
	writeFlightOn       bool
	writeCoalesceWindow time.Duration
}

// NewHTTPConn creates an HTTPConn and starts the background poll goroutine.
// pollInterval is the minimum time to wait between polls when no data is available.
// The actual long-poll timeout is controlled by the broker's poll_timeout configuration.
// httpClient is the HTTP client to use for all requests (allows custom TLS config).
// authToken is the optional bearer token for authentication.
// writeCoalesceWindow is the delay before the *first* send of an idle-to-active
// write burst, giving any immediately-following Write() calls (most reliably,
// yamux's header+body pair for the same frame) a chance to land in the same
// buffer and go out as a single HTTP request. It is paid once per burst, not
// per Write() call: once a request is in flight, any further writes queue up
// and are picked up on the next iteration with no additional wait. Pass <= 0
// to use DefaultCoalesceWindow (2ms) — negligible against real WAN round
// trips (100ms+) but bounds the worst case added latency for a
// near-zero-latency (e.g. same-host/LAN) deployment.
func NewHTTPConn(brokerBaseURL, sessionID string, pollInterval time.Duration, httpClient *http.Client, authToken string, writeCoalesceWindow time.Duration) *HTTPConn {
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 0}
	}
	if writeCoalesceWindow <= 0 {
		writeCoalesceWindow = DefaultCoalesceWindow
	}

	c := &HTTPConn{
		sessionID:           sessionID,
		pollURL:             fmt.Sprintf("%s/tunnel/%s/poll", brokerBaseURL, sessionID),
		deleteURL:           fmt.Sprintf("%s/tunnel/%s", brokerBaseURL, sessionID),
		httpClient:          httpClient,
		authToken:           authToken,
		readPipe:            NewBufferedPipe(),
		transportFailedCh:   make(chan struct{}),
		stopCh:              make(chan struct{}),
		writeCoalesceWindow: writeCoalesceWindow,
	}

	c.wg.Add(1)
	go c.pollLoop(pollInterval)

	return c
}

// TransportFailed returns a channel that is closed when the HTTP transport
// itself fails (network error, broker gone, session invalid). This is distinct
// from a yamux session close caused by the remote peer.
//
// Callers should select on both yamuxSess.CloseChan() and conn.TransportFailed()
// to distinguish provider disconnects (yamux close, transport still alive) from
// broker disconnects (transport failed).
func (c *HTTPConn) TransportFailed() <-chan struct{} {
	return c.transportFailedCh
}

// signalTransportFailed closes transportFailedCh exactly once.
func (c *HTTPConn) signalTransportFailed() {
	c.transportFailOnce.Do(func() {
		close(c.transportFailedCh)
	})
}

// Read blocks until data is available from the broker's response.
func (c *HTTPConn) Read(p []byte) (int, error) {
	return c.readPipe.Read(p)
}

// Write buffers data and ensures it is sent to the broker, coalescing
// back-to-back writes into as few HTTP requests as possible (see
// writeCoalesceWindow). It returns once the data is queued for sending, not
// once the broker has acknowledged it — matching how a real net.Conn's
// Write() only guarantees local queuing, not peer delivery. Any send failure
// is reported asynchronously via TransportFailed(), the same mechanism
// already used by the poll loop.
func (c *HTTPConn) Write(p []byte) (int, error) {
	if atomic.LoadInt32(&c.closed) == 1 {
		return 0, io.ErrClosedPipe
	}

	if len(p) == 0 {
		return 0, nil
	}

	c.writeMu.Lock()
	c.writeBuf = append(c.writeBuf, p...)
	if c.writeFlightOn {
		c.writeMu.Unlock()
		return len(p), nil
	}
	c.writeFlightOn = true
	c.writeMu.Unlock()

	c.wg.Add(1)
	go c.flushLoop()

	return len(p), nil
}

// flushLoop drains writeBuf and sends it to the broker, repeating until the
// buffer is empty. The first send of a burst waits writeCoalesceWindow to
// give immediately-following Write() calls a chance to merge in; subsequent
// sends within the same burst fire as soon as the previous one completes,
// picking up whatever accumulated during that round trip.
func (c *HTTPConn) flushLoop() {
	defer c.wg.Done()

	first := true
	for {
		if first {
			time.Sleep(c.writeCoalesceWindow)
			first = false
		}

		c.writeMu.Lock()
		buf := c.writeBuf
		c.writeBuf = nil
		if len(buf) == 0 {
			c.writeFlightOn = false
			c.writeMu.Unlock()
			return
		}
		c.writeMu.Unlock()

		if err := c.doSend(buf); err != nil {
			log.Printf("httpconn: write failed: %v", err)
			c.signalTransportFailed()
			c.readPipe.Close()
			c.writeMu.Lock()
			c.writeFlightOn = false
			c.writeMu.Unlock()
			return
		}
	}
}

// doSend POSTs buf to the broker as a single send-only request.
func (c *HTTPConn) doSend(buf []byte) error {
	req, err := http.NewRequest(http.MethodPost, c.pollURL, bytes.NewReader(buf))
	if err != nil {
		return fmt.Errorf("failed to create send request: %w", err)
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	req.Header.Set("X-Send-Only", "true") // Signal to broker this is a send-only request

	// Add authentication token if configured
	if c.authToken != "" {
		req.Header.Set("Authorization", "Bearer "+c.authToken)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send data: %w", err)
	}
	defer resp.Body.Close()

	// Read and discard any response body (broker should not send data back on send-only requests)
	io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("broker returned status %d", resp.StatusCode)
	}
	return nil
}

// Close stops polling and sends DELETE /tunnel/{id} to the broker.
func (c *HTTPConn) Close() error {
	if !atomic.CompareAndSwapInt32(&c.closed, 0, 1) {
		return nil // already closed
	}

	close(c.stopCh)
	c.wg.Wait()

	// Close the read pipe so any blocked readers get EOF.
	c.readPipe.Close()

	// Send DELETE to broker to clean up the session.
	req, err := http.NewRequest(http.MethodDelete, c.deleteURL, nil)
	if err == nil {
		// Add authentication token if configured
		if c.authToken != "" {
			req.Header.Set("Authorization", "Bearer "+c.authToken)
		}
		resp, err := c.httpClient.Do(req)
		if err == nil {
			resp.Body.Close()
		}
	}

	return nil
}

// pollLoop maintains a long-polling connection to continuously receive data from the broker.
// It does NOT send any data - all sends are done via temporary connections in Write().
func (c *HTTPConn) pollLoop(pollInterval time.Duration) {
	defer c.wg.Done()

	const retryDelay = 500 * time.Millisecond

	for {
		// Check if we should stop.
		select {
		case <-c.stopCh:
			return
		default:
		}

		// Create a receive-only long-polling request (no body)
		req, err := http.NewRequest(http.MethodPost, c.pollURL, nil)
		if err != nil {
			log.Printf("httpconn: failed to create poll request: %v", err)
			c.sleepOrStop(retryDelay)
			continue
		}
		req.Header.Set("X-Receive-Only", "true") // Signal to broker this is receive-only

		// Add authentication token if configured
		if c.authToken != "" {
			req.Header.Set("Authorization", "Bearer "+c.authToken)
		}

		resp, err := c.httpClient.Do(req)

		// Handle HTTP errors.
		if err != nil {
			if atomic.LoadInt32(&c.closed) == 1 {
				return
			}
			log.Printf("httpconn: poll error: %v", err)
			// Signal transport failure so callers can detect broker disconnect.
			c.signalTransportFailed()
			c.readPipe.Close()
			return
		}

		// Read response body.
		gotData := false
		switch resp.StatusCode {
		case http.StatusOK:
			data, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				log.Printf("httpconn: error reading response body: %v", err)
				continue
			}
			if len(data) > 0 {
				gotData = true
				if _, err := c.readPipe.Write(data); err != nil {
					// readPipe closed, we should stop.
					return
				}
			}
		case http.StatusNoContent:
			resp.Body.Close()
			// No data, continue polling.
		case http.StatusNotFound, http.StatusUnauthorized:
			// Session not found or unauthorized — broker likely restarted or session expired.
			// Signal transport failure and close the connection so upper layers detect it.
			resp.Body.Close()
			log.Printf("httpconn: session invalid (status %d), signalling transport failure", resp.StatusCode)
			c.signalTransportFailed()
			c.readPipe.Close()
			return
		case http.StatusFound: // 302 redirect
			// Broker redirected the request — this usually means authentication failed
			// or broker has unauthorized_redirect enabled.
			location := resp.Header.Get("Location")
			resp.Body.Close()
			log.Printf("httpconn: broker redirected to %q (status 302) — likely authentication failure or misconfiguration. Check auth_token setting. Signalling transport failure", location)
			c.signalTransportFailed()
			c.readPipe.Close()
			return
		default:
			resp.Body.Close()
			log.Printf("httpconn: unexpected status %d from broker", resp.StatusCode)
		}

		// Only sleep after idle (no data) responses.
		// After receiving data, immediately poll again to minimize latency.
		if !gotData && pollInterval > 0 {
			if !c.sleepOrStop(pollInterval) {
				return
			}
		}
	}
}

// sleepOrStop sleeps for the given duration or returns false if stopCh is closed.
func (c *HTTPConn) sleepOrStop(d time.Duration) bool {
	select {
	case <-c.stopCh:
		return false
	case <-time.After(d):
		return true
	}
}
