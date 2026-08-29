package web

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/monbooru/monbooru/internal/gallery"
)

// namePreviewRows caps what the preview shows: enough to see a sequence
// advance, few enough that typing stays cheap on a large scope.
const namePreviewRows = 3

// namePreviewScopes maps the surface a field belongs to onto the parse
// scope, so the preview refuses exactly what the submit would.
var namePreviewScopes = map[string]gallery.Scope{
	"rename":       gallery.ScopeRename,
	"rename-batch": gallery.ScopeRenameBatch,
	"move":         gallery.ScopeMove,
	"move-batch":   gallery.ScopeMoveBatch,
}

// namePreview answers what a template would name the given images, so the
// operator reads the result before committing to it. Every dialog and
// settings field renders through this one endpoint rather than a second
// implementation in the browser. Nothing to show answers an empty body
// rather than a 204: htmx does not swap on No Content, which would leave
// the last render standing under a field that no longer says it.
func (s *Server) namePreview(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if q.Get("scope") == "upload" {
		s.uploadDestPreview(r.Context(), w, q.Get("folder"), q.Get("name"))
		return
	}
	scope, known := namePreviewScopes[q.Get("scope")]
	if !known {
		return
	}
	raw := strings.TrimSpace(q.Get("tmpl"))
	var tmpl *gallery.NameTemplate
	var err error
	if scope == gallery.ScopeRenameBatch {
		// The job's own entry point, so the preview shows the implicit {n}
		// a plain base name gets instead of the whole scope on one name.
		tmpl, err = gallery.ParseBatchRenameTemplate(raw)
	} else {
		tmpl, err = gallery.ParseNameTemplate(raw, scope)
	}
	if err != nil {
		s.renderNamePreviewError(w, "", err.Error())
		return
	}
	if tmpl == nil {
		return
	}

	ids := s.namePreviewIDs(q["ids"])
	if len(ids) == 0 {
		return
	}
	total, _ := strconv.Atoi(q.Get("total"))
	total = max(total, len(ids))

	rows := make([]namePreviewRow, 0, len(ids))
	var rowErr error
	md5Cap := s.previewMD5Cap()
	for i, id := range ids {
		facts, factErr := gallery.LoadNameFacts(r.Context(), s.db(), s.activeGallery(), id, md5Cap, tmpl)
		if factErr != nil {
			rowErr = factErr
			continue
		}
		facts.N, facts.NWidth = i+1, max(len(strconv.Itoa(total)), 2)
		// What the submit will actually use: singleName and batchName both
		// take the literal when the template carries no token of its own,
		// and only a render passes through the path tidying.
		to := raw
		if tmpl.HasTokens() {
			rendered, renderErr := tmpl.Render(facts)
			if renderErr != nil {
				rowErr = renderErr
				continue
			}
			to = rendered
		}
		from := facts.Base
		if scope.Folder() {
			// Containment is the move's other refusal, and the preview is
			// where a destination gets checked: submitting an escaping one
			// answers a status htmx discards, so the dialog would just sit
			// there saying nothing.
			abs, err := gallery.ResolveSubdir(s.galleryPath(), to)
			if err != nil {
				s.renderNamePreviewError(w, "", err.Error())
				return
			}
			// Show where the move lands, not what was typed: ResolveSubdir
			// trims and cleans, so `a//b` files under `a/b` and `./x` under
			// `x`. A preview the job then rewrites is the one thing the
			// affordance must not do.
			to = gallery.FolderPath(s.galleryPath(), filepath.Join(abs, facts.Base))
			// A move keeps the name and changes the folder, so both sides
			// carry the whole path; naming only the destination folder reads
			// as renaming the file to a directory.
			from, to = namePath(facts.Folder, facts.Base), namePath(to, facts.Base)
		} else if onDisk := filepath.Ext(facts.Base); !strings.EqualFold(filepath.Ext(to), onDisk) {
			// Mirror RenameImage: the extension already on disk is what gets
			// appended, spelling included, when the render carries none.
			to += onDisk
		}
		rows = append(rows, namePreviewRow{From: from, To: to})
	}
	if len(rows) == 0 {
		if rowErr != nil {
			s.renderNamePreviewError(w, "", rowErr.Error())
		}
		return
	}
	caption := ""
	if total > len(rows) {
		caption = fmt.Sprintf("first %d of %d, in scope order", len(rows), total)
	}
	s.renderNamePreview(w, caption, rows)
}

type namePreviewRow struct{ From, To string }

// previewMD5Cap bounds the lazy {md5} fill the preview endpoints can
// trigger. They run off a keystroke on ids the caller names, so without
// it typing {md5} into a dialog reads whole files inside the request.
// Over the cap the token renders empty and the backfill job owns the
// digest, matching the detail page's md5 cell.
func (s *Server) previewMD5Cap() int64 {
	return int64(s.maxFileSizeMB()) * 1024 * 1024
}

// namePath places base under dir. An empty dir is the gallery root, where
// the path is the name on its own.
func namePath(dir, base string) string {
	if dir == "" {
		return base
	}
	return dir + "/" + base
}

func (s *Server) renderNamePreview(w http.ResponseWriter, caption string, rows []namePreviewRow) {
	s.renderTemplate(w, "partials/name_preview.html", map[string]any{
		"Caption": caption,
		"Rows":    rows,
	})
}

// renderNamePreviewError names the field a refusal came from when one slot
// serves two of them.
func (s *Server) renderNamePreviewError(w http.ResponseWriter, field, msg string) {
	s.renderTemplate(w, "partials/name_preview.html", map[string]any{
		"Field": field,
		"Error": msg,
	})
}

// uploadDestPreview answers the one path a file arriving now would land
// at, so the two destination settings read as the single destination they
// are. A refusal names the field it came from.
func (s *Server) uploadDestPreview(ctx context.Context, w http.ResponseWriter, folder, name string) {
	folderTmpl, err := gallery.ParseNameTemplate(folder, gallery.ScopeUploadFolder)
	if err != nil {
		s.renderNamePreviewError(w, "folder", err.Error())
		return
	}
	nameTmpl, err := gallery.ParseNameTemplate(name, gallery.ScopeUploadName)
	if err != nil {
		s.renderNamePreviewError(w, "name", err.Error())
		return
	}
	ids := s.namePreviewIDs(nil)
	if (folderTmpl == nil && nameTmpl == nil) || len(ids) == 0 {
		return
	}
	facts, err := gallery.LoadNameFacts(ctx, s.db(), s.activeGallery(), ids[0], s.previewMD5Cap(), folderTmpl, nameTmpl)
	if err != nil {
		s.renderNamePreviewError(w, "", err.Error())
		return
	}

	base := facts.Base
	if nameTmpl != nil {
		if base, err = nameTmpl.Render(facts); err != nil {
			s.renderNamePreviewError(w, "name", err.Error())
			return
		}
		if onDisk := filepath.Ext(facts.Base); !strings.EqualFold(filepath.Ext(base), onDisk) {
			base += onDisk
		}
	}
	to := base
	if folderTmpl != nil {
		dir, dirErr := folderTmpl.Render(facts)
		if dirErr != nil {
			s.renderNamePreviewError(w, "folder", dirErr.Error())
			return
		}
		to = namePath(dir, base)
	}
	s.renderNamePreview(w, "a file arriving now", []namePreviewRow{{From: facts.Base, To: to}})
}

// ingestNaming is where a file found on disk is filed, which is the
// received-file destination behind the operator's opt-in. Empty when the
// opt-in is off, which leaves a dropped file exactly as it arrived.
func (s *Server) ingestNaming(galleryName string) gallery.Naming {
	s.cfgMu.RLock()
	on, folder, name := s.cfg.Gallery.RenameOnIngest, s.cfg.Gallery.DefaultUploadFolder, s.cfg.Gallery.DefaultUploadName
	s.cfgMu.RUnlock()
	if !on {
		return gallery.Naming{}
	}
	return gallery.IngestNaming(galleryName, folder, name)
}

// receivedNaming is where a file monbooru writes itself goes: the directory
// the bytes land in now, and the move that files the row once it exists.
// The name half is deliberately left off - the reader extract, the generated
// pages and the generated cbz each name their own output, and those names
// carry the page order or the generation time.
func (s *Server) receivedNaming(galleryName string) (writeDir string, n gallery.Naming) {
	s.cfgMu.RLock()
	folder := s.cfg.Gallery.DefaultUploadFolder
	s.cfgMu.RUnlock()
	return gallery.ReceivedNaming(galleryName, "", strings.TrimSpace(folder), "")
}

// namePreviewIDs takes the ids the caller named, or falls back to the
// newest row so the settings fields have something real to render
// against without the page knowing an id. htmx flattens an array value
// into one comma-joined parameter, so both shapes are read.
func (s *Server) namePreviewIDs(raw []string) []int64 {
	flat := make([]string, 0, len(raw))
	for _, v := range raw {
		flat = append(flat, strings.Split(v, ",")...)
	}
	ids := parseIDList(flat)
	if len(ids) > namePreviewRows {
		ids = ids[:namePreviewRows]
	}
	if len(ids) > 0 {
		return ids
	}
	var id int64
	if err := s.db().Read.QueryRow(
		`SELECT id FROM images WHERE is_missing = 0 ORDER BY id DESC LIMIT 1`,
	).Scan(&id); err != nil {
		return nil
	}
	return []int64{id}
}
