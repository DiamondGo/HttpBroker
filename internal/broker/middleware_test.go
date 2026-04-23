package broker

import (
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestNoopAuthenticator(t *testing.T) {
	auth := &NoopAuthenticator{}
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	
	err := auth.Authenticate(req)
	if err != nil {
		t.Errorf("NoopAuthenticator should always return nil, got: %v", err)
	}
}

func TestTokenAuthenticator_ValidToken(t *testing.T) {
	token := "test-token-123"
	auth := NewTokenAuthenticator(token)
	
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	
	err := auth.Authenticate(req)
	if err != nil {
		t.Errorf("TokenAuthenticator should accept valid token, got error: %v", err)
	}
}

func TestTokenAuthenticator_InvalidToken(t *testing.T) {
	auth := NewTokenAuthenticator("correct-token")
	
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer wrong-token")
	
	err := auth.Authenticate(req)
	if err == nil {
		t.Error("TokenAuthenticator should reject invalid token")
	}
}

func TestTokenAuthenticator_MissingHeader(t *testing.T) {
	auth := NewTokenAuthenticator("test-token")
	
	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	
	err := auth.Authenticate(req)
	if err == nil {
		t.Error("TokenAuthenticator should reject request without Authorization header")
	}
}

func TestTokenAuthenticator_WrongFormat(t *testing.T) {
	auth := NewTokenAuthenticator("test-token")
	
	testCases := []struct {
		name   string
		header string
	}{
		{"No Bearer prefix", "test-token"},
		{"Wrong prefix", "Basic test-token"},
		{"Empty token", "Bearer "},
	}
	
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/test", nil)
			req.Header.Set("Authorization", tc.header)
			
			err := auth.Authenticate(req)
			if err == nil {
				t.Errorf("TokenAuthenticator should reject malformed header: %s", tc.header)
			}
		})
	}
}

func TestAuthMiddleware(t *testing.T) {
	handlerCalled := false
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true
		w.WriteHeader(http.StatusOK)
	})

	auth := NewTokenAuthenticator("test-token")

	// Test with redirect disabled (default behavior)
	t.Run("Valid token passes through (redirect disabled)", func(t *testing.T) {
		handlerCalled = false
		middleware := AuthMiddleware(auth, false, "", handler)
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req.Header.Set("Authorization", "Bearer test-token")
		w := httptest.NewRecorder()

		middleware.ServeHTTP(w, req)

		if !handlerCalled {
			t.Error("Handler should be called with valid token")
		}
		if w.Code != http.StatusOK {
			t.Errorf("Expected status 200, got %d", w.Code)
		}
	})

	// Test with invalid token and redirect disabled
	t.Run("Invalid token returns 401 (redirect disabled)", func(t *testing.T) {
		handlerCalled = false
		middleware := AuthMiddleware(auth, false, "", handler)
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req.Header.Set("Authorization", "Bearer wrong-token")
		w := httptest.NewRecorder()

		middleware.ServeHTTP(w, req)

		if handlerCalled {
			t.Error("Handler should not be called with invalid token")
		}
		if w.Code != http.StatusUnauthorized {
			t.Errorf("Expected status 401, got %d", w.Code)
		}
	})

	// Test with invalid token and redirect enabled
	t.Run("Invalid token returns 302 redirect (redirect enabled)", func(t *testing.T) {
		handlerCalled = false
		middleware := AuthMiddleware(auth, true, "https://www.google.com", handler)
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		req.Header.Set("Authorization", "Bearer wrong-token")
		w := httptest.NewRecorder()

		middleware.ServeHTTP(w, req)

		if handlerCalled {
			t.Error("Handler should not be called with invalid token")
		}
		if w.Code != http.StatusFound {
			t.Errorf("Expected status 302, got %d", w.Code)
		}
		location := w.Header().Get("Location")
		if location != "https://www.google.com" {
			t.Errorf("Expected redirect to https://www.google.com, got %s", location)
		}
	})

	// Test with missing auth header and redirect enabled
	t.Run("Missing auth header returns 302 redirect (redirect enabled)", func(t *testing.T) {
		handlerCalled = false
		middleware := AuthMiddleware(auth, true, "/login", handler)
		req := httptest.NewRequest(http.MethodGet, "/test", nil)
		// No Authorization header
		w := httptest.NewRecorder()

		middleware.ServeHTTP(w, req)

		if handlerCalled {
			t.Error("Handler should not be called without auth header")
		}
		if w.Code != http.StatusFound {
			t.Errorf("Expected status 302, got %d", w.Code)
		}
		location := w.Header().Get("Location")
		if location != "/login" {
			t.Errorf("Expected redirect to /login, got %s", location)
		}
	})
}

// TestBuildRedirectURL tests the buildRedirectURL function
func TestBuildRedirectURL(t *testing.T) {
	testCases := []struct {
		name           string
		configuredURL  string
		requestScheme  string // "http" or "https"
		expectedURL    string
	}{
		{
			name:          "Same-site path",
			configuredURL: "/login",
			requestScheme: "http",
			expectedURL:   "/login",
		},
		{
			name:          "Full URL with https",
			configuredURL: "https://www.google.com",
			requestScheme: "http",
			expectedURL:   "https://www.google.com",
		},
		{
			name:          "Full URL with http",
			configuredURL: "http://www.example.com",
			requestScheme: "https",
			expectedURL:   "http://www.example.com",
		},
		{
			name:          "Domain only with HTTP request",
			configuredURL: "www.google.com",
			requestScheme: "http",
			expectedURL:   "http://www.google.com",
		},
		{
			name:          "Domain only with HTTPS request",
			configuredURL: "www.google.com",
			requestScheme: "https",
			expectedURL:   "https://www.google.com",
		},
		{
			name:          "Empty URL defaults to /",
			configuredURL: "",
			requestScheme: "http",
			expectedURL:   "/",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Create request with appropriate scheme
			req := httptest.NewRequest(http.MethodGet, "/test", nil)
			if tc.requestScheme == "https" {
				req.TLS = &tls.ConnectionState{} // Non-nil TLS indicates HTTPS
			}

			result := buildRedirectURL(tc.configuredURL, req)
			if result != tc.expectedURL {
				t.Errorf("Expected %q, got %q", tc.expectedURL, result)
			}
		})
	}
}

