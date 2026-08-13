package web

import (
	"cmp"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/logx"
)

// pluginRowView is one peer as Settings > Plugins renders it. Companion marks
// monloader, which keeps its own config keys and carries the two URL fields
// inline.
type pluginRowView struct {
	Name      string
	Version   string
	Address   string
	Conn      string // ok | down | paused | checking
	Buttons   []config.PluginButton
	TokenName string
	Scopes    []string
	Paused    bool
	Paired    bool
	Companion bool
	APIURL    string // companion only
	WebURL    string
	// Command and RunState are set on a managed plugin: the launch line
	// declared on disk, shown read-only, and what its process is doing.
	Command  string
	RunState string
	// Installed marks a plugin discovered under the plugins folder, whose
	// Enable button confirms the launch line the operator never typed;
	// Enabled is the choice it persists.
	Installed bool
	Enabled   bool
	// SelfManaged marks a paired peer monbooru only talks to: its process is
	// the operator's own business (Docker, systemd, another host).
	SelfManaged bool
}

// pluginRows lists every installed peer for the settings section: monloader
// first as the built-in companion, then managed plugins, then self-managed
// paired peers, each group in name order.
func (s *Server) pluginRows() []pluginRowView {
	var managed, paired []pluginRowView
	for _, p := range s.effectivePlugins() {
		// A block with nothing to show - no pairing, no folder, no address
		// override - is residue, typically a folder removed after being
		// enabled. Its lines survive in the file but it earns no row.
		if p.PeerToken == "" && !p.Installed && p.APIURL == "" {
			continue
		}
		row := pluginRowView{
			Name:      p.Name,
			Version:   s.pluginVersion(p.PluginConfig),
			Address:   s.pluginAddress(p.PluginConfig),
			Buttons:   p.Buttons,
			Paused:    p.Paused,
			Installed: p.Installed,
			Enabled:   p.Enabled,
		}
		if p.PeerToken != "" {
			row.Conn = peerConn(p.Paused, s.pluginProbeSeed(p.Name).conn)
			row.TokenName, row.Scopes = s.pairedTokenInfo(p.Name)
			row.Paired = true
		}
		if p.Installed {
			row.Command = commandLine(p)
			row.RunState = s.managedState(p.Name)
			managed = append(managed, row)
			continue
		}
		row.SelfManaged = true
		paired = append(paired, row)
	}
	byName := func(a, b pluginRowView) int { return strings.Compare(a.Name, b.Name) }
	slices.SortFunc(managed, byName)
	slices.SortFunc(paired, byName)

	// The companion row renders whether or not a pairing exists: its api url
	// is an optional override an operator may want to set before pairing.
	rows := make([]pluginRowView, 0, len(managed)+len(paired)+1)
	rows = append(rows, s.monloaderRow())
	rows = append(rows, managed...)
	return append(rows, paired...)
}

// monloaderRow is the companion's row: the footer light's state plus the two
// URLs the old Monloader section carried.
func (s *Server) monloaderRow() pluginRowView {
	conn, version, _, _, _ := s.monloaderStatusSeed()
	s.cfgMu.RLock()
	apiURL, webURL := s.cfg.Monloader.APIURL, s.cfg.Server.MonloaderURL
	paused := s.cfg.Monloader.Paused
	s.cfgMu.RUnlock()
	name, scopes := s.pairedTokenInfo(monloaderApp)
	return pluginRowView{
		Name:      monloaderApp,
		Version:   version,
		Address:   s.monloaderAPIBase(),
		Conn:      peerConn(paused, conn),
		TokenName: name,
		Scopes:    scopes,
		Paused:    paused,
		Paired:    s.pairedWith(monloaderApp),
		Companion: true,
		APIURL:    apiURL,
		WebURL:    webURL,
	}
}

// commandLine renders a managed plugin's launch line as its manifest names
// it, for the read-only row.
func commandLine(p effectivePlugin) string {
	return strings.TrimSpace(p.Command + " " + strings.Join(p.Args, " "))
}

// peerConn maps a pause flag and a cached probe onto the dot the row shows.
// A cold cache reads as checking, the same optimistic state the footer light
// starts in.
func peerConn(paused bool, probe string) string {
	if paused {
		return "paused"
	}
	return cmp.Or(probe, "checking")
}

// pairedTokenInfo returns the name and scopes of the token issued to a peer.
func (s *Server) pairedTokenInfo(app string) (string, []string) {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	if t := s.cfg.FindPairedToken(app); t != nil {
		return t.Name, t.Scopes
	}
	return "", nil
}

// pluginVersion is what a row shows: the last health probe's answer when the
// peer reports one, else the snapshot taken at pairing.
func (s *Server) pluginVersion(p config.PluginConfig) string {
	return cmp.Or(s.pluginProbeSeed(p.Name).version, p.Version)
}

// pairedPeerCount is the signal the pending-request poll compares against, so
// a completed or removed pairing refreshes the rows without a page reload.
func (s *Server) pairedPeerCount() int {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	n := 0
	for _, t := range s.cfg.Auth.Tokens {
		if t.Paired != "" {
			n++
		}
	}
	return n
}

func (s *Server) pairViewData(r *http.Request) map[string]any {
	return map[string]any{
		"Pending":   s.pairs.listPending(),
		"Paired":    s.pairedPeerCount(),
		"CSRFToken": s.csrfToken(sessionFromContext(r.Context())),
	}
}

// pluginPairingFragment serves the pending-request panel's poll. The poll
// carries the browser's last-rendered pairing count; when it moves (a peer
// claimed, or a pairing was removed) the rows and the token list need a
// refresh too.
func (s *Server) pluginPairingFragment(w http.ResponseWriter, r *http.Request) {
	data := s.pairViewData(r)
	count, _ := data["Paired"].(int)
	s.renderTemplate(w, "partials/plugin_pairing.html", data)
	if was := r.URL.Query().Get("paired"); was != "" && was != strconv.Itoa(count) {
		s.renderPluginRows(w, r, true)
		s.renderAuthTokensOOB(w, r)
	}
}

func (s *Server) pluginPairDeny(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	s.pairs.setState(r.PathValue("id"), pairDenied)
	s.renderTemplate(w, "partials/plugin_pairing.html", s.pairViewData(r))
}

// pluginPairRemove tears down one peer's pairing from its settings row: both
// halves locally, plus a best-effort call to the peer so one click removes it
// at the far end too.
func (s *Server) pluginPairRemove(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	name := r.PathValue("name")
	var notifyErr error
	if name == monloaderApp {
		notifyErr = s.teardownMonloaderPairing(r)
	} else {
		notifyErr = s.teardownPluginPairing(name)
		logx.Infof("pairing: removed %s pairing from %s", name, clientIP(r))
		if p, ok := s.effective(name); ok && p.Installed {
			// Remove is offered on a managed row only while it is enabled, so
			// the teardown above reached a living process. Stopping it is what
			// the removal means: a plugin left running would offer to pair
			// again a moment later and undo the click. Enable is then the
			// operator's own decision to start over, and the fresh offer comes
			// with it.
			if p.Enabled {
				s.setPluginEnabled(name, false)
				s.stopManaged(name)
				s.markPluginDown(name)
				s.pairs.dropPending(name)
			}
			// And a folder has no far end to go and clean up by hand, so a
			// peer that refused the call is a log line rather than an
			// instruction the operator cannot follow. The token monbooru
			// issued is revoked either way; a copy the plugin kept on disk
			// authenticates to nothing.
			notifyErr = nil
		}
	}
	s.renderPluginRows(w, r, false)
	s.renderAuthTokensOOB(w, r)
	if notifyErr != nil {
		writeFlashOOB(w, "flash-plugins", "warn", "Removed here, but could not reach "+name+" - remove the pairing there too.")
	}
}

// pluginPause suspends or resumes a peer without dropping its pairing, the
// settings-side twin of the footer light's kill switch.
func (s *Server) pluginPause(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	name := r.PathValue("name")
	paused := r.FormValue("paused") == "1"
	if err := s.setPluginPaused(name, paused); err != nil {
		logx.Errorf("plugins: pause %s: %v", name, err)
	}
	logx.Infof("plugins: %s %s from %s", name, map[bool]string{true: "paused", false: "resumed"}[paused], clientIP(r))
	s.renderPluginRows(w, r, false)
}

// pluginStart and pluginStop drive a dropped plugin's process from its row,
// and are the only levers those rows carry: Pause belongs to a peer whose
// process monbooru does not own. The click also persists as Enabled on the
// block, so it is the standing choice the next boot reads and not just this
// run's state - which is why the row reads Enable / Disable.
//
// Both ends drop the cached probe with the process, so the row's dot and the
// peer's buttons follow the click instead of the next 30 s refresh.
func (s *Server) pluginStart(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	if p, ok := s.effective(r.PathValue("name")); ok && p.Installed {
		s.setPluginEnabled(p.Name, true)
		s.startManaged(p)
		s.clearPluginProbe(p.Name)
		logx.Infof("plugins: started %s from %s", p.Name, clientIP(r))
	}
	s.renderPluginRows(w, r, false)
}

func (s *Server) pluginStop(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	name := r.PathValue("name")
	if p, ok := s.effective(name); ok && p.Installed {
		s.setPluginEnabled(name, false)
	}
	s.stopManaged(name)
	s.markPluginDown(name)
	logx.Infof("plugins: stopped %s from %s", name, clientIP(r))
	s.renderPluginRows(w, r, false)
}

// setPluginEnabled records a discovered plugin's boot-start choice, writing
// the block when the folder has not paired yet and so has none.
func (s *Server) setPluginEnabled(name string, enabled bool) {
	err := s.withConfig(func(c *config.Config) error {
		if p := c.FindPlugin(name); p != nil {
			p.Enabled = enabled
		} else if enabled {
			c.Plugins = append(c.Plugins, config.PluginConfig{Name: name, Enabled: true})
		}
		return nil
	})
	if err != nil {
		logx.Errorf("plugins: persist enabled=%v for %s: %v", enabled, name, err)
	}
}

// renderPluginRows writes the peer rows in place, the shared tail of every
// row-level action. oob swaps them from a response the rows are not the
// target of.
func (s *Server) renderPluginRows(w http.ResponseWriter, r *http.Request, oob bool) {
	s.renderTemplate(w, "partials/plugin_rows.html", map[string]any{
		"Rows":      s.pluginRows(),
		"CSRFToken": s.csrfToken(sessionFromContext(r.Context())),
		"OOB":       oob,
	})
}

// setPluginPaused suspends or resumes a peer without dropping its pairing.
func (s *Server) setPluginPaused(name string, paused bool) error {
	if name == monloaderApp {
		return s.setMonloaderPaused(paused)
	}
	return s.withConfig(func(c *config.Config) error {
		if p := c.FindPlugin(name); p != nil {
			p.Paused = paused
		}
		return nil
	})
}

// teardownPluginPairing drops this side of a plugin pairing and notifies the
// peer, returning the notify error (nil on success), like monloader's.
func (s *Server) teardownPluginPairing(name string) error {
	p, ok := s.plugin(name)
	if !ok {
		return nil
	}
	// Read the address before removePairing drops the token it may live on.
	base, token := s.pluginAddress(p), p.PeerToken
	if err := s.removePairing(name); err != nil {
		logx.Errorf("pairing: remove %s failed: %v", name, err)
	}
	s.clearPluginProbe(name)
	notifyErr := notifyPeerTeardown(name, base, token)
	if notifyErr != nil {
		// The local half is already gone; the call is a courtesy so the peer
		// can drop its own copy.
		logx.Warnf("pairing: could not notify %s of teardown: %v", name, notifyErr)
	}
	return notifyErr
}
