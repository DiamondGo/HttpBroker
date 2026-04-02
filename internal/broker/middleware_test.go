package broker

import (
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
	middleware := AuthMiddleware(auth, handler)
	
	// Test with valid token
	t.Run("Valid token passes through", func(t *testing.T) {
		handlerCalled = false
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
	
	// Test with invalid token
	t.Run("Invalid token returns 401", func(t *testing.T) {
		handlerCalled = false
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
}

