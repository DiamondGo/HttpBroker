package broker

import (
	"context"
	"net/http"
	"testing"
	"time"

	"go.uber.org/zap"
)

// TestStatusEndpointEnabled tests that /status is accessible when enabled
func TestStatusEndpointEnabled(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	defer logger.Sync()

	cfg := Config{
		ListenAddr:            "127.0.0.1:18090",
		UseTLS:                false,
		PollTimeout:           1 * time.Second,
		SessionTimeout:        5 * time.Minute,
		AuthEnabled:           false,
		StatusEndpointEnabled: true, // Enable status endpoint
	}

	srv := NewServer(cfg, logger)
	go func() {
		if err := srv.Start(); err != nil && err != http.ErrServerClosed {
			t.Logf("server error: %v", err)
		}
	}()
	defer srv.Stop(context.Background())

	// Wait for server to start
	time.Sleep(200 * time.Millisecond)

	// Test that /status is accessible
	resp, err := http.Get("http://127.0.0.1:18090/status")
	if err != nil {
		t.Fatalf("failed to get /status: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected status 200, got %d", resp.StatusCode)
	}
}

// TestStatusEndpointDisabled tests that /status returns 404 when disabled
func TestStatusEndpointDisabled(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	defer logger.Sync()

	cfg := Config{
		ListenAddr:            "127.0.0.1:18091",
		UseTLS:                false,
		PollTimeout:           1 * time.Second,
		SessionTimeout:        5 * time.Minute,
		AuthEnabled:           false,
		StatusEndpointEnabled: false, // Disable status endpoint (default)
	}

	srv := NewServer(cfg, logger)
	go func() {
		if err := srv.Start(); err != nil && err != http.ErrServerClosed {
			t.Logf("server error: %v", err)
		}
	}()
	defer srv.Stop(context.Background())

	// Wait for server to start
	time.Sleep(200 * time.Millisecond)

	// Test that /status returns 404
	resp, err := http.Get("http://127.0.0.1:18091/status")
	if err != nil {
		t.Fatalf("failed to get /status: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("expected status 404 when endpoint is disabled, got %d", resp.StatusCode)
	}
}
