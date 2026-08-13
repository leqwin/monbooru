package web

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/logx"
)

const (
	pairPendingTTL = 5 * time.Minute
	pairMaxPending = 16
)

type pairState string

const (
	pairPending  pairState = "pending"
	pairApproved pairState = "approved"
	pairDenied   pairState = "denied"
)

// pairReq is one in-flight pairing handshake, held in memory until the peer
// claims its issued token or the request ages out. Nothing is issued until the
// claim, so an approval the peer never collects mints no token.
type pairReq struct {
	ID        string
	App       string
	URL       string
	Source    string
	Scopes    []string
	PeerToken string
	Version   string
	Buttons   []config.PluginButton
	State     pairState
	Claimed   bool
	CreatedAt time.Time
	// Repair marks an offer from a peer this monbooru is already paired
	// with, so the approval card can say the existing pairing is replaced.
	Repair bool
}

type pairStore struct {
	mu sync.Mutex
	m  map[string]*pairReq
}

func newPairStore() *pairStore { return &pairStore{m: map[string]*pairReq{}} }

func pairID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func (ps *pairStore) sweepLocked() {
	cutoff := time.Now().Add(-pairPendingTTL)
	maps.DeleteFunc(ps.m, func(_ string, r *pairReq) bool {
		return r.CreatedAt.Before(cutoff)
	})
}

// create records a pending request, capping the number outstanding. A second
// request from the same app replaces its pending entry rather than stacking:
// the request endpoint is unauthenticated, so one noisy peer would otherwise
// fill the cap and starve every other pairing for the TTL.
func (ps *pairStore) create(req pairReq) (string, bool) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.sweepLocked()
	maps.DeleteFunc(ps.m, func(_ string, r *pairReq) bool {
		return r.State == pairPending && r.App == req.App
	})
	pending := 0
	for _, r := range ps.m {
		if r.State == pairPending {
			pending++
		}
	}
	if pending >= pairMaxPending {
		return "", false
	}
	req.ID, req.State, req.CreatedAt = pairID(), pairPending, time.Now()
	ps.m[req.ID] = &req
	return req.ID, true
}

// dropPending forgets an app's outstanding offer. A removal that also stops
// the plugin races the offer it makes on its way out, and a card asking to
// pair with what the operator just removed is noise.
func (ps *pairStore) dropPending(app string) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	maps.DeleteFunc(ps.m, func(_ string, r *pairReq) bool {
		return r.State == pairPending && r.App == app
	})
}

func (ps *pairStore) listPending() []pairReq {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.sweepLocked()
	var out []pairReq
	for _, r := range ps.m {
		if r.State == pairPending {
			out = append(out, *r)
		}
	}
	return out
}

func (ps *pairStore) get(id string) (pairReq, bool) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if r, ok := ps.m[id]; ok {
		return *r, true
	}
	return pairReq{}, false
}

// setState moves a pending request to approved or denied (operator action).
func (ps *pairStore) setState(id string, st pairState) bool {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	r, ok := ps.m[id]
	if !ok || r.State != pairPending {
		return false
	}
	r.State = st
	return true
}

// claim transitions an approved request to claimed exactly once, returning the
// request and true only for the first caller.
func (ps *pairStore) claim(id string) (pairReq, bool) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	r, ok := ps.m[id]
	if !ok || r.State != pairApproved || r.Claimed {
		return pairReq{}, false
	}
	r.Claimed = true
	return *r, true
}

// unclaim reverts a claim so a peer can retry after a failed token mint.
func (ps *pairStore) unclaim(id string) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if r, ok := ps.m[id]; ok {
		r.Claimed = false
	}
}

func (ps *pairStore) remove(id string) {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	delete(ps.m, id)
}

func writePairJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// pairedWith reports whether a token issued to the given peer already exists.
func (s *Server) pairedWith(app string) bool {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg.FindPairedToken(app) != nil
}

// pairRequest receives a pairing offer from a peer. It issues nothing; an
// operator approves it in Settings, after which the peer claims the token via
// pairStatus.
func (s *Server) pairRequest(w http.ResponseWriter, r *http.Request) {
	var body struct {
		App             string                `json:"app"`
		URL             string                `json:"url"`
		RequestedScopes []string              `json:"requested_scopes"`
		PeerToken       string                `json:"peer_token"`
		Version         string                `json:"version"`
		Buttons         []config.PluginButton `json:"buttons"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&body); err != nil || body.App == "" {
		writePairJSON(w, http.StatusBadRequest, map[string]string{"code": "invalid_request", "error": "app and a JSON body are required"})
		return
	}
	if err := validatePairOffer(body.App, body.Version, body.Buttons); err != nil {
		writePairJSON(w, http.StatusBadRequest, map[string]string{"code": "invalid_request", "error": err.Error()})
		return
	}
	// An offer from a peer already paired here is a re-pair, not a conflict:
	// a plugin whose folder was replaced comes back with no credentials, and
	// refusing it would leave the operator unpairing by hand before the new
	// copy could work. It still queues for approval like any other offer, and
	// the claim replaces the credentials rather than adding a second set.
	repair := s.pairedWith(body.App)
	id, ok := s.pairs.create(pairReq{
		App: body.App, URL: body.URL, Source: clientIP(r), Scopes: body.RequestedScopes,
		PeerToken: body.PeerToken, Version: body.Version, Buttons: body.Buttons, Repair: repair,
	})
	if !ok {
		writePairJSON(w, http.StatusTooManyRequests, map[string]string{"code": "too_many_requests", "error": "too many pending pairing requests"})
		return
	}
	logx.Infof("pairing: request from %s (%s)", body.App, body.URL)
	writePairJSON(w, http.StatusOK, map[string]string{"request_id": id, "status": "pending"})
}

// validatePairOffer refuses an offer monbooru could not persist or render.
// monloader declares no buttons, so it only ever meets the name check.
func validatePairOffer(app, version string, buttons []config.PluginButton) error {
	if err := config.ValidatePluginName(app); err != nil {
		return err
	}
	if len(version) > config.MaxPluginVersion {
		return fmt.Errorf("version must be at most %d characters", config.MaxPluginVersion)
	}
	return config.ValidatePluginButtons(buttons)
}

// pairStatus reports a request's state. On the first poll after approval it
// mints the peer's token, stores the reverse credentials, and returns the
// secret once.
func (s *Server) pairStatus(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	req, ok := s.pairs.get(id)
	if !ok {
		writePairJSON(w, http.StatusNotFound, map[string]string{"code": "not_found", "error": "unknown pairing request"})
		return
	}
	if req.State != pairApproved {
		writePairJSON(w, http.StatusOK, map[string]string{"status": string(req.State)})
		return
	}
	claimed, won := s.pairs.claim(id)
	if !won {
		writePairJSON(w, http.StatusOK, map[string]string{"status": "approved"})
		return
	}
	secret, err := s.mintPairedToken(claimed)
	if err != nil {
		s.pairs.unclaim(id)
		writePairJSON(w, http.StatusInternalServerError, map[string]string{"code": "mint_failed", "error": err.Error()})
		return
	}
	s.pairs.remove(id)
	writePairJSON(w, http.StatusOK, map[string]string{"status": "approved", "token": secret})
}

// pairTeardown lets a paired peer drop the pairing on this side too, so one
// "remove pairing" tears down both ends. It removes only locally and never
// calls back, which would loop.
func (s *Server) pairTeardown(w http.ResponseWriter, r *http.Request) {
	secret := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	s.cfgMu.RLock()
	tok := s.cfg.FindTokenByHash(config.HashToken(secret))
	var paired string
	if tok != nil {
		paired = tok.Paired
	}
	s.cfgMu.RUnlock()
	if secret == "" || paired == "" {
		writePairJSON(w, http.StatusUnauthorized, map[string]string{"code": "unauthorized", "error": "pairing token required"})
		return
	}
	if err := s.removePairing(paired); err != nil {
		writePairJSON(w, http.StatusInternalServerError, map[string]string{"code": "remove_failed", "error": err.Error()})
		return
	}
	logx.Infof("pairing: %s removed the pairing remotely", paired)
	writePairJSON(w, http.StatusOK, map[string]string{"status": "removed"})
}

// peerCallbackURL rewrites the address a peer advertised so its host is the
// source the pairing request came from, keeping the advertised scheme and
// port. A peer sends its own base_url, which carries the right port but a host
// (usually localhost) that means nothing from monbooru's side; the source is
// where monbooru can actually reach it. Falls back to the advertised value when
// it can't be parsed or the source is unknown.
func peerCallbackURL(advertised, source string) string {
	source = strings.TrimSpace(source)
	u, err := url.Parse(strings.TrimSpace(advertised))
	if err != nil || u.Host == "" || source == "" {
		return advertised
	}
	if port := u.Port(); port != "" {
		u.Host = net.JoinHostPort(source, port)
	} else {
		u.Host = source
	}
	return u.String()
}

// mintPairedToken issues the monbooru token the peer will carry, stores the
// reverse credentials the peer offered, and returns the new secret once. The
// address to call the peer back at is the source the request came from, unless
// an operator override is configured. monloader keeps its own config section;
// every other peer lands in a [[plugin]] block.
func (s *Server) mintPairedToken(req pairReq) (string, error) {
	scopes := filterScopes(req.Scopes)
	if len(scopes) == 0 {
		scopes = []string{config.ScopeRead, config.ScopeWrite}
	}
	tok, secret := config.GenerateToken(req.App+" (paired)", scopes)
	tok.Paired = req.App
	tok.PeerURL = peerCallbackURL(req.URL, req.Source)
	if err := s.withConfig(func(c *config.Config) error {
		// A re-pair replaces the peer's credentials rather than stacking a
		// second set: the copy that held the old token is gone.
		c.Auth.Tokens = slices.DeleteFunc(c.Auth.Tokens, func(t config.Token) bool { return t.Paired == req.App })
		c.Auth.Tokens = append(c.Auth.Tokens, tok)
		if req.App == monloaderApp {
			c.Monloader.APIToken = req.PeerToken
			return nil
		}
		p := c.FindPlugin(req.App)
		if p == nil {
			c.Plugins = append(c.Plugins, config.PluginConfig{Name: req.App})
			p = &c.Plugins[len(c.Plugins)-1]
		}
		p.Version, p.PeerToken, p.Buttons, p.Paused = req.Version, req.PeerToken, req.Buttons, false
		return nil
	}); err != nil {
		return "", err
	}
	logx.Infof("pairing: issued token to %s", req.App)
	return secret, nil
}

func (s *Server) pluginPairApprove(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	id := r.PathValue("id")
	req, ok := s.pairs.get(id)
	if !ok {
		s.renderTemplate(w, "partials/plugin_pairing.html", s.pairViewData(r))
		return
	}
	// Probe the url monbooru will actually call (the peer's configured
	// override, else the address the request came from) and refuse the
	// pairing if unreachable.
	base := cmp.Or(s.peerOverrideURL(req.App), peerCallbackURL(req.URL, req.Source))
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if !s.monloaderReachable(ctx, base) {
		msg := "no api url to reach " + req.App + "; pairing not completed."
		if base != "" {
			msg = req.App + " is unreachable at " + base + "; pairing not completed. Check the api url and that it is running."
		}
		s.renderTemplate(w, "partials/plugin_pairing.html", s.pairViewData(r))
		writeFlashOOB(w, "flash-plugins", "warn", msg)
		return
	}
	s.pairs.setState(id, pairApproved)
	logx.Infof("pairing: approved request %s from %s", id, clientIP(r))
	s.renderTemplate(w, "partials/plugin_pairing.html", s.pairViewData(r))
	writeFlashOOB(w, "flash-plugins", "", "")
}

// monloaderLightDisconnect pauses the monloader link from the footer light's
// kill switch: it suspends every call to monloader without dropping the
// pairing, so the operator can cut the link and later resume it from the same
// light with no re-pair.
func (s *Server) monloaderLightDisconnect(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	if s.pairedWith(monloaderApp) {
		if err := s.setMonloaderPaused(true); err != nil {
			logx.Errorf("pairing: pause failed: %v", err)
		}
		logx.Infof("pairing: monloader link paused from %s", clientIP(r))
	}
	s.renderMonloaderLight(w, r, "paused", "")
}

// monloaderLightReconnect lifts the pause, resuming connectivity with the
// credentials that stayed on disk. The light renders "checking" and its load
// poll probes monloader within a second.
func (s *Server) monloaderLightReconnect(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	if err := s.setMonloaderPaused(false); err != nil {
		logx.Errorf("pairing: resume failed: %v", err)
	}
	logx.Infof("pairing: monloader link resumed from %s", clientIP(r))
	s.renderMonloaderLight(w, r, "", "")
}

// setMonloaderPaused persists the footer light's pause flag.
func (s *Server) setMonloaderPaused(paused bool) error {
	return s.withConfig(func(c *config.Config) error {
		c.Monloader.Paused = paused
		return nil
	})
}

// renderMonloaderLight writes the footer light in the given connection state.
func (s *Server) renderMonloaderLight(w http.ResponseWriter, r *http.Request, conn, version string) {
	s.renderTemplate(w, "partials/monloader_light.html", map[string]any{
		"MonloaderConn":    conn,
		"MonloaderVersion": version,
		"MonloaderURL":     s.monloaderWebBase(),
		"CSRFToken":        s.csrfToken(sessionFromContext(r.Context())),
	})
}

// teardownMonloaderPairing drops this side of the monloader pairing and
// notifies the peer, returning the notify error (nil on success). Shared by
// the settings unpair and the footer light's kill switch.
func (s *Server) teardownMonloaderPairing(r *http.Request) error {
	peerURL := s.monloaderAPIBase()
	s.cfgMu.RLock()
	peerToken := s.cfg.Monloader.APIToken
	s.cfgMu.RUnlock()
	if err := s.removePairing(monloaderApp); err != nil {
		logx.Errorf("pairing: remove failed: %v", err)
	}
	notifyErr := notifyPeerTeardown(monloaderApp, peerURL, peerToken)
	if notifyErr != nil {
		logx.Errorf("pairing: could not notify monloader of teardown: %v", notifyErr)
	}
	logx.Infof("pairing: removed monloader pairing from %s", clientIP(r))
	return notifyErr
}

// removePairing tears down this side of a pairing: it drops the pairing token
// and the credential monbooru uses to authenticate to the peer, but keeps the
// configured api_url so an operator's URL survives an unpair/re-pair cycle.
func (s *Server) removePairing(app string) error {
	return s.withConfig(func(c *config.Config) error {
		c.Auth.Tokens = slices.DeleteFunc(c.Auth.Tokens, func(t config.Token) bool { return t.Paired == app })
		if app == monloaderApp {
			c.Monloader.APIToken = ""
			return nil
		}
		if p := c.FindPlugin(app); p != nil {
			p.PeerToken, p.Version, p.Buttons = "", "", nil
		}
		// A block that only ever held the pairing goes with it; one carrying
		// the operator's own lines stays, since config.Save re-encodes the
		// whole file from the struct and would otherwise erase them. Enabled
		// counts: dropping it would leave a dropped plugin running with a row
		// that offers to enable it, and stopped after the next boot.
		c.Plugins = slices.DeleteFunc(c.Plugins, func(p config.PluginConfig) bool {
			return p.Name == app && p.APIURL == "" && !p.Enabled
		})
		return nil
	})
}
