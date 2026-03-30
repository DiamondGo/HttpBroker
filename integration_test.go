package httpbroker_test

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/proxy"
)

const (
	brokerAddr   = "127.0.0.1:19090"
	socks5Addr   = "127.0.0.1:11080"
	brokerURL    = "http://127.0.0.1:19090"
	testEndpoint = "integration-test"
)

// TestIntegration runs a full end-to-end integration test of the HttpBroker system.
// It starts broker, provider, and consumer locally and tests HTTP/HTTPS GET/POST requests.
func TestIntegration(t *testing.T) {
	// Build binaries first
	t.Log("Building binaries...")
	if err := buildBinaries(); err != nil {
		t.Fatalf("Failed to build binaries: %v", err)
	}

	// Start broker
	t.Log("Starting broker...")
	brokerCmd, err := startBroker()
	if err != nil {
		t.Fatalf("Failed to start broker: %v", err)
	}
	defer killProcess(brokerCmd)

	// Wait for broker to be ready
	if err := waitForBroker(brokerURL, 10*time.Second); err != nil {
		t.Fatalf("Broker failed to start: %v", err)
	}
	t.Log("Broker is ready")

	// Start provider
	t.Log("Starting provider...")
	providerCmd, err := startProvider()
	if err != nil {
		t.Fatalf("Failed to start provider: %v", err)
	}
	defer killProcess(providerCmd)

	// Wait for provider to connect
	time.Sleep(2 * time.Second)
	t.Log("Provider started")

	// Start consumer
	t.Log("Starting consumer...")
	consumerCmd, err := startConsumer()
	if err != nil {
		t.Fatalf("Failed to start consumer: %v", err)
	}
	defer killProcess(consumerCmd)

	// Wait for consumer to be ready
	if err := waitForSOCKS5(socks5Addr, 10*time.Second); err != nil {
		t.Fatalf("Consumer SOCKS5 server failed to start: %v", err)
	}
	t.Log("Consumer is ready")

	// Create SOCKS5 dialer
	dialer, err := createSOCKS5Dialer(socks5Addr)
	if err != nil {
		t.Fatalf("Failed to create SOCKS5 dialer: %v", err)
	}

	// Create HTTP client with SOCKS5 proxy
	httpClient := &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return dialer.Dial(network, addr)
			},
		},
		Timeout: 30 * time.Second,
	}

	// Run test cases
	t.Run("HTTP_GET", func(t *testing.T) {
		testHTTPGet(t, httpClient)
	})

	t.Run("HTTPS_GET", func(t *testing.T) {
		testHTTPSGet(t, httpClient)
	})

	t.Run("HTTP_POST", func(t *testing.T) {
		testHTTPPost(t, httpClient)
	})

	t.Run("HTTPS_POST", func(t *testing.T) {
		testHTTPSPost(t, httpClient)
	})

	t.Run("Multiple_Requests", func(t *testing.T) {
		testMultipleRequests(t, httpClient)
	})
}

// buildBinaries compiles all three binaries
func buildBinaries() error {
	cmd := exec.Command("go", "build", "-o", "bin/httpbroker-broker", "./cmd/broker")
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("build broker failed: %v\n%s", err, output)
	}

	cmd = exec.Command("go", "build", "-o", "bin/httpbroker-consumer", "./cmd/consumer")
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("build consumer failed: %v\n%s", err, output)
	}

	cmd = exec.Command("go", "build", "-o", "bin/httpbroker-provider", "./cmd/provider")
	if output, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("build provider failed: %v\n%s", err, output)
	}

	return nil
}

// startBroker starts the broker process
func startBroker() (*exec.Cmd, error) {
	cmd := exec.Command("./bin/httpbroker-broker", "--listen", brokerAddr)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}

// startProvider starts the provider process
func startProvider() (*exec.Cmd, error) {
	cmd := exec.Command("./bin/httpbroker-provider",
		"--broker-url", brokerURL,
		"--endpoint", testEndpoint)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}

// startConsumer starts the consumer process
func startConsumer() (*exec.Cmd, error) {
	cmd := exec.Command("./bin/httpbroker-consumer",
		"--broker-url", brokerURL,
		"--endpoint", testEndpoint,
		"--socks5-listen", socks5Addr)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return cmd, nil
}

// killProcess kills a process gracefully
func killProcess(cmd *exec.Cmd) {
	if cmd != nil && cmd.Process != nil {
		cmd.Process.Kill()
		cmd.Wait()
	}
}

// waitForBroker waits for the broker to become ready
func waitForBroker(url string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get(url + "/status")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("broker did not become ready within %v", timeout)
}

// waitForSOCKS5 waits for the SOCKS5 server to become ready
func waitForSOCKS5(addr string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			conn.Close()
			return nil
		}
		time.Sleep(200 * time.Millisecond)
	}
	return fmt.Errorf("SOCKS5 server did not become ready within %v", timeout)
}

// createSOCKS5Dialer creates a SOCKS5 dialer
func createSOCKS5Dialer(addr string) (proxy.Dialer, error) {
	return proxy.SOCKS5("tcp", addr, nil, proxy.Direct)
}

// testHTTPGet tests HTTP GET request
func testHTTPGet(t *testing.T, client *http.Client) {
	t.Log("Testing HTTP GET request to example.com...")

	resp, err := client.Get("http://example.com/")
	if err != nil {
		t.Fatalf("HTTP GET request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("Failed to read response body: %v", err)
	}

	bodyStr := string(body)
	if !strings.Contains(bodyStr, "Example Domain") {
		t.Errorf("Response body doesn't contain 'Example Domain'")
	}

	t.Logf("✓ HTTP GET successful, received %d bytes", len(body))
}

// testHTTPSGet tests HTTPS GET request
func testHTTPSGet(t *testing.T, client *http.Client) {
	t.Log("Testing HTTPS GET request to httpbin.org...")

	resp, err := client.Get("https://httpbin.org/get")
	if err != nil {
		t.Fatalf("HTTPS GET request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("Failed to read response body: %v", err)
	}

	bodyStr := string(body)
	if !strings.Contains(bodyStr, "httpbin.org") {
		t.Errorf("Response body doesn't contain 'httpbin.org'")
	}

	t.Logf("✓ HTTPS GET successful, received %d bytes", len(body))
}

// testHTTPPost tests HTTP POST request
func testHTTPPost(t *testing.T, client *http.Client) {
	t.Log("Testing HTTP POST request to httpbin.org...")

	payload := strings.NewReader(`{"name":"test","value":"integration-test"}`)
	resp, err := client.Post("http://httpbin.org/post", "application/json", payload)
	if err != nil {
		t.Fatalf("HTTP POST request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("Failed to read response body: %v", err)
	}

	bodyStr := string(body)
	if !strings.Contains(bodyStr, "integration-test") {
		t.Errorf("Response body doesn't contain posted data")
	}

	t.Logf("✓ HTTP POST successful, received %d bytes", len(body))
}

// testHTTPSPost tests HTTPS POST request
func testHTTPSPost(t *testing.T, client *http.Client) {
	t.Log("Testing HTTPS POST request to httpbin.org...")

	payload := strings.NewReader(`{"test":"https-post","number":12345}`)
	resp, err := client.Post("https://httpbin.org/post", "application/json", payload)
	if err != nil {
		t.Fatalf("HTTPS POST request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("Expected status 200, got %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("Failed to read response body: %v", err)
	}

	bodyStr := string(body)
	if !strings.Contains(bodyStr, "https-post") || !strings.Contains(bodyStr, "12345") {
		t.Errorf("Response body doesn't contain posted data")
	}

	t.Logf("✓ HTTPS POST successful, received %d bytes", len(body))
}

// testMultipleRequests tests multiple consecutive requests
func testMultipleRequests(t *testing.T, client *http.Client) {
	t.Log("Testing multiple consecutive requests...")

	for i := 0; i < 5; i++ {
		resp, err := client.Get("http://example.com/")
		if err != nil {
			t.Fatalf("Request %d failed: %v", i+1, err)
		}
		resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("Request %d: expected status 200, got %d", i+1, resp.StatusCode)
		}

		t.Logf("  Request %d: ✓", i+1)
		time.Sleep(100 * time.Millisecond)
	}

	t.Log("✓ All consecutive requests successful")
}

