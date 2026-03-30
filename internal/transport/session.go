package transport

import (
	"sync"
	"time"
)

// Session represents a connected client (consumer or provider) on the broker.
// It implements io.ReadWriteCloser so yamux can use it as a transport.
//
// Data flow:
//
//	Client → POST body → ToBroker pipe → Session.Read() → yamux
//	yamux → Session.Write() → FromBroker pipe → GET poll response → Client
type Session struct {
	ID         string
	Role       string // "consumer" or "provider"
	Endpoint   string
	ToBroker   *BufferedPipe // client uploads → broker yamux reads
	FromBroker *BufferedPipe // broker yamux writes → client poll receives
	LastActive time.Time
	mu         sync.Mutex
	closed     bool
}

// NewSession creates a new Session with initialized pipes and timestamps.
func NewSession(id, role, endpoint string) *Session {
	return &Session{
		ID:         id,
		Role:       role,
		Endpoint:   endpoint,
		ToBroker:   NewBufferedPipe(),
		FromBroker: NewBufferedPipe(),
		LastActive: time.Now(),
	}
}

// Read reads data sent by the client (from ToBroker pipe).
// Used by the broker's yamux session.
func (s *Session) Read(p []byte) (int, error) {
	return s.ToBroker.Read(p)
}

// Write sends data to the client (into FromBroker pipe).
// Used by the broker's yamux session.
func (s *Session) Write(p []byte) (int, error) {
	return s.FromBroker.Write(p)
}

// Close closes both pipes.
func (s *Session) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return nil
	}
	s.closed = true

	s.ToBroker.Close()
	s.FromBroker.Close()
	return nil
}

// Touch updates LastActive to current time.
func (s *Session) Touch() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.LastActive = time.Now()
}

// IsExpired returns true if LastActive is older than maxAge.
func (s *Session) IsExpired(maxAge time.Duration) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return time.Since(s.LastActive) > maxAge
}
