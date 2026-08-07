package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// TestConnectHandshake exercises the new JSON-body connect protocol directly:
// happy path, missing meta fields, an invalid role, and a protocol version
// mismatch (426) — the version-negotiation check called for in the migration
// doc's verification checklist (§十).
func TestConnectHandshake(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	defer logger.Sync()

	cfg := Config{
		ListenAddr:     "127.0.0.1:18095",
		PollTimeout:    1 * time.Second,
		SessionTimeout: 5 * time.Minute,
		AuthEnabled:    false,
	}

	srv := NewServer(cfg, logger)
	go func() {
		if err := srv.Start(); err != nil && err != http.ErrServerClosed {
			t.Logf("server error: %v", err)
		}
	}()
	defer srv.Stop(context.Background())

	waitForServerReady(t, "127.0.0.1:18095")

	post := func(body string) *http.Response {
		resp, err := http.Post(
			"http://127.0.0.1:18095/tunnel/connect",
			"application/json",
			strings.NewReader(body),
		)
		if err != nil {
			t.Fatalf("failed to POST connect: %v", err)
		}
		return resp
	}

	t.Run("happy path returns session_id, limits, and meta", func(t *testing.T) {
		resp := post(`{"protocol_version":1,"meta":{"role":"consumer","endpoint":"ep1"}}`)
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200, got %d", resp.StatusCode)
		}

		var out struct {
			ProtocolVersion int               `json:"protocol_version"`
			SessionID       string            `json:"session_id"`
			Limits          map[string]any    `json:"limits"`
			Meta            map[string]string `json:"meta"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("failed to decode response: %v", err)
		}
		if out.SessionID == "" {
			t.Error("expected non-empty session_id")
		}
		if out.Limits == nil {
			t.Error("expected limits to be present")
		}
		if out.Meta["role"] != "consumer" || out.Meta["endpoint"] != "ep1" {
			t.Errorf("expected meta role/endpoint to round-trip, got %+v", out.Meta)
		}
	})

	t.Run("missing role and endpoint in meta returns 400", func(t *testing.T) {
		resp := post(`{"protocol_version":1,"meta":{}}`)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", resp.StatusCode)
		}
	})

	t.Run("invalid role returns 400", func(t *testing.T) {
		resp := post(`{"protocol_version":1,"meta":{"role":"admin","endpoint":"ep1"}}`)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("expected 400, got %d", resp.StatusCode)
		}
	})

	t.Run("protocol version mismatch returns 426", func(t *testing.T) {
		resp := post(`{"protocol_version":0,"meta":{"role":"consumer","endpoint":"ep1"}}`)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUpgradeRequired {
			t.Errorf("expected 426, got %d", resp.StatusCode)
		}
	})

	t.Run("empty body (old query-param client) returns 426", func(t *testing.T) {
		resp, err := http.Post(
			"http://127.0.0.1:18095/tunnel/connect?role=consumer&endpoint=ep1",
			"application/json",
			nil,
		)
		if err != nil {
			t.Fatalf("failed to POST connect: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusUpgradeRequired {
			t.Errorf("expected 426 for an old-protocol client, got %d", resp.StatusCode)
		}
	})
}

// TestPollOversizedBodyClosesSession verifies the new 413 behavior: a poll
// request whose body exceeds max_send_bytes gets 413 and the session is
// closed outright, rather than the old silent truncation.
func TestPollOversizedBodyClosesSession(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	defer logger.Sync()

	cfg := Config{
		ListenAddr:     "127.0.0.1:18096",
		PollTimeout:    1 * time.Second,
		SessionTimeout: 5 * time.Minute,
		MaxSendBytes:   16, // tiny, to trigger 413 without a huge payload
		AuthEnabled:    false,
	}

	srv := NewServer(cfg, logger)
	go func() {
		if err := srv.Start(); err != nil && err != http.ErrServerClosed {
			t.Logf("server error: %v", err)
		}
	}()
	defer srv.Stop(context.Background())

	waitForServerReady(t, "127.0.0.1:18096")

	resp, err := http.Post(
		"http://127.0.0.1:18096/tunnel/connect",
		"application/json",
		strings.NewReader(`{"protocol_version":1,"meta":{"role":"consumer","endpoint":"ep1"}}`),
	)
	if err != nil {
		t.Fatalf("failed to connect: %v", err)
	}
	var conn struct {
		SessionID string `json:"session_id"`
	}
	json.NewDecoder(resp.Body).Decode(&conn)
	resp.Body.Close()
	if conn.SessionID == "" {
		t.Fatal("expected a session id from connect")
	}

	oversized := bytes.Repeat([]byte("x"), 1024)
	req, _ := http.NewRequest(
		http.MethodPost,
		"http://127.0.0.1:18096/tunnel/"+conn.SessionID+"/poll",
		bytes.NewReader(oversized),
	)
	req.Header.Set("X-Send-Only", "true")
	pollResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("failed to poll: %v", err)
	}
	defer pollResp.Body.Close()

	if pollResp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413, got %d", pollResp.StatusCode)
	}

	// The session must now be gone — a protocol violation closes it, it
	// isn't a recoverable per-request error.
	req2, _ := http.NewRequest(
		http.MethodPost,
		"http://127.0.0.1:18096/tunnel/"+conn.SessionID+"/poll",
		nil,
	)
	req2.Header.Set("X-Send-Only", "true")
	pollResp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatalf("failed to poll again: %v", err)
	}
	defer pollResp2.Body.Close()
	if pollResp2.StatusCode != http.StatusNotFound {
		t.Errorf("expected session to be gone (404) after a protocol violation, got %d", pollResp2.StatusCode)
	}
}

// TestRemoveProviderKeepsConsumerSessionsInStore is a regression test for a
// pre-migration bug: EndpointRegistry.RemoveProvider used to delete live
// consumer sessions from the broker's global session index, so their next
// poll got a meaningless 404 instead of a proper session-closed response.
// Deleting that index as part of the pollmux migration structurally fixes
// this (there is no longer a second index to accidentally purge from), but
// this test pins the observable behavior down so a future refactor can't
// quietly reintroduce it: after a provider disconnects, the consumer's
// session must still be resolvable in the SessionStore.
func TestRemoveProviderKeepsConsumerSessionsInStore(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	defer logger.Sync()

	cfg := Config{
		ListenAddr:     "127.0.0.1:18097",
		PollTimeout:    1 * time.Second,
		SessionTimeout: 5 * time.Minute,
		AuthEnabled:    false,
	}

	srv := NewServer(cfg, logger)
	go func() {
		if err := srv.Start(); err != nil && err != http.ErrServerClosed {
			t.Logf("server error: %v", err)
		}
	}()
	defer srv.Stop(context.Background())

	waitForServerReady(t, "127.0.0.1:18097")

	connect := func(role string) string {
		resp, err := http.Post(
			"http://127.0.0.1:18097/tunnel/connect",
			"application/json",
			strings.NewReader(`{"protocol_version":1,"meta":{"role":"`+role+`","endpoint":"ep1"}}`),
		)
		if err != nil {
			t.Fatalf("failed to connect as %s: %v", role, err)
		}
		defer resp.Body.Close()
		var out struct {
			SessionID string `json:"session_id"`
		}
		json.NewDecoder(resp.Body).Decode(&out)
		if out.SessionID == "" {
			t.Fatalf("expected a session id for %s", role)
		}
		return out.SessionID
	}

	providerID := connect("provider")
	consumerID := connect("consumer")

	// Session registration in the store happens synchronously before the
	// connect handler responds, but poll (with a generous bound) rather
	// than assume that ordering forever.
	waitFor(t, time.Second, "provider and consumer sessions present in the store", func() bool {
		_, providerOK := srv.store.Get(providerID)
		_, consumerOK := srv.store.Get(consumerID)
		return providerOK && consumerOK
	})

	// Disconnect the provider the same way a real client does.
	req, _ := http.NewRequest(http.MethodDelete, "http://127.0.0.1:18097/tunnel/"+providerID, nil)
	delResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("failed to delete provider session: %v", err)
	}
	delResp.Body.Close()

	if _, ok := srv.registry.GetEndpoint("ep1"); !ok {
		t.Fatal("expected endpoint ep1 to still exist")
	}

	// The DELETE closes the provider's pollmux Session, which the yamux
	// Client session wrapping it detects as EOF, unblocking HandleProvider's
	// CloseChan() wait and running its deferred RemoveProvider. That happens
	// on a goroutine, so poll for it instead of sleeping and hoping.
	waitFor(t, time.Second, "provider removed from endpoint ep1 after disconnect", func() bool {
		return !srv.registry.HasProvider("ep1")
	})

	// The bug: RemoveProvider used to delete the consumer from the global
	// session index too, even though its long-poll tunnel was still alive.
	if _, ok := srv.store.Get(consumerID); !ok {
		t.Error("consumer session must remain in the SessionStore after the provider disconnects " +
			"— RemoveProvider should only touch topology bookkeeping, never the SessionStore")
	}
}

// TestShutdownOrdering verifies Stop() closes every session through pollmux
// (removing them from the store) and that no further activity is possible
// once Stop returns — the ordering guarantee that replaced the old
// close(s.done) approach.
func TestShutdownOrdering(t *testing.T) {
	obsCore, logs := observer.New(zapcore.InfoLevel)
	logger := zap.New(obsCore)

	cfg := Config{
		ListenAddr:     "127.0.0.1:18098",
		PollTimeout:    1 * time.Second,
		SessionTimeout: 5 * time.Minute,
		AuthEnabled:    false,
	}

	srv := NewServer(cfg, logger)
	go func() {
		if err := srv.Start(); err != nil && err != http.ErrServerClosed {
			t.Logf("server error: %v", err)
		}
	}()

	waitForServerReady(t, "127.0.0.1:18098")

	resp, err := http.Post(
		"http://127.0.0.1:18098/tunnel/connect",
		"application/json",
		strings.NewReader(`{"protocol_version":1,"meta":{"role":"consumer","endpoint":"ep1"}}`),
	)
	if err != nil {
		t.Fatalf("failed to connect: %v", err)
	}
	resp.Body.Close()

	waitFor(t, time.Second, "at least one live session before Stop", func() bool {
		return srv.store.Len() > 0
	})

	if err := srv.Stop(context.Background()); err != nil {
		t.Fatalf("Stop returned error: %v", err)
	}

	if got := srv.store.Len(); got != 0 {
		t.Errorf("expected all sessions removed from the store after Stop, got %d remaining", got)
	}

	found := false
	for _, entry := range logs.All() {
		if entry.Message == "session ended" {
			for _, f := range entry.Context {
				if f.Key == "reason" && f.String == "server_close" {
					found = true
				}
			}
		}
	}
	if !found {
		t.Error(`expected at least one "session ended" log with reason=server_close after Stop`)
	}
}
