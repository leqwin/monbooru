package web

import (
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/desktop"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/tagger"
)

// desktopLocal gates the controls that reach the machine itself rather than
// the library: the directory picker, the folder opener and Quit. The profile
// keeps a server install out entirely - it is a launch flag, not something a
// request can set - and the remote address keeps a LAN viewer out. The bind
// address is deliberately not a condition: the wizard offers the network as
// a choice, and gating on it would take Restart away from the operator at
// the moment the choice needs one.
//
// A forwarded hop is refused whatever the peer says: a same-host proxy
// makes every request look loopback, which would otherwise hand a LAN
// viewer a filesystem browser and a Quit button.
//
// A refusal is a 404 rather than a 403 so the endpoints do not advertise
// themselves where they are switched off.
func (s *Server) desktopLocal(r *http.Request) bool {
	if !s.desktop {
		return false
	}
	if r.Header.Get("X-Forwarded-For") != "" || r.Header.Get("Forwarded") != "" {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// maxBrowseEntries bounds one listing. A directory holding tens of
// thousands of subfolders is a page nobody can read and a render nobody
// waits for; the operator types the path in that case.
const maxBrowseEntries = 500

// browseIntoRe gates the id the chosen path is written back into. It ends
// up in an attribute the picker's script reads, so it stays an identifier.
var browseIntoRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// browseEntry is one row of the picker: a directory to descend into, or a
// drive root on Windows.
type browseEntry struct {
	Name string
	Path string
}

type browseData struct {
	Into    string
	Path    string
	Parent  string
	Entries []browseEntry
	Roots   []browseEntry
	// Sandboxed changes the empty state: under Flatpak a folder the user
	// knows exists can simply be invisible, which reads as a bug unless
	// the page says who grants it.
	Sandboxed bool
	Err       string
	Truncated bool
	Limit     int
}

// browseDirs lists the subdirectories of path so a browser can hand the
// server a filesystem location, which it otherwise cannot do. Directories
// only, never files; an unreadable entry is skipped rather than failing the
// whole listing.
func (s *Server) browseDirs(w http.ResponseWriter, r *http.Request) {
	if !s.desktopLocal(r) {
		http.NotFound(w, r)
		return
	}
	into := r.URL.Query().Get("into")
	if !browseIntoRe.MatchString(into) {
		http.Error(w, "bad target", http.StatusBadRequest)
		return
	}
	data := browseData{Into: into, Sandboxed: desktop.Sandboxed(), Limit: maxBrowseEntries}
	for _, root := range desktop.Roots() {
		data.Roots = append(data.Roots, browseEntry{Name: root, Path: root})
	}

	path := strings.TrimSpace(r.URL.Query().Get("path"))
	if path == "" {
		path, _ = os.UserHomeDir()
	}
	if path == "" {
		path = string(filepath.Separator)
	}
	data.Path = filepath.Clean(path)
	if parent := filepath.Dir(data.Path); parent != data.Path {
		data.Parent = parent
	}

	entries, err := os.ReadDir(data.Path)
	if err != nil {
		data.Err = "cannot read this folder"
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			continue
		}
		full := filepath.Join(data.Path, e.Name())
		// Stat rather than trusting the entry type so a symlinked folder
		// still shows up as one.
		fi, err := os.Stat(full)
		if err != nil || !fi.IsDir() {
			continue
		}
		data.Entries = append(data.Entries, browseEntry{Name: e.Name(), Path: full})
	}
	// ReadDir already sorts by byte order, which puts every capitalised
	// folder above every lowercase one; a picker reads better folded. Sort
	// before the cap, or the listing is an arbitrary slice of the folder
	// rather than its alphabetical head.
	slices.SortFunc(data.Entries, func(a, b browseEntry) int {
		return strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
	})
	if len(data.Entries) > maxBrowseEntries {
		data.Entries = data.Entries[:maxBrowseEntries]
		data.Truncated = true
	}
	s.renderTemplate(w, "partials/dir_picker.html", data)
}

// openFolderKinds are the folders the opener will show. The request names a
// kind, never a path: every folder worth opening is one the server can name
// itself, so putting an operator string into a launcher argument buys
// nothing.
var openFolderKinds = []string{"config", "data", "gallery", "logs", "models"}

// openFolder shows one of monbooru's own directories in the desktop's file
// manager, creating it first when it is one monbooru owns.
func (s *Server) openFolder(w http.ResponseWriter, r *http.Request) {
	if !s.desktopLocal(r) {
		http.NotFound(w, r)
		return
	}
	if !parseFormOK(w, r) {
		return
	}
	kind := r.FormValue("kind")
	if !slices.Contains(openFolderKinds, kind) {
		flashStatus(w, http.StatusBadRequest, "Unknown folder.")
		return
	}
	dir, ours := s.desktopFolder(kind)
	// The one sub-folder a request may name is a tagger's, and it names the
	// tagger rather than the path: the name is checked against the same
	// allowlist the folder itself has to match, so nothing operator-supplied
	// reaches the launcher.
	if kind == "models" && dir != "" {
		name := r.FormValue("name")
		if name != "" {
			if err := tagger.ValidateTaggerName(name); err != nil {
				flashStatus(w, http.StatusBadRequest, err.Error())
				return
			}
			dir = filepath.Join(dir, name)
		}
	}
	if dir == "" {
		flashStatus(w, http.StatusBadRequest, "That folder is not configured.")
		return
	}
	if ours {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			flashStatus(w, http.StatusInternalServerError, "Could not create "+dir+": "+err.Error())
			return
		}
	}
	if err := s.folderOpener(dir); err != nil {
		logx.Warnf("open-folder %s: %v", kind, err)
		flashStatus(w, http.StatusInternalServerError, "Could not open "+dir+".")
		return
	}
	writeInlineFlash(w, "ok", "Opened "+dir+".")
}

// desktopFolder maps a kind to a directory and reports whether monbooru
// owns it, which is what decides between creating it and only opening it.
// The gallery is the operator's: an empty one created behind their back
// would be a second library nobody asked for.
func (s *Server) desktopFolder(kind string) (string, bool) {
	s.cfgMu.RLock()
	dataPath := s.cfg.Paths.DataPath
	modelPath := s.cfg.Paths.ModelPath
	s.cfgMu.RUnlock()
	switch kind {
	case "config":
		return filepath.Dir(s.configPath), true
	case "data":
		return dataPath, true
	case "logs":
		return s.logDir, true
	case "models":
		return modelPath, true
	case "gallery":
		if cx := s.Active(); cx != nil {
			return cx.GalleryPath, false
		}
	}
	return "", false
}

// availableFolders drops the kinds this install has nowhere to point at, so
// the row never offers a button that can only refuse.
func (s *Server) availableFolders() []string {
	out := make([]string, 0, len(openFolderKinds))
	for _, kind := range openFolderKinds {
		if dir, _ := s.desktopFolder(kind); dir != "" {
			out = append(out, kind)
		}
	}
	return out
}

// DesktopHook is desktopHook for the command, which builds the tray menu
// from the same identity the Settings controls use.
func (s *Server) DesktopHook() desktop.Hook { return s.desktopHook() }

// desktopHook describes this install to the platform's integration points.
// The icon is appicon.png, the mascot on monbooru's accent plate: it is
// square at a size the icon theme declares, and the plate is what tells the
// two apps apart in a launcher. A theme's logo is a browsing decoration,
// not the identity a launcher should show.
func (s *Server) desktopHook() desktop.Hook {
	icon, _ := fs.ReadFile(s.staticFS, "appicon.png")
	return desktop.Hook{
		App:     "monbooru",
		Name:    s.booruName(),
		Comment: "Lightweight and fast private booru",
		Icon:    icon,
	}
}

// desktopIntegration is what the Settings controls render from. The menu
// and autostart states are read back from disk on every load so a file
// removed by hand shows as off; the tray is a config key, since a tray icon
// leaves nothing behind to read.
type desktopIntegration struct {
	MenuSupported      bool
	MenuEnabled        bool
	AutostartSupported bool
	AutostartEnabled   bool
	TraySupported      bool
	TrayEnabled        bool
}

// Supported reports whether either switch can be written here, which is
// what decides whether a screen offering them is worth rendering at all.
func (d desktopIntegration) Supported() bool { return d.MenuSupported || d.AutostartSupported }

func (s *Server) desktopIntegration() desktopIntegration {
	h := s.desktopHook()
	return desktopIntegration{
		MenuSupported:      h.MenuSupported(),
		MenuEnabled:        h.MenuEnabled(),
		AutostartSupported: h.AutostartSupported(),
		AutostartEnabled:   h.AutostartEnabled(),
		TraySupported:      desktop.TrayAvailable(),
		TrayEnabled:        s.TrayEnabled(),
	}
}

// TrayEnabled is the tray switch, read by the command at startup and by the
// Settings render.
func (s *Server) TrayEnabled() bool {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return s.cfg.Desktop.Tray
}

// settingsDesktopPost applies the integration switches. The menu entry and
// start-at-login are files (or, on Windows, a registry value) rather than
// config keys, so the form posts what it wants and the answer is re-read
// from disk.
func (s *Server) settingsDesktopPost(w http.ResponseWriter, r *http.Request) {
	if !s.desktopLocal(r) {
		http.NotFound(w, r)
		return
	}
	if !parseFormOK(w, r) {
		return
	}
	h := s.desktopHook()
	var failures []string
	if h.MenuSupported() {
		if err := h.SetMenu(r.FormValue("menu_entry") == "on"); err != nil {
			failures = append(failures, "menu entry: "+err.Error())
		}
	}
	if h.AutostartSupported() {
		if err := h.SetAutostart(r.FormValue("start_at_login") == "on"); err != nil {
			failures = append(failures, "start at login: "+err.Error())
		}
	}
	if desktop.TrayAvailable() {
		tray := r.FormValue("tray") == "on"
		if err := s.withConfig(func(c *config.Config) error {
			c.Desktop.Tray = tray
			return nil
		}); err != nil {
			failures = append(failures, "tray icon: "+err.Error())
		}
	}
	if len(failures) > 0 {
		flashStatus(w, http.StatusInternalServerError, strings.Join(failures, "; "))
		return
	}
	writeInlineFlash(w, "ok", "Saved.")
}

// desktopJobWarning is the danger line Stop and Restart carry. What either
// actually costs is the work in flight, and nothing here resumes on the
// next start, so the line names the job rather than warning in the
// abstract. Empty while nothing is running, which leaves both controls
// asking a plain question.
func (s *Server) desktopJobWarning() string {
	st := s.jobs.Get()
	if st == nil || !st.Running {
		return ""
	}
	return runningJobName(st.JobType) + " is running and will not resume."
}

// quitDelay lets the response reach the browser before the listener goes
// away. Long enough for a loopback write, short enough that the click
// feels like it did something.
const quitDelay = 300 * time.Millisecond

// settingsQuit stops the process from the UI, for the case where the tray
// did not appear and a task manager is the only other route.
func (s *Server) settingsQuit(w http.ResponseWriter, r *http.Request) {
	s.stopAfterRender(w, r, map[string]any{
		"Heading": s.booruName() + " has stopped",
		"Hint":    "You can close this tab. Start it again from the applications menu.",
	}, "quit", s.RequestQuit)
}

// settingsRestart stops the process and has the command start it again, for
// the settings that only take effect at boot - the bind address and the tray.
func (s *Server) settingsRestart(w http.ResponseWriter, r *http.Request) {
	s.stopAfterRender(w, r, map[string]any{
		"Heading": s.booruName() + " is restarting",
		"Hint":    "This page will reload automatically.",
		"Poll":    true,
	}, "restart", s.RequestRestart)
}

// stopAfterRender answers with the message page, gets it onto the wire,
// and only then asks the command to stop - the listener is about to go
// away, so the browser has to have the page first.
func (s *Server) stopAfterRender(w http.ResponseWriter, r *http.Request, page map[string]any, verb string, then func()) {
	if !s.desktopLocal(r) {
		http.NotFound(w, r)
		return
	}
	heading, _ := page["Heading"].(string)
	s.renderTemplate(w, "message_page.html", s.standalonePageData(heading, page))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	logx.Infof("%s requested from the UI", verb)
	go func() {
		time.Sleep(quitDelay)
		then()
	}()
}

// RequestQuit asks the command to shut down, for the tray's Quit entry and
// for the settings control once its page has reached the browser.
func (s *Server) RequestQuit() {
	s.quitOnce.Do(func() { close(s.quit) })
}

// RequestRestart stops the process and asks the command to start it again,
// for the settings that only take effect at boot.
func (s *Server) RequestRestart() {
	s.restart.Store(true)
	s.RequestQuit()
}
