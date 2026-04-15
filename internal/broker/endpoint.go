package broker

import (
	"fmt"
	"sync"
	"time"

	"github.com/DiamondGo/HttpBroker/internal/transport"
	"github.com/hashicorp/yamux"
)

// Endpoint represents a named proxy endpoint with one provider and N consumers.
type Endpoint struct {
	Name            string
	ProviderSession *transport.Session // nil if no provider connected
	ProviderYamux   *yamux.Session     // yamux client toward provider

	// consumerYamuxSessions tracks all active consumer yamux sessions for this
	// endpoint so that when the provider disconnects we can close them all,
	// causing each consumer to detect the failure and re-register.
	consumerYamuxSessions map[string]*yamux.Session // session ID → yamux session

	mu   sync.RWMutex
	cond *sync.Cond // broadcast when provider connects or disconnects
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

	ep := &Endpoint{
		Name:                  name,
		consumerYamuxSessions: make(map[string]*yamux.Session),
	}
	// cond uses its own dedicated mutex so it doesn't conflict with ep.mu.
	ep.cond = sync.NewCond(&sync.Mutex{})
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

// NotifyProviderArrived broadcasts on the endpoint's cond so that any goroutines
// blocked in WaitForProvider wake up and retry.
func (r *EndpointRegistry) NotifyProviderArrived(endpointName string) {
	r.mu.RLock()
	ep, ok := r.endpoints[endpointName]
	r.mu.RUnlock()
	if !ok {
		return
	}
	ep.cond.Broadcast()
}

// WaitForProvider returns the provider yamux session for an endpoint, blocking
// until one appears or the done channel is closed (consumer yamux session ended).
//
// Unlike a fixed timeout, this waits indefinitely for the provider — the caller
// passes the yamux session's CloseChan() as done so that if the consumer
// disconnects, the wait is cancelled immediately rather than leaking a goroutine.
//
// Returns (session, true) when a provider is available.
// Returns (nil, false) if done is closed before a provider arrives.
func (r *EndpointRegistry) WaitForProvider(
	endpointName string,
	done <-chan struct{},
) (*yamux.Session, bool) {
	// Fast path: provider already available.
	if ys, ok := r.GetProviderYamux(endpointName); ok {
		return ys, true
	}

	// Slow path: wait for provider to arrive.
	ep := r.GetOrCreate(endpointName)

	// We need to wake the cond.Wait() when done fires. Use a background
	// goroutine that broadcasts on the cond when done closes.
	stopBroadcast := make(chan struct{})
	go func() {
		select {
		case <-done:
			ep.cond.Broadcast()
		case <-stopBroadcast:
		}
	}()
	defer close(stopBroadcast)

	ep.cond.L.Lock()
	defer ep.cond.L.Unlock()

	for {
		// Check if done was closed (consumer yamux session ended).
		select {
		case <-done:
			return nil, false
		default:
		}

		// Check under the cond lock whether provider is now available.
		ep.mu.RLock()
		ys := ep.ProviderYamux
		ep.mu.RUnlock()

		if ys != nil {
			return ys, true
		}

		// Also wake periodically (every 30s) to re-check done in case the
		// broadcast goroutine races with cond.Wait().
		timer := time.AfterFunc(30*time.Second, func() {
			ep.cond.Broadcast()
		})
		ep.cond.Wait()
		timer.Stop()
	}
}

// RemoveProvider removes the provider from an endpoint and closes all consumer
// yamux sessions so that consumers detect the disconnection immediately.
// The consumerYamuxSessions map is cleared so stale entries don't accumulate.
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

	// Collect consumer yamux sessions to close outside the lock, then clear
	// the map so stale entries don't prevent new registrations.
	consumerSessions := make([]*yamux.Session, 0, len(ep.consumerYamuxSessions))
	for _, ys := range ep.consumerYamuxSessions {
		consumerSessions = append(consumerSessions, ys)
	}
	// Clear the map — consumers will re-register when they reconnect.
	ep.consumerYamuxSessions = make(map[string]*yamux.Session)
	ep.mu.Unlock()

	if providerSession != nil {
		r.mu.Lock()
		delete(r.sessions, providerSession.ID)
		r.mu.Unlock()
	}

	// Close all consumer yamux sessions. This causes each consumer's
	// yamuxSess.CloseChan() to fire, triggering a re-registration loop.
	// It also unblocks any bridgeStream goroutines waiting in WaitForProvider.
	for _, ys := range consumerSessions {
		ys.Close()
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

// RegisterConsumerYamux stores the consumer's yamux session so it can be
// closed when the provider disconnects.
func (r *EndpointRegistry) RegisterConsumerYamux(
	endpointName, sessionID string,
	ys *yamux.Session,
) {
	r.mu.RLock()
	ep, ok := r.endpoints[endpointName]
	r.mu.RUnlock()
	if !ok {
		return
	}

	ep.mu.Lock()
	ep.consumerYamuxSessions[sessionID] = ys
	ep.mu.Unlock()
}

// UnregisterConsumerYamux removes a consumer's yamux session from the endpoint.
func (r *EndpointRegistry) UnregisterConsumerYamux(endpointName, sessionID string) {
	r.mu.RLock()
	ep, ok := r.endpoints[endpointName]
	r.mu.RUnlock()
	if !ok {
		return
	}

	ep.mu.Lock()
	delete(ep.consumerYamuxSessions, sessionID)
	ep.mu.Unlock()
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

// HasProvider returns true if the endpoint has an active provider.
func (r *EndpointRegistry) HasProvider(endpointName string) bool {
	r.mu.RLock()
	ep, ok := r.endpoints[endpointName]
	r.mu.RUnlock()
	if !ok {
		return false
	}

	ep.mu.RLock()
	defer ep.mu.RUnlock()
	return ep.ProviderSession != nil
}

// ConsumerCount returns the number of active consumers for an endpoint.
func (r *EndpointRegistry) ConsumerCount(endpointName string) int {
	r.mu.RLock()
	ep, ok := r.endpoints[endpointName]
	r.mu.RUnlock()
	if !ok {
		return 0
	}

	ep.mu.RLock()
	defer ep.mu.RUnlock()
	return len(ep.consumerYamuxSessions)
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
