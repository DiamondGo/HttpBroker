package broker

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/DiamondGo/pollmux"
	"go.uber.org/zap"
)

// TestResolvePollMode is a table test for the pure PollMode-resolution
// function: empty defaults to stream, "batch" opts out, and anything else
// passes through unchanged so pollmux.ServerConfig.check() rejects a typo
// loudly at startup instead of this function silently coercing it.
func TestResolvePollMode(t *testing.T) {
	cases := []struct {
		configured string
		want       string
	}{
		{"", pollmux.PollModeStream},
		{"batch", pollmux.PollModeBatch},
		{"stream", pollmux.PollModeStream},
		{"nonsense", "nonsense"}, // passed through; check() panics on this at startup
	}
	for _, c := range cases {
		if got := resolvePollMode(c.configured); got != c.want {
			t.Errorf("resolvePollMode(%q) = %q, want %q", c.configured, got, c.want)
		}
	}
}

// connectStreamPreferring POSTs a raw connect request asking the server to
// negotiate stream mode, and returns the poll_mode the server actually
// negotiated. It exercises the real wire protocol end to end — the exact
// path a pollmux.Connector with PreferStream=true takes — rather than only
// the resolvePollMode helper in isolation.
func connectStreamPreferring(t *testing.T, addr string) string {
	t.Helper()
	body := strings.NewReader(`{"protocol_version":1,"meta":{"role":"consumer","endpoint":"ep1"},"prefer_stream_mode":true}`)
	resp, err := http.Post("http://"+addr+"/tunnel/connect", "application/json", body)
	if err != nil {
		t.Fatalf("connect request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("connect status = %d, want 200", resp.StatusCode)
	}
	var cr pollmux.ConnectResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		t.Fatalf("decode connect response: %v", err)
	}
	if cr.PollMode == "" {
		t.Fatal("connect response carried no poll_mode")
	}
	return cr.PollMode
}

// TestBrokerDefaultsToStreamModeWhenClientPrefersIt is the regression test
// for the whole point of this change: a broker.Server built from a zero-
// valued Config (PollMode left unset, exactly what happens when the config
// file has no poll_mode line) must negotiate stream mode with a client that
// asks for it — not silently stay on batch.
func TestBrokerDefaultsToStreamModeWhenClientPrefersIt(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	defer logger.Sync()

	cfg := Config{
		ListenAddr:     "127.0.0.1:18099",
		PollTimeout:    1 * time.Second,
		SessionTimeout: 5 * time.Minute,
	}
	srv := NewServer(cfg, logger)
	go func() {
		if err := srv.Start(); err != nil && err != http.ErrServerClosed {
			t.Logf("server error: %v", err)
		}
	}()
	defer srv.Stop(context.Background())
	waitForServerReady(t, "127.0.0.1:18099")

	if got := connectStreamPreferring(t, "127.0.0.1:18099"); got != pollmux.PollModeStream {
		t.Fatalf("negotiated poll_mode = %q, want %q (default Config.PollMode must prefer stream)",
			got, pollmux.PollModeStream)
	}
}

// TestBrokerPollModeBatchOverridesClientPreference confirms the escape
// hatch still works: an operator who explicitly sets PollMode: "batch"
// gets batch even from a client that asks for stream — the server is
// always the authority in pollmux's negotiation.
func TestBrokerPollModeBatchOverridesClientPreference(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	defer logger.Sync()

	cfg := Config{
		ListenAddr:     "127.0.0.1:18100",
		PollTimeout:    1 * time.Second,
		SessionTimeout: 5 * time.Minute,
		PollMode:       "batch",
	}
	srv := NewServer(cfg, logger)
	go func() {
		if err := srv.Start(); err != nil && err != http.ErrServerClosed {
			t.Logf("server error: %v", err)
		}
	}()
	defer srv.Stop(context.Background())
	waitForServerReady(t, "127.0.0.1:18100")

	if got := connectStreamPreferring(t, "127.0.0.1:18100"); got != pollmux.PollModeBatch {
		t.Fatalf("negotiated poll_mode = %q, want %q (PollMode: \"batch\" must override the client's preference)",
			got, pollmux.PollModeBatch)
	}
}

// connectWebSocketPreferring POSTs a raw connect request asking the server to
// negotiate WebSocket transport, and returns the transport the server
// actually negotiated ("" or pollmux.TransportWebSocket). Exercises the same
// real wire protocol a pollmux.Connector with PreferWebSocket=true takes.
func connectWebSocketPreferring(t *testing.T, addr string) string {
	t.Helper()
	body := strings.NewReader(`{"protocol_version":1,"meta":{"role":"consumer","endpoint":"ep1"},"prefer_websocket":true}`)
	resp, err := http.Post("http://"+addr+"/tunnel/connect", "application/json", body)
	if err != nil {
		t.Fatalf("connect request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("connect status = %d, want 200", resp.StatusCode)
	}
	var cr pollmux.ConnectResponse
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil {
		t.Fatalf("decode connect response: %v", err)
	}
	return cr.Transport
}

// TestBrokerNegotiatesWebSocketWhenEnabledAndRequested confirms the actual
// wiring added for HttpBroker's WebSocket support: with Config.EnableWebSocket
// set, a client asking for it via PreferWebSocket gets it negotiated — proving
// broker.Config.EnableWebSocket really reaches pollmux.ServerConfig.EnableWebSocket
// (a struct literal field-name typo would silently compile since both are
// plain bools, so this exercises the wire protocol rather than trusting the
// struct literal alone).
func TestBrokerNegotiatesWebSocketWhenEnabledAndRequested(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	defer logger.Sync()

	cfg := Config{
		ListenAddr:      "127.0.0.1:18101",
		PollTimeout:     1 * time.Second,
		SessionTimeout:  5 * time.Minute,
		EnableWebSocket: true,
	}
	srv := NewServer(cfg, logger)
	go func() {
		if err := srv.Start(); err != nil && err != http.ErrServerClosed {
			t.Logf("server error: %v", err)
		}
	}()
	defer srv.Stop(context.Background())
	waitForServerReady(t, "127.0.0.1:18101")

	if got := connectWebSocketPreferring(t, "127.0.0.1:18101"); got != pollmux.TransportWebSocket {
		t.Fatalf("negotiated transport = %q, want %q (Config.EnableWebSocket must reach pollmux.ServerConfig.EnableWebSocket)",
			got, pollmux.TransportWebSocket)
	}
}

// TestBrokerStaysOffWebSocketWhenNotEnabled confirms the default (off) is
// wired correctly too: a zero-valued Config.EnableWebSocket must not
// negotiate WebSocket transport even when the client asks for it, so
// existing deployments that upgrade HttpBroker without touching config stay
// on poll/send-stream exactly as before.
func TestBrokerStaysOffWebSocketWhenNotEnabled(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	defer logger.Sync()

	cfg := Config{
		ListenAddr:     "127.0.0.1:18102",
		PollTimeout:    1 * time.Second,
		SessionTimeout: 5 * time.Minute,
	}
	srv := NewServer(cfg, logger)
	go func() {
		if err := srv.Start(); err != nil && err != http.ErrServerClosed {
			t.Logf("server error: %v", err)
		}
	}()
	defer srv.Stop(context.Background())
	waitForServerReady(t, "127.0.0.1:18102")

	if got := connectWebSocketPreferring(t, "127.0.0.1:18102"); got != "" {
		t.Fatalf("negotiated transport = %q, want \"\" (default Config.EnableWebSocket=false must not offer WebSocket)", got)
	}
}
