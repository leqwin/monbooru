package web

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/desktop"
	"github.com/monbooru/monbooru/internal/logx"
)

// setupData drives setup.html. GalleryPath and LAN are the current state
// read from the config; Menu and Autostart are the offers a first render
// makes. All four are echoed back from the form when a submit has to be
// shown again.
type setupData struct {
	BooruName string
	CSRFToken string
	// The rest of what partials/head.html reads: the wizard is the first
	// page a desktop install shows, so it carries the operator's branding
	// like every other page.
	Title        string
	BooruFavicon string
	Theme        bool
	CustomCSS    bool
	Err          string
	GalleryPath  string
	LAN          bool
	Menu         bool
	Autostart    bool
	Port         string
	// Integration drives the offer to add the app to the applications menu
	// and to start at login, which is the one thing a tarball install lacks.
	Integration desktopIntegration
}

// setupPending reports whether the first-run wizard still owes the operator
// a pass. Only the desktop profile has one: a container or a server install
// was configured by whoever deployed it.
func (s *Server) setupPending() bool {
	if !s.desktop {
		return false
	}
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return !s.cfg.SetupDone
}

// SetupMiddleware sends every page to the wizard until it has been through
// once. The exemptions are what the wizard itself needs plus the health
// probe, which the single-instance check and the container healthcheck both
// depend on answering at all times.
func (s *Server) SetupMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.setupPending() || setupExempt(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		if isHTMXRequest(r) {
			w.Header().Set("HX-Redirect", "/setup")
			w.WriteHeader(http.StatusOK)
			return
		}
		// An API client cannot follow a browser redirect to a wizard: a
		// 303 lands it on an HTML page with a 200, which reads as an
		// answer. It gets the refusal in the envelope it parses.
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{
				"error": "monbooru has not been set up yet",
				"code":  "setup_pending",
			})
			return
		}
		http.Redirect(w, r, "/setup", http.StatusSeeOther)
	})
}

func setupExempt(path string) bool {
	switch path {
	case "/health", "/setup", "/internal/browse":
		return true
	}
	// The login form and the assets it pulls: with auth on and the wizard
	// still owed, the session gate sends /setup to /login and this one
	// would send /login back, leaving neither page reachable.
	return isStaticPath(path) || isPublicPath(path)
}

func (s *Server) setupPage(w http.ResponseWriter, r *http.Request) {
	if !s.desktop {
		http.NotFound(w, r)
		return
	}
	s.cfgMu.RLock()
	galleryPath := ""
	if len(s.cfg.Galleries) > 0 {
		galleryPath = s.cfg.Galleries[0].GalleryPath
	}
	bind := s.cfg.Server.BindAddress
	s.cfgMu.RUnlock()
	s.renderSetup(w, r, setupData{
		GalleryPath: galleryPath,
		LAN:         !desktop.IsLoopbackAddr(bind),
		// Both offers are worth making by default: a desktop install that
		// sits in the launcher and comes back at login is what the profile
		// is for.
		Menu:      true,
		Autostart: true,
	})
}

func (s *Server) renderSetup(w http.ResponseWriter, r *http.Request, d setupData) {
	s.cfgMu.RLock()
	bind := s.cfg.Server.BindAddress
	s.cfgMu.RUnlock()
	_, port, err := net.SplitHostPort(bind)
	if err != nil {
		port = "8455"
	}
	d.BooruName = s.booruName()
	d.Title = d.BooruName + " setup"
	d.CSRFToken = s.csrfToken(sessionFromContext(r.Context()))
	d.BooruFavicon = s.booruFaviconURL()
	d.Theme = s.activeTheme().Path != ""
	d.CustomCSS = s.customCSSPath() != ""
	d.Port = port
	d.Integration = s.desktopIntegration()
	s.renderTemplate(w, "setup.html", d)
}

// setupPost is the wizard's only submit. The gallery folder goes first
// because it is the one input that can fail and every handler assumes an
// active gallery; the flag is recorded with the address, so a submit that
// never reaches the end leaves the gate up and the tour re-runnable.
func (s *Server) setupPost(w http.ResponseWriter, r *http.Request) {
	if !s.desktop {
		http.NotFound(w, r)
		return
	}
	if !parseFormOK(w, r) {
		return
	}
	form := setupData{
		GalleryPath: strings.TrimSpace(r.FormValue("gallery_path")),
		LAN:         r.FormValue("reach") == "lan",
		Menu:        r.FormValue("menu_entry") == "on",
		Autostart:   r.FormValue("start_at_login") == "on",
	}
	if err := s.RepointGallery(s.defaultGallery(), form.GalleryPath); err != nil {
		form.Err = err.Error()
		s.renderSetup(w, r, form)
		return
	}
	err := s.withConfig(func(c *config.Config) error {
		_, port, splitErr := net.SplitHostPort(c.Server.BindAddress)
		if splitErr != nil {
			return fmt.Errorf("server.bind_address %q is not a host:port", c.Server.BindAddress)
		}
		host := "127.0.0.1"
		if form.LAN {
			host = "0.0.0.0"
		}
		c.Server.BindAddress = net.JoinHostPort(host, port)
		c.SetupDone = true
		return nil
	})
	if err != nil {
		form.Err = "Could not save: " + err.Error()
		s.renderSetup(w, r, form)
		return
	}
	h := s.desktopHook()
	if h.MenuSupported() {
		if err := h.SetMenu(form.Menu); err != nil {
			logx.Warnf("setup: could not write the menu entry: %v", err)
		}
	}
	if h.AutostartSupported() {
		if err := h.SetAutostart(form.Autostart); err != nil {
			logx.Warnf("setup: could not write start at login: %v", err)
		}
	}
	logx.Infof("setup: done, reachable from %s", map[bool]string{true: "the network", false: "this computer"}[form.LAN])
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
