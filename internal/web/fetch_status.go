package web

import (
	"cmp"
	"fmt"
	"html"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// fetchStatusEntry is the last known outcome of a source metadata fetch for one
// image. State is "pending" while monloader works, then "ok" once the enrich
// lands. Hashes names what a hash lookup searched ("md5 …" / "md5 …, sha256 …"),
// recorded at enqueue time so a not-found outcome can show it.
type fetchStatusEntry struct {
	State  string
	Msg    string
	Hashes string
	At     time.Time
}

const (
	// fetchStatusTTL bounds how long a recorded outcome lingers, so a batch
	// fetch that no detail page polls can't grow the map without bound.
	fetchStatusTTL = 10 * time.Minute
	// fetchPollMax caps the pill's self-poll (~2s cadence) so a fetch that
	// never completes stops nagging instead of polling forever.
	fetchPollMax     = 30
	fetchPollDelayMs = 2000
)

func fetchStatusKey(gallery string, id int64) string {
	return gallery + "\x00" + strconv.FormatInt(id, 10)
}

// recordFetchStatus stores the latest fetch outcome for (gallery, id), pruning
// entries past fetchStatusTTL first. A terminal report inherits the pending
// entry's Hashes (monloader's callback doesn't know them); a fresh pending
// resets them so a plain refetch never shows a stale hash line.
func (s *Server) recordFetchStatus(gallery string, id int64, state, msg string) {
	s.fetchStatusMu.Lock()
	defer s.fetchStatusMu.Unlock()
	now := time.Now()
	s.pruneFetchStatusLocked(now)
	key := fetchStatusKey(gallery, id)
	entry := fetchStatusEntry{State: state, Msg: msg, At: now}
	if prev, ok := s.fetchStatus[key]; ok && state != "pending" {
		entry.Hashes = prev.Hashes
	}
	s.fetchStatus[key] = entry
}

// recordFetchLookup records the pending state for a hash lookup, remembering
// the searched hashes so a not-found outcome can name them. Set before the
// enqueue so a fast local (PTR) callback can't be overwritten back to pending.
func (s *Server) recordFetchLookup(gallery string, id int64, hashes string) {
	s.fetchStatusMu.Lock()
	defer s.fetchStatusMu.Unlock()
	now := time.Now()
	s.pruneFetchStatusLocked(now)
	s.fetchStatus[fetchStatusKey(gallery, id)] = fetchStatusEntry{State: "pending", At: now, Hashes: hashes}
}

// pruneFetchStatus evicts entries past the TTL. The recording paths
// prune as they write, so this is for the reclaim loop: once the last
// fetch of a session lands, nothing writes again and the entries would
// outlive their TTL until the next one does.
func (s *Server) pruneFetchStatus() {
	s.fetchStatusMu.Lock()
	defer s.fetchStatusMu.Unlock()
	s.pruneFetchStatusLocked(time.Now())
}

// pruneFetchStatusLocked initialises the map and evicts entries past the TTL.
// Callers hold fetchStatusMu.
func (s *Server) pruneFetchStatusLocked(now time.Time) {
	if s.fetchStatus == nil {
		s.fetchStatus = map[string]fetchStatusEntry{}
		return
	}
	maps.DeleteFunc(s.fetchStatus, func(_ string, e fetchStatusEntry) bool {
		return now.Sub(e.At) > fetchStatusTTL
	})
}

func (s *Server) loadFetchStatus(gallery string, id int64) (fetchStatusEntry, bool) {
	s.fetchStatusMu.Lock()
	defer s.fetchStatusMu.Unlock()
	e, ok := s.fetchStatus[fetchStatusKey(gallery, id)]
	return e, ok
}

func (s *Server) clearFetchStatus(gallery string, id int64) {
	s.fetchStatusMu.Lock()
	defer s.fetchStatusMu.Unlock()
	delete(s.fetchStatus, fetchStatusKey(gallery, id))
}

// writeFetchPending renders the "fetching..." pill into the target slot. Each
// render re-arms a delayed poll of fetchStatusHandler; n is the attempt count
// that fetchPollMax bounds.
func writeFetchPending(w http.ResponseWriter, id, n int64) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = fmt.Fprintf(w,
		`<span class="fetch-status-msg monloader-accent" hx-get="/internal/images/%d/fetch-status?n=%d" hx-trigger="load delay:%dms" hx-swap="outerHTML">Fetching data via monloader...</span>`,
		id, n+1, fetchPollDelayMs)
}

// writeFetchOutcome swaps a terminal outcome into the top #fetch-status slot
// out-of-band, leaving the main body empty so the pending pill clears wherever
// the triggering button placed it (#fetch-status or #fetch-pending). body must
// be valid HTML; escaping is the caller's responsibility.
func writeFetchOutcome(w http.ResponseWriter, kind, body string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(`<div id="fetch-status" class="fetch-status" hx-swap-oob="true"><div class="flash flash-` + kind + `">` + body + `</div></div>`))
}

// fetchStatusHandler is the detail page's poll for a source fetch's outcome.
// While pending it re-emits the polling pill; on success it triggers a page
// reload so the freshly-applied tags render.
func (s *Server) fetchStatusHandler(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	n, _ := strconv.ParseInt(r.URL.Query().Get("n"), 10, 64)
	e, ok := s.loadFetchStatus(s.activeName, id)
	if !ok {
		// Nothing in flight (or already consumed): stop polling, clear the slot.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		return
	}
	switch e.State {
	case "pending":
		if n >= fetchPollMax {
			writeFetchOutcome(w, "warn", html.EscapeString("Still fetching from monloader; reload to check for new tags."))
			return
		}
		writeFetchPending(w, id, n)
	case "ok":
		// The refresh reloads the page so the applied tags render; the flash
		// rides the stash-and-show bridge to survive the reload.
		s.clearFetchStatus(s.activeName, id)
		msg := e.Msg
		msg = cmp.Or(msg, "Fetched tags from the source.")
		setFlashHeader(w, msg, "ok", nil)
		w.Header().Set("HX-Refresh", "true")
		w.WriteHeader(http.StatusOK)
	case "hash_not_found":
		// A lookup found no site (or the PTR) holding the hash. Common and
		// expected - a resized or re-encoded copy won't match - so it reads
		// as a result, not an error. monloader's message is a "; "-joined
		// per-source trail; render it as a list with the searched hashes
		// recorded at enqueue time.
		s.clearFetchStatus(s.activeName, id)
		writeFetchOutcome(w, "warn", lookupMissBody(e.Msg, e.Hashes))
	case "canceled":
		// monloader dropped the job before it ran - an operator cancel, or a
		// restart draining its queue. Nothing was tried, so it reads as a
		// standing state rather than a failure.
		s.clearFetchStatus(s.activeName, id)
		writeFetchOutcome(w, "warn", "monloader dropped this job before it ran; nothing was looked up.")
	case "already_exists":
		// A replace found its original already in the library as another
		// image; the pair was recorded as potential duplicates. A standing
		// state the operator resolves in the dup workflow, not an error.
		s.clearFetchStatus(s.activeName, id)
		writeFetchOutcome(w, "warn", alreadyExistsBody(e.Msg))
	default:
		// Any other state is terminal: a hash mismatch or apply error from
		// enrich, or a code monloader reported for a fetch that failed before
		// it could enrich. Surface it inline and stop polling.
		s.clearFetchStatus(s.activeName, id)
		writeFetchOutcome(w, "err", html.EscapeString(fetchFailureMessage(e.State, e.Msg)))
	}
}

// imageRefRe matches the "image N" reference the already-exists refusal
// names.
var imageRefRe = regexp.MustCompile(`image (\d+)`)

// alreadyExistsBody renders the already-exists refusal with the image it
// names linked, so the operator can jump straight to the recorded pair.
func alreadyExistsBody(msg string) string {
	return imageRefRe.ReplaceAllString(html.EscapeString(msg), `<a href="/images/$1">image $1</a>`)
}

// ptrTrailName is how monloader's trail entries name its local PTR backend.
const ptrTrailName = "Public Tag Repository"

// lookupMissBody renders a hash lookup's not-found outcome: the PTR's own
// line when it was searched, the online per-source trail as a list, then the
// searched hashes, then a hint that altered copies don't match by hash. Trail
// and hashes come pre-shaped - the trail from monloader's report, the hashes
// from the enqueue record - so an empty value just drops its section.
func lookupMissBody(trail, hashes string) string {
	var b strings.Builder
	if trail == "" {
		b.WriteString("No source found; no tags found.")
	} else {
		var online []string
		for _, entry := range strings.Split(trail, "; ") {
			if !strings.HasPrefix(entry, ptrTrailName+":") {
				online = append(online, entry)
				continue
			}
			// The PTR is local and populates no source URL, so its answer -
			// match or miss - reads apart from the online walk. A match means
			// tags already landed even though the lookup reports a miss.
			b.WriteString("<div>" + html.EscapeString(entry))
			if strings.HasPrefix(entry, ptrTrailName+": match") {
				b.WriteString(" (reload the page to see them)")
			}
			b.WriteString("</div>")
		}
		if len(online) > 0 {
			b.WriteString("No online source found:<ul class=\"lookup-miss-trail\">")
			for _, entry := range online {
				b.WriteString("<li>" + linkifyTrailEntry(entry) + "</li>")
			}
			b.WriteString("</ul>")
		}
	}
	if hashes != "" {
		b.WriteString(`<span class="field-hint">Searched ` + html.EscapeString(hashes) + `</span>`)
	}
	if !similarityLookupRan(trail) {
		b.WriteString(`<div class="field-hint">A miss can happen when the file was compressed or re-encoded, since its hash no longer matches the original. Set up a similarity lookup service in monloader so it can still find such copies.</div>`)
	}
	return b.String()
}

// linkifyTrailEntry escapes one trail entry, turning bare http(s) URLs (the
// closest candidates monloader lists on a similarity miss) into links
// labeled by their host so the list stays scannable.
func linkifyTrailEntry(entry string) string {
	var b strings.Builder
	for {
		i := strings.Index(entry, "http://")
		if j := strings.Index(entry, "https://"); j != -1 && (i == -1 || j < i) {
			i = j
		}
		if i == -1 {
			b.WriteString(html.EscapeString(entry))
			return b.String()
		}
		b.WriteString(html.EscapeString(entry[:i]))
		rest := entry[i:]
		end := strings.IndexAny(rest, " ,")
		if end == -1 {
			end = len(rest)
		}
		raw := rest[:end]
		label := raw
		if u, err := url.Parse(raw); err == nil && u.Host != "" {
			label = strings.TrimPrefix(u.Host, "www.")
		}
		b.WriteString(`<a href="` + html.EscapeString(raw) + `" target="_blank" rel="noopener">` + html.EscapeString(label) + `</a>`)
		entry = rest[end:]
	}
}

// similarityLookupRan reports whether the miss trail shows one of monloader's
// similarity services (iqdb, SauceNAO) answering; a "skipped, needs ..." entry
// means it isn't set up. When one ran, the set-it-up hint is noise.
func similarityLookupRan(trail string) bool {
	for _, entry := range strings.Split(trail, "; ") {
		name, reason, ok := strings.Cut(entry, ": ")
		if !ok {
			continue
		}
		if (name == "iqdb" || name == "saucenao") && !strings.HasPrefix(reason, "skipped") {
			return true
		}
	}
	return false
}

// fetchFailureMessage renders a terminal fetch state into operator-facing text.
// monloader reports a fetch that failed before it could enrich with one of its
// queue's stable error codes; map the actionable ones to a plain sentence and
// fall back to the recorded message otherwise (the enrich path already supplies
// a readable one for a hash mismatch or an apply error).
var fetchFailureMessages = map[string]string{
	"unsupported_url":      "monloader can't fetch this source URL.",
	"network_unreachable":  "monloader couldn't reach the source.",
	"auth_required":        "The source needs a login monloader doesn't have.",
	"blocked":              "The source blocked monloader's fetch.",
	"rate_limited":         "The source is rate-limiting; try again later.",
	"download_failed":      "The source fetch failed on monloader.",
	"monbooru_unreachable": "monloader fetched the source but couldn't apply the tags.",
	"monbooru_rejected":    "monloader fetched the source but couldn't apply the tags.",
	"mapping_failed":       "monloader couldn't read the source's metadata.",
}

func fetchFailureMessage(state, msg string) string {
	return cmp.Or(fetchFailureMessages[state], msg, "The source fetch failed.")
}
