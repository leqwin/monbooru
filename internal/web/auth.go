package web

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

type contextKey int

const sessionContextKey contextKey = 1

// Session holds session data for a logged-in user.
type Session struct {
	ID        string
	ExpiresAt time.Time
}

// SessionStore is an in-memory session store.
type SessionStore struct {
	mu       sync.RWMutex
	sessions map[string]Session
}

// NewSessionStore creates an empty session store.
func NewSessionStore() *SessionStore {
	return &SessionStore{sessions: map[string]Session{}}
}

// NewSession creates a new session and returns its ID.
func (s *SessionStore) NewSession(lifetimeDays int) (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	id := base64.RawURLEncoding.EncodeToString(buf)

	s.mu.Lock()
	s.sessions[id] = Session{
		ID:        id,
		ExpiresAt: time.Now().Add(time.Duration(lifetimeDays) * 24 * time.Hour),
	}
	s.mu.Unlock()

	return id, nil
}

// GetSession returns the session for the given ID, or false if invalid/expired.
func (s *SessionStore) GetSession(id string) (Session, bool) {
	s.mu.RLock()
	sess, ok := s.sessions[id]
	s.mu.RUnlock()

	if !ok || time.Now().After(sess.ExpiresAt) {
		return Session{}, false
	}
	return sess, true
}

// DeleteSession removes a session.
func (s *SessionStore) DeleteSession(id string) {
	s.mu.Lock()
	delete(s.sessions, id)
	s.mu.Unlock()
}

// Clear removes all sessions (e.g. when password auth is disabled).
func (s *SessionStore) Clear() {
	s.mu.Lock()
	s.sessions = map[string]Session{}
	s.mu.Unlock()
}

// SweepExpired removes all expired sessions.
func (s *SessionStore) SweepExpired() {
	now := time.Now()
	s.mu.Lock()
	for id, sess := range s.sessions {
		if now.After(sess.ExpiresAt) {
			delete(s.sessions, id)
		}
	}
	s.mu.Unlock()
}

// sessionFromRequest returns the session ID from the cookie, or "".
func sessionFromRequest(r *http.Request) string {
	c, err := r.Cookie("monbooru_session")
	if err != nil {
		return ""
	}
	return c.Value
}

// sessionFromContext returns the session attached to the request context, or "".
func sessionFromContext(ctx context.Context) string {
	v, _ := ctx.Value(sessionContextKey).(string)
	return v
}

// SessionMiddleware validates the session cookie and redirects to /login if absent.
// When authEnabled is false, it passes through with a synthetic session ID.
func (s *Server) SessionMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// API routes bypass session middleware (they use bearer token auth)
		if strings.HasPrefix(r.URL.Path, "/api/v1/") {
			next.ServeHTTP(w, r)
			return
		}

		// Public paths inject "anon" session so CSRF works on the login form
		if r.URL.Path == "/login" || isStaticPath(r.URL.Path) {
			ctx := context.WithValue(r.Context(), sessionContextKey, "anon")
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}

		if !s.cfg.Auth.EnablePassword {
			// No auth - inject synthetic session so CSRF validation still works.
			ctx := context.WithValue(r.Context(), sessionContextKey, "anon")
			next.ServeHTTP(w, r.WithContext(ctx))
			return
		}

		sessID := sessionFromRequest(r)
		_, ok := s.sessions.GetSession(sessID)
		if !ok {
			if isHTMXRequest(r) {
				w.Header().Set("HX-Redirect", "/login")
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}

		ctx := context.WithValue(r.Context(), sessionContextKey, sessID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func isStaticPath(path string) bool {
	return len(path) >= 8 && path[:8] == "/static/"
}

func isHTMXRequest(r *http.Request) bool {
	return r.Header.Get("HX-Request") == "true"
}

// clientIP returns the best-effort remote IP for rate-limiting and audit
// logging. When monbooru runs behind a reverse proxy (Caddy, Traefik, nginx
// - the README shows Caddy) every request's RemoteAddr is the proxy itself,
// which would collapse every LAN client into a single rate-limit bucket.
// Prefer the first entry of X-Forwarded-For when the immediate peer is a
// loopback address (the typical reverse-proxy setup); otherwise fall back
// to RemoteAddr so a direct public deploy is not trivially spoofable.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			first := xff
			if idx := strings.Index(xff, ","); idx >= 0 {
				first = xff[:idx]
			}
			if first = strings.TrimSpace(first); first != "" {
				return first
			}
		}
	}
	return host
}

// loginRateLimiter tracks failed login attempts per IP with exponential backoff.
type loginRateLimiter struct {
	mu       sync.Mutex
	failures map[string]loginAttempt
}

type loginAttempt struct {
	count    int
	lastFail time.Time
}

func newLoginRateLimiter() *loginRateLimiter {
	return &loginRateLimiter{failures: map[string]loginAttempt{}}
}

// check returns true if the IP is allowed to attempt a login, false if rate-limited.
func (l *loginRateLimiter) check(ip string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	a, ok := l.failures[ip]
	if !ok {
		return true
	}
	// Exponential backoff: 1s, 2s, 4s, 8s, ... capped at 30s
	delay := time.Duration(1<<min(a.count-1, 4)) * time.Second
	if delay > 30*time.Second {
		delay = 30 * time.Second
	}
	return time.Since(a.lastFail) >= delay
}

// recordFailure increments the failure count for an IP.
func (l *loginRateLimiter) recordFailure(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.failures[ip]
	a.count++
	a.lastFail = time.Now()
	l.failures[ip] = a
}

// recordSuccess resets the failure count for an IP.
func (l *loginRateLimiter) recordSuccess(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.failures, ip)
}

// sweep removes entries older than 5 minutes (called from session sweep).
func (l *loginRateLimiter) sweep() {
	cutoff := time.Now().Add(-5 * time.Minute)
	l.mu.Lock()
	defer l.mu.Unlock()
	for ip, a := range l.failures {
		if a.lastFail.Before(cutoff) {
			delete(l.failures, ip)
		}
	}
}
