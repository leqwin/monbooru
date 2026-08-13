package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/monbooru/monbooru/internal/lookup"
)

// monloaderClient is the outbound HTTP client for the companion monloader,
// and it is only ever pointed at the configured instance (SPECIFICATIONS.md
// 14.5). Per-call deadlines belong to the request contexts (probes 4-5 s,
// contribution previews 8 s, sends 10 s); the client timeout is only a
// backstop for the callers that pass an unbounded context, and must stay
// above the largest per-call deadline or it aborts a send monloader may
// still commit.
var monloaderClient = &http.Client{Timeout: 15 * time.Second}

// errMonloaderUnconfigured is what every outbound call answers before any
// I/O when no link is set up.
var errMonloaderUnconfigured = errors.New("monloader is not configured")

// monloaderDo issues one authed request to a monloader API path and returns
// the live response for the caller to map. The token read happens under
// cfgMu; the Do call must not, so a slow monloader can't block a settings
// write. A nil body sends no payload and no content type.
func (s *Server) monloaderDo(ctx context.Context, method, path string, body []byte) (*http.Response, error) {
	base := strings.TrimRight(s.monloaderAPIBase(), "/")
	s.cfgMu.RLock()
	token := s.cfg.Monloader.APIToken
	s.cfgMu.RUnlock()
	if base == "" || token == "" {
		return nil, errMonloaderUnconfigured
	}
	var payload io.Reader
	if body != nil {
		payload = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, payload)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	return monloaderClient.Do(req)
}

// monloaderPost sends one JSON body to a monloader API path.
func (s *Server) monloaderPost(ctx context.Context, path string, payload map[string]any) (*http.Response, error) {
	body, _ := json.Marshal(payload)
	return s.monloaderDo(ctx, http.MethodPost, path, body)
}

// enqueueMonloader posts one enqueue payload and maps the reply: any
// non-2xx is a per-request refusal the caller can skip a row over.
// onConflict, when set, names what a 409 means for that endpoint. The
// returned job id is what lets a caller resolve an attempt whose callback
// never arrives; it is zero against a monloader that reports none.
func (s *Server) enqueueMonloader(ctx context.Context, path string, payload map[string]any, onConflict error) (int64, error) {
	resp, err := s.monloaderPost(ctx, path, payload)
	if err != nil {
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if onConflict != nil && resp.StatusCode == http.StatusConflict {
		return 0, onConflict
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return 0, errLookupBudgetSpent
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		return 0, peerStatusError{monloaderApp, resp.Status}
	}
	var out struct {
		JobID int64 `json:"job_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out.JobID, nil
}

// EnqueueMetadataFetch asks monloader to re-read the post at url (metadata
// only, no download) and enrich monbooru image imageID in gallery. All the
// work - gallery-dl, mapping, the enrich call back into monbooru - runs on
// monloader; monbooru only enqueues, keeping its single-egress model intact.
func (s *Server) EnqueueMetadataFetch(ctx context.Context, imageID int64, gallery, url string) error {
	_, err := s.enqueueMonloader(ctx, "/api/v1/metadata",
		map[string]any{"image_id": imageID, "gallery": gallery, "url": url}, nil)
	return err
}

// EnqueueReplace asks monloader to download the file the post at url serves
// and push it back over monbooru image imageID's bytes. Like a metadata
// fetch, only the enqueue happens here; the download, the hash verify, and
// the push back into the replace endpoint all run on monloader.
func (s *Server) EnqueueReplace(ctx context.Context, imageID int64, gallery, url string) error {
	_, err := s.enqueueMonloader(ctx, "/api/v1/replace",
		map[string]any{"image_id": imageID, "gallery": gallery, "url": url}, nil)
	return err
}

// errPTRUnavailable marks a lookup monloader refused because its PTR backend
// is off - a stale capability read, not a connectivity failure.
var errPTRUnavailable = errors.New("the PTR lookup is unavailable on monloader")

// errLookupBudgetSpent marks a budgeted lookup monloader refused for the day.
// Only the scheduled phase sends one, and it stops the phase rather than
// walking the rest of the gallery into the same answer.
var errLookupBudgetSpent = errors.New("monloader's daily lookup budget is spent")

// errPTRBatchUnsupported marks a monloader too old for the batch PTR
// endpoint. The phase reports it once and does nothing else: falling back to
// one queued job per image is the exact thing the batch endpoint exists to
// stop.
var errPTRBatchUnsupported = errors.New("monloader is too old for batch PTR lookup")

// peerStatusError is a non-2xx reply to one request (a bad url, a malformed
// hash, an endpoint the peer does not implement) - the request was refused,
// not a sign the peer is down, so a batch can skip the row instead of
// aborting. It names the peer: the same client calls monloader and every
// third-party plugin, and a message about a plugin that says "monloader"
// sends the operator looking in the wrong place.
type peerStatusError struct{ peer, status string }

func (e peerStatusError) Error() string { return e.peer + " returned " + e.status }

// isPeerStatusErr reports whether err is a per-request status refusal.
func isPeerStatusErr(err error) bool {
	var se peerStatusError
	return errors.As(err, &se)
}

// EnqueueHashLookup asks monloader to find tags for image imageID by file
// hash - backend "booru" walks the opted-in sites' md5 search, backend "ptr"
// queries monloader's local PTR index by sha256 - and enrich the image back
// through the same callbacks a source refetch uses. background puts the job
// behind anything a person is watching; budgeted also spends a slot of
// monloader's daily allowance and can be refused. Returns monloader's job id
// so the attempt can be reconciled if its callback goes missing.
func (s *Server) EnqueueHashLookup(ctx context.Context, imageID int64, gallery, backend, md5, sha256 string, background, budgeted bool) (int64, error) {
	return s.enqueueMonloader(ctx, "/api/v1/lookup", map[string]any{
		"image_id": imageID, "gallery": gallery, "backend": backend, "md5": md5, "sha256": sha256,
		"background": background, "budgeted": budgeted,
	}, errPTRUnavailable)
}

// ptrLookupImage is one image in a batch PTR lookup: the hash to look up and
// the id monloader names it by on the history row it files.
type ptrLookupImage struct {
	ImageID int64  `json:"image_id"`
	SHA256  string `json:"sha256"`
}

// ptrBatchLookup asks monloader for the PTR tags of a batch of images. The
// answer is the outcome - no enqueue, no callback to lose - which is why a
// whole-library pass goes through here rather than one queued job per image;
// monloader files the batch as one history row, in the same scheduled / bulk
// lane a queued lookup would take. Returns the tags per matched hash (misses
// are absent) and its index cursor at answer time.
func (s *Server) ptrBatchLookup(ctx context.Context, gallery string, scheduled bool, images []ptrLookupImage) (map[string][]string, uint64, error) {
	resp, err := s.monloaderPost(ctx, "/api/v1/ptr/lookup", map[string]any{
		"images": images, "gallery": gallery, "scheduled": scheduled,
	})
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusConflict {
		return nil, 0, errPTRUnavailable
	}
	if resp.StatusCode == http.StatusNotFound {
		return nil, 0, errPTRBatchUnsupported
	}
	if resp.StatusCode != http.StatusOK {
		return nil, 0, peerStatusError{monloaderApp, resp.Status}
	}
	var out struct {
		Index   uint64              `json:"index"`
		Results map[string][]string `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, 0, err
	}
	return out.Results, out.Index, nil
}

// ptrTagInfo is one tag's answer from monloader's PTR graph query, in
// monbooru-form names (bare or category:name).
type ptrTagInfo struct {
	Known        bool     `json:"known"`
	Ideal        string   `json:"ideal"`
	Aliases      []string `json:"aliases"`
	Implications []string `json:"implications"`
	// ImpliedBy arrives only from monloaders that serve the reverse
	// implication edge; older ones leave it empty and the dialog simply
	// offers no implied-by petitions.
	ImpliedBy []string `json:"implied_by"`
}

// ptrTagLookup asks monloader's PTR index for the alias / implication graph
// of the given monbooru-form tag names (at most ptrLookupBatch per call).
func (s *Server) ptrTagLookup(ctx context.Context, names []string) (map[string]ptrTagInfo, error) {
	out, err := monloaderPostJSON[struct {
		Results map[string]ptrTagInfo `json:"results"`
	}](s, ctx, "/api/v1/ptr/tags", map[string]any{"tags": names})
	if err != nil {
		return nil, err
	}
	return out.Results, nil
}

// monloaderAPIBase is the address monbooru calls monloader at: the operator's
// configured override when set, otherwise the address discovered during pairing
// (the source the request came from, stored on the paired token). A paused
// pairing reports no base, so every outbound call short-circuits as if
// unconfigured while the credentials stay on disk.
func (s *Server) monloaderAPIBase() string {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	if s.cfg.Monloader.Paused {
		return ""
	}
	if u := strings.TrimSpace(s.cfg.Monloader.APIURL); u != "" {
		return u
	}
	if t := s.cfg.FindPairedToken(monloaderApp); t != nil {
		return t.PeerURL
	}
	return ""
}

// monloaderUsable reports whether the link is actually up, so the
// monloader-backed surfaces and the scheduled phases can skip when it is
// paused, unreachable, or rejecting. A cold cache ("") stays optimistic so a
// fresh boot does not blank the buttons before the first probe lands.
func (s *Server) monloaderUsable() bool {
	conn, _, _, _, _ := s.monloaderStatusSeed()
	if s.monloaderPaused() {
		return false
	}
	return s.pairedWith(monloaderApp) && conn != "down" && conn != "rejected"
}

// monloaderPaused reports whether the operator has suspended the monloader
// link from the footer light. Read directly from config so the light can tell
// a paused pairing apart from an unconfigured or unreachable one.
func (s *Server) monloaderPaused() bool {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg.Monloader.Paused
}

// checkMonloader probes the configured monloader for the footer light: /health
// for up/down + version, then one authed read to surface a revoked token. The
// PTR capability rides the same probe: the lookup buttons only need a
// fresh-ish answer, and monloader 409s a lookup sent on a stale "enabled".
func (s *Server) checkMonloader(ctx context.Context) (status, version string, ptrReady, ptrSyncing, ptrContrib bool, contribFailed int, contribBanned bool) {
	base := strings.TrimRight(s.monloaderAPIBase(), "/")
	if base == "" {
		return "", "", false, false, false, 0, false
	}
	version, up := probePeer(ctx, monloaderClient, base)
	if !up {
		return "down", "", false, false, false, 0, false
	}
	s.cfgMu.RLock()
	tok := s.cfg.Monloader.APIToken
	s.cfgMu.RUnlock()
	if tok != "" {
		qreq, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/v1/queue?limit=1", nil)
		qreq.Header.Set("Authorization", "Bearer "+tok)
		if qresp, qerr := monloaderClient.Do(qreq); qerr == nil {
			defer func() { _ = qresp.Body.Close() }()
			if qresp.StatusCode == http.StatusUnauthorized || qresp.StatusCode == http.StatusForbidden {
				return "rejected", version, false, false, false, 0, false
			}
		}
		preq, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/v1/ptr/status", nil)
		preq.Header.Set("Authorization", "Bearer "+tok)
		if presp, perr := monloaderClient.Do(preq); perr == nil {
			var p struct {
				Enabled  bool `json:"enabled"`
				Progress struct {
					UpdateIndex uint64 `json:"update_index"`
				} `json:"progress"`
				State   string `json:"state"`
				Contrib *struct {
					Account bool `json:"account"`
					Banned  bool `json:"banned"`
					Failed  int  `json:"failed"`
				} `json:"contrib"`
			}
			_ = json.NewDecoder(presp.Body).Decode(&p)
			_ = presp.Body.Close()
			on := presp.StatusCode == http.StatusOK && p.Enabled
			// The `lookup:due` filter reads the cursor to skip images whose
			// PTR miss predates no index movement, and this probe is the one
			// thing that runs on a box nobody has a page open on.
			if p.Progress.UpdateIndex > 0 {
				lookup.PTRCursor.Store(p.Progress.UpdateIndex)
			}
			// monloader refuses every PTR read until its index is caught
			// up, so an index that is merely enabled is not usable yet.
			ptrReady = on && p.State == "ready"
			ptrSyncing = on && !ptrReady
			// An absent contrib field (older monloader) leaves this
			// false, so contribution UI stays off against it. Gated on a
			// fully-synced index so nothing is contributed against a
			// stale copy.
			ptrContrib = ptrReady && p.Contrib != nil && p.Contrib.Account && !p.Contrib.Banned
			contribBanned = on && p.Contrib != nil && p.Contrib.Banned
			if p.Contrib != nil {
				contribFailed = p.Contrib.Failed
			}
		}
	}
	return "ok", version, ptrReady, ptrSyncing, ptrContrib, contribFailed, contribBanned
}

// monloaderReachable reports whether monloader answers a health probe at base.
// Approving a pairing monbooru can't reach would leave a dead pairing (no light,
// no refetch), so the operator is blocked until the api url responds.
func (s *Server) monloaderReachable(ctx context.Context, base string) bool {
	if strings.TrimRight(base, "/") == "" {
		return false
	}
	_, up := probePeer(ctx, monloaderClient, base)
	return up
}

// monloaderStatusTTL bounds how often the footer light re-probes monloader, so
// a burst of navigations (each firing the light's load poll) reuses one probe
// instead of fanning out into a probe per page. Kept under the 15s poll cadence
// so a page left open still refreshes on schedule.
const monloaderStatusTTL = 10 * time.Second

// monloaderStatusSeed returns the last cached probe result without probing, for
// seeding a page's initial light render so it shows its last known state rather
// than "checking". A cold cache yields "", which the partial renders as
// "checking monloader". The PTR flag seeds the lookup buttons the same way: a
// cold cache hides them until the light's first poll lands.
func (s *Server) monloaderStatusSeed() (status, version string, ptrReady, ptrSyncing, ptrContrib bool) {
	s.monloaderStatusMu.Lock()
	defer s.monloaderStatusMu.Unlock()
	return s.monloaderConn, s.monloaderVersion, s.monloaderPTR, s.monloaderPTRSyncing, s.monloaderContrib
}

// monloaderContribBannedSeed reports whether the paired account is banned, so
// the panel hint can say so rather than telling the operator to make an
// account (which a ban forbids).
func (s *Server) monloaderContribBannedSeed() bool {
	s.monloaderStatusMu.Lock()
	defer s.monloaderStatusMu.Unlock()
	return s.monloaderContribBanned
}

// monloaderContribFailedSeed is the cached count of contribution uploads
// stuck failed on monloader, for the panels' warning line.
func (s *Server) monloaderContribFailedSeed() int {
	s.monloaderStatusMu.Lock()
	defer s.monloaderStatusMu.Unlock()
	return s.monloaderContribFailed
}

// monloaderStatusCached probes monloader at most once per monloaderStatusTTL and
// serves the cached result otherwise, so the light's per-navigation poll does
// not re-probe on every page load. The probe runs without the lock held so a
// slow monloader never serializes concurrent page renders.
func (s *Server) monloaderStatusCached(ctx context.Context) (status, version string) {
	s.monloaderStatusMu.Lock()
	if s.monloaderConn != "" && time.Since(s.monloaderCheckedAt) < monloaderStatusTTL {
		status, version = s.monloaderConn, s.monloaderVersion
		s.monloaderStatusMu.Unlock()
		return status, version
	}
	s.monloaderStatusMu.Unlock()

	status, version, ptrReady, ptrSyncing, ptrContrib, contribFailed, contribBanned := s.checkMonloader(ctx)

	s.monloaderStatusMu.Lock()
	s.monloaderConn, s.monloaderVersion, s.monloaderPTR, s.monloaderPTRSyncing, s.monloaderContrib, s.monloaderContribFailed, s.monloaderContribBanned, s.monloaderCheckedAt = status, version, ptrReady, ptrSyncing, ptrContrib, contribFailed, contribBanned, time.Now()
	s.monloaderStatusMu.Unlock()
	return status, version
}

func (s *Server) monloaderStatusHandler(w http.ResponseWriter, r *http.Request) {
	if !s.pairedWith(monloaderApp) {
		// Stop polling and clear the light once the pairing is gone.
		_, _ = w.Write([]byte(`<span id="monloader-light"></span>`))
		return
	}
	if s.monloaderPaused() {
		// Paused: keep the light and its poll alive so the operator can
		// resume, but never probe.
		s.renderMonloaderLight(w, r, "paused", "")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	_, _, ptrBefore, syncBefore, contribBefore := s.monloaderStatusSeed()
	failedBefore := s.monloaderContribFailedSeed()
	status, version := s.monloaderStatusCached(ctx)
	// A flag flip re-mounts the surfaces that seeded from the stale
	// value - a fresh session's contribution panels render empty until
	// this first poll lands, and they listen for the change.
	_, _, ptrAfter, syncAfter, contribAfter := s.monloaderStatusSeed()
	if ptrBefore != ptrAfter || syncBefore != syncAfter || contribBefore != contribAfter || failedBefore != s.monloaderContribFailedSeed() {
		w.Header().Set("HX-Trigger", "monloader-status-changed")
	}
	s.renderTemplate(w, "partials/monloader_light.html", map[string]any{
		"MonloaderConn":    status,
		"MonloaderVersion": version,
		"MonloaderURL":     s.monloaderWebBase(),
		"CSRFToken":        s.csrfToken(sessionFromContext(r.Context())),
	})
}
