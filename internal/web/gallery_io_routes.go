package web

// The gallery import / export and transfer routes. The IO itself lives in
// internal/galleryio; what stays here is what needs the server: resolving a
// gallery, holding the job lane, swapping a context under ctxMu, and the
// HTTP shape of each route.

import (
	"cmp"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/galleryio"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/models"
)

// ImportGallery replaces the target gallery's database (and optionally its
// source files) with the contents of the uploaded archive/file. Destructive;
// the caller's UI is responsible for confirming intent.
//
// format is one of "db", "json", "zip". For "zip" the inner format is detected
// from the archive. importOver is rejected when the target is the active or
// default gallery (mirrors RemoveGallery's guard).
func (s *Server) ImportGallery(name, format string, upload io.Reader) error {
	if s.jobs.IsRunning() {
		return errJobRunning
	}

	s.ctxMu.Lock()
	cx, ok := s.contexts[name]
	if !ok {
		s.ctxMu.Unlock()
		return fmt.Errorf("unknown gallery %q", name)
	}
	if name == s.activeName {
		s.ctxMu.Unlock()
		return fmt.Errorf("cannot import over the active gallery; switch to another first")
	}
	s.cfgMu.RLock()
	isDefault := name == s.cfg.DefaultGallery
	s.cfgMu.RUnlock()
	if isDefault {
		s.ctxMu.Unlock()
		return fmt.Errorf("cannot import over the default gallery; set another as default first")
	}

	galleryPath := cx.GalleryPath
	dbPath := cx.DBPath
	thumbsPath := cx.ThumbnailsPath
	dataDir := filepath.Dir(dbPath)

	// Buffer the upload to a temp file on the same filesystem as the data
	// directory so the later rename is atomic. The upload may be a multi-GB
	// zip; we cannot keep it in RAM.
	tmp, err := os.CreateTemp(dataDir, "import-*.upload")
	if err != nil {
		s.ctxMu.Unlock()
		return fmt.Errorf("create temp: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if _, err := io.Copy(tmp, upload); err != nil {
		_ = tmp.Close()
		s.ctxMu.Unlock()
		return fmt.Errorf("buffer upload: %w", err)
	}
	_ = tmp.Close()

	// Close the target DB and stop its watcher before touching on-disk state.
	cx.close()

	applyErr := galleryio.ApplyImport(format, tmpPath, dbPath, thumbsPath, galleryPath, s.maxFileSizeMB())

	// Reopen regardless so we leave the gallery usable even after a failed import.
	newCx, openErr := openGalleryCtx(config.Gallery{
		Name: name, GalleryPath: galleryPath, DBPath: dbPath, ThumbnailsPath: thumbsPath,
	})
	if openErr != nil {
		s.ctxMu.Unlock()
		if applyErr != nil {
			return fmt.Errorf("import failed: %w (reopen also failed: %v)", applyErr, openErr)
		}
		return fmt.Errorf("reopen gallery: %w", openErr)
	}
	s.contexts[name] = newCx
	watch, maxMB := s.watcherSettings()
	newCx.startWatcher(watch, maxMB, s.ingestNaming(newCx.Name), s.jobs)
	s.ctxMu.Unlock()

	go newCx.warmCaches()

	if applyErr != nil {
		return applyErr
	}
	logx.Infof("gallery: imported %q (format=%s)", name, format)

	// Make the imported gallery active before queuing the rebuild-thumbs job.
	// Otherwise the job-manager lock the rebuild takes would keep SwitchGallery
	// blocked for the duration of the rebuild, leaving the user pinned to
	// whatever gallery they had active at Import time. Failures here are
	// non-fatal: the import already succeeded and a failed switch just leaves
	// the previous gallery active.
	if err := s.SwitchGallery(name); err != nil {
		logx.Infof("gallery %q: post-import switch skipped: %v", name, err)
	}

	// Import wiped the thumbnails directory as part of the swap, so the newly
	// installed DB now references images that have no thumbnail on disk.
	// Queue a rebuild so the user doesn't have to reach for Maintenance →
	// Rebuild thumbnails manually. Non-fatal: import already succeeded; a
	// concurrent job or empty gallery just skips the kickoff.
	if err := s.startRebuildThumbsJob(newCx); err != nil {
		logx.Infof("gallery %q: skipped post-import rebuild: %v", name, err)
	}
	return nil
}

// settingsGalleryExport serves GET /settings/galleries/{name}/export?format=&with_images=.
// Plain GET so the browser saves the response as a file without HTMX wiring.
func (s *Server) settingsGalleryExport(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	format := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
	withImages := r.URL.Query().Get("with_images") == "true"

	if s.Get(name) == nil {
		http.Error(w, "unknown gallery", http.StatusNotFound)
		return
	}
	switch format {
	case "db", "json", "light":
	default:
		http.Error(w, "format must be db, json, or light", http.StatusBadRequest)
		return
	}

	filename, contentType := exportFilename(name, format, withImages)
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)

	var err error
	switch {
	case format == "light" && withImages:
		err = s.ExportGalleryLight(name, w)
	case format == "light":
		err = s.ExportGalleryLightManifest(name, w)
	case withImages:
		err = s.ExportGalleryArchive(name, format, w)
	case format == "db":
		err = s.ExportGalleryDB(name, w)
	case format == "json":
		err = s.ExportGalleryJSON(name, w)
	}
	if err != nil {
		logx.Warnf("gallery export %q: %v", name, err)
	}
}

func exportFilename(name, format string, withImages bool) (string, string) {
	if format == "light" && withImages {
		return name + "-light.zip", "application/zip"
	}
	if format == "light" {
		return name + "-light.json", "application/json"
	}
	if withImages {
		return name + ".zip", "application/zip"
	}
	switch format {
	case "db":
		return name + ".db", "application/vnd.sqlite3"
	case "json":
		return name + ".json", "application/json"
	}
	return name, "application/octet-stream"
}

// settingsGalleryImport serves POST /settings/galleries/{name}/import.
// Expects a multipart form with `mode`, `confirm_name` (replace only),
// and `file`. The handler reads parts in order with MultipartReader so
// the type-to-confirm gate runs before the (possibly multi-GB) file
// part is consumed; this requires the dialog template to put mode and
// confirm_name fields ahead of the file input. CSRF is validated by
// the middleware off the X-CSRF-Token header so it never triggers
// implicit form parsing on this route.
func (s *Server) settingsGalleryImport(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")

	const maxImport = 16 << 30 // 16 GiB cap; protects against runaway uploads on a LAN setup.
	r.Body = http.MaxBytesReader(w, r.Body, maxImport)

	mr, err := r.MultipartReader()
	if err != nil {
		writeInlineFlash(w, "err", "expected multipart/form-data")
		return
	}

	const maxFieldBytes = 1 << 20 // 1 MiB per form field; values are short
	fields := map[string]string{}
	var filePart *multipart.Part
	var fileFilename string
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			writeInlineFlash(w, "err", "malformed upload")
			return
		}
		if part.FileName() == "" {
			body, readErr := io.ReadAll(io.LimitReader(part, maxFieldBytes))
			_ = part.Close()
			if readErr != nil {
				writeInlineFlash(w, "err", "malformed upload")
				return
			}
			fields[part.FormName()] = strings.TrimSpace(string(body))
			continue
		}
		filePart = part
		fileFilename = part.FileName()
		break
	}
	if filePart == nil {
		writeInlineFlash(w, "err", "missing file")
		return
	}
	defer func() { _ = filePart.Close() }()

	mode := fields["mode"]
	mode = cmp.Or(mode, "replace")
	if mode != "replace" && mode != "merge" {
		writeInlineFlash(w, "err", "mode must be replace or merge")
		return
	}
	if mode == "replace" {
		if fields["confirm_name"] != name {
			writeInlineFlash(w, "err", "type-to-confirm name does not match")
			return
		}
	}
	format := galleryio.FormatFromExt(fileFilename)
	if format == "" {
		writeInlineFlash(w, "err", "file must be .db, .json, or .zip")
		return
	}

	if mode == "merge" {
		res, err := s.MergeGallery(name, format, filePart)
		if err != nil {
			writeInlineFlash(w, "err", err.Error())
			return
		}
		// Mirror the replace path (ImportGallery → SwitchGallery): a merge
		// brings new images into the target gallery, so the user expects to
		// land on it. No-op if the target is already active.
		if err := s.SwitchGallery(name); err != nil {
			logx.Infof("gallery %q: post-merge switch skipped: %v", name, err)
		}
		writeInlineFlash(w, "ok", "Gallery "+name+" merged: "+res.Summary()+".")
		return
	}
	if err := s.ImportGallery(name, format, filePart); err != nil {
		writeInlineFlash(w, "err", err.Error())
		return
	}
	// Write the success flash into #flash-galleries; the dialog's
	// after-request hook detects the flash-ok, closes the modal, and
	// triggers a reload so the newly-active gallery badge shows.
	writeInlineFlash(w, "ok", "Gallery "+name+" imported. Rebuilding thumbnails in the background.")
}

// batchTransfer copies every image in the resolved scope into another gallery:
// each file is re-ingested there (fresh thumbnail + metadata + phash, SHA-
// deduped into any existing target row) and its tags, sources, commentary,
// annotations, note and favorite ride along. Relations and collections do not.
// With remove_after set, each confirmed copy deletes the source image. Runs as
// a transfer job so the global jobs lane serializes it and the target gallery's
// watcher stays suppressed against the file copy.
func (s *Server) batchTransfer(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	dstCx, removeAfter, ok := s.transferTarget(w, r)
	if !ok {
		return
	}
	s.startScopedJob(w, r, "batch-transfer", models.JobTypeTransfer, func(ids []int64) {
		s.runBatchTransfer(ids, dstCx, removeAfter)
	})
}

// transferImage copies the one image at {id} into the target gallery, mirroring
// moveImage's single-image job shape so the watcher-suppression pattern is
// reused. Without remove_after the operator stays on the source image; with it
// the source is gone, so the redirect returns to the gallery.
func (s *Server) transferImage(w http.ResponseWriter, r *http.Request) {
	id, ok := idAndForm(w, r)
	if !ok {
		return
	}
	dstCx, removeAfter, ok := s.transferTarget(w, r)
	if !ok {
		return
	}
	if !s.startJob(w, models.JobTypeTransfer) {
		return
	}
	srcCx := s.Active()
	if err := s.transferOneImage(srcCx, dstCx, id, removeAfter); err != nil {
		s.jobs.Fail(err.Error())
		flashStatus(w, http.StatusBadRequest, err.Error())
		return
	}
	dstCx.InvalidateCaches()
	if removeAfter {
		srcCx.InvalidateCaches()
	}
	msg := fmt.Sprintf("Transferred image to %s.", dstCx.Name)
	s.jobs.Complete(msg)

	dest := fmt.Sprintf("/images/%d", id)
	if removeAfter {
		dest = "/"
	}
	if isHTMXRequest(r) {
		setFlashHeader(w, msg, "ok", nil)
		w.Header().Set("HX-Redirect", dest)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, dest, http.StatusSeeOther)
}

// transferTarget parses and validates the target gallery + remove_after fields
// shared by the batch and single-image handlers. The target must be a different,
// live gallery.
func (s *Server) transferTarget(w http.ResponseWriter, r *http.Request) (*galleryCtx, bool, bool) {
	target := strings.TrimSpace(r.FormValue("target"))
	msg := ""
	switch target {
	case "":
		msg = "Pick a target gallery."
	case s.activeName:
		msg = "The target must be a different gallery."
	}
	if msg == "" {
		if dst := s.Get(target); dst == nil || dst.DB == nil {
			msg = "Unknown target gallery."
		} else if dst.Degraded {
			msg = "The target gallery is unavailable."
		} else {
			return dst, r.FormValue("remove_after") != "", true
		}
	}
	flashStatus(w, http.StatusBadRequest, msg)
	return nil, false, false
}

// runBatchTransfer processes targets one image at a time with per-image error
// isolation, mirroring runBatchMove: a single unreadable file can't strand the
// rest.
func (s *Server) runBatchTransfer(ids []int64, dstCx *galleryCtx, removeAfter bool) {
	srcCx := s.Active()
	total := len(ids)
	transferred, failed, cancelled := s.perImageLoop(ids, "transfer", "transferring", func(_ int, id int64) error {
		return s.transferOneImage(srcCx, dstCx, id, removeAfter)
	})

	if transferred > 0 {
		dstCx.InvalidateCaches()
		if removeAfter {
			srcCx.InvalidateCaches()
		}
	}
	if cancelled {
		s.jobs.Complete(fmt.Sprintf("transfer cancelled (%d/%d done)", transferred, total))
		return
	}
	summary := fmt.Sprintf("Transferred %d image(s) to %s.", transferred, dstCx.Name)
	if failed > 0 {
		summary = fmt.Sprintf("Transferred %d image(s) to %s, %d failed.", transferred, dstCx.Name, failed)
	}
	s.jobs.Complete(summary)
}

// The export entry points address a gallery by name - what the route and the
// tests both have - and hand the resolved handle to internal/galleryio, which
// never needs to know the server exists.

func (s *Server) ExportGalleryDB(name string, w io.Writer) error {
	return s.exportGallery(name, func(cx *galleryCtx) error { return galleryio.ExportGalleryDB(cx.Handle, w) })
}

func (s *Server) ExportGalleryJSON(name string, w io.Writer) error {
	return s.exportGallery(name, func(cx *galleryCtx) error { return galleryio.ExportGalleryJSON(cx.Handle, w) })
}

func (s *Server) ExportGalleryArchive(name, format string, w io.Writer) error {
	return s.exportGallery(name, func(cx *galleryCtx) error { return galleryio.ExportGalleryArchive(cx.Handle, format, w) })
}

func (s *Server) ExportGalleryLight(name string, w io.Writer) error {
	return s.exportGallery(name, func(cx *galleryCtx) error { return galleryio.ExportGalleryLight(cx.Handle, w) })
}

func (s *Server) ExportGalleryLightManifest(name string, w io.Writer) error {
	return s.exportGallery(name, func(cx *galleryCtx) error { return galleryio.ExportGalleryLightManifest(cx.Handle, w) })
}

func (s *Server) exportGallery(name string, write func(*galleryCtx) error) error {
	cx := s.Get(name)
	if cx == nil {
		return fmt.Errorf("unknown gallery %q", name)
	}
	return write(cx)
}

// MergeGallery additively brings the upload's tags - and its images, when it
// carries their files - into the named gallery. Unlike ImportGallery it wipes
// nothing and is permitted on the active and default galleries.
func (s *Server) MergeGallery(name, format string, upload io.Reader) (galleryio.MergeResult, error) {
	if s.jobs.IsRunning() {
		return galleryio.MergeResult{}, errJobRunning
	}
	cx := s.Get(name)
	if cx == nil {
		return galleryio.MergeResult{}, fmt.Errorf("unknown gallery %q", name)
	}
	res, err := galleryio.MergeGallery(cx.Handle, format, upload, s.maxFileSizeMB())
	if err == nil {
		cx.InvalidateCaches()
	}
	return res, err
}

// transferOneImage resolves the server-side pieces internal/galleryio takes
// by parameter - the size cap and the relations cleanup - so a caller with
// two gallery contexts names only what it is transferring.
func (s *Server) transferOneImage(srcCx, dstCx *galleryCtx, id int64, removeAfter bool) error {
	return galleryio.TransferOneImage(srcCx.Handle, dstCx.Handle, id, removeAfter,
		s.maxFileSizeMB(), s.onImageDeleteCallback())
}
