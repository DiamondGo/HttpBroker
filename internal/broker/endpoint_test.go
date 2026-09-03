package broker

import (
	"testing"

	"github.com/DiamondGo/pollmux"
)

// testSession builds a pollmux Session carrying only an ID — enough for the
// registry bookkeeping under test, which keys off Session.ID and never
// touches the pipes.
func testSession(id string) *brokerSession {
	return &brokerSession{
		Session:  &pollmux.Session{ID: id},
		Role:     "consumer",
		Endpoint: "ep",
	}
}

// TestRemoveProviderDoesNotEvictADifferentProvider covers the duplicate
// provider bug: a rejected second provider must not clear the active
// provider's registration (or tear down consumer tunnels) when it leaves.
func TestRemoveProviderDoesNotEvictADifferentProvider(t *testing.T) {
	r := NewEndpointRegistry()

	providerA := &brokerSession{
		Session:  &pollmux.Session{ID: "prov-a"},
		Role:     "provider",
		Endpoint: "ep",
	}

	if _, err := r.SetProvider("ep", providerA, nil); err != nil {
		t.Fatalf("SetProvider: %v", err)
	}

	// A different session (the rejected duplicate) leaves: provider A's
	// registration must survive.
	r.RemoveProvider("ep", "prov-b")
	if !r.HasProvider("ep") {
		t.Fatal("provider A evicted by a different session's RemoveProvider")
	}

	// Provider A itself leaving clears the registration.
	r.RemoveProvider("ep", "prov-a")
	if r.HasProvider("ep") {
		t.Fatal("provider A not evicted by its own RemoveProvider")
	}
}

// TestRemoveProviderClosesConsumerYamuxSessions is a regression check that
// the identity guard did not weaken the normal path: removing the real
// provider still resets the endpoint's consumer bookkeeping.
func TestRemoveProviderResetsConsumerBookkeeping(t *testing.T) {
	r := NewEndpointRegistry()

	provider := &brokerSession{Session: &pollmux.Session{ID: "prov-a"}, Role: "provider", Endpoint: "ep"}
	consumer := testSession("cons-1")

	if _, err := r.SetProvider("ep", provider, nil); err != nil {
		t.Fatalf("SetProvider: %v", err)
	}
	if _, err := r.AddConsumer("ep", consumer); err != nil {
		t.Fatalf("AddConsumer: %v", err)
	}

	r.RemoveProvider("ep", "prov-a")

	if r.HasProvider("ep") {
		t.Error("provider still registered after RemoveProvider")
	}
	if r.ConsumerCount("ep") != 0 {
		t.Errorf("consumer count = %d, want 0 after RemoveProvider", r.ConsumerCount("ep"))
	}
}

// TestGetOrCreateEndpointLimit verifies the registry rejects growth past
// maxEndpoints (unauthenticated clients control endpoint names, so this is
// a memory-DoS bound) while existing endpoints keep working.
func TestGetOrCreateEndpointLimit(t *testing.T) {
	r := NewEndpointRegistry()

	for i := 0; i < maxEndpoints; i++ {
		if _, err := r.GetOrCreate(endpointNameFor(i)); err != nil {
			t.Fatalf("GetOrCreate #%d: %v", i, err)
		}
	}

	if _, err := r.GetOrCreate("one-too-many"); err == nil {
		t.Fatal("expected an error when creating an endpoint past the limit")
	}

	// Existing endpoints are still returned, not rejected.
	if _, err := r.GetOrCreate(endpointNameFor(0)); err != nil {
		t.Fatalf("existing endpoint rejected: %v", err)
	}
}

// TestAddConsumerFailsAtEndpointLimit verifies the connect path (AddConsumer)
// reports the full table so the hook can reject the session cleanly.
func TestAddConsumerFailsAtEndpointLimit(t *testing.T) {
	r := NewEndpointRegistry()

	for i := 0; i < maxEndpoints; i++ {
		if _, err := r.GetOrCreate(endpointNameFor(i)); err != nil {
			t.Fatalf("GetOrCreate #%d: %v", i, err)
		}
	}

	if _, err := r.AddConsumer("one-too-many", testSession("c1")); err == nil {
		t.Fatal("expected AddConsumer to fail past the endpoint limit")
	}
}

// TestEndpointCapacityIsReclaimed verifies maxEndpoints limits concurrent
// topology, not every endpoint name seen over the process lifetime.
func TestEndpointCapacityIsReclaimed(t *testing.T) {
	r := NewEndpointRegistry()

	for i := 0; i < maxEndpoints; i++ {
		name := endpointNameFor(i)
		session := testSession("consumer-" + name)
		if _, err := r.AddConsumer(name, session); err != nil {
			t.Fatalf("AddConsumer #%d: %v", i, err)
		}
		r.Forget(session.ID, "consumer", name)
	}

	if _, err := r.GetOrCreate("capacity-reused"); err != nil {
		t.Fatalf("capacity was not reclaimed after disconnects: %v", err)
	}
}

// TestForgetKeepsEndpointWithOtherSessions ensures reclamation does not remove
// topology still owned by another connected session.
func TestForgetKeepsEndpointWithOtherSessions(t *testing.T) {
	r := NewEndpointRegistry()
	first, second := testSession("first"), testSession("second")
	if _, err := r.AddConsumer("shared", first); err != nil {
		t.Fatal(err)
	}
	if _, err := r.AddConsumer("shared", second); err != nil {
		t.Fatal(err)
	}

	r.Forget(first.ID, "consumer", "shared")
	if _, ok := r.GetEndpoint("shared"); !ok {
		t.Fatal("endpoint with a live consumer was reclaimed")
	}

	r.Forget(second.ID, "consumer", "shared")
	if _, ok := r.GetEndpoint("shared"); ok {
		t.Fatal("empty endpoint was not reclaimed")
	}
}

// endpointNameFor produces distinct endpoint names for the limit tests.
func endpointNameFor(i int) string {
	const prefix = "ep-"
	name := make([]byte, 0, len(prefix)+10)
	name = append(name, prefix...)
	if i == 0 {
		return string(append(name, '0'))
	}
	for i > 0 {
		name = append(name, byte('0'+i%10))
		i /= 10
	}
	// reverse
	for l, r := 0, len(name)-len(prefix)-1; l < r; l, r = l+1, r-1 {
		name[l], name[r] = name[r], name[l]
	}
	return string(name)
}
