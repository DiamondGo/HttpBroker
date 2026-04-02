package broker

import (
	"errors"
	"net/http"
	"strings"
)

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
// If authentication fails, it returns a 401 Unauthorized response.
func AuthMiddleware(auth Authenticator, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := auth.Authenticate(r); err != nil {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
