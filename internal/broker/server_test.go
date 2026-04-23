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

// TestUnauthorizedRedirectDisabled tests that unauthorized requests return 401 when redirect is disabled
func TestUnauthorizedRedirectDisabled(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	defer logger.Sync()

	cfg := Config{
		ListenAddr:                  "127.0.0.1:18092",
		UseTLS:                      false,
		PollTimeout:                 1 * time.Second,
		SessionTimeout:              5 * time.Minute,
		AuthEnabled:                 true,
		AuthToken:                   "test-secret-token",
		UnauthorizedRedirectEnabled: false, // Redirect disabled (default)
		UnauthorizedRedirectURL:     "",
	}

	srv := NewServer(cfg, logger)
	go func() {
		if err := srv.Start(); err != nil && err != http.ErrServerClosed {
			t.Logf("server error: %v", err)
		}
	}()
	defer srv.Stop(context.Background())

	time.Sleep(200 * time.Millisecond)

	// Test unauthorized request to /tunnel/connect returns 401
	t.Run("Unauthorized tunnel request returns 401", func(t *testing.T) {
		req, _ := http.NewRequest(
			http.MethodPost,
			"http://127.0.0.1:18092/tunnel/connect?role=consumer&endpoint=test",
			nil,
		)
		// No auth token
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("failed to make request: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("expected status 401, got %d", resp.StatusCode)
		}
	})

	// Test non-existent path returns 404
	t.Run("Non-existent path returns 404", func(t *testing.T) {
		resp, err := http.Get("http://127.0.0.1:18092/nonexistent")
		if err != nil {
			t.Fatalf("failed to make request: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("expected status 404, got %d", resp.StatusCode)
		}
	})
}

// TestUnauthorizedRedirectEnabled tests that unauthorized requests are redirected when enabled
func TestUnauthorizedRedirectEnabled(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	defer logger.Sync()

	cfg := Config{
		ListenAddr:                  "127.0.0.1:18093",
		UseTLS:                      false,
		PollTimeout:                 1 * time.Second,
		SessionTimeout:              5 * time.Minute,
		AuthEnabled:                 true,
		AuthToken:                   "test-secret-token",
		UnauthorizedRedirectEnabled: true,
		UnauthorizedRedirectURL:     "https://www.example.com",
	}

	srv := NewServer(cfg, logger)
	go func() {
		if err := srv.Start(); err != nil && err != http.ErrServerClosed {
			t.Logf("server error: %v", err)
		}
	}()
	defer srv.Stop(context.Background())

	time.Sleep(200 * time.Millisecond)

	// Create a client that does NOT follow redirects
	noRedirectClient := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse // Don't follow redirects
		},
	}

	// Test unauthorized request to /tunnel/connect returns 302
	t.Run("Unauthorized tunnel request returns 302", func(t *testing.T) {
		req, _ := http.NewRequest(
			http.MethodPost,
			"http://127.0.0.1:18093/tunnel/connect?role=consumer&endpoint=test",
			nil,
		)
		// No auth token
		resp, err := noRedirectClient.Do(req)
		if err != nil {
			t.Fatalf("failed to make request: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusFound {
			t.Errorf("expected status 302, got %d", resp.StatusCode)
		}

		location := resp.Header.Get("Location")
		if location != "https://www.example.com" {
			t.Errorf("expected redirect to https://www.example.com, got %s", location)
		}
	})

	// Test non-existent path returns 302
	t.Run("Non-existent path returns 302", func(t *testing.T) {
		resp, err := noRedirectClient.Get("http://127.0.0.1:18093/nonexistent")
		if err != nil {
			t.Fatalf("failed to make request: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusFound {
			t.Errorf("expected status 302, got %d", resp.StatusCode)
		}

		location := resp.Header.Get("Location")
		if location != "https://www.example.com" {
			t.Errorf("expected redirect to https://www.example.com, got %s", location)
		}
	})

	// Test authorized request still works
	t.Run("Authorized request works normally", func(t *testing.T) {
		req, _ := http.NewRequest(
			http.MethodPost,
			"http://127.0.0.1:18093/tunnel/connect?role=consumer&endpoint=test",
			nil,
		)
		req.Header.Set("Authorization", "Bearer test-secret-token")
		resp, err := noRedirectClient.Do(req)
		if err != nil {
			t.Fatalf("failed to make request: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			t.Errorf("expected status 200 for authorized request, got %d", resp.StatusCode)
		}
	})
}

// TestUnauthorizedRedirectWithRelativePath tests redirect with same-site path
func TestUnauthorizedRedirectWithRelativePath(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	defer logger.Sync()

	cfg := Config{
		ListenAddr:                  "127.0.0.1:18094",
		UseTLS:                      false,
		PollTimeout:                 1 * time.Second,
		SessionTimeout:              5 * time.Minute,
		AuthEnabled:                 true,
		AuthToken:                   "test-secret-token",
		UnauthorizedRedirectEnabled: true,
		UnauthorizedRedirectURL:     "/login", // Same-site redirect
	}

	srv := NewServer(cfg, logger)
	go func() {
		if err := srv.Start(); err != nil && err != http.ErrServerClosed {
			t.Logf("server error: %v", err)
		}
	}()
	defer srv.Stop(context.Background())

	time.Sleep(200 * time.Millisecond)

	noRedirectClient := &http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	t.Run("Redirect to same-site path", func(t *testing.T) {
		req, _ := http.NewRequest(
			http.MethodPost,
			"http://127.0.0.1:18094/tunnel/connect?role=consumer&endpoint=test",
			nil,
		)
		resp, err := noRedirectClient.Do(req)
		if err != nil {
			t.Fatalf("failed to make request: %v", err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusFound {
			t.Errorf("expected status 302, got %d", resp.StatusCode)
		}

		location := resp.Header.Get("Location")
		if location != "/login" {
			t.Errorf("expected redirect to /login, got %s", location)
		}
	})
}
