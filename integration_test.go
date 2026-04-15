// Package httpbroker_test contains end-to-end integration tests for the
// HttpBroker system. Each test case starts broker, provider, and consumer
// processes on random high-numbered ports, verifies connectivity through the
// SOCKS5 proxy, then gracefully shuts down all three components.
//
// Run with:
//
//	go test -v -timeout 300s -run TestIntegration ./...
package httpbroker_test

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"

	"golang.org/x/net/proxy"
)

// ────────────────────────────────────────────────────────────────────────────
// Port helpers
// ────────────────────────────────────────────────────────────────────────────

// randomHighPort returns a free TCP port in the range [40000, 60000).
// It binds a listener to reserve the port, closes it, and returns the address.
// There is a small TOCTOU window, but it is acceptable for tests.
func randomHighPort() (int, error) {
	for attempts := 0; attempts < 20; attempts++ {
		port := 40000 + rand.Intn(20000)
		ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err == nil {
			ln.Close()
			return port, nil
		}
	}
	// Fall back to OS-assigned port.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	return port, nil
}

// ────────────────────────────────────────────────────────────────────────────
// Process management
// ────────────────────────────────────────────────────────────────────────────

// component wraps an os/exec.Cmd and provides graceful-stop helpers.
type component struct {
	name string
	cmd  *exec.Cmd
}

// stop sends SIGTERM and waits up to 5 s for the process to exit.
func (c *component) stop(t *testing.T) {
	t.Helper()
	if c == nil || c.cmd == nil || c.cmd.Process == nil {
		return
	}
	t.Logf("[%s] sending SIGTERM (pid %d)", c.name, c.cmd.Process.Pid)
	if err := c.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Logf("[%s] SIGTERM failed (%v), falling back to Kill", c.name, err)
		c.cmd.Process.Kill()
	}
	done := make(chan error, 1)
	go func() { done <- c.cmd.Wait() }()
	select {
	case <-done:
		t.Logf("[%s] exited cleanly", c.name)
	case <-time.After(5 * time.Second):
		t.Logf("[%s] did not exit within 5 s, killing", c.name)
		c.cmd.Process.Kill()
		<-done
	}
}

// kill immediately kills the process without waiting.
func (c *component) kill(t *testing.T) {
	t.Helper()
	if c == nil || c.cmd == nil || c.cmd.Process == nil {
		return
	}
	t.Logf("[%s] killing (pid %d)", c.name, c.cmd.Process.Pid)
	c.cmd.Process.Kill()
	c.cmd.Wait()
}

// ────────────────────────────────────────────────────────────────────────────
// Binary build (once per test run)
// ────────────────────────────────────────────────────────────────────────────

func buildBinaries(t *testing.T) {
	t.Helper()
	t.Log("Building binaries…")
	for _, target := range []struct{ out, pkg string }{
		{"bin/httpbroker-broker", "./cmd/broker"},
		{"bin/httpbroker-provider", "./cmd/provider"},
		{"bin/httpbroker-consumer", "./cmd/consumer"},
	} {
		cmd := exec.Command("go", "build", "-o", target.out, target.pkg)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build %s failed: %v\n%s", target.pkg, err, out)
		}
	}
	t.Log("Binaries built.")
}

// ────────────────────────────────────────────────────────────────────────────
// Component launchers
// ────────────────────────────────────────────────────────────────────────────

const testEndpoint = "integration-test"

func launchBroker(t *testing.T, port int) *component {
	t.Helper()
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	cmd := exec.Command("./bin/httpbroker-broker", "--listen", addr)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start broker: %v", err)
	}
	t.Logf("[broker] started on %s (pid %d)", addr, cmd.Process.Pid)
	return &component{name: "broker", cmd: cmd}
}

func launchProvider(t *testing.T, brokerPort int) *component {
	t.Helper()
	brokerURL := fmt.Sprintf("http://127.0.0.1:%d", brokerPort)
	cmd := exec.Command("./bin/httpbroker-provider",
		"--broker-url", brokerURL,
		"--endpoint", testEndpoint,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start provider: %v", err)
	}
	t.Logf("[provider] started → broker %s (pid %d)", brokerURL, cmd.Process.Pid)
	return &component{name: "provider", cmd: cmd}
}

func launchConsumer(t *testing.T, brokerPort, socks5Port int) *component {
	t.Helper()
	brokerURL := fmt.Sprintf("http://127.0.0.1:%d", brokerPort)
	socks5Addr := fmt.Sprintf("127.0.0.1:%d", socks5Port)
	cmd := exec.Command("./bin/httpbroker-consumer",
		"--broker-url", brokerURL,
		"--endpoint", testEndpoint,
		"--socks5-listen", socks5Addr,
	)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start consumer: %v", err)
	}
	t.Logf(
		"[consumer] started → broker %s, socks5 %s (pid %d)",
		brokerURL,
		socks5Addr,
		cmd.Process.Pid,
	)
	return &component{name: "consumer", cmd: cmd}
}

// ────────────────────────────────────────────────────────────────────────────
// Readiness probes
// ────────────────────────────────────────────────────────────────────────────

// waitForBroker polls GET /status until it returns 200 or timeout expires.
func waitForBroker(t *testing.T, port int, timeout time.Duration) {
	t.Helper()
	url := fmt.Sprintf("http://127.0.0.1:%d/status", port)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				t.Logf("[broker] ready at :%d", port)
				return
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("[broker] did not become ready within %v", timeout)
}

// waitForSOCKS5 polls a TCP dial until it succeeds or timeout expires.
func waitForSOCKS5(t *testing.T, port int, timeout time.Duration) {
	t.Helper()
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			t.Logf("[consumer] SOCKS5 ready at :%d", port)
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("[consumer] SOCKS5 did not become ready within %v", timeout)
}

// waitForBrokerGone polls until the broker port is no longer reachable.
func waitForBrokerGone(t *testing.T, port int, timeout time.Duration) {
	t.Helper()
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err != nil {
			t.Logf("[broker] confirmed gone at :%d", port)
			return
		}
		conn.Close()
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("[broker] still reachable after %v", timeout)
}

// ────────────────────────────────────────────────────────────────────────────
// SOCKS5 HTTP client factory
// ────────────────────────────────────────────────────────────────────────────

func newSOCKS5Client(t *testing.T, socks5Port int) *http.Client {
	t.Helper()
	addr := fmt.Sprintf("127.0.0.1:%d", socks5Port)
	dialer, err := proxy.SOCKS5("tcp", addr, nil, proxy.Direct)
	if err != nil {
		t.Fatalf("create SOCKS5 dialer: %v", err)
	}
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, a string) (net.Conn, error) {
				return dialer.Dial(network, a)
			},
			// Disable keep-alives so each request gets a fresh SOCKS5 stream.
			// This makes reconnect tests more deterministic.
			DisableKeepAlives: true,
		},
		Timeout: 30 * time.Second,
	}
}

// ────────────────────────────────────────────────────────────────────────────
// Connectivity assertion
// ────────────────────────────────────────────────────────────────────────────

// assertConnectivity verifies end-to-end connectivity through the SOCKS5 proxy.
//
// Strategy:
//   - Primary:  https://www.google.com/ — tried once with a 10 s timeout.
//     If the test environment cannot reach Google (firewall, etc.) we skip it
//     immediately rather than waiting for a long timeout.
//   - Fallback: http://example.com/ — tried up to 3 times with a 15 s timeout.
//
// A 200 response from either site is considered a pass.
func assertConnectivity(t *testing.T, client *http.Client, label string) {
	t.Helper()
	t.Logf("[%s] verifying connectivity via SOCKS5…", label)

	// tryOnce performs a single GET with its own per-request timeout so that
	// an unreachable host does not block for the full client.Timeout.
	tryOnce := func(rawURL string, perReqTimeout time.Duration) (int, int, error) {
		ctx, cancel := context.WithTimeout(context.Background(), perReqTimeout)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
		if err != nil {
			return 0, 0, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return 0, 0, err
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode, len(body), nil
	}

	// ── Primary: Google (single attempt, 10 s) ──────────────────────────────
	const googleURL = "https://www.google.com/"
	status, n, err := tryOnce(googleURL, 10*time.Second)
	if err == nil && status == http.StatusOK {
		t.Logf("[%s] ✓ connectivity OK via %s (%d bytes)", label, googleURL, n)
		return
	}
	if err != nil {
		t.Logf("[%s] %s unreachable (%v), falling back to example.com…", label, googleURL, err)
	} else {
		t.Logf("[%s] %s returned status %d, falling back to example.com…", label, googleURL, status)
	}

	// ── Fallback: example.com (3 attempts, 15 s each) ───────────────────────
	const exampleURL = "http://example.com/"
	var lastErr error
	for attempt := 1; attempt <= 3; attempt++ {
		status, n, err = tryOnce(exampleURL, 15*time.Second)
		if err != nil {
			lastErr = err
			t.Logf("[%s] %s attempt %d failed: %v", label, exampleURL, attempt, err)
			time.Sleep(time.Duration(attempt) * time.Second)
			continue
		}
		if status != http.StatusOK {
			lastErr = fmt.Errorf("status %d", status)
			t.Logf("[%s] %s attempt %d: unexpected status %d", label, exampleURL, attempt, status)
			time.Sleep(time.Duration(attempt) * time.Second)
			continue
		}
		t.Logf("[%s] ✓ connectivity OK via %s (%d bytes)", label, exampleURL, n)
		return
	}

	t.Fatalf(
		"[%s] connectivity check failed: all targets unreachable (last error: %v)",
		label,
		lastErr,
	)
}

// ────────────────────────────────────────────────────────────────────────────
// Harness: a fully-running broker + provider + consumer cluster
// ────────────────────────────────────────────────────────────────────────────

type harness struct {
	brokerPort int
	socks5Port int
	broker     *component
	provider   *component
	consumer   *component
	httpClient *http.Client
}

// newHarness allocates ports, starts all three components in the given order,
// waits for readiness, and returns a harness ready for testing.
// order is a permutation of [0,1,2] where 0=broker, 1=provider, 2=consumer.
func newHarness(t *testing.T, order []int) *harness {
	t.Helper()

	brokerPort, err := randomHighPort()
	if err != nil {
		t.Fatalf("allocate broker port: %v", err)
	}
	socks5Port, err := randomHighPort()
	if err != nil {
		t.Fatalf("allocate socks5 port: %v", err)
	}

	h := &harness{
		brokerPort: brokerPort,
		socks5Port: socks5Port,
	}

	// We always need the broker running before provider/consumer can connect,
	// but we honour the requested order by starting them in sequence and
	// letting the clients retry until the broker is up.
	starters := []func(){
		func() { // 0 = broker
			h.broker = launchBroker(t, brokerPort)
			waitForBroker(t, brokerPort, 15*time.Second)
		},
		func() { // 1 = provider
			h.provider = launchProvider(t, brokerPort)
		},
		func() { // 2 = consumer
			h.consumer = launchConsumer(t, brokerPort, socks5Port)
		},
	}

	for _, idx := range order {
		starters[idx]()
	}

	// If broker was not started first, wait for it now.
	if h.broker == nil {
		t.Fatal("broker must be in the order slice")
	}

	// Wait for SOCKS5 to be ready (consumer may still be connecting to broker).
	waitForSOCKS5(t, socks5Port, 20*time.Second)

	// Give provider a moment to register with the broker.
	time.Sleep(1 * time.Second)

	h.httpClient = newSOCKS5Client(t, socks5Port)
	return h
}

// stopAll gracefully stops all three components.
func (h *harness) stopAll(t *testing.T) {
	t.Helper()
	h.consumer.stop(t)
	h.provider.stop(t)
	h.broker.stop(t)
}

// ────────────────────────────────────────────────────────────────────────────
// Test Case 1 – Normal startup in random order
// ────────────────────────────────────────────────────────────────────────────

// TestIntegration_NormalStartup starts broker, provider, and consumer in a
// random permutation of orders, verifies SOCKS5 connectivity, then gracefully
// shuts everything down.
func TestIntegration_NormalStartup(t *testing.T) {
	buildBinaries(t)

	orders := [][]int{
		{0, 1, 2}, // broker → provider → consumer
		{0, 2, 1}, // broker → consumer → provider
		{1, 0, 2}, // provider → broker → consumer  (provider retries until broker up)
		{2, 0, 1}, // consumer → broker → provider
		{1, 2, 0}, // provider → consumer → broker
		{2, 1, 0}, // consumer → provider → broker
	}

	// Pick a random order for this run.
	chosen := orders[rand.Intn(len(orders))]
	names := []string{"broker", "provider", "consumer"}
	orderNames := make([]string, len(chosen))
	for i, idx := range chosen {
		orderNames[i] = names[idx]
	}
	t.Logf("Startup order: %v", orderNames)

	h := newHarness(t, chosen)
	defer h.stopAll(t)

	assertConnectivity(t, h.httpClient, "NormalStartup")
}

// ────────────────────────────────────────────────────────────────────────────
// Test Case 2 – Provider disconnect and reconnect
// ────────────────────────────────────────────────────────────────────────────

// TestIntegration_ProviderDisconnect kills the provider for a specified
// duration, then restarts it and verifies that the SOCKS5 proxy recovers.
func TestIntegration_ProviderDisconnect(t *testing.T) {
	buildBinaries(t)

	downDurations := []struct {
		name string
		d    time.Duration
	}{
		{"1s", 1 * time.Second},
		{"5s", 5 * time.Second},
		{"31s", 31 * time.Second},
	}

	for _, tc := range downDurations {
		tc := tc
		t.Run("ProviderDown_"+tc.name, func(t *testing.T) {
			// Each sub-test gets its own cluster on fresh random ports.
			h := newHarness(t, []int{0, 1, 2})
			defer h.stopAll(t)

			// Verify baseline connectivity.
			assertConnectivity(t, h.httpClient, "before-disconnect")

			// Kill the provider.
			t.Logf("[provider] disconnecting for %v…", tc.d)
			h.provider.kill(t)
			h.provider = nil

			// Wait for the provider to be down.
			time.Sleep(tc.d)

			// Restart the provider.
			t.Log("[provider] restarting…")
			h.provider = launchProvider(t, h.brokerPort)

			// Allow time for provider to reconnect and consumer to re-register.
			// The consumer detects provider disconnect and re-registers with the
			// broker within ~500 ms; the provider reconnects within 1 s backoff.
			// We wait generously to cover the 31 s case where yamux keepalive
			// may have already timed out.
			reconnectWait := 15 * time.Second
			if tc.d >= 30*time.Second {
				reconnectWait = 20 * time.Second
			}
			t.Logf("Waiting %v for reconnect…", reconnectWait)
			time.Sleep(reconnectWait)

			assertConnectivity(t, h.httpClient, "after-reconnect-"+tc.name)
		})
	}
}

// ────────────────────────────────────────────────────────────────────────────
// Test Case 3 – Consumer disconnect and reconnect
// ────────────────────────────────────────────────────────────────────────────

// TestIntegration_ConsumerDisconnect kills the consumer, restarts it on the
// same SOCKS5 port, and verifies that connectivity is restored.
func TestIntegration_ConsumerDisconnect(t *testing.T) {
	buildBinaries(t)

	h := newHarness(t, []int{0, 1, 2})
	defer h.stopAll(t)

	// Verify baseline.
	assertConnectivity(t, h.httpClient, "before-disconnect")

	// Kill the consumer.
	t.Log("[consumer] disconnecting…")
	h.consumer.kill(t)
	h.consumer = nil

	// Brief pause to let the OS release the port.
	time.Sleep(500 * time.Millisecond)

	// Restart the consumer on the same SOCKS5 port.
	t.Log("[consumer] restarting…")
	h.consumer = launchConsumer(t, h.brokerPort, h.socks5Port)
	waitForSOCKS5(t, h.socks5Port, 15*time.Second)

	// Give the consumer time to re-register with the broker.
	time.Sleep(2 * time.Second)

	// Rebuild the HTTP client because the old SOCKS5 connection is gone.
	h.httpClient = newSOCKS5Client(t, h.socks5Port)

	assertConnectivity(t, h.httpClient, "after-reconnect")
}

// ────────────────────────────────────────────────────────────────────────────
// Test Case 4 – Broker disconnect and reconnect
// ────────────────────────────────────────────────────────────────────────────

// TestIntegration_BrokerDisconnect kills the broker, restarts it on the same
// port, and verifies that provider and consumer reconnect automatically and
// SOCKS5 traffic flows again.
func TestIntegration_BrokerDisconnect(t *testing.T) {
	buildBinaries(t)

	h := newHarness(t, []int{0, 1, 2})
	defer h.stopAll(t)

	// Verify baseline.
	assertConnectivity(t, h.httpClient, "before-disconnect")

	// Kill the broker.
	t.Logf("[broker] disconnecting (port %d)…", h.brokerPort)
	h.broker.kill(t)
	h.broker = nil

	// Confirm the broker port is gone before restarting.
	waitForBrokerGone(t, h.brokerPort, 10*time.Second)

	// Restart the broker on the same port.
	t.Log("[broker] restarting…")
	h.broker = launchBroker(t, h.brokerPort)
	waitForBroker(t, h.brokerPort, 15*time.Second)

	// Provider and consumer both use exponential backoff starting at 1 s.
	// Give them time to reconnect (up to ~10 s should be more than enough).
	t.Log("Waiting for provider and consumer to reconnect…")
	time.Sleep(10 * time.Second)

	assertConnectivity(t, h.httpClient, "after-reconnect")
}
