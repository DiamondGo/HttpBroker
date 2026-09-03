package broker

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
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

// connectResumableTestSession negotiates a resumable session (stream mode in
// both directions plus prefer_resume) against a broker with EnableResume on.
func connectResumableTestSession(t *testing.T, ts *httptest.Server, role, endpoint string) string {
	t.Helper()
	resp, err := http.Post(
		ts.URL+"/tunnel/connect",
		"application/json",
		strings.NewReader(`{"protocol_version":1,"meta":{"role":"`+role+`","endpoint":"`+endpoint+`"},`+
			`"prefer_stream_mode":true,"prefer_stream_upload":true,"prefer_resume":true}`),
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
		Resumable bool   `json:"resumable"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode connect: %v", err)
	}
	if !out.Resumable {
		t.Fatal("expected the broker to negotiate a resumable session")
	}
	return out.SessionID
}

// TestSweepBrokenPollsSkipsResumableSession is the resume path: a resumable
// session whose transport dropped must be left for pollmux's resume-aware
// sweeper (ResumeGrace), not evicted by the 5s fast reaper — otherwise a
// /resume arriving between brokenPollGrace and ResumeGrace would find the
// session gone and the streams it carried would die anyway.
func TestSweepBrokenPollsSkipsResumableSession(t *testing.T) {
	logger, err := zap.NewDevelopment()
	if err != nil {
		t.Fatalf("logger: %v", err)
	}
	t.Cleanup(func() { _ = logger.Sync() })
	srv := NewServer(Config{
		ListenAddr:     "127.0.0.1:0",
		PollTimeout:    time.Second,
		SessionTimeout: 5 * time.Minute,
		EnableResume:   true,
		ResumeGrace:    time.Minute,
	}, logger)
	ts := httptest.NewServer(srv.httpSrv.Handler)
	t.Cleanup(ts.Close)

	id := connectResumableTestSession(t, ts, "consumer", "ep-reaper-resumable")
	sess, ok := srv.store.Get(id)
	if !ok {
		t.Fatal("session missing from store")
	}
	if !sess.Resumable() {
		t.Fatal("session should report Resumable()")
	}

	// A resumable session's dropped poll must not even be recorded.
	req := httptest.NewRequest(http.MethodPost, "/tunnel/"+id+"/poll", http.NoBody)
	req = mux.SetURLVars(req, map[string]string{"id": id})
	srv.markBrokenPoll(req)
	if hasBrokenPoll(srv, id) {
		t.Fatal("markBrokenPoll must ignore a resumable session")
	}

	// And even a stale entry (e.g. recorded before negotiation settled)
	// must be dropped without evicting the session.
	seedBrokenPoll(srv, id)
	srv.sweepBrokenPolls()

	if cur, ok := srv.store.Get(id); !ok || cur.IsClosed() {
		t.Fatal("resumable session must survive the fast reaper while inside its ResumeGrace")
	}
	if hasBrokenPoll(srv, id) {
		t.Fatal("stale brokenPolls entry for a resumable session must be cleared")
	}
}

// TestSweepBrokenPollsStillEvictsNonResumableWhenResumeEnabled pins the
// regression guarantee: turning EnableResume on must not slow down eviction
// of a client that never asked for resume (an older client, or one that
// negotiated batch), so a replacement provider still takes over in ~5s.
func TestSweepBrokenPollsStillEvictsNonResumableWhenResumeEnabled(t *testing.T) {
	logger, err := zap.NewDevelopment()
	if err != nil {
		t.Fatalf("logger: %v", err)
	}
	t.Cleanup(func() { _ = logger.Sync() })
	srv := NewServer(Config{
		ListenAddr:     "127.0.0.1:0",
		PollTimeout:    time.Second,
		SessionTimeout: 5 * time.Minute,
		EnableResume:   true,
	}, logger)
	ts := httptest.NewServer(srv.httpSrv.Handler)
	t.Cleanup(ts.Close)

	id := connectTestSession(t, ts, "provider", "ep-reaper-legacy")
	sess, ok := srv.store.Get(id)
	if !ok {
		t.Fatal("session missing from store")
	}
	if sess.Resumable() {
		t.Fatal("a client that did not ask for resume must not get a resumable session")
	}

	seedBrokenPoll(srv, id)
	srv.sweepBrokenPolls()
	if _, still := srv.store.Get(id); still {
		t.Fatal("non-resumable session must still be fast-evicted with EnableResume on")
	}
}

// TestStatusReportsDetachedResumableProvider: a resumable provider whose
// transport is down must show up on /status as registered-but-detached,
// with its remaining grace — the operator's view of "requests to this
// endpoint are stalling, waiting for the provider to resume".
func TestStatusReportsDetachedResumableProvider(t *testing.T) {
	logger, err := zap.NewDevelopment()
	if err != nil {
		t.Fatalf("logger: %v", err)
	}
	t.Cleanup(func() { _ = logger.Sync() })
	srv := NewServer(Config{
		ListenAddr:            "127.0.0.1:0",
		PollTimeout:           time.Second,
		SessionTimeout:        5 * time.Minute,
		EnableResume:          true,
		ResumeGrace:           time.Minute,
		StatusEndpointEnabled: true,
	}, logger)
	ts := httptest.NewServer(srv.httpSrv.Handler)
	t.Cleanup(ts.Close)

	// Connected but never attached a transport: detached from creation.
	connectResumableTestSession(t, ts, "provider", "ep-status-detached")
	waitFor(t, time.Second, "provider registered", func() bool {
		_, ok := srv.registry.GetProviderYamux("ep-status-detached")
		return ok
	})

	resp, err := http.Get(ts.URL + "/status")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	defer resp.Body.Close()
	var out struct {
		Endpoints []struct {
			Name                    string `json:"name"`
			HasProvider             bool   `json:"has_provider"`
			ProviderResumable       bool   `json:"provider_resumable"`
			ProviderDetached        bool   `json:"provider_detached"`
			ProviderResumeGraceLeft string `json:"provider_resume_grace_left"`
		} `json:"endpoints"`
		TotalSessions             int `json:"total_sessions"`
		ResumableSessions         int `json:"resumable_sessions"`
		DetachedResumableSessions int `json:"detached_resumable_sessions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if len(out.Endpoints) != 1 || out.Endpoints[0].Name != "ep-status-detached" {
		t.Fatalf("unexpected endpoints: %+v", out.Endpoints)
	}
	ep := out.Endpoints[0]
	if !ep.HasProvider || !ep.ProviderResumable || !ep.ProviderDetached {
		t.Fatalf("expected registered, resumable, detached provider; got %+v", ep)
	}
	left, err := time.ParseDuration(ep.ProviderResumeGraceLeft)
	if err != nil || left <= 0 || left > time.Minute {
		t.Fatalf("expected grace left in (0, 1m], got %q (%v)", ep.ProviderResumeGraceLeft, err)
	}
	if out.TotalSessions != 1 || out.ResumableSessions != 1 || out.DetachedResumableSessions != 1 {
		t.Fatalf("expected 1/1/1 total/resumable/detached, got %d/%d/%d",
			out.TotalSessions, out.ResumableSessions, out.DetachedResumableSessions)
	}

}

// TestHalfResumableTunnelIsReported: a resumable provider paired with a
// consumer that did not negotiate resume (or the other way round) is the
// configuration that looks fine and still drops streams. The broker must
// warn about it and /status must make it visible.
func TestHalfResumableTunnelIsReported(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	logger := zap.New(core)
	srv := NewServer(Config{
		ListenAddr:            "127.0.0.1:0",
		PollTimeout:           time.Second,
		SessionTimeout:        5 * time.Minute,
		EnableResume:          true,
		StatusEndpointEnabled: true,
	}, logger)
	ts := httptest.NewServer(srv.httpSrv.Handler)
	t.Cleanup(ts.Close)

	const ep = "ep-half-resumable"
	connectResumableTestSession(t, ts, "provider", ep)
	waitFor(t, time.Second, "provider registered", func() bool {
		_, ok := srv.registry.GetProviderYamux(ep)
		return ok
	})
	// Consumer joins without prefer_resume: half-resumable from here on.
	connectTestSession(t, ts, "consumer", ep)

	const want = "tunnel is only half resumable"
	waitFor(t, time.Second, "half-resumable warning", func() bool {
		return logs.FilterMessageSnippet(want).Len() == 1
	})
	entry := logs.FilterMessageSnippet(want).All()[0]
	if got := entry.ContextMap()["endpoint"]; got != ep {
		t.Fatalf("warning endpoint = %v, want %q", got, ep)
	}
	if got := entry.ContextMap()["joined_resumable"]; got != false {
		t.Fatalf("joined_resumable = %v, want false (the consumer is the non-resumable side)", got)
	}

	resp, err := http.Get(ts.URL + "/status")
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	defer resp.Body.Close()
	var out struct {
		Endpoints []struct {
			ConsumerCount          int  `json:"consumer_count"`
			ProviderResumable      bool `json:"provider_resumable"`
			ResumableConsumerCount int  `json:"resumable_consumer_count"`
		} `json:"endpoints"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if len(out.Endpoints) != 1 {
		t.Fatalf("expected 1 endpoint, got %d", len(out.Endpoints))
	}
	st := out.Endpoints[0]
	if !st.ProviderResumable || st.ConsumerCount != 1 || st.ResumableConsumerCount != 0 {
		t.Fatalf("status should show a resumable provider with 1 consumer of which 0 resumable, got %+v", st)
	}

	// A matching (both resumable) consumer joining must not warn again.
	connectResumableTestSession(t, ts, "consumer", ep)
	time.Sleep(200 * time.Millisecond)
	if n := logs.FilterMessageSnippet(want).Len(); n != 1 {
		t.Fatalf("expected no further warning for a matching consumer, got %d total", n)
	}
}

// TestRejectedDuplicateProviderDoesNotWarnHalfResumable: a second provider
// for an endpoint is refused by SetProvider. It must not be compared against
// the consumers first — that would log a half-resumable warning about a
// provider that never served anyone, while the real pairing is fine.
func TestRejectedDuplicateProviderDoesNotWarnHalfResumable(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	logger := zap.New(core)
	srv := NewServer(Config{
		ListenAddr:     "127.0.0.1:0",
		PollTimeout:    time.Second,
		SessionTimeout: 5 * time.Minute,
		EnableResume:   true,
	}, logger)
	ts := httptest.NewServer(srv.httpSrv.Handler)
	t.Cleanup(ts.Close)

	const ep = "ep-duplicate-provider"
	connectResumableTestSession(t, ts, "provider", ep)
	waitFor(t, time.Second, "provider registered", func() bool {
		_, ok := srv.registry.GetProviderYamux(ep)
		return ok
	})
	connectResumableTestSession(t, ts, "consumer", ep)

	// A non-resumable duplicate provider: refused, and must stay silent.
	connectTestSession(t, ts, "provider", ep)
	waitFor(t, 2*time.Second, "duplicate provider refused", func() bool {
		return logs.FilterMessageSnippet("failed to register provider").Len() == 1
	})
	if n := logs.FilterMessageSnippet("tunnel is only half resumable").Len(); n != 0 {
		t.Fatalf("a refused duplicate provider must not trigger the half-resumable warning, got %d", n)
	}
}
