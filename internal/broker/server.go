package broker

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/DiamondGo/pollmux"
	"github.com/gorilla/mux"
	"go.uber.org/zap"
	"go.uber.org/zap/exp/zapslog"
)

// Config holds broker server configuration.
type Config struct {
	ListenAddr                  string
	TLSCertFile                 string
	TLSKeyFile                  string
	UseTLS                      bool
	PollTimeout                 time.Duration // how long to hold poll before empty response (default 30s)
	SessionTimeout              time.Duration // inactive session cleanup interval (default 60s; must be >= 2x PollTimeout)
	CoalesceWindow              time.Duration // how long to let a poll response accumulate more data once any is available (default: pollmux.DefaultCoalesceWindow)
	PollBufferSize              int           // bytes per long-poll response (default: pollmux.DefaultPollBufferSize, 256KiB)
	MaxSendBytes                int           // cap on a single request body (default: pollmux.DefaultMaxSendBytes, 1MiB)
	HighWaterWarn               int           // log once when a session buffers this many bytes; 0 disables
	PollMode                    string        // "" defaults to stream mode; "batch" forces the older discrete poll mode; any other value panics at startup (see resolvePollMode)
	EnableWebSocket             bool          // whether to offer pollmux's WebSocket transport when a client asks for it (default: false)
	EnableResume                bool          // whether to negotiate pollmux's resumable sessions when a client asks for it (default: false)
	ResumeGrace                 time.Duration // how long a resumable session is kept after its transport drops, waiting for /resume (default: pollmux.DefaultResumeGrace, 30s; max pollmux.MaxResumeGrace, 5m)
	AuthEnabled                 bool          // whether authentication is enabled
	AuthToken                   string        // authentication token (used when AuthEnabled is true)
	StatusEndpointEnabled       bool          // whether to expose GET /status endpoint (default: false)
	UnauthorizedRedirectEnabled bool          // whether to redirect unauthorized requests instead of returning 401/404
	UnauthorizedRedirectURL     string        // redirect target URL for unauthorized requests
	Version                     string        // broker version
}

// brokerSession pairs a pollmux.Session with the role/endpoint declared at
// connect time. pollmux itself carries no application semantics, so the
// registry and relay key off these two fields instead of Meta directly.
type brokerSession struct {
	*pollmux.Session
	Role     string
	Endpoint string
}

// brokenPollGrace is how long a session may stay quiet after a poll request
// ended with its TCP connection dropped (the client, or an intermediate
// proxy, died mid-poll) before the fast reaper evicts it. A healthy client
// re-polls promptly — its stream-mode idle watchdog reopens a poll within
// HeartbeatInterval+grace even when an intermediate proxy swallows the
// response — so this only needs to absorb reconnection latency. A session
// evicted this way answers 404/410 to its late re-poll, which pollmux clients
// treat as a normal reconnect trigger.
//
// Without this, a dead client's session lingers until pollmux's own sweeper
// expires it after the full session_timeout (default 60s). For a dead
// provider that is a 60-second tunnel blackout: its registration keeps
// rejecting the replacement provider the whole time.
//
// A resumable session (Config.EnableResume negotiated with a client that
// asked for it) is exempt from this fast path: its transport dropping is
// exactly the event resumability exists for, and evicting it after 5s
// would defeat the ResumeGrace it is supposed to wait out. pollmux's own
// sweeper is resume-aware and retires such a session once its grace runs
// out with no /resume, so the reaper just leaves it alone.
const brokenPollGrace = 5 * time.Second

// fastReaperInterval is how often the fast reaper scans for broken-poll
// sessions that have passed brokenPollGrace.
const fastReaperInterval = 2 * time.Second

// Server is the broker HTTP server.
type Server struct {
	config      Config
	registry    *EndpointRegistry
	relay       *Relay
	logger      *zap.Logger
	httpSrv     *http.Server
	store       *pollmux.SessionStore
	pcfg        pollmux.ServerConfig
	hooks       pollmux.Hooks
	stopSweeper func()
	stopOnce    sync.Once // ensures Stop() is idempotent
	version     string    // broker version

	// brokenPolls records, per session id, when a poll request last ended
	// with its TCP connection dropped. The fast reaper evicts entries that
	// age past brokenPollGrace while the session has no poll in flight.
	brokenPollsMu sync.Mutex
	brokenPolls   map[string]time.Time
}

// markBrokenPoll records that session id's poll request just ended with its
// TCP connection dropped. No-op for unknown or already-closed sessions, and
// for resumable ones: their transport dropping is expected and handled by
// pollmux's resume-aware sweeper (see brokenPollGrace).
func (s *Server) markBrokenPoll(r *http.Request) {
	id := s.pcfg.SessionIDFunc(r)
	sess, ok := s.store.Get(id)
	if !ok || sess.IsClosed() || sess.Resumable() {
		return
	}
	s.brokenPollsMu.Lock()
	s.brokenPolls[sess.ID] = time.Now()
	s.brokenPollsMu.Unlock()
}

// startFastReaper runs the broken-poll reaper until stop is closed. It is a
// fast path on top of pollmux's own sweeper, which stays as the backstop for
// sessions that never had a broken poll (e.g. a client that died between
// polls).
func (s *Server) startFastReaper(stop <-chan struct{}) {
	go func() {
		ticker := time.NewTicker(fastReaperInterval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				s.sweepBrokenPolls()
			}
		}
	}()
}

// sweepBrokenPolls evicts every session whose last broken poll is older than
// brokenPollGrace and which has no poll or persistent transport in flight.
// pollmux performs the idle check and close atomically, so a re-poll racing
// this scan either attaches first and survives, or observes the closed session.
//
// A resumable session is skipped (and its entry dropped) even if it made it
// into brokenPolls — markBrokenPoll already filters these out, this is the
// second line of defence. pollmux's CloseSessionIfNoPollInFlight would also
// refuse to close a detached resumable session inside its grace, so the skip
// here is about not competing with pollmux's sweeper (and not logging an
// eviction that never happens), not about correctness.
func (s *Server) sweepBrokenPolls() {
	now := time.Now()

	type dueSession struct {
		sess *pollmux.Session
		at   time.Time
	}

	s.brokenPollsMu.Lock()
	var due []dueSession
	for id, at := range s.brokenPolls {
		if now.Sub(at) < brokenPollGrace {
			continue
		}
		sess, ok := s.store.Get(id)
		if !ok || sess.IsClosed() || sess.Resumable() {
			delete(s.brokenPolls, id)
			continue
		}
		delete(s.brokenPolls, id)
		due = append(due, dueSession{sess: sess, at: at})
	}
	s.brokenPollsMu.Unlock()

	for _, d := range due {
		if !pollmux.CloseSessionIfNoPollInFlight(
			s.store, s.hooks, d.sess, pollmux.ReasonServerClose,
		) {
			continue
		}
		s.logger.Info("evicted session whose poll connection dropped",
			zap.String("session_id", d.sess.ID),
			zap.String("role", d.sess.Meta()["role"]),
			zap.String("endpoint", d.sess.Meta()["endpoint"]),
			zap.Duration("quiet_for", now.Sub(d.at)))
	}
}

// resolvePollMode maps Config.PollMode to the pollmux.ServerConfig.PollMode
// value: empty (unset) defaults to stream, "batch" opts out. Any other value
// is passed through as-is so pollmux.ServerConfig.check() rejects it loudly
// at startup instead of this function silently coercing a typo to a mode the
// operator didn't ask for.
func resolvePollMode(configured string) string {
	if configured == "" {
		return pollmux.PollModeStream
	}
	return configured
}

// NewServer creates a new broker Server.
func NewServer(config Config, logger *zap.Logger) *Server {
	registry := NewEndpointRegistry()
	relay := NewRelay(registry, logger)
	store := pollmux.NewSessionStore()
	slogger := slog.New(zapslog.NewHandler(logger.Core()))

	s := &Server{
		config:      config,
		registry:    registry,
		relay:       relay,
		logger:      logger,
		store:       store,
		version:     config.Version,
		brokenPolls: make(map[string]time.Time),
	}

	s.pcfg = pollmux.ServerConfig{
		PollTimeout:     config.PollTimeout,
		SessionTimeout:  config.SessionTimeout,
		CoalesceWindow:  config.CoalesceWindow,
		PollBufferSize:  config.PollBufferSize,
		MaxSendBytes:    config.MaxSendBytes,
		HighWaterWarn:   config.HighWaterWarn,
		PollMode:        resolvePollMode(config.PollMode),
		EnableWebSocket: config.EnableWebSocket,
		EnableResume:    config.EnableResume,
		ResumeGrace:     config.ResumeGrace,
		// This project uses gorilla/mux, not net/http's own router.
		SessionIDFunc: func(r *http.Request) string { return mux.Vars(r)["id"] },
		Logger:        slogger,
	}
	s.hooks = pollmux.Hooks{
		Authenticate: s.authenticateConnect,
		OnConnect:    s.onConnect,
		OnDisconnect: s.onDisconnect,
	}

	router := mux.NewRouter()

	// Choose authenticator based on configuration
	var auth Authenticator
	if config.AuthEnabled {
		if config.AuthToken == "" {
			logger.Warn("auth enabled but no token configured, using noop authenticator")
			auth = &NoopAuthenticator{}
		} else {
			auth = NewTokenAuthenticator(config.AuthToken)
			logger.Info("token authentication enabled")
		}
	} else {
		auth = &NoopAuthenticator{}
		logger.Info("authentication disabled")
	}

	router.Handle("/tunnel/connect",
		AuthMiddleware(auth, config.UnauthorizedRedirectEnabled, config.UnauthorizedRedirectURL,
			pollmux.ConnectHandler(store, s.pcfg, s.hooks))).
		Methods("POST")
	pollHandler := pollmux.PollHandler(store, s.pcfg, s.hooks)
	router.Handle("/tunnel/{id}/poll",
		AuthMiddleware(auth, config.UnauthorizedRedirectEnabled, config.UnauthorizedRedirectURL,
			http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				pollHandler.ServeHTTP(w, r)
				// A poll request that ends with its context cancelled had its
				// TCP connection dropped underneath it: the client (or an
				// intermediate proxy) is gone. Mark it so the fast reaper can
				// evict the session long before session_timeout.
				if r.Context().Err() != nil {
					s.markBrokenPoll(r)
				}
			}))).Methods("POST")
	router.Handle("/tunnel/{id}",
		AuthMiddleware(auth, config.UnauthorizedRedirectEnabled, config.UnauthorizedRedirectURL,
			pollmux.DeleteHandler(store, s.pcfg, s.hooks))).
		Methods("DELETE")
	router.Handle("/tunnel/{id}/ws",
		AuthMiddleware(auth, config.UnauthorizedRedirectEnabled, config.UnauthorizedRedirectURL,
			pollmux.WebSocketHandler(store, s.pcfg, s.hooks))).
		Methods("GET")
	// /resume must sit behind the very same auth middleware as /poll, /ws
	// and DELETE: an unauthenticated caller that reaches it could end a
	// session's resumability (pollmux answers an out-of-range offset with
	// 409 and marks the session non-resumable for good, by design).
	router.Handle("/tunnel/{id}/resume",
		AuthMiddleware(auth, config.UnauthorizedRedirectEnabled, config.UnauthorizedRedirectURL,
			pollmux.ResumeHandler(store, s.pcfg, s.hooks))).
		Methods("POST")

	// Conditionally register /status endpoint based on configuration
	if config.StatusEndpointEnabled {
		router.HandleFunc("/status", s.handleStatus).Methods("GET")
		logger.Info("status endpoint enabled at GET /status")
	} else {
		logger.Info("status endpoint disabled")
	}

	// If unauthorized redirect is enabled, set up a catch-all handler for non-tunnel requests
	if config.UnauthorizedRedirectEnabled && config.UnauthorizedRedirectURL != "" {
		router.NotFoundHandler = http.HandlerFunc(s.handleUnauthorizedRedirect)
		logger.Info("unauthorized redirect enabled",
			zap.String("target_url", config.UnauthorizedRedirectURL),
		)
	}

	s.httpSrv = &http.Server{
		Addr: config.ListenAddr,
		// ReadHeaderTimeout bounds how long a client may take sending request
		// headers, so a slow trickle of header bytes (Slowloris-style) cannot
		// pin a goroutine per connection forever. Read/Write timeouts stay
		// unset on purpose: long polls hold their requests and responses open
		// for up to poll_timeout, and stream mode keeps request bodies open
		// for the life of the tunnel.
		ReadHeaderTimeout: 30 * time.Second,
		Handler:           router,
	}

	return s
}

// authenticateConnect vets the role/endpoint declared in a connect request's
// meta. Bearer token validation happens earlier in AuthMiddleware; this hook
// only concerns itself with the business fields pollmux itself doesn't know
// about.
func (s *Server) authenticateConnect(
	r *http.Request, req pollmux.ConnectRequest,
) (map[string]string, error) {
	role, endpoint := req.Meta["role"], req.Meta["endpoint"]
	if role == "" || endpoint == "" {
		return nil, pollmux.StatusErrorf(http.StatusBadRequest,
			"meta must carry both role and endpoint")
	}
	if role != "consumer" && role != "provider" {
		return nil, pollmux.StatusErrorf(http.StatusBadRequest,
			"role must be 'consumer' or 'provider', got %q", role)
	}
	if len(endpoint) > maxEndpointNameLen {
		return nil, pollmux.StatusErrorf(http.StatusBadRequest,
			"endpoint name too long (%d bytes, max %d)",
			len(endpoint), maxEndpointNameLen)
	}
	return nil, nil
}

// onConnect runs after the session is already registered in the store, so
// there's no race where an early poll finds no session and gets a 404.
func (s *Server) onConnect(sess *pollmux.Session, meta map[string]string) error {
	bs := &brokerSession{Session: sess, Role: meta["role"], Endpoint: meta["endpoint"]}

	if bs.Role == "provider" {
		go s.relay.HandleProvider(bs)
	} else {
		if err := s.registry.AddConsumer(bs.Endpoint, bs); err != nil {
			return err
		}
		go s.relay.HandleConsumer(bs)
	}

	s.logger.Info("session created",
		zap.String("session_id", bs.ID),
		zap.String("role", bs.Role),
		zap.String("endpoint", bs.Endpoint),
	)
	return nil
}

// onDisconnect is the single exit point for a session's end, whatever
// triggered it (client DELETE, sweeper eviction, or server-initiated close).
func (s *Server) onDisconnect(sess *pollmux.Session, reason pollmux.DisconnectReason) {
	meta := sess.Meta()
	s.registry.Forget(sess.ID, meta["role"], meta["endpoint"])
	s.logger.Info("session ended",
		zap.String("session_id", sess.ID),
		zap.String("role", meta["role"]),
		zap.String("endpoint", meta["endpoint"]),
		zap.String("reason", reason.String()),
	)
}

// Start starts the HTTP server, the session sweeper, and the broken-poll
// fast reaper. Blocks until the server stops.
func (s *Server) Start() error {
	s.stopSweeper = pollmux.StartSweeper(s.store, s.pcfg, s.hooks)

	reaperStop := make(chan struct{})
	s.startFastReaper(reaperStop)
	defer close(reaperStop)

	s.logger.Info("broker server starting", zap.String("addr", s.config.ListenAddr))

	if s.config.UseTLS {
		return s.httpSrv.ListenAndServeTLS(s.config.TLSCertFile, s.config.TLSKeyFile)
	}
	return s.httpSrv.ListenAndServe()
}

// Stop gracefully stops the server. Safe to call multiple times.
//
// Shutdown sequence:
//  1. Stop the HTTP server (no new requests accepted; in-flight requests drain).
//  2. Close every live session so relay goroutines exit cleanly and connected
//     consumers/providers detect the closure and reconnect.
//  3. Stop the sweeper. It returns only once its goroutine has exited, so no
//     further OnDisconnect calls arrive after Stop returns.
func (s *Server) Stop(ctx context.Context) error {
	var err error
	s.stopOnce.Do(func() {
		s.logger.Info("broker shutting down — draining HTTP connections and closing all sessions")

		err = s.httpSrv.Shutdown(ctx)

		for _, sess := range s.store.All() {
			pollmux.CloseSession(s.store, s.hooks, sess, pollmux.ReasonServerClose)
		}

		if s.stopSweeper != nil {
			s.stopSweeper()
		}

		s.logger.Info("broker shutdown complete")
	})
	return err
}

// writeJSON writes a JSON response with the given status code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// endpointStatus is the JSON representation of an endpoint's status.
type endpointStatus struct {
	Name          string `json:"name"`
	HasProvider   bool   `json:"has_provider"`
	ConsumerCount int    `json:"consumer_count"`
	// ProviderResumable is whether the provider session negotiated resume.
	ProviderResumable bool `json:"provider_resumable"`
	// ProviderDetached is whether that resumable provider currently has no
	// transport attached: it is registered, but streams opened towards it
	// stall until it resumes or its grace expires (see Relay.bridgeStream).
	ProviderDetached bool `json:"provider_detached"`
	// ProviderResumeGraceLeft is how long the detached provider has left to
	// resume before the broker retires its session. Empty unless detached.
	ProviderResumeGraceLeft string `json:"provider_resume_grace_left,omitempty"`
}

// handleStatus handles GET /status.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	s.registry.mu.RLock()
	statuses := make([]endpointStatus, 0, len(s.registry.endpoints))

	for name, ep := range s.registry.endpoints {
		ep.mu.RLock()
		provider := ep.ProviderSession
		consumerCount := len(ep.ConsumerSessions)
		ep.mu.RUnlock()

		st := endpointStatus{
			Name:          name,
			HasProvider:   provider != nil,
			ConsumerCount: consumerCount,
		}
		if provider != nil {
			st.ProviderResumable = provider.Resumable()
			if deadline, detached := provider.ResumeDeadline(); detached {
				st.ProviderDetached = true
				st.ProviderResumeGraceLeft = time.Until(deadline).Truncate(time.Millisecond).String()
			}
		}
		statuses = append(statuses, st)
	}
	s.registry.mu.RUnlock()

	// Resumable sessions waiting detached are the memory-amplification
	// surface of resume (each holds its replay buffer for the whole grace),
	// so count them for whoever is watching.
	sessions := s.store.All()
	resumable, detached := 0, 0
	for _, sess := range sessions {
		if !sess.Resumable() {
			continue
		}
		resumable++
		if _, d := sess.ResumeDeadline(); d {
			detached++
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"version":                     s.version,
		"endpoints":                   statuses,
		"total_sessions":              len(sessions),
		"resumable_sessions":          resumable,
		"detached_resumable_sessions": detached,
	})
}

// handleUnauthorizedRedirect handles all non-tunnel requests when redirect is enabled.
// It redirects the request to the configured URL with a 302 status code.
func (s *Server) handleUnauthorizedRedirect(w http.ResponseWriter, r *http.Request) {
	targetURL := buildRedirectURL(s.config.UnauthorizedRedirectURL, r)
	http.Redirect(w, r, targetURL, http.StatusFound)
}
