package broker

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

func newReaperTestBroker(t *testing.T, pollTimeout time.Duration) (*Server, *httptest.Server) {
	t.Helper()
	logger, err := zap.NewDevelopment()
	if err != nil {
		t.Fatalf("logger: %v", err)
	}
	t.Cleanup(func() { _ = logger.Sync() })

	srv := NewServer(Config{
		ListenAddr:     "127.0.0.1:0",
		PollTimeout:    pollTimeout,
		SessionTimeout: 5 * time.Minute,
		AuthEnabled:    false,
	}, logger)
	ts := httptest.NewServer(srv.httpSrv.Handler)
	t.Cleanup(ts.Close)
	return srv, ts
}

func connectTestSession(t *testing.T, ts *httptest.Server, role, endpoint string) string {
	t.Helper()
	resp, err := http.Post(
		ts.URL+"/tunnel/connect",
		"application/json",
		strings.NewReader(`{"protocol_version":1,"meta":{"role":"`+role+`","endpoint":"`+endpoint+`"}}`),
	)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("connect status %d", resp.StatusCode)
	}
	var out struct {
		SessionID string `json:"session_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode connect: %v", err)
	}
	if out.SessionID == "" {
		t.Fatal("expected a session id")
	}
	return out.SessionID
}

func seedBrokenPoll(srv *Server, id string) {
	srv.brokenPollsMu.Lock()
	srv.brokenPolls[id] = time.Now().Add(-brokenPollGrace - time.Second)
	srv.brokenPollsMu.Unlock()
}

func hasBrokenPoll(srv *Server, id string) bool {
	srv.brokenPollsMu.Lock()
	defer srv.brokenPollsMu.Unlock()
	_, ok := srv.brokenPolls[id]
	return ok
}

// TestSweepBrokenPollsEvictsQuietSession is the intended fast-reaper path: a
// dropped poll, no replacement, and the grace period elapsed.
func TestSweepBrokenPollsEvictsQuietSession(t *testing.T) {
	srv, ts := newReaperTestBroker(t, time.Second)
	id := connectTestSession(t, ts, "consumer", "ep-reaper-dead")

	waitFor(t, time.Second, "session present in store", func() bool {
		_, ok := srv.store.Get(id)
		return ok
	})

	seedBrokenPoll(srv, id)
	srv.sweepBrokenPolls()

	if _, ok := srv.store.Get(id); ok {
		t.Fatal("expected quiet session to be evicted after grace")
	}
	if hasBrokenPoll(srv, id) {
		t.Fatal("expected brokenPolls entry to be cleared on eviction")
	}
}

// TestSweepBrokenPollsClearsEntryWhenRePollLands is the healthy-reconnect
// path: a replacement poll is in flight when the reaper scans. The entry
// must be dropped (not merely skipped), otherwise the next sweep after that
// poll returns would evict the still-alive session.
func TestSweepBrokenPollsClearsEntryWhenRePollLands(t *testing.T) {
	srv, ts := newReaperTestBroker(t, 2*time.Second)
	id := connectTestSession(t, ts, "consumer", "ep-reaper-alive")

	pollDone := make(chan struct{})
	go func() {
		defer close(pollDone)
		resp, err := http.Post(ts.URL+"/tunnel/"+id+"/poll", "application/octet-stream", http.NoBody)
		if err != nil {
			t.Errorf("poll: %v", err)
			return
		}
		resp.Body.Close()
	}()

	waitFor(t, time.Second, "replacement poll in flight", func() bool {
		sess, ok := srv.store.Get(id)
		return ok && sess.PollInFlight() > 0
	})

	seedBrokenPoll(srv, id)
	srv.sweepBrokenPolls()

	sess, ok := srv.store.Get(id)
	if !ok || sess.IsClosed() {
		t.Fatal("session with a poll in flight must not be evicted")
	}
	if hasBrokenPoll(srv, id) {
		t.Fatal("brokenPolls entry must be cleared once a re-poll is observed")
	}

	<-pollDone
	if sess.PollInFlight() != 0 {
		t.Fatal("poll should have returned")
	}

	srv.sweepBrokenPolls()
	if _, still := srv.store.Get(id); !still {
		t.Fatal("healthy session was evicted after its replacement poll completed")
	}
}
