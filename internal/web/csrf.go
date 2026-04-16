package web

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"log"
	"net/http"
)

// mustRandBytes returns n cryptographically-random bytes and terminates the
// process if the system RNG is unavailable; the CSRF secret is computed once
// at server startup so failing loudly is preferable to silently rolling a
// deterministic value.
func mustRandBytes(n int) []byte {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		log.Fatalf("FATAL: failed to generate CSRF secret: %v", err)
	}
	return b
}

// csrfToken computes a token for the given session ID using HMAC-SHA256 with
// the Server's own secret. Kept as a method so the secret travels with the
// server instance — tests can now stand up a fresh Server without relying on
// a package-level global set at init time.
func (s *Server) csrfToken(sessionID string) string {
	mac := hmac.New(sha256.New, s.csrfSecret)
	mac.Write([]byte(sessionID))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// validateCSRF checks whether the token matches the session ID.
func (s *Server) validateCSRF(sessionID, token string) bool {
	expected := s.csrfToken(sessionID)
	return subtle.ConstantTimeCompare([]byte(token), []byte(expected)) == 1
}

// CSRFMiddleware validates the CSRF token on mutating requests.
// /api/v1/ routes are exempt (bearer token serves as CSRF mitigation).
func (s *Server) CSRFMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only validate on state-changing methods
		if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}

		// API routes are exempt
		if len(r.URL.Path) >= 8 && r.URL.Path[:8] == "/api/v1/" {
			next.ServeHTTP(w, r)
			return
		}

		sessID := sessionFromContext(r.Context())

		var token string
		if t := r.FormValue("_csrf"); t != "" {
			token = t
		} else {
			token = r.Header.Get("X-CSRF-Token")
		}

		if !s.validateCSRF(sessID, token) {
			http.Error(w, "CSRF token invalid", http.StatusForbidden)
			return
		}

		next.ServeHTTP(w, r)
	})
}
