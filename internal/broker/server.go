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
		config:   config,
		registry: registry,
		relay:    relay,
		logger:   logger,
		store:    store,
		version:  config.Version,
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
	router.Handle("/tunnel/{id}/poll",
		AuthMiddleware(auth, config.UnauthorizedRedirectEnabled, config.UnauthorizedRedirectURL,
			pollmux.PollHandler(store, s.pcfg, s.hooks))).
		Methods("POST")
	router.Handle("/tunnel/{id}",
		AuthMiddleware(auth, config.UnauthorizedRedirectEnabled, config.UnauthorizedRedirectURL,
			pollmux.DeleteHandler(store, s.pcfg, s.hooks))).
		Methods("DELETE")
	router.Handle("/tunnel/{id}/ws",
		AuthMiddleware(auth, config.UnauthorizedRedirectEnabled, config.UnauthorizedRedirectURL,
			pollmux.WebSocketHandler(store, s.pcfg, s.hooks))).
		Methods("GET")

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
		Addr:    config.ListenAddr,
		Handler: router,
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
	return nil, nil
}

// onConnect runs after the session is already registered in the store, so
// there's no race where an early poll finds no session and gets a 404.
func (s *Server) onConnect(sess *pollmux.Session, meta map[string]string) error {
	bs := &brokerSession{Session: sess, Role: meta["role"], Endpoint: meta["endpoint"]}

	if bs.Role == "provider" {
		go s.relay.HandleProvider(bs)
	} else {
		s.registry.AddConsumer(bs.Endpoint, bs)
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

// Start starts the HTTP server and the session sweeper.
// Blocks until the server stops.
func (s *Server) Start() error {
	s.stopSweeper = pollmux.StartSweeper(s.store, s.pcfg, s.hooks)

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
}

// handleStatus handles GET /status.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	s.registry.mu.RLock()
	statuses := make([]endpointStatus, 0, len(s.registry.endpoints))

	for name, ep := range s.registry.endpoints {
		ep.mu.RLock()
		hasProvider := ep.ProviderSession != nil
		consumerCount := len(ep.ConsumerSessions)
		ep.mu.RUnlock()

		statuses = append(statuses, endpointStatus{
			Name:          name,
			HasProvider:   hasProvider,
			ConsumerCount: consumerCount,
		})
	}
	s.registry.mu.RUnlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"version":        s.version,
		"endpoints":      statuses,
		"total_sessions": s.store.Len(),
	})
}

// handleUnauthorizedRedirect handles all non-tunnel requests when redirect is enabled.
// It redirects the request to the configured URL with a 302 status code.
func (s *Server) handleUnauthorizedRedirect(w http.ResponseWriter, r *http.Request) {
	targetURL := buildRedirectURL(s.config.UnauthorizedRedirectURL, r)
	http.Redirect(w, r, targetURL, http.StatusFound)
}
