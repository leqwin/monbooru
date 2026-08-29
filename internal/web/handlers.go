package web

import (
	"cmp"
	"context"
	"encoding/json"
	"html"
	"net/http"
	"os"
	"strings"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/logx"
	meta "github.com/monbooru/monbooru/internal/metadata"
	"github.com/monbooru/monbooru/internal/models"
)

// setFlashHeader merges a `monbooru:flash` HX-Trigger event into the
// response so a post-redirect / post-reload page can surface the
// summary via the shared #gallery-flash / #detail-flash slot. extras
// carries other triggers the handler also wants to fire (e.g. the
// delete handler's delete-go-back). kind picks the flash-ok / flash-err
// palette; pass "" for ok.
func setFlashHeader(w http.ResponseWriter, text, kind string, extras map[string]any) {
	kind = cmp.Or(kind, "ok")
	// The client renders this through innerHTML (showActionFlash), so the
	// text is escaped here at the single boundary, the same way
	// writeInlineFlash escapes the body path. Without it an operator-
	// supplied value spliced into the message (e.g. a folder name in the
	// move flash) would land as live markup.
	triggers := map[string]any{
		"monbooru:flash": map[string]any{"text": html.EscapeString(text), "kind": kind},
	}
	for k, v := range extras {
		triggers[k] = v
	}
	if b, err := json.Marshal(triggers); err == nil {
		w.Header().Set("HX-Trigger", string(b))
	}
}

// hxDone finishes a successful mutating handler: HTMX callers get the ok
// flash plus a full refresh (hxDest == "") or an HX-Redirect to hxDest;
// plain form submits get a 303 to fallback.
func hxDone(w http.ResponseWriter, r *http.Request, flash, hxDest, fallback string) {
	if isHTMXRequest(r) {
		setFlashHeader(w, flash, "ok", nil)
		if hxDest == "" {
			w.Header().Set("HX-Refresh", "true")
		} else {
			w.Header().Set("HX-Redirect", hxDest)
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, fallback, http.StatusSeeOther)
}

// hxRedirect sends an htmx caller to dest via the HX-Redirect header and
// everyone else via a 303. The bare counterpart to hxDone, which carries
// a flash along with the redirect.
func hxRedirect(w http.ResponseWriter, r *http.Request, dest string) {
	if isHTMXRequest(r) {
		w.Header().Set("HX-Redirect", dest)
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

// writeInlineFlash writes a `<div class="flash flash-{kind}">...</div>`
// fragment with text HTML-escaped, for handlers that need the flash
// payload in the response body (htmx partial swap target) rather than
// only as an HX-Trigger. kind is "ok" / "err" / "warn" (callers pass
// the bare suffix; the function adds the `flash-` prefix). text is
// taken verbatim and HTML-escaped here so every call site shares one
// escape boundary.
func writeInlineFlash(w http.ResponseWriter, kind, text string) {
	kind = cmp.Or(kind, "ok")
	_, _ = w.Write([]byte(`<div class="flash flash-` + kind + `">` + html.EscapeString(text) + `</div>`))
}

// writeInlineFlashHTML mirrors writeInlineFlash but takes a body that is
// already valid HTML; escaping is the caller's responsibility. Used by
// the few flashes that carry markup (e.g. links to affected rows) which
// the plain-text escaper would render as literal angle brackets.
func writeInlineFlashHTML(w http.ResponseWriter, kind, body string) {
	kind = cmp.Or(kind, "ok")
	_, _ = w.Write([]byte(`<div class="flash flash-` + kind + `">` + body + `</div>`))
}

// writeFlashOOB swaps a flash into a slot out-of-band so the message outlives a
// polling fragment that would otherwise overwrite the region it sat in. Empty
// text clears the slot.
func writeFlashOOB(w http.ResponseWriter, id, kind, text string) {
	body := ""
	if text != "" {
		kind = cmp.Or(kind, "ok")
		body = `<div class="flash flash-` + kind + `">` + html.EscapeString(text) + `</div>`
	}
	_, _ = w.Write([]byte(`<div id="` + id + `" hx-swap-oob="true">` + body + `</div>`))
}

// notFoundHandler renders a styled 404 for any unmatched GET path. The
// mux's default behaviour is unstyled `404 page not found` text on a
// white page; routing through the standard layout keeps the user inside
// the app with a back link.
func (s *Server) notFoundHandler(w http.ResponseWriter, r *http.Request) {
	// Routes are registered slash-less; retry /categories/ style paths
	// without the trailing slash before giving up.
	if p := strings.TrimRight(r.URL.Path, "/"); p != "" && p != r.URL.Path {
		if r.URL.RawQuery != "" {
			p += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, p, http.StatusMovedPermanently)
		return
	}
	w.WriteHeader(http.StatusNotFound)
	s.renderTemplate(w, "notfound.html", s.base(r, "", "Not found - "+s.booruName()))
}

func loadImage(ctx context.Context, database *db.DB, id int64) (*models.Image, error) {
	img, err := models.ScanImageRow(database.Read.QueryRowContext(ctx,
		`SELECT `+models.ImageRowColumns+` FROM images i WHERE i.id = ?`, id))
	if err != nil {
		return nil, err
	}
	return &img, nil
}

func loadSDMeta(ctx context.Context, database *db.DB, id int64) *models.SDMetadata {
	var m models.SDMetadata
	var rawParams, genHash *string
	err := database.Read.QueryRowContext(ctx,
		`SELECT image_id, prompt, negative_prompt, model, seed, sampler, steps, cfg_scale, raw_params, generation_hash
		 FROM sd_metadata WHERE image_id = ?`, id,
	).Scan(&m.ImageID, &m.Prompt, &m.NegativePrompt, &m.Model, &m.Seed, &m.Sampler, &m.Steps, &m.CFGScale, &rawParams, &genHash)
	if err != nil {
		return nil
	}
	if rawParams != nil {
		m.RawParams = *rawParams
	}
	if genHash != nil {
		m.GenerationHash = *genHash
	}
	if m.RawParams != "" {
		m.ParsedParams = meta.ParseAllSDParams(m.RawParams)
	}
	return &m
}

func loadComfyMeta(ctx context.Context, database *db.DB, id int64) *models.ComfyUIMetadata {
	var m models.ComfyUIMetadata
	var genHash *string
	err := database.Read.QueryRowContext(ctx,
		`SELECT image_id, prompt, model_checkpoint, seed, sampler, steps, cfg_scale, raw_workflow, generation_hash
		 FROM comfyui_metadata WHERE image_id = ?`, id,
	).Scan(&m.ImageID, &m.Prompt, &m.ModelCheckpoint, &m.Seed, &m.Sampler, &m.Steps, &m.CFGScale, &m.RawWorkflow, &genHash)
	if err != nil {
		return nil
	}
	if genHash != nil {
		m.GenerationHash = *genHash
	}
	return &m
}

// userAndStaleTags reports what the remove-tags dialog needs to know about
// an image's tag list: whether it carries any of the operator's own, and
// whether any of them went stale.
func userAndStaleTags(imageTags []models.ImageTag) (hasUser, hasStale bool) {
	for _, t := range imageTags {
		if !t.IsAuto && t.TaggerName == "" {
			hasUser = true
		}
		if t.Stale {
			hasStale = true
		}
		if hasUser && hasStale {
			break
		}
	}
	return hasUser, hasStale
}

func loadImagePaths(ctx context.Context, database *db.DB, id int64) []models.ImagePath {
	rows, err := database.Read.QueryContext(ctx,
		`SELECT id, image_id, path, is_canonical FROM image_paths WHERE image_id = ? ORDER BY is_canonical DESC, id`,
		id,
	)
	if err != nil {
		return nil
	}
	defer func() { _ = rows.Close() }()
	var paths []models.ImagePath
	for rows.Next() {
		var p models.ImagePath
		var isCanon int
		if err := rows.Scan(&p.ID, &p.ImageID, &p.Path, &isCanon); err != nil {
			logx.Warnf("load image paths scan: %v", err)
			continue
		}
		p.IsCanonical = isCanon == 1
		// A non-canonical path whose file is gone is move/copy history, not
		// a live duplicate; keep it out of the Duplicates panel.
		if !p.IsCanonical {
			if _, statErr := os.Stat(p.Path); os.IsNotExist(statErr) {
				continue
			}
		}
		paths = append(paths, p)
	}
	if err := rows.Err(); err != nil {
		logx.Warnf("load image paths: %v", err)
	}
	return paths
}

// defaultThemeColor mirrors the stylesheet's --bg, the splash a viewer
// gets when server.theme_color is unset.
const defaultThemeColor = "#0e0e0e"

// manifestHandler serves the web app manifest behind the layout's
// <link rel="manifest">, so a browser can install the gallery as a
// home-screen app. The icon follows server.logo the same way
// booruFaviconURL does - an override replaces the bundled icon rather
// than sitting beside it, and an active theme's logo.png does not reach
// it - and carries no sizes hint, since the operator's file has
// whatever dimensions it has.
func (s *Server) manifestHandler(w http.ResponseWriter, r *http.Request) {
	icon := map[string]any{
		"src":     "/static/icon-192.png",
		"sizes":   "192x192",
		"type":    "image/png",
		"purpose": "any maskable",
	}
	if logo := s.customLogoURL(); logo != "" {
		icon = map[string]any{"src": logo}
	}
	name := s.booruName()
	color := cmp.Or(s.themeColor(), defaultThemeColor)
	w.Header().Set("Content-Type", "application/manifest+json; charset=utf-8")
	// Built from live config, so a heuristic cache would keep serving the
	// old name after a rename; /custom.css and /custom.logo revalidate for
	// the same reason.
	w.Header().Set("Cache-Control", "no-cache")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"name":             name,
		"short_name":       name,
		"id":               "/",
		"start_url":        "/",
		"display":          "standalone",
		"background_color": color,
		"theme_color":      color,
		"icons":            []map[string]any{icon},
	})
}
