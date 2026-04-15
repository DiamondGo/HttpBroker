package broker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/DiamondGo/HttpBroker/internal/transport"
	"github.com/gorilla/mux"
	"go.uber.org/zap"
)

// Config holds broker server configuration.
type Config struct {
	ListenAddr     string
	TLSCertFile    string
	TLSKeyFile     string
	UseTLS         bool
	PollTimeout    time.Duration // how long to hold poll before empty response (default 30s)
	SessionTimeout time.Duration // inactive session cleanup interval (default 5m)
	AuthEnabled    bool          // whether authentication is enabled
	AuthToken      string        // authentication token (used when AuthEnabled is true)
}

// Server is the broker HTTP server.
type Server struct {
	config   Config
	registry *EndpointRegistry
	relay    *Relay
	logger   *zap.Logger
	httpSrv  *http.Server
	done     chan struct{} // signals cleanup goroutine to stop
	stopOnce sync.Once     // ensures Stop() is idempotent
}

// NewServer creates a new broker Server.
func NewServer(config Config, logger *zap.Logger) *Server {
	if config.PollTimeout == 0 {
		config.PollTimeout = 30 * time.Second
	}
	if config.SessionTimeout == 0 {
		config.SessionTimeout = 5 * time.Minute
	}

	registry := NewEndpointRegistry()
	relay := NewRelay(registry, logger)

	s := &Server{
		config:   config,
		registry: registry,
		relay:    relay,
		logger:   logger,
		done:     make(chan struct{}),
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

	router.Handle("/tunnel/connect", AuthMiddleware(auth, http.HandlerFunc(s.handleConnect))).
		Methods("POST")
	router.Handle("/tunnel/{id}/poll", AuthMiddleware(auth, http.HandlerFunc(s.handlePoll))).
		Methods("POST")
	router.Handle("/tunnel/{id}", AuthMiddleware(auth, http.HandlerFunc(s.handleDelete))).
		Methods("DELETE")
	router.HandleFunc("/status", s.handleStatus).Methods("GET")

	s.httpSrv = &http.Server{
		Addr:    config.ListenAddr,
		Handler: router,
	}

	return s
}

// Start starts the HTTP server and the session cleanup goroutine.
// Blocks until the server stops.
func (s *Server) Start() error {
	go s.cleanupLoop()

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
//  2. Close all active sessions so HandleProvider/HandleConsumer goroutines exit.
//     Closing a session closes its BufferedPipes, causing yamux to get EOF and
//     close, which unblocks CloseChan() and Accept() in the relay goroutines.
//     Connected consumers and providers will detect the closure and reconnect.
//  3. Stop the session cleanup goroutine.
func (s *Server) Stop(ctx context.Context) error {
	var err error
	s.stopOnce.Do(func() {
		s.logger.Info("broker shutting down — draining HTTP connections and closing all sessions")

		// 1. Stop accepting new HTTP connections and drain in-flight requests.
		err = s.httpSrv.Shutdown(ctx)

		// 2. Close all active sessions so relay goroutines exit cleanly.
		//    This notifies connected consumers and providers that the broker is gone.
		for _, session := range s.registry.AllSessions() {
			session.Close()
		}

		// 3. Stop the cleanup goroutine.
		close(s.done)

		s.logger.Info("broker shutdown complete")
	})
	return err
}

// generateSessionID generates a random 32-character hex string.
func generateSessionID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// writeJSON writes a JSON response with the given status code.
func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// writeError writes a JSON error response.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// handleConnect handles POST /tunnel/connect.
func (s *Server) handleConnect(w http.ResponseWriter, r *http.Request) {
	role := r.URL.Query().Get("role")
	endpoint := r.URL.Query().Get("endpoint")

	if role == "" || endpoint == "" {
		writeError(w, http.StatusBadRequest, "missing required query params: role, endpoint")
		return
	}

	if role != "consumer" && role != "provider" {
		writeError(w, http.StatusBadRequest, "role must be 'consumer' or 'provider'")
		return
	}

	sessionID, err := generateSessionID()
	if err != nil {
		s.logger.Error("failed to generate session ID", zap.Error(err))
		writeError(w, http.StatusInternalServerError, "failed to generate session ID")
		return
	}

	session := transport.NewSession(sessionID, role, endpoint)

	if role == "provider" {
		// Pre-register session so handlePoll can find it immediately.
		// Without this, polls arriving before HandleProvider calls SetProvider
		// would get 404 (race condition that caused persistent polling failures).
		s.registry.RegisterSession(session)
		go s.relay.HandleProvider(session)
	} else {
		s.registry.AddConsumer(endpoint, session)
		go s.relay.HandleConsumer(session)
	}

	s.logger.Info("session created",
		zap.String("session_id", sessionID),
		zap.String("role", role),
		zap.String("endpoint", endpoint),
	)

	writeJSON(w, http.StatusOK, map[string]string{"session_id": sessionID})
}

// handlePoll handles POST /tunnel/{id}/poll.
//
// This endpoint now supports two modes:
//  1. Send-only (X-Send-Only: true): Immediately sends data to ToBroker and returns 200 OK.
//     Used by HTTPConn.Write() for immediate data transmission.
//  2. Receive-only (X-Receive-Only: true): Long-polls FromBroker for data to send back.
//     Used by HTTPConn.pollLoop() for continuous data reception.
func (s *Server) handlePoll(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	sessionID := vars["id"]

	session, ok := s.registry.GetSession(sessionID)
	if !ok {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}

	session.Touch()

	sendOnly := r.Header.Get("X-Send-Only") == "true"
	receiveOnly := r.Header.Get("X-Receive-Only") == "true"

	// Read request body and write to session's ToBroker pipe (if body not empty).
	// Limit body to 1MB.
	body := http.MaxBytesReader(w, r.Body, 1<<20)
	data, err := io.ReadAll(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "failed to read request body")
		return
	}

	if len(data) > 0 {
		s.logger.Debug("poll: writing data to ToBroker",
			zap.String("session_id", sessionID),
			zap.Int("bytes", len(data)),
			zap.Bool("send_only", sendOnly),
		)
		if _, err := session.ToBroker.Write(data); err != nil {
			s.logger.Debug("failed to write to ToBroker pipe",
				zap.String("session_id", sessionID),
				zap.Error(err),
			)
		}
	}

	// If this is a send-only request, return immediately without waiting for data
	if sendOnly {
		w.WriteHeader(http.StatusOK)
		return
	}

	// Read from session's FromBroker pipe using ReadAvailable (long-polling).
	s.logger.Debug("poll: long-polling FromBroker",
		zap.String("session_id", sessionID),
		zap.Bool("receive_only", receiveOnly),
	)

	buf := make([]byte, 64*1024) // 64KB read buffer
	n, err := session.FromBroker.ReadAvailable(buf, s.config.PollTimeout)
	if n > 0 {
		s.logger.Debug("poll: returning data from FromBroker",
			zap.String("session_id", sessionID),
			zap.Int("bytes", n),
		)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		w.Write(buf[:n])
		return
	}

	if err == io.EOF {
		// Session pipe closed.
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Timeout with no data.
	w.WriteHeader(http.StatusNoContent)
}

// handleDelete handles DELETE /tunnel/{id}.
func (s *Server) handleDelete(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)
	sessionID := vars["id"]

	session, ok := s.registry.GetSession(sessionID)
	if !ok {
		writeError(w, http.StatusNotFound, "session not found")
		return
	}

	session.Close()
	s.registry.RemoveSession(sessionID)

	s.logger.Info("session deleted", zap.String("session_id", sessionID))

	w.WriteHeader(http.StatusOK)
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
	defer s.registry.mu.RUnlock()

	statuses := make([]endpointStatus, 0, len(s.registry.endpoints))

	for name, ep := range s.registry.endpoints {
		ep.mu.RLock()
		hasProvider := ep.ProviderSession != nil
		ep.mu.RUnlock()

		// Count consumers for this endpoint.
		consumerCount := 0
		for _, sess := range s.registry.sessions {
			if sess.Endpoint == name && sess.Role == "consumer" {
				consumerCount++
			}
		}

		statuses = append(statuses, endpointStatus{
			Name:          name,
			HasProvider:   hasProvider,
			ConsumerCount: consumerCount,
		})
	}

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"endpoints": statuses,
	})
}

// cleanupLoop periodically removes expired sessions.
func (s *Server) cleanupLoop() {
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-s.done:
			return
		case <-ticker.C:
			s.cleanupExpiredSessions()
		}
	}
}

// cleanupExpiredSessions removes all expired sessions.
func (s *Server) cleanupExpiredSessions() {
	sessions := s.registry.AllSessions()
	for _, session := range sessions {
		if session.IsExpired(s.config.SessionTimeout) {
			s.logger.Info("cleaning up expired session",
				zap.String("session_id", session.ID),
				zap.String("role", session.Role),
				zap.String("endpoint", session.Endpoint),
			)
			session.Close()
			s.registry.RemoveSession(session.ID)
		}
	}
}
