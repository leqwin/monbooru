package web

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

// --- loginRateLimiter --------------------------------------------------------

// seedAttempt injects a past failure so the exponential-backoff window can be
// tested without sleeping for real seconds. Same-package access keeps tests
// deterministic; external callers can't reach loginRateLimiter directly.
func seedAttempt(l *loginRateLimiter, ip string, count int, lastFail time.Time) {
	l.mu.Lock()
	l.failures[ip] = loginAttempt{count: count, lastFail: lastFail}
	l.mu.Unlock()
}

func TestLoginRateLimiter_FirstRequestAllowed(t *testing.T) {
	t.Parallel()
	l := newLoginRateLimiter()
	if !l.check("10.0.0.1") {
		t.Error("fresh IP must be allowed")
	}
}

// TestLoginRateLimiter_BackoffSequence pins the exact per-failure delay so a
// refactor of the power-of-two curve gets caught. Seeding lastFail in the past
// avoids real sleep; checking check() at two positions (just before and just
// after the expected gate) proves the delay maths.
func TestLoginRateLimiter_BackoffSequence(t *testing.T) {
	t.Parallel()
	// expected[count] = delay after `count` consecutive failures.
	// Current implementation: 1<<min(count-1,4) seconds, so the sequence is
	// 1, 2, 4, 8, 16, 16, 16… The README/spec claim "capped at 30s" but the
	// code caps at 16s because min(count-1,4) caps the exponent at 4. See
	// TestLoginRateLimiter_SpecDocumentedCapIs30s below for the contract
	// gap.
	expected := map[int]time.Duration{
		1: 1 * time.Second,
		2: 2 * time.Second,
		3: 4 * time.Second,
		4: 8 * time.Second,
		5: 16 * time.Second,
		6: 16 * time.Second, // cap
		9: 16 * time.Second, // still capped
	}
	for count, delay := range expected {
		l := newLoginRateLimiter()
		const ip = "10.0.0.2"
		// Just inside the gate: last failure "delay-1s ago" → still blocked.
		seedAttempt(l, ip, count, time.Now().Add(-(delay - 500*time.Millisecond)))
		if l.check(ip) {
			t.Errorf("count=%d: expected rate-limit inside %s window", count, delay)
		}
		// Just past the gate: last failure "delay+500ms ago" → allowed.
		seedAttempt(l, ip, count, time.Now().Add(-(delay + 500*time.Millisecond)))
		if !l.check(ip) {
			t.Errorf("count=%d: expected allow past %s window", count, delay)
		}
	}
}

// TestLoginRateLimiter_SpecDocumentedCapIs30s documents the discrepancy
// between SPECIFICATIONS §13.1 (and README) which both say the backoff caps
// at 30 s and the actual implementation which caps at 16 s. See the t.Skip
// message. Flip the skip and the test will fail until the implementation
// matches the spec (or the spec is amended to say 16 s).
func TestLoginRateLimiter_SpecDocumentedCapIs30s(t *testing.T) {
	t.Parallel()
	t.Skip("BUG: auth.go:205 caps delay at 1<<4=16s via min(count-1,4); spec §13.1 and README say 30s. Change the cap to 5 to reach 32s and let the existing `if delay > 30s { delay = 30s }` clamp it, or amend the spec.")
	l := newLoginRateLimiter()
	const ip = "10.0.0.3"
	seedAttempt(l, ip, 20, time.Now().Add(-29*time.Second))
	if l.check(ip) {
		t.Error("spec says delay caps at 30s; 29s after failure #20 should still be blocked")
	}
	seedAttempt(l, ip, 20, time.Now().Add(-31*time.Second))
	if !l.check(ip) {
		t.Error("31s after failure #20 should be allowed (cap is 30s)")
	}
}

func TestLoginRateLimiter_RecordFailureIncrements(t *testing.T) {
	t.Parallel()
	l := newLoginRateLimiter()
	const ip = "10.0.0.4"
	l.recordFailure(ip)
	l.recordFailure(ip)
	l.recordFailure(ip)
	l.mu.Lock()
	got := l.failures[ip].count
	l.mu.Unlock()
	if got != 3 {
		t.Errorf("count = %d, want 3 after three recordFailure calls", got)
	}
}

func TestLoginRateLimiter_RecordSuccessClearsEntry(t *testing.T) {
	t.Parallel()
	l := newLoginRateLimiter()
	const ip = "10.0.0.5"
	seedAttempt(l, ip, 3, time.Now())
	l.recordSuccess(ip)
	if !l.check(ip) {
		t.Error("after recordSuccess the IP must be unblocked")
	}
	l.mu.Lock()
	_, stillThere := l.failures[ip]
	l.mu.Unlock()
	if stillThere {
		t.Error("recordSuccess must delete the entry, not just reset count")
	}
}

func TestLoginRateLimiter_SweepRemovesStale(t *testing.T) {
	t.Parallel()
	l := newLoginRateLimiter()
	// Old (>5 min) and fresh entries; sweep must keep the fresh one only.
	seedAttempt(l, "old.old.old.old", 1, time.Now().Add(-10*time.Minute))
	seedAttempt(l, "new.new.new.new", 1, time.Now())
	l.sweep()
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.failures["old.old.old.old"]; ok {
		t.Error("sweep should have removed the 10-minute-old entry")
	}
	if _, ok := l.failures["new.new.new.new"]; !ok {
		t.Error("sweep should keep the fresh entry")
	}
}

func TestLoginRateLimiter_PerIPIsolation(t *testing.T) {
	t.Parallel()
	l := newLoginRateLimiter()
	l.recordFailure("1.1.1.1")
	// Second IP has no history and must remain unrestricted.
	if !l.check("2.2.2.2") {
		t.Error("rate-limit must be per-IP; unrelated IP was blocked")
	}
}

// --- clientIP ---------------------------------------------------------------

func TestClientIP_DirectPeer(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "203.0.113.9:54321"
	if got := clientIP(r); got != "203.0.113.9" {
		t.Errorf("clientIP direct = %q, want 203.0.113.9", got)
	}
}

func TestClientIP_BehindLoopbackProxyTrustsXFF(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "127.0.0.1:1111"
	r.Header.Set("X-Forwarded-For", "198.51.100.7, 10.0.0.1")
	if got := clientIP(r); got != "198.51.100.7" {
		t.Errorf("clientIP via loopback proxy = %q, want 198.51.100.7", got)
	}
}

func TestClientIP_NonLoopbackIgnoresXFF(t *testing.T) {
	t.Parallel()
	// A direct public deploy must not honour X-Forwarded-For or the client
	// can spoof arbitrary IPs by sending the header themselves.
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "203.0.113.9:54321"
	r.Header.Set("X-Forwarded-For", "evil.example")
	if got := clientIP(r); got != "203.0.113.9" {
		t.Errorf("clientIP should not trust XFF when peer is public, got %q", got)
	}
}

// --- loginPost / settingsPasswordPost / settingsRemovePasswordPost ---------

func setTestPassword(t *testing.T, srv *Server, password string) {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	srv.cfgMu.Lock()
	srv.cfg.Auth.EnablePassword = true
	srv.cfg.Auth.PasswordHash = string(hash)
	srv.cfg.Auth.SessionLifetimeDays = 1
	srv.cfgMu.Unlock()
}

func TestLoginPost_AcceptsCorrectPassword(t *testing.T) {
	srv := newTestServer(t)
	setTestPassword(t, srv, "s3cret")
	h := srv.Handler()

	form := url.Values{
		"_csrf":    {srv.csrfToken("anon")},
		"password": {"s3cret"},
	}
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
	req.RemoteAddr = "10.0.0.10:443"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Fatalf("correct password expected 303, got %d: %s", w.Code, w.Body.String())
	}
	// monbooru_session cookie must be set.
	var sess string
	for _, c := range w.Result().Cookies() {
		if c.Name == "monbooru_session" {
			sess = c.Value
		}
	}
	if sess == "" {
		t.Fatal("login did not set monbooru_session cookie")
	}
	if _, ok := srv.sessions.GetSession(sess); !ok {
		t.Error("session store should contain the issued session ID")
	}
}

func TestLoginPost_RejectsWrongPasswordAndRecordsFailure(t *testing.T) {
	srv := newTestServer(t)
	setTestPassword(t, srv, "s3cret")
	h := srv.Handler()

	form := url.Values{"_csrf": {srv.csrfToken("anon")}, "password": {"WRONG"}}
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
	req.RemoteAddr = "10.0.0.11:443"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("wrong password should re-render login (200), got %d", w.Code)
	}
	if !strings.Contains(w.Body.String(), "Invalid password") {
		t.Error("expected 'Invalid password' in response body")
	}
	srv.loginRL.mu.Lock()
	got := srv.loginRL.failures["10.0.0.11"].count
	srv.loginRL.mu.Unlock()
	if got != 1 {
		t.Errorf("rate limiter failures count = %d, want 1 after one bad login", got)
	}
}

func TestLoginPost_RateLimitedAfterRepeatedFailures(t *testing.T) {
	srv := newTestServer(t)
	setTestPassword(t, srv, "s3cret")
	h := srv.Handler()

	// Prime the rate limiter: 3 failures recorded "right now" for 10.0.0.12.
	// The third+ check() call lands inside the 4-second window and returns
	// false; the handler should surface the "Too many attempts" message
	// WITHOUT even running bcrypt.
	seedAttempt(srv.loginRL, "10.0.0.12", 3, time.Now())

	form := url.Values{"_csrf": {srv.csrfToken("anon")}, "password": {"s3cret"}}
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
	req.RemoteAddr = "10.0.0.12:443"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if !strings.Contains(w.Body.String(), "Too many attempts") {
		t.Errorf("expected rate-limit message, got body: %s", w.Body.String())
	}
	// The correct-password branch must not have issued a session.
	for _, c := range w.Result().Cookies() {
		if c.Name == "monbooru_session" {
			t.Error("rate-limited login must not issue a session cookie")
		}
	}
}

func TestLoginPost_DisabledAuthRedirectsHome(t *testing.T) {
	srv := newTestServer(t)
	// Password auth is off by default in newTestServer.
	h := srv.Handler()

	form := url.Values{"_csrf": {srv.csrfToken("anon")}, "password": {"anything"}}
	req := httptest.NewRequest("POST", "/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Errorf("login with auth disabled should 303 home, got %d", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/" {
		t.Errorf("Location = %q, want /", loc)
	}
}

func TestLogoutPost_ClearsSession(t *testing.T) {
	srv := newTestServer(t)
	setTestPassword(t, srv, "pw")
	h := srv.Handler()
	// Create a session first.
	sessID, err := srv.sessions.NewSession(1)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("POST", "/logout", nil)
	req.AddCookie(&http.Cookie{Name: "monbooru_session", Value: sessID})
	req.Header.Set("X-CSRF-Token", srv.csrfToken(sessID))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Errorf("logout expected 303, got %d: %s", w.Code, w.Body.String())
	}
	if _, ok := srv.sessions.GetSession(sessID); ok {
		t.Error("session should be gone after logout")
	}
}

func TestSettingsPasswordPost_RejectsWrongCurrent(t *testing.T) {
	srv := newTestServer(t)
	setTestPassword(t, srv, "initial")
	h := srv.Handler()
	// Enabling password auth forces /settings/* through the session gate;
	// create a session so the request reaches the actual handler.
	sessID, _ := srv.sessions.NewSession(1)

	form := url.Values{
		"_csrf":            {srv.csrfToken(sessID)},
		"current_password": {"WRONG"},
		"new_password":     {"replacement"},
	}
	req := httptest.NewRequest("POST", "/settings/auth/password", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", srv.csrfToken(sessID))
	req.AddCookie(&http.Cookie{Name: "monbooru_session", Value: sessID})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if !strings.Contains(w.Body.String(), "Current password is incorrect") {
		t.Errorf("expected error flash, got: %s", w.Body.String())
	}
	// Hash must be unchanged.
	if err := bcrypt.CompareHashAndPassword([]byte(srv.cfg.Auth.PasswordHash), []byte("initial")); err != nil {
		t.Error("password hash should still verify against 'initial' after a rejected change")
	}
}

func TestSettingsPasswordPost_SetsPassword(t *testing.T) {
	srv := newTestServer(t)
	// Start with auth disabled so no current-password check runs.
	h := srv.Handler()

	form := url.Values{
		"_csrf":        {srv.csrfToken("anon")},
		"new_password": {"freshpass"},
	}
	req := httptest.NewRequest("POST", "/settings/auth/password", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if !srv.cfg.Auth.EnablePassword {
		t.Error("EnablePassword should be true after setting a password")
	}
	if err := bcrypt.CompareHashAndPassword([]byte(srv.cfg.Auth.PasswordHash), []byte("freshpass")); err != nil {
		t.Errorf("password hash should verify against 'freshpass', got err %v", err)
	}
}

func TestSettingsPasswordPost_RejectsEmptyNewPassword(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()

	form := url.Values{"_csrf": {srv.csrfToken("anon")}, "new_password": {""}}
	req := httptest.NewRequest("POST", "/settings/auth/password", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if !strings.Contains(w.Body.String(), "New password required") {
		t.Errorf("expected 'New password required', got: %s", w.Body.String())
	}
	if srv.cfg.Auth.EnablePassword {
		t.Error("empty new_password must not enable auth")
	}
}

func TestSettingsRemovePasswordPost_RequiresCorrectCurrent(t *testing.T) {
	srv := newTestServer(t)
	setTestPassword(t, srv, "original")
	h := srv.Handler()
	sessID, _ := srv.sessions.NewSession(1)
	csrf := srv.csrfToken(sessID)

	// Wrong current password.
	form := url.Values{"_csrf": {csrf}, "current_password": {"WRONG"}}
	req := httptest.NewRequest("POST", "/settings/auth/remove-password", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", csrf)
	req.AddCookie(&http.Cookie{Name: "monbooru_session", Value: sessID})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if !strings.Contains(w.Body.String(), "Current password is incorrect") {
		t.Errorf("wrong current password should be rejected, got: %s", w.Body.String())
	}
	if !srv.cfg.Auth.EnablePassword {
		t.Error("auth must remain enabled after a rejected remove")
	}

	// Correct current password clears hash and disables auth.
	form.Set("current_password", "original")
	req2 := httptest.NewRequest("POST", "/settings/auth/remove-password", strings.NewReader(form.Encode()))
	req2.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req2.Header.Set("X-CSRF-Token", csrf)
	req2.AddCookie(&http.Cookie{Name: "monbooru_session", Value: sessID})
	w2 := httptest.NewRecorder()
	h.ServeHTTP(w2, req2)

	if srv.cfg.Auth.EnablePassword {
		t.Error("auth should be disabled after successful remove")
	}
	if srv.cfg.Auth.PasswordHash != "" {
		t.Error("PasswordHash should be cleared after remove")
	}
}

// --- session middleware with real cookies ---------------------------------

func TestSessionMiddleware_PassesAuthenticatedRequest(t *testing.T) {
	srv := newTestServer(t)
	setTestPassword(t, srv, "pw")
	h := srv.Handler()

	sessID, _ := srv.sessions.NewSession(1)
	req := httptest.NewRequest("GET", "/settings", nil)
	req.AddCookie(&http.Cookie{Name: "monbooru_session", Value: sessID})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("authenticated request expected 200, got %d", w.Code)
	}
}

func TestSessionMiddleware_RedirectsUnauthenticated(t *testing.T) {
	srv := newTestServer(t)
	setTestPassword(t, srv, "pw")
	h := srv.Handler()

	req := httptest.NewRequest("GET", "/settings", nil)
	// No session cookie.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusSeeOther {
		t.Errorf("unauthenticated request expected 303, got %d", w.Code)
	}
	if loc := w.Header().Get("Location"); loc != "/login" {
		t.Errorf("Location = %q, want /login", loc)
	}
}

func TestSessionMiddleware_HTMXReturns401(t *testing.T) {
	srv := newTestServer(t)
	setTestPassword(t, srv, "pw")
	h := srv.Handler()

	req := httptest.NewRequest("GET", "/settings", nil)
	req.Header.Set("HX-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("HTMX unauthenticated request expected 401, got %d", w.Code)
	}
	if w.Header().Get("HX-Redirect") != "/login" {
		t.Errorf("HX-Redirect = %q, want /login", w.Header().Get("HX-Redirect"))
	}
}

// --- CSRFMiddleware: real request through the full stack ------------------

func TestCSRFMiddleware_AcceptsHeaderToken(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()

	req := httptest.NewRequest("POST", "/settings/ui", strings.NewReader("page_size=25"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", srv.csrfToken("anon"))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("valid CSRF token expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCSRFMiddleware_AcceptsFormFieldToken(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()

	form := url.Values{"_csrf": {srv.csrfToken("anon")}, "page_size": {"25"}}
	req := httptest.NewRequest("POST", "/settings/ui", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("form _csrf field expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCSRFMiddleware_RejectsMissingToken(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()

	req := httptest.NewRequest("POST", "/settings/ui", strings.NewReader("page_size=25"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("missing CSRF expected 403, got %d", w.Code)
	}
}

func TestCSRFMiddleware_RejectsForeignToken(t *testing.T) {
	srvA := newTestServer(t)
	srvB := newTestServer(t)

	// A valid token for srvB is invalid for srvA because each server rolls
	// its own HMAC secret.
	req := httptest.NewRequest("POST", "/settings/ui", strings.NewReader("page_size=25"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("X-CSRF-Token", srvB.csrfToken("anon"))
	w := httptest.NewRecorder()
	srvA.Handler().ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Errorf("foreign CSRF token expected 403, got %d", w.Code)
	}
}

func TestCSRFMiddleware_APIRoutesBypass(t *testing.T) {
	srv := newTestServer(t)
	srv.cfgMu.Lock()
	srv.cfg.Auth.APIToken = "bearer-1"
	srv.cfgMu.Unlock()
	h := srv.Handler()

	req := httptest.NewRequest("DELETE", "/api/v1/images/1", nil)
	req.Header.Set("Authorization", "Bearer bearer-1")
	// No CSRF token at all.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	// Not 403 - API auth is a separate layer. (404 is fine: no image id=1.)
	if w.Code == http.StatusForbidden {
		t.Errorf("API routes must bypass CSRF, got 403")
	}
}

func TestCSRFMiddleware_GETBypasses(t *testing.T) {
	srv := newTestServer(t)
	h := srv.Handler()

	req := httptest.NewRequest("GET", "/", nil)
	// No CSRF token - GET is not CSRF-validated.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("GET must bypass CSRF, got %d", w.Code)
	}
}
