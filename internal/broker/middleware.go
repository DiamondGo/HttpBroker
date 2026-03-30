package broker

import "net/http"

// Authenticator is the interface for request authentication.
// Currently a no-op; future implementations can check tokens, JWTs, etc.
type Authenticator interface {
	Authenticate(r *http.Request) error
}

// NoopAuthenticator allows all requests through.
type NoopAuthenticator struct{}

// Authenticate always returns nil, allowing all requests.
func (a *NoopAuthenticator) Authenticate(r *http.Request) error {
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
