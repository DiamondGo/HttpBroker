package broker

import (
	"fmt"
	"go.uber.org/zap"
	"sync"
	"time"

	"github.com/hashicorp/yamux"
)

// Endpoint represents a named proxy endpoint with one provider and N consumers.
type Endpoint struct {
	Name             string
	ProviderSession  *brokerSession            // nil if no provider connected
	ProviderYamux    *yamux.Session            // yamux client toward provider
	ConsumerSessions map[string]*brokerSession // session ID -> consumer session
	waiters          int                       // streams currently waiting for a provider

	// consumerYamuxSessions tracks all active consumer yamux sessions for this
	// endpoint so that when the provider disconnects we can close them all,
	// causing each consumer to detect the failure and re-register.
	consumerYamuxSessions map[string]*yamux.Session // session ID → yamux session

	mu   sync.RWMutex
	cond *sync.Cond // broadcast when provider connects or disconnects
}

// maxEndpoints caps how many distinct endpoints may exist at once. Endpoint
// names are attacker-controllable whenever auth is disabled (they come from
// the connect request's meta), so without a cap an unauthenticated client
// could grow the registry unbounded by connecting with fresh names.
const maxEndpoints = 1024

// maxEndpointNameLen caps a single endpoint name's length for the same reason.
const maxEndpointNameLen = 128

// EndpointRegistry manages all named endpoints. Session lifecycle (creation,
// lookup, expiry) is entirely owned by pollmux.SessionStore; this registry is
// a derived topology view — which endpoint has a provider, which consumers
// are attached — updated from the connect/disconnect hooks.
type EndpointRegistry struct {
	mu        sync.RWMutex
	endpoints map[string]*Endpoint
}

// NewEndpointRegistry creates a new empty EndpointRegistry.
func NewEndpointRegistry() *EndpointRegistry {
	return &EndpointRegistry{
		endpoints: make(map[string]*Endpoint),
	}
}

// GetOrCreate returns the endpoint with the given name, creating it if needed.
// It returns an error when creating a *new* endpoint would exceed maxEndpoints
// (existing endpoints are always returned, so in-flight streams keep working).
func (r *EndpointRegistry) GetOrCreate(name string) (*Endpoint, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.getOrCreateLocked(name)
}

// getOrCreateLocked is GetOrCreate with r.mu already held.
func (r *EndpointRegistry) getOrCreateLocked(name string) (*Endpoint, error) {
	if ep, ok := r.endpoints[name]; ok {
		return ep, nil
	}
	if len(r.endpoints) >= maxEndpoints {
		return nil, fmt.Errorf("endpoint limit of %d reached", maxEndpoints)
	}

	ep := &Endpoint{
		Name:                  name,
		ConsumerSessions:      make(map[string]*brokerSession),
		consumerYamuxSessions: make(map[string]*yamux.Session),
	}
	// cond uses its own dedicated mutex so it doesn't conflict with ep.mu.
	ep.cond = sync.NewCond(&sync.Mutex{})
	r.endpoints[name] = ep
	return ep, nil
}

// deleteIfEmptyLocked reclaims an unused endpoint. Both r.mu and ep.mu must be
// held so no concurrent registration can retain an orphaned endpoint pointer.
func (r *EndpointRegistry) deleteIfEmptyLocked(name string, ep *Endpoint) {
	if ep.ProviderSession == nil && len(ep.ConsumerSessions) == 0 &&
		len(ep.consumerYamuxSessions) == 0 && ep.waiters == 0 {
		if r.endpoints[name] == ep {
			delete(r.endpoints, name)
		}
	}
}

// SetProvider registers a provider session for an endpoint.
// Returns error if a provider is already registered.
func (r *EndpointRegistry) SetProvider(
	endpointName string,
	session *brokerSession,
	yamuxSess *yamux.Session,
) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	ep, err := r.getOrCreateLocked(endpointName)
	if err != nil {
		return err
	}

	ep.mu.Lock()
	defer ep.mu.Unlock()

	if ep.ProviderSession != nil {
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

	// Slow path: acquire a waiter reference while holding the registry lock so
	// cleanup cannot remove this endpoint and leave us waiting on an orphan.
	r.mu.Lock()
	ep, err := r.getOrCreateLocked(endpointName)
	if err != nil {
		r.mu.Unlock()
		return nil, false
	}
	ep.mu.Lock()
	ep.waiters++
	ep.mu.Unlock()
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		ep.mu.Lock()
		ep.waiters--
		r.deleteIfEmptyLocked(endpointName, ep)
		ep.mu.Unlock()
		r.mu.Unlock()
	}()

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
//
// sessionID identifies which provider is leaving: the provider fields are only
// cleared when they still belong to that session, so a second (duplicate)
// provider whose registration was rejected can never wipe out the active
// provider's bookkeeping.
func (r *EndpointRegistry) RemoveProvider(endpointName, sessionID string) {
	r.mu.Lock()
	ep, ok := r.endpoints[endpointName]
	if !ok {
		r.mu.Unlock()
		return
	}

	ep.mu.Lock()
	if ep.ProviderSession != nil && ep.ProviderSession.ID != sessionID {
		// A different provider owns this endpoint now; leave it alone.
		ep.mu.Unlock()
		r.mu.Unlock()
		return
	}
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
	ep.ConsumerSessions = make(map[string]*brokerSession)
	r.deleteIfEmptyLocked(endpointName, ep)
	ep.mu.Unlock()
	r.mu.Unlock()

	// Close all consumer yamux sessions. This causes each consumer's
	// yamuxSess.CloseChan() to fire, triggering a re-registration loop. Each
	// consumer's underlying pollmux session closes along with it, so its next
	// poll correctly gets 410 (not a stale 404) — the session stays in
	// pollmux.SessionStore until the sweeper (or its own DELETE) retires it,
	// this call only tears down topology bookkeeping, never the store.
	// It also unblocks any bridgeStream goroutines waiting in WaitForProvider.
	for _, ys := range consumerSessions {
		ys.Close()
	}
}

// Forget removes a session's topology bookkeeping (consumer or provider) for
// the given role and endpoint. Called from Server.onDisconnect once pollmux
// has already removed the session from its own SessionStore — this only
// cleans up the derived registry view.
func (r *EndpointRegistry) Forget(sessionID, role, endpoint string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ep, ok := r.endpoints[endpoint]
	if !ok {
		return
	}

	ep.mu.Lock()
	defer ep.mu.Unlock()
	if role == "provider" {
		if ep.ProviderSession != nil && ep.ProviderSession.ID == sessionID {
			ep.ProviderSession = nil
			ep.ProviderYamux = nil
		}
	} else if role == "consumer" {
		delete(ep.ConsumerSessions, sessionID)
	}
	r.deleteIfEmptyLocked(endpoint, ep)
}

// AddConsumer registers a consumer session. It returns an error when the
// endpoint table is full, so the caller (the connect hook) can reject the
// session instead of leaking it into a half-registered state.
func (r *EndpointRegistry) AddConsumer(
	endpointName string,
	session *brokerSession,
) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	ep, err := r.getOrCreateLocked(endpointName)
	if err != nil {
		return err
	}

	ep.mu.Lock()
	ep.ConsumerSessions[session.ID] = session
	ep.mu.Unlock()
	return nil
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
	r.mu.Lock()
	defer r.mu.Unlock()
	ep, ok := r.endpoints[endpointName]
	if !ok {
		return
	}

	ep.mu.Lock()
	delete(ep.consumerYamuxSessions, sessionID)
	r.deleteIfEmptyLocked(endpointName, ep)
	ep.mu.Unlock()
}

// GetEndpoint retrieves an endpoint by name.
func (r *EndpointRegistry) GetEndpoint(name string) (*Endpoint, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()

	ep, ok := r.endpoints[name]
	return ep, ok
}

// GetProviderYamux returns the provider yamux session for an endpoint.
// halfResumablePeers returns the sessions on the other side of joined's
// endpoint (the provider for a consumer, every consumer for a provider)
// whose resumability differs from joined's. A tunnel is only as resilient
// as its weaker hop: a stream survives a transport drop only if both the
// consumer's and the provider's session can resume, so any mismatch means
// the operator's intent (resume on) is silently not being met on one side —
// prefer_resume left off, an older binary, or a client whose upload-stream
// probe failed and fell back to a non-resumable session.
func (r *EndpointRegistry) halfResumablePeers(joined *brokerSession) []*brokerSession {
	r.mu.RLock()
	ep, ok := r.endpoints[joined.Endpoint]
	r.mu.RUnlock()
	if !ok {
		return nil
	}
	ep.mu.RLock()
	defer ep.mu.RUnlock()

	want := joined.Resumable()
	var peers []*brokerSession
	switch joined.Role {
	case "consumer":
		if p := ep.ProviderSession; p != nil && p.Resumable() != want {
			peers = append(peers, p)
		}
	case "provider":
		for _, c := range ep.ConsumerSessions {
			if c.Resumable() != want {
				peers = append(peers, c)
			}
		}
	}
	return peers
}

// warnIfHalfResumable logs, once per session that joins an endpoint, when
// that session and its counterpart(s) disagree on resumability — the
// silent misconfiguration a resumable deployment is most likely to have.
func warnIfHalfResumable(logger *zap.Logger, registry *EndpointRegistry, joined *brokerSession) {
	peers := registry.halfResumablePeers(joined)
	if len(peers) == 0 {
		return
	}
	ids := make([]string, 0, len(peers))
	for _, p := range peers {
		ids = append(ids, p.Role+":"+p.ID)
	}
	logger.Warn("tunnel is only half resumable: a transport drop on the non-resumable hop will still kill its streams",
		zap.String("endpoint", joined.Endpoint),
		zap.String("joined", joined.Role+":"+joined.ID),
		zap.Bool("joined_resumable", joined.Resumable()),
		zap.Strings("mismatched_peers", ids),
	)
}

// ProviderResumeDeadline reports whether the endpoint's provider session is
// resumable and currently has no transport attached — i.e. it dropped its
// connection and the broker is holding the session open waiting for its
// /resume — and, if so, when that grace runs out. A provider in this state
// stays registered on purpose (see brokenPollGrace in server.go): streams
// opened towards it stall until it resumes, and fail only if the grace
// expires and pollmux retires the session.
func (r *EndpointRegistry) ProviderResumeDeadline(endpointName string) (time.Time, bool) {
	r.mu.RLock()
	ep, ok := r.endpoints[endpointName]
	r.mu.RUnlock()
	if !ok {
		return time.Time{}, false
	}
	ep.mu.RLock()
	sess := ep.ProviderSession
	ep.mu.RUnlock()
	if sess == nil {
		return time.Time{}, false
	}
	return sess.ResumeDeadline()
}

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
