package broker

import (
	"net"
	"testing"
	"time"
)

// waitForServerReady blocks until something is accepting TCP connections on
// addr, polling instead of sleeping a fixed, hopefully-long-enough duration.
// Fails the test if nothing is listening within timeout.
func waitForServerReady(t *testing.T, addr string) {
	t.Helper()
	waitFor(t, 2*time.Second, "server listening on "+addr, func() bool {
		conn, err := net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if err != nil {
			return false
		}
		conn.Close()
		return true
	})
}

// waitFor polls condition every few milliseconds until it returns true, or
// fails the test once timeout elapses. It replaces fixed sleeps that stand
// in for "give some async goroutine a moment to run" with a deterministic
// wait on the actual state being waited on.
func waitFor(t *testing.T, timeout time.Duration, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if condition() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v waiting for: %s", timeout, what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
