package web

import (
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/logx"
	webFS "github.com/monbooru/monbooru/web"
)

// themeEntry is one theme: a folder holding theme.css and optionally
// logo.png, a bare <name>.css for the CSS-only form, or one of the two
// built into the binary. Path and Logo are disk paths for an installed
// theme and embedded-FS paths for a built-in. Path is empty for the shipped
// dark look, which is main.css itself rather than an override.
type themeEntry struct {
	Name    string
	Path    string
	Logo    string
	Builtin bool
}

// builtinLightDir holds the bundled light theme in the same folder shape an
// installed one takes, so seeding it is a copy of the folder.
const builtinLightDir = "static/themes/light"

// builtinThemes ship with the binary so both looks work before anything is
// installed. A theme of the same name on disk takes over (see listThemes),
// which is what makes the seeded copy of light editable.
var builtinThemes = []themeEntry{
	{Name: "dark", Builtin: true},
	{Name: "light", Builtin: true, Path: builtinLightDir + "/theme.css", Logo: builtinLightDir + "/logo.png"},
}

// themesDir is where operator-installed themes live, next to monbooru.toml.
func (s *Server) themesDir() string { return s.configSubdir("themes") }

// seedThemesDir writes the light theme into the config directory the first
// time monbooru starts, so the folder themes go in exists and carries a
// worked example: the whole package, logo included, not just the sheet.
// Only when the directory is absent: an operator who emptied it meant it.
func (s *Server) seedThemesDir() {
	dir := s.themesDir()
	if dir == "" {
		return
	}
	if _, err := os.Stat(dir); err == nil {
		return
	}
	if err := os.MkdirAll(filepath.Join(dir, "light"), 0o755); err != nil {
		logx.Warnf("themes: could not create %s: %v", dir, err)
		return
	}
	for _, name := range []string{"theme.css", "logo.png"} {
		body, err := webFS.FS.ReadFile(builtinLightDir + "/" + name)
		if err != nil {
			continue
		}
		if err := os.WriteFile(filepath.Join(dir, "light", name), body, 0o644); err != nil {
			logx.Warnf("themes: could not write the example theme: %v", err)
		}
	}
}

// listThemes returns the built-in themes plus whatever is installed, in
// both supported shapes. Built-ins come first; installed themes sort after
// and override a built-in of the same name. Among installed themes the
// folder form wins a collision, since it is the one that can carry a logo.
func (s *Server) listThemes() []themeEntry {
	byName := map[string]themeEntry{}
	for _, e := range builtinThemes {
		byName[e.Name] = e
	}
	dir := s.themesDir()
	if dir == "" {
		return orderThemes(byName)
	}
	items, err := os.ReadDir(dir)
	if err != nil {
		return orderThemes(byName)
	}
	for _, it := range items {
		if it.IsDir() {
			sheet := filepath.Join(dir, it.Name(), "theme.css")
			if _, err := os.Stat(sheet); err != nil {
				continue
			}
			e := themeEntry{Name: it.Name(), Path: sheet}
			logo := filepath.Join(dir, it.Name(), "logo.png")
			if _, err := os.Stat(logo); err == nil {
				e.Logo = logo
			}
			byName[e.Name] = e
			continue
		}
		if !strings.HasSuffix(it.Name(), ".css") {
			continue
		}
		name := strings.TrimSuffix(it.Name(), ".css")
		if prev, taken := byName[name]; taken && !prev.Builtin {
			logx.Warnf("themes: %q exists as both a folder and a .css file; the folder wins", name)
			continue
		}
		byName[name] = themeEntry{Name: name, Path: filepath.Join(dir, it.Name())}
	}
	return orderThemes(byName)
}

// orderThemes puts the built-ins first in their declared order, then every
// installed theme by name, so the picker reads dark, light, then yours.
func orderThemes(byName map[string]themeEntry) []themeEntry {
	out := make([]themeEntry, 0, len(byName))
	for _, b := range builtinThemes {
		if e, ok := byName[b.Name]; ok {
			out = append(out, e)
			delete(byName, b.Name)
		}
	}
	rest := slices.SortedFunc(maps.Values(byName), func(a, b themeEntry) int {
		return strings.Compare(a.Name, b.Name)
	})
	return append(out, rest...)
}

// activeTheme resolves server.theme against the listing. The stored value is
// a basename, never a path: anything that does not match a listed entry (a
// separator, a `..`, a theme that was deleted) falls back to the shipped
// dark look, whose entry carries no stylesheet because it is main.css.
func (s *Server) activeTheme() themeEntry {
	s.cfgMu.RLock()
	name := strings.TrimSpace(s.cfg.Server.Theme)
	s.cfgMu.RUnlock()
	if name == "" {
		return builtinThemes[0]
	}
	for _, e := range s.listThemes() {
		if e.Name == name {
			return e
		}
	}
	s.warnThemeOnce(name)
	return builtinThemes[0]
}

// warnThemeOnce reports an unresolvable theme name at most once per distinct
// value; resolution runs on every render, so an unconditional warn would
// flood the log.
func (s *Server) warnThemeOnce(name string) {
	s.themeWarnMu.Lock()
	defer s.themeWarnMu.Unlock()
	if s.themeWarned == name {
		return
	}
	s.themeWarned = name
	logx.Warnf("server.theme %q is not an installed or built-in theme; the shipped look is used", name)
}

func (s *Server) serveThemeCSS(w http.ResponseWriter, r *http.Request) {
	e := s.activeTheme()
	s.serveThemeFile(w, r, e, e.Path, "theme")
}

func (s *Server) serveThemeLogo(w http.ResponseWriter, r *http.Request) {
	e := s.activeTheme()
	s.serveThemeFile(w, r, e, e.Logo, "themelogo")
}

// serveThemeFile serves one file of the active theme: out of the binary for
// a built-in, off disk for an installed one, 404 when the theme carries no
// such file.
func (s *Server) serveThemeFile(w http.ResponseWriter, r *http.Request, e themeEntry, path, kind string) {
	if !e.Builtin {
		s.serveConfiguredFile(w, r, path, kind)
		return
	}
	if path == "" {
		http.NotFound(w, r)
		return
	}
	http.ServeFileFS(w, r, webFS.FS, path)
}

// themeCluster is the picker's view: every theme, with the active one marked.
type themeCluster struct {
	Names  []string
	Active string
}

func (s *Server) themeCluster() themeCluster {
	entries := s.listThemes()
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name)
	}
	return themeCluster{Names: names, Active: s.activeTheme().Name}
}

// settingsThemePost switches the active theme.
func (s *Server) settingsThemePost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	name := strings.TrimSpace(r.FormValue("theme"))
	if name != "" && !slices.ContainsFunc(s.listThemes(), func(e themeEntry) bool { return e.Name == name }) {
		flashStatus(w, http.StatusBadRequest, "No theme named "+name+".")
		return
	}
	if err := s.withConfig(func(c *config.Config) error {
		c.Server.Theme = name
		return nil
	}); err != nil {
		flashStatus(w, http.StatusInternalServerError, "Could not save: "+err.Error())
		return
	}
	logx.Infof("settings: theme set to %q", name)
	// The stylesheet link and the logo both change, so the page repaints.
	w.Header().Set("HX-Refresh", "true")
	w.WriteHeader(http.StatusNoContent)
}
