package web

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/models"
)

// generateMangaCollection serves POST /images/{id}/generate-collection:
// every page of the cbz archive is extracted as its own image row and
// filed under a new collection in page order. Runs as a background job.
func (s *Server) generateMangaCollection(w http.ResponseWriter, r *http.Request) {
	img, ok := s.loadMangaImage(w, r)
	if !ok {
		return
	}
	name, cx, writeDir, naming, ok := s.collectionJobPrologue(w, r)
	if !ok {
		return
	}
	s.goGenerationJob(func(ctx context.Context) (string, error) {
		done, err := s.runMangaCollection(ctx, cx, img, name, writeDir, naming)
		return fmt.Sprintf("Generated %d page(s) into collection %q.", done, name), err
	})
	w.WriteHeader(http.StatusAccepted)
}

// collectionJobPrologue validates a generation request's label, claims the
// job lane, and snapshots what the job runs against. SwitchGallery refuses
// swaps while a job runs, so the snapshot stays valid for its lifetime.
func (s *Server) collectionJobPrologue(w http.ResponseWriter, r *http.Request) (name string, cx *galleryCtx, writeDir string, naming gallery.Naming, ok bool) {
	if !parseFormOK(w, r) {
		return "", nil, "", naming, false
	}
	name = strings.TrimSpace(r.FormValue("collection"))
	if name == "" {
		flashStatus(w, http.StatusBadRequest, "Collection label required.")
		return "", nil, "", naming, false
	}
	if len(name) > maxExternalSourceLen {
		flashStatus(w, http.StatusBadRequest, "Collection label too long.")
		return "", nil, "", naming, false
	}
	if active := s.Active(); active == nil || active.Degraded {
		flashStatus(w, http.StatusServiceUnavailable, "Generation unavailable: gallery path is unreadable.")
		return "", nil, "", naming, false
	}
	if !s.startJob(w, models.JobTypeTag) {
		return "", nil, "", naming, false
	}
	cx = s.Active()
	writeDir, naming = s.receivedNaming(cx.Name)
	return name, cx, writeDir, naming, true
}

// goGenerationJob runs one generation in the claimed lane. A user cancel is
// checked before the error so it surfaces as a summary, not a failure.
func (s *Server) goGenerationJob(run func(ctx context.Context) (string, error)) {
	go func() {
		ctx := s.jobs.Context()
		summary, err := run(ctx)
		if ctx.Err() != nil {
			s.jobs.Complete("generation cancelled")
			return
		}
		if err != nil {
			s.jobs.Fail(err.Error())
			return
		}
		s.jobs.Complete(summary)
	}()
}

// runMangaCollection extracts every page of img into cx's gallery,
// filing each under the collection label, and returns the page count. A
// page that fails to extract aborts the job.
func (s *Server) runMangaCollection(ctx context.Context, cx *galleryCtx, img *models.Image, name, writeDir string, naming gallery.Naming) (int, error) {
	destDir, err := gallery.ResolveSubdir(cx.GalleryPath, writeDir)
	if err != nil {
		return 0, err
	}
	stem := strings.TrimSuffix(filepath.Base(img.CanonicalPath), filepath.Ext(img.CanonicalPath))
	// Each archive unpacks into its own {stem}-{hash} folder so a long
	// comic never floods the upload root; hash is a content-address prefix.
	sub := stem
	if h := shortHash(img.SHA256); h != "" {
		sub = stem + "-" + h
	}
	destDir = filepath.Join(destDir, sub)
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return 0, err
	}
	// Every page can fold onto a row the gallery already holds (generating
	// a collection out of the cbz those same images were packed into), in
	// which case each copy is removed again and nothing is left but the
	// directory. Remove only succeeds while it is empty, so a run that did
	// land pages keeps its folder.
	defer func() { _ = os.Remove(destDir) }()
	total := *img.PageCount
	s.jobs.Update(0, total, "generating…")
	done := 0
	filedDir := ""
	for n := 1; n <= total; n++ {
		if ctx.Err() != nil {
			return done, ctx.Err()
		}
		pageID, filed, err := s.extractMangaPageToGallery(cx, img, n, destDir, "p")
		if err != nil {
			return done, fmt.Errorf("page %d/%d: %w", n, total, err)
		}
		// The destination is resolved once, off the first page: the pages of
		// one archive belong in one folder, and a template carrying {id} or
		// a clock would scatter them otherwise.
		if filed && naming.Folder != nil {
			if filedDir == "" {
				rendered, folderErr := naming.FolderFor(cx.DB, pageID)
				if folderErr != nil {
					return done, fmt.Errorf("page %d/%d destination: %w", n, total, folderErr)
				}
				filedDir = path.Join(rendered, sub)
			}
			if _, moveErr := gallery.PlaceImage(cx.DB, cx.GalleryPath, pageID, &filedDir, nil); moveErr != nil {
				return done, fmt.Errorf("page %d/%d file: %w", n, total, moveErr)
			}
		}
		// Archive page order becomes the collection position.
		pos := n
		if err := gallery.AddCollectionMembership(cx.DB, pageID, name, &pos); err != nil {
			return done, fmt.Errorf("page %d/%d membership: %w", n, total, err)
		}
		done = n
		s.jobs.Update(done, total, "generating…")
	}
	cx.InvalidateCaches()
	return done, nil
}

// generateCollectionCBZ serves POST /collections/generate-cbz: the
// collection's members are packed into a cbz archive ingested as a new
// manga row. JobTypeTag is deliberate: the watcher suppresses ingests
// while a tag-type job runs (see Watcher.jobSuppressesIngest), so the
// mid-job archive is not double-ingested behind us.
func (s *Server) generateCollectionCBZ(w http.ResponseWriter, r *http.Request) {
	name, cx, writeDir, naming, ok := s.collectionJobPrologue(w, r)
	if !ok {
		return
	}
	s.goGenerationJob(func(ctx context.Context) (string, error) {
		res, err := s.runCollectionCBZ(ctx, cx, name, writeDir, naming)
		return res.summary(), err
	})
	w.WriteHeader(http.StatusAccepted)
}

// collectionCBZResult is what one generate-cbz run produced. Existing is
// set instead of Filename when the archive folded onto one the gallery
// already held, which is what regenerating an unchanged collection does.
type collectionCBZResult struct {
	Pages    int
	Skipped  int
	Filename string
	Existing string
}

// summary renders the run for the job status bar.
func (r collectionCBZResult) summary() string {
	if r.Existing != "" {
		return fmt.Sprintf("These %d page(s) are already packed as %s; nothing new was created.", r.Pages, r.Existing)
	}
	msg := fmt.Sprintf("Generated %d image(s) as %s.", r.Pages, r.Filename)
	if r.Skipped > 0 {
		msg += fmt.Sprintf(" %d member(s) skipped (missing files).", r.Skipped)
	}
	return msg
}

// runCollectionCBZ builds a cbz from name's members in reading order and
// ingests it as a manga row. An animated image, video or nested archive
// among the members denies the whole generation.
func (s *Server) runCollectionCBZ(ctx context.Context, cx *galleryCtx, name, writeDir string, naming gallery.Naming) (collectionCBZResult, error) {
	var res collectionCBZResult
	members, err := gallery.CollectionCBZMembers(cx.DB, name)
	if err != nil {
		return res, err
	}
	if len(members) == 0 {
		return res, fmt.Errorf("collection %q has no visible members", name)
	}
	destDir, err := gallery.ResolveSubdir(cx.GalleryPath, writeDir)
	if err != nil {
		return res, err
	}
	// UniqueDestPath resolves a name collision with an existing file, so
	// the returned path is the one actually created.
	dst := gallery.UniqueDestPath(destDir, collectionCBZFilename(name))
	res.Filename = filepath.Base(dst)
	res.Pages, res.Skipped, err = gallery.WriteCollectionCBZ(ctx, dst, members, name,
		func(processed, total int, message string) { s.jobs.Update(processed, total, message) })
	if err != nil {
		return collectionCBZResult{}, err
	}
	archive, isDup, err := gallery.Ingest(cx.DB, cx.GalleryPath, cx.ThumbnailsPath, dst, models.OriginGenerate)
	if err != nil {
		return collectionCBZResult{}, fmt.Errorf("ingest %q: %w", res.Filename, err)
	}
	// The same members in the same order pack byte for byte identically,
	// so regenerating an unchanged collection lands on the archive the
	// previous run produced. Unwind the redundant copy the way the page
	// extract does rather than leaving a file no row points at. A dup
	// whose canonical path is the new file is a reactivated archive the
	// operator had deleted, so that copy stays.
	if isDup && archive.CanonicalPath != dst {
		gallery.DropDuplicateCopy(cx.DB, archive.ID, dst, "generate cbz")
		res.Existing = filepath.Base(archive.CanonicalPath)
	} else if _, err := naming.Apply(cx.DB, cx.GalleryPath, archive.ID, "", ""); err != nil {
		logx.Warnf("generate cbz %q: file: %v", res.Filename, err)
	}
	cx.InvalidateCaches()
	return res, nil
}

// collectionCBZFilename maps a collection label onto the generated cbz
// filename. Collections have no id (they exist only through
// image_collections), so the leading token is a generation timestamp in
// the operator's local timezone (time.Local, driven by TZ), keeping the
// day aligned with what the user sees. It leads because a label can be
// long enough to be truncated by a filesystem, and it carries the time
// so the same collection repacked twice in a day still reads apart.
func collectionCBZFilename(name string) string {
	return fmt.Sprintf("%s-%s.cbz", time.Now().Format("20060102-150405"), sanitizeCollectionFilename(name))
}

// shortHash returns the first 8 hex chars of a sha256 (or all of it);
// used to build unique folder names for generated content.
func shortHash(hash string) string {
	return hash[:min(8, len(hash))]
}

// maxCBZStemBytes leaves room under the usual 255-byte filename limit
// for the timestamp prefix, the ".cbz" suffix and a collision counter.
const maxCBZStemBytes = 180

// sanitizeCollectionFilename maps a collection label onto a cbz filename
// stem, under the shared rule every templated name goes through.
func sanitizeCollectionFilename(name string) string {
	out := gallery.TruncateFilename(gallery.SanitizeFilename(name), maxCBZStemBytes)
	if out == "" {
		return "collection"
	}
	return out
}

// extractMangaPageToGallery turns the n-th page of archive img into its
// own image row in cx's gallery: the page bytes are copied from the
// per-image cache as <prefix><n padded to 4>.<ext> (prefix "manga_p" for
// the reader's extract button, "p" for the generate-collection job),
// ingested, and linked back to the archive by a derivative edge. Returns
// the image id and whether the bytes landed as a new row - a page that
// folded onto one the gallery already held is not this job's to file.
func (s *Server) extractMangaPageToGallery(cx *galleryCtx, img *models.Image, n int, destDir, prefix string) (int64, bool, error) {
	pagePath, err := gallery.EnsureMangaPage(cx.ThumbnailsPath, img.CanonicalPath, img.ID, n)
	if err != nil {
		return 0, false, err
	}
	dstPath := gallery.UniqueDestPath(destDir, fmt.Sprintf("%s%04d", prefix, n)+filepath.Ext(pagePath))
	if err := copyFileContents(pagePath, dstPath); err != nil {
		return 0, false, fmt.Errorf("copy page: %w", err)
	}
	if _, err := gallery.DetectFileType(dstPath); err != nil {
		_ = os.Remove(dstPath)
		return 0, false, fmt.Errorf("detect type: %w", err)
	}
	// No MaxFileSizeMB check: the bytes are already in the library.
	page, isDup, err := gallery.Ingest(cx.DB, cx.GalleryPath, cx.ThumbnailsPath, dstPath, models.OriginExtract)
	if err != nil {
		_ = os.Remove(dstPath)
		return 0, false, err
	}
	folded := isDup && page.CanonicalPath != dstPath
	if folded {
		// Same unwind as the upload drop zone: the bytes already live in
		// the gallery, so the fresh copy and its alias ingest are dead weight.
		gallery.DropDuplicateCopy(cx.DB, page.ID, dstPath, "extract")
	}
	if cx.RelationsSvc != nil {
		// A conflicting relation or existing source is a standing operator
		// decision; skip the link, the extract still stands.
		if err := cx.RelationsSvc.AddDerivativeEdge(img.ID, page.ID); err != nil {
			logx.Debugf("extract: link %d -> %d skipped: %v", img.ID, page.ID, err)
		}
	}
	return page.ID, !folded, nil
}
