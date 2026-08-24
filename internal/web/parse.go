package web

import (
	"fmt"
	"html"
	"net/http"
	"strconv"
	"strings"

	"github.com/monbooru/monbooru/internal/tagger"
)

// pathInt64 parses a numeric path segment, writing 404 on failure.
func pathInt64(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	raw := r.PathValue(name)
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return 0, false
	}
	return v, true
}

// pathTaggerName trims the named path segment and validates it through
// tagger.ValidateTaggerName, writing a 400 plain-text error on failure.
func pathTaggerName(w http.ResponseWriter, r *http.Request) (string, bool) {
	v := strings.TrimSpace(r.PathValue("name"))
	if err := tagger.ValidateTaggerName(v); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return "", false
	}
	return v, true
}

// formInt64 parses an integer form field, writing the error flash on
// failure. Status 200 (rather than 400) so HTMX picks the body up and
// swaps it into the dialog target the caller hands it; default config
// drops 4xx swaps and the operator would otherwise see no feedback.
func formInt64(w http.ResponseWriter, r *http.Request, name string) (int64, bool) {
	// Status 200 so HTMX swaps the flash into the dialog (it drops 4xx swaps).
	writeFieldFlash := func(verb string) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprintf(w, `<div class="flash flash-err">%s %s.</div>`, verb, html.EscapeString(name))
	}
	raw := strings.TrimSpace(r.FormValue(name))
	if raw == "" {
		writeFieldFlash("Missing")
		return 0, false
	}
	v, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		writeFieldFlash("Invalid")
		return 0, false
	}
	return v, true
}

// idAndForm resolves the {id} path value and parses the form, writing the
// refusal and reporting false when either fails. The id goes first, so a
// request that is wrong about both answers 404 rather than a form error -
// no route reads the form to decide what the id means.
func idAndForm(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return 0, false
	}
	if !parseFormOK(w, r) {
		return 0, false
	}
	return id, true
}

// taggerNameAndForm is idAndForm for the per-tagger settings routes.
func taggerNameAndForm(w http.ResponseWriter, r *http.Request) (string, bool) {
	name, ok := pathTaggerName(w, r)
	if !ok {
		return "", false
	}
	if !parseFormOK(w, r) {
		return "", false
	}
	return name, true
}
