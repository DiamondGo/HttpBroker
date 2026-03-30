package broker

import (
	"fmt"
	"sync"

	"github.com/hashicorp/yamux"
	"github.com/kexiaowen/httpbroker/internal/transport"
)

// Endpoint represents a named proxy endpoint with one provider and N consumers.
type Endpoint struct {
	Name            string
	ProviderSession *transport.Session // nil if no provider connected
	ProviderYamux   *yamux.Session     // yamux client toward provider
	mu              sync.RWMutex
}

// EndpointRegistry manages all named endpoints.
type EndpointRegistry struct {
	mu        sync.RWMutex
	endpoints map[string]*Endpoint
	sessions  map[string]*transport.Session // session ID → session (all sessions)
}

// NewEndpointRegistry creates a new empty EndpointRegistry.
func NewEndpointRegistry() *EndpointRegistry {
	return &EndpointRegistry{
		endpoints: make(map[string]*Endpoint),
		sessions:  make(map[string]*transport.Session),
	}
}

// GetOrCreate returns the endpoint with the given name, creating it if needed.
func (r *EndpointRegistry) GetOrCreate(name string) *Endpoint {
	r.mu.Lock()
	defer r.mu.Unlock()

	if ep, ok := r.endpoints[name]; ok {
		return ep
	}

	ep := &Endpoint{Name: name}
	r.endpoints[name] = ep
	return ep
}

// SetProvider registers a provider session for an endpoint.
// Returns error if a provider is already registered.
func (r *EndpointRegistry) SetProvider(
	endpointName string,
	session *transport.Session,
	yamuxSess *yamux.Session,
) error {
	ep := r.GetOrCreate(endpointName)

	// Register session in r.sessions BEFORE acquiring ep.mu to avoid
	// lock ordering inversion with handleStatus (which holds r.mu then ep.mu).
	r.mu.Lock()
	r.sessions[session.ID] = session
	r.mu.Unlock()

	ep.mu.Lock()
	defer ep.mu.Unlock()

	if ep.ProviderSession != nil {
		// Roll back the session registration.
		r.mu.Lock()
		delete(r.sessions, session.ID)
		r.mu.Unlock()
		return fmt.Errorf("endpoint %q already has a provider", endpointName)
	}

	ep.ProviderSession = session
	ep.ProviderYamux = yamuxSess

	return nil
}

// RemoveProvider removes the provider from an endpoint.
func (r *EndpointRegistry) RemoveProvider(endpointName string) {
	r.mu.RLock()
	ep, ok := r.endpoints[endpointName]
	r.mu.RUnlock()

	if !ok {
		return
	}

	ep.mu.Lock()
	providerSession := ep.ProviderSession
	ep.ProviderSession = nil
	ep.ProviderYamux = nil
	ep.mu.Unlock()

	if providerSession != nil {
		r.mu.Lock()
		delete(r.sessions, providerSession.ID)
		r.mu.Unlock()
	}
}

// RegisterSession adds a session to the sessions map so that handlePoll can
// find it immediately. This is used for provider sessions before HandleProvider
// is started in a goroutine (eliminating the race where early polls get 404).
func (r *EndpointRegistry) RegisterSession(session *transport.Session) {
	r.mu.Lock()
	r.sessions[session.ID] = session
	r.mu.Unlock()
}

// AddConsumer registers a consumer session.
func (r *EndpointRegistry) AddConsumer(endpointName string, session *transport.Session) {
	_ = r.GetOrCreate(endpointName)

	r.mu.Lock()
	r.sessions[session.ID] = session
	r.mu.Unlock()
}

// RemoveSession removes a session (consumer or provider) by session ID.
func (r *EndpointRegistry) RemoveSession(sessionID string) {
	r.mu.Lock()
	session, ok := r.sessions[sessionID]
	if ok {
		delete(r.sessions, sessionID)
	}
	r.mu.Unlock()

	if !ok {
		return
	}

	// If this was a provider session, clear the endpoint's provider reference.
	if session.Role == "provider" {
		r.mu.RLock()
		ep, epOk := r.endpoints[session.Endpoint]
		r.mu.RUnlock()

		if epOk {
			ep.mu.Lock()
			if ep.ProviderSession != nil && ep.ProviderSession.ID == sessionID {
				ep.ProviderSession = nil
				ep.ProviderYamux = nil
			}
			ep.mu.Unlock()
		}
	}
}

// GetSession retrieves a session by ID (for poll handler).
func (r *EndpointRegistry) GetSession(sessionID string) (*transport.Session, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	s, ok := r.sessions[sessionID]
	return s, ok
}

// GetEndpoint retrieves an endpoint by name.
func (r *EndpointRegistry) GetEndpoint(name string) (*Endpoint, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	ep, ok := r.endpoints[name]
	return ep, ok
}

// GetProviderYamux returns the provider yamux session for an endpoint.
func (r *EndpointRegistry) GetProviderYamux(endpointName string) (*yamux.Session, bool) {
	r.mu.RLock()
	ep, ok := r.endpoints[endpointName]
	r.mu.RUnlock()

	if !ok {
		return nil, false
	}

	ep.mu.RLock()
	defer ep.mu.RUnlock()

	if ep.ProviderYamux == nil {
		return nil, false
	}

	return ep.ProviderYamux, true
}

// AllSessions returns all sessions (for cleanup).
func (r *EndpointRegistry) AllSessions() []*transport.Session {
	r.mu.RLock()
	defer r.mu.RUnlock()

	sessions := make([]*transport.Session, 0, len(r.sessions))
	for _, s := range r.sessions {
		sessions = append(sessions, s)
	}
	return sessions
}
