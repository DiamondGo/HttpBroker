package consumer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/DiamondGo/pollmux"
	"go.uber.org/zap"
)

// TestPreferStreamMode covers the PollMode -> PreferStream mapping: empty
// (the default, exactly what an unset poll_mode in the config file produces)
// must prefer stream, and only "batch" opts out.
func TestPreferStreamMode(t *testing.T) {
	cases := []struct {
		pollMode string
		want     bool
	}{
		{"", true},
		{"stream", true},
		{"batch", false},
	}
	for _, c := range cases {
		if got := preferStreamMode(c.pollMode); got != c.want {
			t.Errorf("preferStreamMode(%q) = %v, want %v", c.pollMode, got, c.want)
		}
	}
}

// fakeBroker is a minimal stand-in for the real broker: just enough of the
// pollmux wire protocol for a pollmux.Connector to complete Connect() and
// keep polling without erroring, so a test can capture what the consumer
// actually sent on the wire rather than only what a helper function returns
// in isolation.
type fakeBroker struct {
	mu       sync.Mutex
	connects []pollmux.ConnectRequest
}

func (f *fakeBroker) firstConnect() (pollmux.ConnectRequest, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.connects) == 0 {
		return pollmux.ConnectRequest{}, false
	}
	return f.connects[0], true
}

func (f *fakeBroker) server() *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /tunnel/connect", func(w http.ResponseWriter, r *http.Request) {
		var req pollmux.ConnectRequest
		json.NewDecoder(r.Body).Decode(&req)
		f.mu.Lock()
		f.connects = append(f.connects, req)
		f.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(pollmux.ConnectResponse{
			ProtocolVersion: pollmux.ProtocolVersion,
			SessionID:       "test-session",
			PollMode:        pollmux.PollModeBatch,
			Limits: pollmux.Limits{
				MaxSendBytes:     1 << 20,
				PollTimeoutMS:    1000,
				SessionTimeoutMS: 60000,
				PollBufferBytes:  1 << 18,
			},
		})
	})
	// Batch-mode responses forever: the test only needs Connect() to
	// succeed and the poll loop to stay quiet, not real data flow.
	mux.HandleFunc("POST /tunnel/{id}/poll", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("DELETE /tunnel/{id}", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	return httptest.NewServer(mux)
}

// TestClientPrefersStreamModeByDefault confirms Client.Run actually wires
// Config.PollMode through to pollmux.Connector.PreferStream on the real
// connect request. TestPreferStreamMode above only proves the helper
// function's logic is correct in isolation — it would still pass even if
// the Connector literal never referenced it, so this test is what actually
// guards against that wiring being dropped.
func TestClientPrefersStreamModeByDefault(t *testing.T) {
	fb := &fakeBroker{}
	ts := fb.server()
	defer ts.Close()

	logger, _ := zap.NewDevelopment()
	defer logger.Sync()

	client := NewClient(Config{
		BrokerURL:    ts.URL,
		Endpoint:     "ep1",
		Socks5Listen: "127.0.0.1:0",
		PollInterval: 5 * time.Millisecond,
		RetryBackoff: 50 * time.Millisecond,
		// PollMode left unset — this is the point of the test.
	}, logger)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx) }()

	waitForFirstConnect(t, fb)

	req, _ := fb.firstConnect()
	if !req.PreferStreamMode {
		t.Fatal("consumer's connect request did not set prefer_stream_mode — default should prefer stream")
	}

	cancel()
	waitForClientRunToReturn(t, done)
}

// TestClientPollModeBatchDisablesPreferStream confirms the escape hatch:
// PollMode: "batch" in config must reach the wire as prefer_stream_mode
// absent/false, not just resolve to false in the helper function.
func TestClientPollModeBatchDisablesPreferStream(t *testing.T) {
	fb := &fakeBroker{}
	ts := fb.server()
	defer ts.Close()

	logger, _ := zap.NewDevelopment()
	defer logger.Sync()

	client := NewClient(Config{
		BrokerURL:    ts.URL,
		Endpoint:     "ep1",
		Socks5Listen: "127.0.0.1:0",
		PollInterval: 5 * time.Millisecond,
		RetryBackoff: 50 * time.Millisecond,
		PollMode:     "batch",
	}, logger)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx) }()

	waitForFirstConnect(t, fb)

	req, _ := fb.firstConnect()
	if req.PreferStreamMode {
		t.Fatal("consumer's connect request set prefer_stream_mode with PollMode: \"batch\" configured")
	}

	cancel()
	waitForClientRunToReturn(t, done)
}

func waitForFirstConnect(t *testing.T, fb *fakeBroker) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, ok := fb.firstConnect(); ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the consumer to connect")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitForClientRunToReturn(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Client.Run did not return after context cancellation")
	}
}
