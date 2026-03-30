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
// It maintains a background goroutine that continuously POSTs to the broker.
// Each POST sends buffered write data and receives data into the read buffer.
//
// Write() buffers data for the next poll.
// Read() blocks until the poll goroutine receives data.
// Close() stops the poll goroutine and deletes the session on the broker.
type HTTPConn struct {
	sessionID  string
	pollURL    string // e.g. http://broker:8080/tunnel/{id}/poll
	deleteURL  string // e.g. http://broker:8080/tunnel/{id}
	httpClient *http.Client

	// Write-side: data buffered for next poll's request body
	writeMu  sync.Mutex
	writeBuf bytes.Buffer

	// Read-side: data received from poll responses
	readPipe *BufferedPipe

	closed int32 // atomic: 0=open, 1=closed
	stopCh chan struct{}
	wg     sync.WaitGroup
}

// NewHTTPConn creates an HTTPConn and starts the background poll goroutine.
// pollInterval is the minimum time to wait between polls when data is flowing.
func NewHTTPConn(brokerBaseURL, sessionID string, pollInterval time.Duration) *HTTPConn {
	c := &HTTPConn{
		sessionID:  sessionID,
		pollURL:    fmt.Sprintf("%s/tunnel/%s/poll", brokerBaseURL, sessionID),
		deleteURL:  fmt.Sprintf("%s/tunnel/%s", brokerBaseURL, sessionID),
		httpClient: &http.Client{Timeout: 0},
		readPipe:   NewBufferedPipe(),
		stopCh:     make(chan struct{}),
	}

	c.wg.Add(1)
	go c.pollLoop(pollInterval)

	return c
}

// Read blocks until data is available from the broker's response.
func (c *HTTPConn) Read(p []byte) (int, error) {
	return c.readPipe.Read(p)
}

// Write buffers data to be sent in the next poll's request body.
func (c *HTTPConn) Write(p []byte) (int, error) {
	if atomic.LoadInt32(&c.closed) == 1 {
		return 0, io.ErrClosedPipe
	}

	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.writeBuf.Write(p)
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
		resp, err := c.httpClient.Do(req)
		if err == nil {
			resp.Body.Close()
		}
	}

	return nil
}

// pollLoop continuously POSTs to the broker, sending buffered write data
// and receiving data into the read pipe.
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

		// Step 1: Collect pending write data.
		c.writeMu.Lock()
		var body []byte
		if c.writeBuf.Len() > 0 {
			body = make([]byte, c.writeBuf.Len())
			copy(body, c.writeBuf.Bytes())
			c.writeBuf.Reset()
		}
		c.writeMu.Unlock()

		// Step 2: POST to pollURL with write data as body.
		var reqBody io.Reader
		if len(body) > 0 {
			reqBody = bytes.NewReader(body)
			log.Printf("httpconn: poll sending %d bytes", len(body))
		}

		req, err := http.NewRequest(http.MethodPost, c.pollURL, reqBody)
		if err != nil {
			log.Printf("httpconn: failed to create request: %v", err)
			c.sleepOrStop(retryDelay)
			continue
		}
		req.Header.Set("Content-Type", "application/octet-stream")

		resp, err := c.httpClient.Do(req)

		// Step 3: Handle HTTP errors.
		if err != nil {
			if atomic.LoadInt32(&c.closed) == 1 {
				return
			}
			log.Printf("httpconn: poll error: %v", err)
			c.sleepOrStop(retryDelay)
			continue
		}

		// Step 4: Read response body.
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
				log.Printf("httpconn: poll received %d bytes", len(data))
				gotData = true
				if _, err := c.readPipe.Write(data); err != nil {
					// readPipe closed, we should stop.
					return
				}
			}
		case http.StatusNoContent:
			resp.Body.Close()
			// No data, continue polling.
		default:
			resp.Body.Close()
			log.Printf("httpconn: unexpected status %d from broker", resp.StatusCode)
		}

		// Step 5: Only sleep after idle (no data) responses.
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
