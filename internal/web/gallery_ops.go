package web

import (
	"errors"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/leqwin/monbooru/internal/config"
	"github.com/leqwin/monbooru/internal/logx"
)

var errJobRunning = errors.New("a job is running; try again when it finishes")

// SwitchGallery changes the runtime-active gallery. The change is ephemeral:
// the persisted default_gallery in monbooru.toml is only touched by
// SetDefault. Every gallery runs its own watcher for the whole process
// lifetime, so the swap does not stop/start watchers or trigger a sync.
func (s *Server) SwitchGallery(name string) error {
	if s.jobs.IsRunning() {
		return errJobRunning
	}
	s.ctxMu.Lock()
	if name == s.activeName {
		s.ctxMu.Unlock()
		return nil
	}
	if _, ok := s.contexts[name]; !ok {
		s.ctxMu.Unlock()
		return fmt.Errorf("unknown gallery %q", name)
	}
	oldName := s.activeName
	s.activeName = name
	s.ctxMu.Unlock()

	logx.Infof("gallery: switched from %q to %q", oldName, name)
	return nil
}

// SetDefault persists cfg.DefaultGallery so the given gallery loads on
// startup. Doesn't change the runtime-active gallery.
func (s *Server) SetDefault(name string) error {
	s.ctxMu.Lock()
	if _, ok := s.contexts[name]; !ok {
		s.ctxMu.Unlock()
		return fmt.Errorf("unknown gallery %q", name)
	}
	if s.cfg.DefaultGallery == name {
		s.ctxMu.Unlock()
		return nil
	}
	s.cfg.DefaultGallery = name
	s.ctxMu.Unlock()

	if err := s.saveConfig(); err != nil {
		return fmt.Errorf("persist default gallery: %w", err)
	}
	logx.Infof("gallery: default set to %q", name)
	return nil
}

// AddGallery opens a new gallery and appends it to the config. DB and
// thumbnails directories are created under paths.data_path/<name>/.
func (s *Server) AddGallery(name, galleryPath string) error {
	name = strings.TrimSpace(name)
	galleryPath = strings.TrimSpace(galleryPath)
	if err := config.ValidateGalleryName(name); err != nil {
		return err
	}
	if galleryPath == "" {
		return fmt.Errorf("gallery path must not be empty")
	}
	// Reject non-existent or unreadable paths at add time. Existing configs
	// that point at a temporarily-unreadable path still load (degraded mode);
	// this gate only applies to the explicit Add mutation so the user doesn't
	// walk away with a gallery that can never sync or watch.
	if _, err := os.ReadDir(galleryPath); err != nil {
		return fmt.Errorf("gallery path %q is not readable: %w", galleryPath, err)
	}
	if s.jobs.IsRunning() {
		return errJobRunning
	}

	s.ctxMu.Lock()
	if _, ok := s.contexts[name]; ok {
		s.ctxMu.Unlock()
		return fmt.Errorf("gallery %q already exists", name)
	}
	dbPath, thumbnailsPath := s.cfg.DerivePaths(name)
	if _, err := os.Stat(dbPath); err == nil {
		s.ctxMu.Unlock()
		return fmt.Errorf("data for gallery %q already exists at %q", name, filepath.Dir(dbPath))
	}
	g := config.Gallery{
		Name:           name,
		GalleryPath:    galleryPath,
		DBPath:         dbPath,
		ThumbnailsPath: thumbnailsPath,
	}
	cx, err := openGalleryCtx(g)
	if err != nil {
		s.ctxMu.Unlock()
		return err
	}
	s.contexts[name] = cx
	s.cfg.Galleries = append(s.cfg.Galleries, g)
	cx.startWatcher(s.cfg.Gallery.WatchEnabled, s.cfg.Gallery.MaxFileSizeMB, s.jobs)
	s.ctxMu.Unlock()

	if err := s.saveConfig(); err != nil {
		return fmt.Errorf("persist new gallery: %w", err)
	}
	logx.Infof("gallery: added %q (path=%q)", name, galleryPath)
	return nil
}

// RemoveGallery drops a gallery and deletes its DB + thumbnails on disk.
// When removeFolder is true, the gallery's source folder is also removed
// (best-effort). Refuses to remove the active, default, or last gallery.
func (s *Server) RemoveGallery(name string, removeFolder bool) error {
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
		return fmt.Errorf("cannot remove the active gallery; switch to another first")
	}
	if name == s.cfg.DefaultGallery {
		s.ctxMu.Unlock()
		return fmt.Errorf("cannot remove the default gallery; set another as default first")
	}
	if len(s.contexts) <= 1 {
		s.ctxMu.Unlock()
		return fmt.Errorf("cannot remove the last gallery")
	}

	galleryPath := cx.GalleryPath
	dataDir := filepath.Dir(cx.DBPath) // /<data_path>/<name>
	cx.close()
	delete(s.contexts, name)
	for i := range s.cfg.Galleries {
		if s.cfg.Galleries[i].Name == name {
			s.cfg.Galleries = append(s.cfg.Galleries[:i], s.cfg.Galleries[i+1:]...)
			break
		}
	}
	s.ctxMu.Unlock()

	if err := os.RemoveAll(dataDir); err != nil {
		logx.Warnf("remove gallery data dir %q: %v", dataDir, err)
	}
	if removeFolder {
		// Refuse to follow a symlink: the target sitting behind the
		// gallery_path config field could point anywhere, and os.RemoveAll
		// would happily wipe whatever directory the link resolves to.
		// On a LAN single-operator setup this guard is "foot-shot
		// prevention" rather than a security boundary, but the
		// destructive blast radius warrants it.
		if info, err := os.Lstat(galleryPath); err != nil {
			logx.Warnf("remove gallery folder %q: stat: %v", galleryPath, err)
		} else if info.Mode()&os.ModeSymlink != 0 {
			logx.Warnf("remove gallery folder %q: refusing to follow symlink", galleryPath)
		} else if err := os.RemoveAll(galleryPath); err != nil {
			logx.Warnf("remove gallery folder %q: %v", galleryPath, err)
		}
	}

	if err := s.saveConfig(); err != nil {
		return fmt.Errorf("persist gallery removal: %w", err)
	}
	logx.Infof("gallery: removed %q (folder removed=%t)", name, removeFolder)
	return nil
}

// RenameGallery moves the in-memory key and rewrites the TOML. The data
// directory is also renamed so the derived paths stay consistent.
func (s *Server) RenameGallery(oldName, newName string) error {
	oldName = strings.TrimSpace(oldName)
	newName = strings.TrimSpace(newName)
	if oldName == newName {
		return nil
	}
	if err := config.ValidateGalleryName(newName); err != nil {
		return err
	}
	if s.jobs.IsRunning() {
		return errJobRunning
	}
	s.ctxMu.Lock()
	cx, ok := s.contexts[oldName]
	if !ok {
		s.ctxMu.Unlock()
		return fmt.Errorf("unknown gallery %q", oldName)
	}
	if _, exists := s.contexts[newName]; exists {
		s.ctxMu.Unlock()
		return fmt.Errorf("gallery %q already exists", newName)
	}
	// Close the DB, rename the data dir on disk, then reopen under the new
	// name so derived paths stay consistent.
	newDB, newThumbs := s.cfg.DerivePaths(newName)
	newDir := filepath.Dir(newDB)
	if _, err := os.Stat(newDir); err == nil {
		s.ctxMu.Unlock()
		return fmt.Errorf("data dir %q already exists", newDir)
	}
	cx.close()
	oldDir := filepath.Dir(cx.DBPath)
	if err := os.Rename(oldDir, newDir); err != nil && !os.IsNotExist(err) {
		// Reopen the old ctx so we don't leave the gallery unusable on failure.
		if reopened, reopenErr := openGalleryCtx(config.Gallery{
			Name: oldName, GalleryPath: cx.GalleryPath, DBPath: cx.DBPath, ThumbnailsPath: cx.ThumbnailsPath,
		}); reopenErr == nil {
			s.contexts[oldName] = reopened
		}
		s.ctxMu.Unlock()
		return fmt.Errorf("rename data dir %q -> %q: %w", oldDir, newDir, err)
	}
	newCx, err := openGalleryCtx(config.Gallery{
		Name: newName, GalleryPath: cx.GalleryPath, DBPath: newDB, ThumbnailsPath: newThumbs,
	})
	if err != nil {
		s.ctxMu.Unlock()
		return err
	}
	delete(s.contexts, oldName)
	s.contexts[newName] = newCx
	for i := range s.cfg.Galleries {
		if s.cfg.Galleries[i].Name == oldName {
			s.cfg.Galleries[i].Name = newName
			s.cfg.Galleries[i].DBPath = newDB
			s.cfg.Galleries[i].ThumbnailsPath = newThumbs
			break
		}
	}
	if s.activeName == oldName {
		s.activeName = newName
	}
	if s.cfg.DefaultGallery == oldName {
		s.cfg.DefaultGallery = newName
	}
	newCx.startWatcher(s.cfg.Gallery.WatchEnabled, s.cfg.Gallery.MaxFileSizeMB, s.jobs)
	s.ctxMu.Unlock()

	if err := s.saveConfig(); err != nil {
		return fmt.Errorf("persist gallery rename: %w", err)
	}
	logx.Infof("gallery: renamed %q to %q", oldName, newName)
	return nil
}

// galleryList returns a name-sorted copy for the Settings table.
func (s *Server) galleryList() []config.Gallery {
	out := make([]config.Gallery, len(s.cfg.Galleries))
	copy(out, s.cfg.Galleries)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func writeFlash(rw http.ResponseWriter, class, msg string) {
	rw.Write([]byte(`<div class="flash flash-` + class + `">` + html.EscapeString(msg) + `</div>`))
}

// gallerySwitchHandler handles POST /internal/gallery/switch. Errors render
// as an inline flash inside the topbar dialog; success triggers HX-Refresh.
func (s *Server) gallerySwitchHandler(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	name := strings.TrimSpace(r.FormValue("name"))
	if err := s.SwitchGallery(name); err != nil {
		if isHTMXRequest(r) {
			writeFlash(w, "err", err.Error())
			return
		}
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if isHTMXRequest(r) {
		// Detail URLs are image-id-scoped and usually 404 in the target gallery,
		// and gallery URLs carrying a search/sort/folder query belong to the
		// old gallery's namespace - re-running them in the new one just
		// surfaces unrelated results. Send the browser home in both cases
		// instead of refreshing the current URL.
		if cur := r.Header.Get("HX-Current-URL"); cur != "" {
			if u, err := url.Parse(cur); err == nil {
				if strings.HasPrefix(u.Path, "/images/") || (u.Path == "/" && u.RawQuery != "") {
					w.Header().Set("HX-Redirect", "/")
					w.WriteHeader(http.StatusOK)
					return
				}
			}
		}
		w.Header().Set("HX-Refresh", "true")
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// settingsGallery*Post handlers write a flash on error and fire HX-Refresh on
// success. The refresh reloads the whole page, so a success-path flash would
// never render; the page itself is the confirmation.
func (s *Server) settingsGalleriesPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	name := r.FormValue("name")
	path := r.FormValue("gallery_path")
	if err := s.AddGallery(name, path); err != nil {
		writeFlash(w, "err", err.Error())
		return
	}
	w.Header().Set("HX-Refresh", "true")
}

func (s *Server) settingsGalleryRenamePost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	oldName := r.PathValue("name")
	newName := r.FormValue("new_name")
	if err := s.RenameGallery(oldName, newName); err != nil {
		writeFlash(w, "err", err.Error())
		return
	}
	w.Header().Set("HX-Refresh", "true")
}

func (s *Server) settingsGalleryDeletePost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	name := r.PathValue("name")
	confirm := strings.TrimSpace(r.FormValue("confirm_name"))
	if confirm != name {
		writeFlash(w, "err", "type-to-confirm name does not match")
		return
	}
	removeFolder := r.FormValue("remove_folder") == "on"
	if err := s.RemoveGallery(name, removeFolder); err != nil {
		writeFlash(w, "err", err.Error())
		return
	}
	w.Header().Set("HX-Refresh", "true")
}

func (s *Server) settingsGalleryDefaultPost(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if err := s.SetDefault(name); err != nil {
		writeFlash(w, "err", err.Error())
		return
	}
	w.Header().Set("HX-Refresh", "true")
}
