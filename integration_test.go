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
	"path/filepath"
	"regexp"
	"strings"
	"sync"
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

// extraArgs lets callers append config-file/mode overrides (e.g. --config
// pointing at a temp YAML file that turns on tunnel.enable_websocket) without
// every other launch* caller needing to know about them.
func launchBroker(t *testing.T, port int, extraArgs ...string) *component {
	t.Helper()
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	args := append([]string{"--listen", addr, "--enable-status"}, extraArgs...)
	cmd := exec.Command("./bin/httpbroker-broker", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start broker: %v", err)
	}
	t.Logf("[broker] started on %s (pid %d)", addr, cmd.Process.Pid)
	return &component{name: "broker", cmd: cmd}
}

func launchProvider(t *testing.T, brokerPort int, extraArgs ...string) *component {
	t.Helper()
	brokerURL := fmt.Sprintf("http://127.0.0.1:%d", brokerPort)
	args := append([]string{
		"--broker-url", brokerURL,
		"--endpoint", testEndpoint,
	}, extraArgs...)
	cmd := exec.Command("./bin/httpbroker-provider", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start provider: %v", err)
	}
	t.Logf("[provider] started → broker %s (pid %d)", brokerURL, cmd.Process.Pid)
	return &component{name: "provider", cmd: cmd}
}

func launchConsumer(t *testing.T, brokerPort, socks5Port int, extraArgs ...string) *component {
	t.Helper()
	brokerURL := fmt.Sprintf("http://127.0.0.1:%d", brokerPort)
	socks5Addr := fmt.Sprintf("127.0.0.1:%d", socks5Port)
	args := append([]string{
		"--broker-url", brokerURL,
		"--endpoint", testEndpoint,
		"--socks5-listen", socks5Addr,
	}, extraArgs...)
	cmd := exec.Command("./bin/httpbroker-consumer", args...)
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

// ────────────────────────────────────────────────────────────────────────────
// Test Case 5 – WebSocket transport
// ────────────────────────────────────────────────────────────────────────────

// writeTempConfig writes a minimal YAML config file under t.TempDir() and
// returns its path. Fields left out are zero, so the corresponding main.go
// picks its own default (or a CLI flag overrides it, as launchBroker et al.
// already do for listen/broker-url/endpoint/socks5-listen) — this only needs
// to carry the one setting the test cares about.
func writeTempConfig(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write temp config %s: %v", name, err)
	}
	return path
}

// TestIntegration_WebSocketTransport starts broker, provider, and consumer
// with tunnel.enable_websocket / transport.prefer_websocket turned on via
// config file, and verifies SOCKS5 traffic actually flows — i.e. that
// negotiation lands on pollmux's WebSocket transport end to end through real
// subprocess binaries, not just that the code compiles against the new
// pollmux fields.
func TestIntegration_WebSocketTransport(t *testing.T) {
	buildBinaries(t)

	brokerCfg := writeTempConfig(t, "broker.yaml", "tunnel:\n  enable_websocket: true\n")
	consumerCfg := writeTempConfig(t, "consumer.yaml", "transport:\n  prefer_websocket: true\n")
	providerCfg := writeTempConfig(t, "provider.yaml", "transport:\n  prefer_websocket: true\n")

	brokerPort, err := randomHighPort()
	if err != nil {
		t.Fatalf("allocate broker port: %v", err)
	}
	socks5Port, err := randomHighPort()
	if err != nil {
		t.Fatalf("allocate socks5 port: %v", err)
	}

	broker := launchBroker(t, brokerPort, "--config", brokerCfg)
	defer broker.stop(t)
	waitForBroker(t, brokerPort, 15*time.Second)

	provider := launchProvider(t, brokerPort, "--config", providerCfg)
	defer provider.stop(t)

	consumer := launchConsumer(t, brokerPort, socks5Port, "--config", consumerCfg)
	defer consumer.stop(t)
	waitForSOCKS5(t, socks5Port, 20*time.Second)

	// Give provider a moment to register with the broker.
	time.Sleep(1 * time.Second)

	client := newSOCKS5Client(t, socks5Port)
	assertConnectivity(t, client, "WebSocketTransport")
}

// ────────────────────────────────────────────────────────────────────────────
// Resumable-session helpers
// ────────────────────────────────────────────────────────────────────────────

// logSink tees a subprocess's stdout/stderr to the test's own stdout while
// keeping a copy the test can grep for negotiation and resume events.
type logSink struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (l *logSink) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *logSink) count(substr string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Count(l.buf.String(), substr)
}

// launchBrokerCapturing is launchBroker with the broker's output also copied
// into sink.
func launchBrokerCapturing(t *testing.T, port int, sink *logSink, extraArgs ...string) *component {
	t.Helper()
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	args := append([]string{"--listen", addr, "--enable-status"}, extraArgs...)
	cmd := exec.Command("./bin/httpbroker-broker", args...)
	out := io.MultiWriter(os.Stdout, sink)
	cmd.Stdout = out
	cmd.Stderr = out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start broker: %v", err)
	}
	t.Logf("[broker] started on %s (pid %d)", addr, cmd.Process.Pid)
	return &component{name: "broker", cmd: cmd}
}

// tcpProxy is a transparent TCP forwarder placed between a client and the
// broker so a test can sever every live connection on that hop at will —
// the local stand-in for a CDN/reverse proxy cutting a connection at its
// max connection age. The listener stays up, so the client can reconnect
// (and, for a resumable session, resume) straight away.
type tcpProxy struct {
	ln     net.Listener
	target string
	mu     sync.Mutex
	conns  map[net.Conn]struct{}
}

func newTCPProxy(t *testing.T, targetPort int) *tcpProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("proxy listen: %v", err)
	}
	p := &tcpProxy{
		ln:     ln,
		target: fmt.Sprintf("127.0.0.1:%d", targetPort),
		conns:  make(map[net.Conn]struct{}),
	}
	t.Cleanup(func() { _ = ln.Close(); p.dropAll() })
	go p.serve()
	return p
}

func (p *tcpProxy) port() int { return p.ln.Addr().(*net.TCPAddr).Port }

func (p *tcpProxy) serve() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		go p.handle(c)
	}
}

func (p *tcpProxy) handle(client net.Conn) {
	upstream, err := net.Dial("tcp", p.target)
	if err != nil {
		client.Close()
		return
	}
	p.mu.Lock()
	p.conns[client] = struct{}{}
	p.conns[upstream] = struct{}{}
	p.mu.Unlock()
	defer func() {
		p.mu.Lock()
		delete(p.conns, client)
		delete(p.conns, upstream)
		p.mu.Unlock()
		client.Close()
		upstream.Close()
	}()
	done := make(chan struct{}, 2)
	go func() { io.Copy(upstream, client); done <- struct{}{} }()
	go func() { io.Copy(client, upstream); done <- struct{}{} }()
	<-done
}

// dropAll severs every connection currently flowing through the proxy.
func (p *tcpProxy) dropAll() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := 0
	for c := range p.conns {
		c.Close()
		n++
	}
	return n
}

// startEchoServer serves a TCP echo on a random loopback port; the provider
// dials it as the SOCKS5 CONNECT target so the test controls both ends of
// the tunnelled stream and can check it byte for byte.
func startEchoServer(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("echo listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

// echoRoundTrip writes a message on the tunnelled stream and expects it
// echoed back within timeout. After a transport drop this is the assertion
// that the very same yamux stream is still alive.
func echoRoundTrip(t *testing.T, conn net.Conn, msg string, timeout time.Duration) {
	t.Helper()
	_ = conn.SetDeadline(time.Now().Add(timeout))
	defer conn.SetDeadline(time.Time{})
	if _, err := io.WriteString(conn, msg); err != nil {
		t.Fatalf("write %q: %v", msg, err)
	}
	buf := make([]byte, len(msg))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read echo of %q: %v", msg, err)
	}
	if string(buf) != msg {
		t.Fatalf("echo mismatch: sent %q, got %q", msg, buf)
	}
}

// sessionIDForRole pulls the session id the broker logged for the given
// role out of its "session created" line.
func sessionIDForRole(t *testing.T, sink *logSink, role string) string {
	t.Helper()
	sink.mu.Lock()
	log := sink.buf.String()
	sink.mu.Unlock()
	re := regexp.MustCompile(`"msg":"session created","session_id":"([0-9a-f]+)","role":"` + role + `"`)
	m := re.FindStringSubmatch(log)
	if m == nil {
		t.Fatalf("no session-created line for role %q in broker log", role)
	}
	return m[1]
}

func waitForLogCount(t *testing.T, sink *logSink, substr string, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if sink.count(substr) >= want {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d× %q in broker log (have %d)", want, substr, sink.count(substr))
}

const (
	// brokerSessionCreated is HttpBroker's own log line, one per session
	// the broker admits; pollmux's line for the same event carries the
	// negotiated flags.
	brokerSessionCreated   = `"msg":"session created"`
	pollmuxResumableCreate = `"msg":"pollmux: session created"`
	resumableTrue          = `"resumable":true`
)

// resumedLine is the prefix of pollmux's log line for a successful /resume
// of the given session.
func resumedLine(sessionID string) string {
	return `"msg":"pollmux: session resumed","session_id":"` + sessionID + `"`
}

// runResumableIntegration launches broker/provider/consumer with the given
// config snippets, with each client hop routed through a tcpProxy, and
// proves the session actually resumes rather than merely reconnects:
//
//  1. the broker's connect log must report resumable:true for both the
//     consumer and the provider session;
//  2. a tunnelled TCP stream (SOCKS5 → provider → local echo server) is
//     opened and exercised;
//  3. every connection on the consumer hop is severed, then every one on the
//     provider hop; after each cut the broker must log a successful resume
//     and the *same* stream must still echo;
//  4. the broker must never have created more than the original two
//     sessions — a client falling back to today's reconnect path would
//     show up as a third.
//
// Both transports pollmux can resume are covered by the two tests below.
func runResumableIntegration(t *testing.T, label, brokerTunnel, clientTransport string) {
	t.Helper()
	buildBinaries(t)

	brokerCfg := writeTempConfig(t, "broker.yaml", "tunnel:\n"+brokerTunnel)
	consumerCfg := writeTempConfig(t, "consumer.yaml", "transport:\n"+clientTransport)
	providerCfg := writeTempConfig(t, "provider.yaml", "transport:\n"+clientTransport)

	brokerPort, err := randomHighPort()
	if err != nil {
		t.Fatalf("allocate broker port: %v", err)
	}
	socks5Port, err := randomHighPort()
	if err != nil {
		t.Fatalf("allocate socks5 port: %v", err)
	}

	sink := &logSink{}
	broker := launchBrokerCapturing(t, brokerPort, sink, "--config", brokerCfg)
	defer broker.stop(t)
	waitForBroker(t, brokerPort, 15*time.Second)

	providerHop := newTCPProxy(t, brokerPort)
	consumerHop := newTCPProxy(t, brokerPort)

	provider := launchProvider(t, providerHop.port(), "--config", providerCfg)
	defer provider.stop(t)

	consumer := launchConsumer(t, consumerHop.port(), socks5Port, "--config", consumerCfg)
	defer consumer.stop(t)
	waitForSOCKS5(t, socks5Port, 20*time.Second)

	// 1. Both sessions negotiated resume.
	waitForLogCount(t, sink, brokerSessionCreated, 2, 10*time.Second)
	if got := sink.count(pollmuxResumableCreate); got != 2 {
		t.Fatalf("[%s] expected 2 pollmux session-created lines, got %d", label, got)
	}
	if got := sink.count(resumableTrue); got != 2 {
		t.Fatalf("[%s] expected both sessions negotiated resumable:true, broker log shows %d", label, got)
	}

	// 2. Open a tunnelled stream and prove it works.
	echoPort := startEchoServer(t)
	dialer, err := proxy.SOCKS5("tcp", fmt.Sprintf("127.0.0.1:%d", socks5Port), nil, proxy.Direct)
	if err != nil {
		t.Fatalf("socks5 dialer: %v", err)
	}
	var conn net.Conn
	waitFor := time.Now().Add(15 * time.Second)
	for {
		conn, err = dialer.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", echoPort))
		if err == nil || time.Now().After(waitFor) {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("[%s] dial echo through SOCKS5: %v", label, err)
	}
	defer conn.Close()
	echoRoundTrip(t, conn, "before any drop\n", 10*time.Second)

	// 3. Cut each hop in turn; the session on that hop must resume (not
	//    reconnect) and the stream must survive both cuts.
	for _, hop := range []struct {
		name  string
		proxy *tcpProxy
	}{{"consumer", consumerHop}, {"provider", providerHop}} {
		sessionID := sessionIDForRole(t, sink, hop.name)
		if sink.count(resumedLine(sessionID)) != 0 {
			t.Fatalf("[%s] %s session resumed before its hop was cut", label, hop.name)
		}
		n := hop.proxy.dropAll()
		t.Logf("[%s] severed %d connection(s) on the %s hop", label, n, hop.name)
		if n == 0 {
			t.Fatalf("[%s] no live connections on the %s hop to sever", label, hop.name)
		}
		waitForLogCount(t, sink, resumedLine(sessionID), 1, 20*time.Second)
		echoRoundTrip(t, conn, fmt.Sprintf("after %s drop\n", hop.name), 20*time.Second)
	}

	// 4. No client fell back to a fresh session.
	if got := sink.count(brokerSessionCreated); got != 2 {
		t.Fatalf("[%s] expected exactly 2 sessions for the whole run, broker created %d "+
			"(a client reconnected with a new session instead of resuming)", label, got)
	}

	// The ordinary path still works on top of the resumed sessions.
	assertConnectivity(t, newSOCKS5Client(t, socks5Port), label)
}

// TestIntegration_ResumableStream: resume negotiated over stream mode in
// both directions. upload_stream_preference is forced to "stream" so the
// connect-time probe is skipped — with it on auto, a probe failure would
// silently fall back to a non-resumable session, which the resumable:true
// assertion would then catch rather than the test passing by accident.
func TestIntegration_ResumableStream(t *testing.T) {
	runResumableIntegration(t, "ResumableStream",
		"  enable_resume: true\n  resume_grace: \"30s\"\n",
		"  poll_mode: \"stream\"\n  upload_stream_preference: \"stream\"\n  prefer_resume: true\n")
}

// TestIntegration_ResumableWebSocket: resume negotiated over the WebSocket
// transport — the combination local/*.yaml actually deploys.
func TestIntegration_ResumableWebSocket(t *testing.T) {
	runResumableIntegration(t, "ResumableWebSocket",
		"  enable_websocket: true\n  enable_resume: true\n  resume_grace: \"30s\"\n",
		"  prefer_websocket: true\n  prefer_resume: true\n")
}
