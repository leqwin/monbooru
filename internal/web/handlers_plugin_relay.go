package web

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"time"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/logx"
)

const (
	// pluginPayloadVersion is the relay contract version. It bumps only on a
	// breaking change to the request shape below.
	pluginPayloadVersion = 1
	// pluginRelayTimeout bounds one relay call. There is no retry: the peer
	// may have committed the work, and a second call would repeat it.
	pluginRelayTimeout = 10 * time.Second
	// pluginMessageMax caps the peer's message before it is flashed.
	pluginMessageMax = 200
)

// pluginRelayRequest is what monbooru POSTs to a peer when a relay button is
// clicked. image_ids is the resolved scope: the detail image, or the gallery
// selection.
type pluginRelayRequest struct {
	Payload  int     `json:"payload"`
	Monbooru string  `json:"monbooru"`
	Gallery  string  `json:"gallery"`
	Slot     string  `json:"slot"`
	Button   string  `json:"button"`
	ImageIDs []int64 `json:"image_ids"`
}

// relayRefused answers a click monbooru will not carry, the same way the peer's
// own outcomes are answered: a flash header on a 204. The click swaps nothing,
// so an error status leaves htmx with a body it discards and the operator with
// no sign the button did anything at all.
func relayRefused(w http.ResponseWriter, msg string) {
	setFlashHeader(w, msg, "err", nil)
	w.WriteHeader(http.StatusNoContent)
}

// pluginRelay carries one button click to its peer and flashes the answer.
// The outbound call runs off the gallery read lock (see
// contextMiddlewareBypass) so a slow peer cannot stall foreground requests
// for the length of its timeout.
func (s *Server) pluginRelay(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	name := r.FormValue("plugin")
	p, ok := s.plugin(name)
	if !ok || !s.pluginUsable(p) {
		relayRefused(w, "plugin "+name+" is not available")
		return
	}
	idx, err := strconv.Atoi(r.FormValue("button"))
	if err != nil || idx < 0 || idx >= len(p.Buttons) || p.Buttons[idx].Mode != config.ModeRelay {
		relayRefused(w, "unknown plugin button")
		return
	}
	ids := parseIDList(r.Form["ids"])
	if len(ids) == 0 {
		relayRefused(w, "no images selected")
		return
	}
	button := p.Buttons[idx]
	scoped := s.scopeForButton(button, ids)
	if len(scoped) == 0 {
		relayRefused(w, "nothing selected that "+name+" handles")
		return
	}
	// The peer never hears about the rows its media excluded, so its own
	// message cannot account for a selection smaller than the one the
	// operator built by hand.
	var narrowed string
	if n := len(ids) - len(scoped); n > 0 {
		narrowed = fmt.Sprintf(" (%d of %d sent; %d not handled by %s)", len(scoped), len(ids), n, name)
	}
	ids = scoped
	base := s.pluginBase(p)
	if base == "" || p.PeerToken == "" {
		relayRefused(w, "plugin "+name+" has no address to call")
		return
	}

	// The route bypasses ContextMiddleware so the peer call never runs under
	// ctxMu; snapshot the active name instead.
	galleryName := s.activeGallery()

	answer, err := s.callPluginRelay(r.Context(), p.Name, base+button.Path, p.PeerToken, pluginRelayRequest{
		Payload:  pluginPayloadVersion,
		Monbooru: Version,
		Gallery:  galleryName,
		Slot:     button.Slot,
		Button:   button.Label,
		ImageIDs: ids,
	})
	if err != nil {
		s.markPluginDown(name)
		logx.Warnf("plugin relay %s: %v", name, err)
		relayRefused(w, "plugin "+name+" did not answer")
		return
	}
	message := truncateRunes(answer.Message, pluginMessageMax)
	if !answer.OK {
		if message == "" {
			message = "plugin " + name + " refused the request"
		}
		relayRefused(w, message+narrowed)
		return
	}
	setFlashHeader(w, message+narrowed, "ok", nil)
	// An in-place edit (a rotate) only shows once the page re-reads it.
	if answer.Refresh {
		w.Header().Set("HX-Refresh", "true")
	}
	w.WriteHeader(http.StatusNoContent)
}

// scopeForButton drops the ids whose medium the button declared it does not
// handle, so a mixed selection never sends a peer what it could only refuse.
// A read that fails leaves the scope as the operator picked it.
func (s *Server) scopeForButton(b config.PluginButton, ids []int64) []int64 {
	if b.Media == "" {
		return ids
	}
	d := s.db()
	if d == nil {
		return ids
	}
	in, args := db.InPlaceholders(ids)
	rows, err := d.Read.Query(`SELECT id, file_type FROM images WHERE id IN (`+in+`)`, args...)
	if err != nil {
		return ids
	}
	defer func() { _ = rows.Close() }()
	handled := make(map[int64]bool, len(ids))
	for rows.Next() {
		var id int64
		var fileType string
		if err := rows.Scan(&id, &fileType); err == nil && b.AppliesTo(fileType) {
			handled[id] = true
		}
	}
	// A read that stopped part-way knows nothing about the rows it never
	// reached, and dropping them would send the peer a quietly smaller scope.
	if err := rows.Err(); err != nil {
		logx.Warnf("plugin scope for %s: %v", b.Label, err)
		return ids
	}
	return slices.DeleteFunc(ids, func(id int64) bool { return !handled[id] })
}

// pluginRelayAnswer is the peer's reply.
type pluginRelayAnswer struct {
	OK      bool   `json:"ok"`
	Message string `json:"message"`
	Refresh bool   `json:"refresh"`
}

// callPluginRelay posts the payload and decodes the answer. A transport error
// or a non-2xx status is reported as a failure to answer, which also marks the
// peer down.
func (s *Server) callPluginRelay(ctx context.Context, peer, target, token string, payload pluginRelayRequest) (pluginRelayAnswer, error) {
	ctx, cancel := context.WithTimeout(ctx, pluginRelayTimeout)
	defer cancel()
	body, err := json.Marshal(payload)
	if err != nil {
		return pluginRelayAnswer{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		return pluginRelayAnswer{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := pluginClient.Do(req)
	if err != nil {
		return pluginRelayAnswer{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return pluginRelayAnswer{}, peerStatusError{peer, resp.Status}
	}
	var answer pluginRelayAnswer
	if err := json.NewDecoder(http.MaxBytesReader(nil, resp.Body, 1<<16)).Decode(&answer); err != nil {
		return pluginRelayAnswer{}, err
	}
	return answer, nil
}

// truncateRunes clips s to at most n runes, so a peer's message can't push
// arbitrary length into the flash slot and a multi-byte character is never
// cut in half. Byte length bounds rune count, so the first check skips the
// conversion for a short string.
func truncateRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}
