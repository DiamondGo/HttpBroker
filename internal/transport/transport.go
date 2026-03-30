package transport

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// Connector creates a new tunnel connection to the broker.
// It returns an io.ReadWriteCloser that can be used by yamux.
// This interface allows swapping the transport implementation in future
// (e.g., HTTP/2 streaming instead of HTTP/1.1 long-polling).
type Connector interface {
	// Connect registers this client with the broker for the given role and
	// endpoint, then returns a virtual bidirectional connection.
	// role: "consumer" or "provider"
	Connect(brokerBaseURL, role, endpoint string) (io.ReadWriteCloser, error)
}

// HTTPConnector implements Connector using HTTP long-polling.
type HTTPConnector struct {
	PollInterval time.Duration
	PollTimeout  time.Duration
	HTTPClient   *http.Client
}

// connectResponse is the JSON response from POST /tunnel/connect.
type connectResponse struct {
	SessionID string `json:"session_id"`
}

// Connect sends POST /tunnel/connect?role=role&endpoint=endpoint,
// parses the session ID from the JSON response, and returns a new HTTPConn.
func (c *HTTPConnector) Connect(brokerBaseURL, role, endpoint string) (io.ReadWriteCloser, error) {
	client := c.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}

	params := url.Values{}
	params.Set("role", role)
	params.Set("endpoint", endpoint)
	connectURL := brokerBaseURL + "/tunnel/connect?" + params.Encode()

	resp, err := client.Post(connectURL, "application/json", nil)
	if err != nil {
		return nil, fmt.Errorf("transport: failed to connect to broker: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf(
			"transport: broker returned status %d: %s",
			resp.StatusCode,
			string(body),
		)
	}

	var cr connectResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		return nil, fmt.Errorf("transport: failed to parse connect response: %w", err)
	}

	if cr.SessionID == "" {
		return nil, fmt.Errorf("transport: broker returned empty session_id")
	}

	conn := NewHTTPConn(brokerBaseURL, cr.SessionID, c.PollInterval, c.PollTimeout)
	return conn, nil
}
