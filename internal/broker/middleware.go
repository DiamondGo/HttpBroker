package broker

import (
	"errors"
	"net/http"
	"strings"
)

// buildRedirectURL constructs the redirect URL based on the configured URL and current request.
// Supports three formats:
//  1. "/path" - same-site path, used as-is
//  2. "www.example.com" - domain name, auto-prefixed with http/https based on current request
//  3. "https://example.com" - full URL with scheme, used as-is
func buildRedirectURL(configuredURL string, r *http.Request) string {
	if configuredURL == "" {
		return "/"
	}

	// Case 1: Relative path starting with "/"
	if strings.HasPrefix(configuredURL, "/") {
		return configuredURL
	}

	// Case 2: Full URL with scheme (http:// or https://)
	if strings.HasPrefix(configuredURL, "http://") || strings.HasPrefix(configuredURL, "https://") {
		return configuredURL
	}

	// Case 3: Domain name without scheme - determine scheme from current request
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	// Support reverse proxy scenario (X-Forwarded-Proto header)
	if proto := r.Header.Get("X-Forwarded-Proto"); proto != "" {
		scheme = proto
	}

	return scheme + "://" + configuredURL
}

// Authenticator is the interface for request authentication.
type Authenticator interface {
	Authenticate(r *http.Request) error
}

// NoopAuthenticator allows all requests through.
type NoopAuthenticator struct{}

// Authenticate always returns nil, allowing all requests.
func (a *NoopAuthenticator) Authenticate(r *http.Request) error {
	return nil
}

// TokenAuthenticator validates Bearer token from Authorization header.
type TokenAuthenticator struct {
	token string
}

// NewTokenAuthenticator creates a new TokenAuthenticator with the given token.
func NewTokenAuthenticator(token string) *TokenAuthenticator {
	return &TokenAuthenticator{token: token}
}

// Authenticate checks if the request contains a valid Bearer token.
// Expected format: Authorization: Bearer <token>
func (a *TokenAuthenticator) Authenticate(r *http.Request) error {
	authHeader := r.Header.Get("Authorization")
	if authHeader == "" {
		return errors.New("missing Authorization header")
	}

	// Check for "Bearer " prefix
	const bearerPrefix = "Bearer "
	if !strings.HasPrefix(authHeader, bearerPrefix) {
		return errors.New("invalid Authorization header format, expected 'Bearer <token>'")
	}

	// Extract token
	token := strings.TrimPrefix(authHeader, bearerPrefix)
	if token == "" {
		return errors.New("empty token")
	}

	// Compare with configured token
	if token != a.token {
		return errors.New("invalid token")
	}

	return nil
}

// AuthMiddleware wraps an http.Handler with authentication.
// If authentication fails:
//   - When redirectEnabled is true and redirectURL is set: redirects to the configured URL with 302
//   - Otherwise: returns a 401 Unauthorized response
func AuthMiddleware(auth Authenticator, redirectEnabled bool, redirectURL string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := auth.Authenticate(r); err != nil {
			// If redirect is enabled, redirect instead of returning 401
			if redirectEnabled && redirectURL != "" {
				targetURL := buildRedirectURL(redirectURL, r)
				http.Redirect(w, r, targetURL, http.StatusFound)
				return
			}
			// Default behavior: return 401 Unauthorized
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
