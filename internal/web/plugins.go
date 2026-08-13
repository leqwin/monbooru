package web

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/monbooru/monbooru/internal/config"
)

// monloaderApp is the reserved peer name the companion pairs under. It keeps
// its own config section and its own surfaces; every other name routes to the
// generic plugin path.
const monloaderApp = "monloader"

// pluginClient is the outbound client for third-party peers: probes and relay
// calls. Per-call deadlines belong to the request contexts; this timeout is
// only a backstop above the longest of them (the 10 s relay).
var pluginClient = &http.Client{Timeout: 15 * time.Second}

const (
	// pluginProbeTTL bounds how often a peer's /health is re-checked, so a
	// settings render never fans out a probe per row per navigation.
	pluginProbeTTL = 10 * time.Second
	// pluginProbeInterval is how often the background prober refreshes every
	// peer, so buttons stop rendering within half a minute of a peer dying
	// even on pages nobody reloads.
	pluginProbeInterval = 30 * time.Second
	pluginProbeTimeout  = 4 * time.Second
)

// pluginProbe is one peer's cached connectivity state. An empty conn is a
// cold cache, which renders optimistically like MonloaderUsable does.
type pluginProbe struct {
	conn      string // "" | "ok" | "down"
	version   string
	checkedAt time.Time
}

// plugins copies the configured blocks under the lock its mutators take.
func (s *Server) plugins() []config.PluginConfig {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	return slices.Clone(s.cfg.Plugins)
}

// plugin returns the block with the given name, or false.
func (s *Server) plugin(name string) (config.PluginConfig, bool) {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	if p := s.cfg.FindPlugin(name); p != nil {
		return *p, true
	}
	return config.PluginConfig{}, false
}

// pluginBase is the address monbooru calls a peer at. A paused block reports
// none, so every outbound call short-circuits while the credentials stay on
// disk.
func (s *Server) pluginBase(p config.PluginConfig) string {
	if p.Paused {
		return ""
	}
	return s.pluginAddress(p)
}

// pluginAddress is where the peer lives regardless of the pause flag, which
// suspends calls rather than moving the peer: the operator's override when
// set, else the address learned at pairing (stored on the paired token).
func (s *Server) pluginAddress(p config.PluginConfig) string {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	if u := strings.TrimSpace(p.APIURL); u != "" {
		return strings.TrimRight(u, "/")
	}
	if t := s.cfg.FindPairedToken(p.Name); t != nil {
		return strings.TrimRight(t.PeerURL, "/")
	}
	return ""
}

// pluginUsable reports whether a peer's surfaces should render: not paused
// and not known down. A cold probe cache stays optimistic so a fresh boot
// doesn't blank the buttons before the first probe lands.
func (s *Server) pluginUsable(p config.PluginConfig) bool {
	if p.Paused {
		return false
	}
	return s.pluginProbeSeed(p.Name).conn != "down"
}

// markPluginDown records a failed call so the peer's buttons stop rendering
// without waiting for the next scheduled probe.
func (s *Server) markPluginDown(name string) {
	s.setPluginProbe(name, pluginProbe{conn: "down", checkedAt: time.Now()})
}

// peerOverrideURL is the operator's configured address for a peer, before any
// pairing has stored one. monloader keeps its own key; plugins carry theirs on
// the block a re-pair may reuse.
func (s *Server) peerOverrideURL(app string) string {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()
	if app == monloaderApp {
		return strings.TrimSpace(s.cfg.Monloader.APIURL)
	}
	if p := s.cfg.FindPlugin(app); p != nil {
		return strings.TrimSpace(p.APIURL)
	}
	return ""
}

func (s *Server) pluginProbeSeed(name string) pluginProbe {
	s.pluginProbeMu.Lock()
	defer s.pluginProbeMu.Unlock()
	return s.pluginProbes[name]
}

// clearPluginProbe forgets what a peer's last probe said, leaving the cold
// cache's optimistic reading until the next one lands.
func (s *Server) clearPluginProbe(name string) {
	s.pluginProbeMu.Lock()
	defer s.pluginProbeMu.Unlock()
	delete(s.pluginProbes, name)
}

func (s *Server) setPluginProbe(name string, pr pluginProbe) {
	s.pluginProbeMu.Lock()
	defer s.pluginProbeMu.Unlock()
	if s.pluginProbes == nil {
		s.pluginProbes = map[string]pluginProbe{}
	}
	s.pluginProbes[name] = pr
}

// probePeer reports whether base answers a health probe, and the version it
// names when it reports one. Any other body is ignored, so answering 200 with
// nothing is enough to pass. The client is the caller's: monloader and the
// plugins keep separate pools.
func probePeer(ctx context.Context, client *http.Client, base string) (string, bool) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/health", nil)
	if err != nil {
		return "", false
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", false
	}
	var h struct {
		Version string `json:"version"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&h)
	return h.Version, true
}

// notifyPeerTeardown asks a peer to drop its side of the pairing and returns
// an error when it could not be reached, so the caller can tell the operator
// to remove the far end by hand. Shared by monloader and plugin removals.
func notifyPeerTeardown(app, baseURL, token string) error {
	base := strings.TrimRight(baseURL, "/")
	if base == "" || token == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/v1/pair/remove", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := pluginClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	// Any 2xx: the teardown is idempotent and carries no body worth reading,
	// so a peer answering 204 has done what was asked.
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return peerStatusError{app, resp.Status}
	}
	return nil
}

// refreshPluginProbes probes every paired, unpaused peer whose cached state
// has aged past the TTL. Peers are probed concurrently so one slow host can't
// serialise the rest.
func (s *Server) refreshPluginProbes(ctx context.Context) {
	var wg sync.WaitGroup
	for _, p := range s.plugins() {
		if p.PeerToken == "" || p.Paused {
			continue
		}
		if time.Since(s.pluginProbeSeed(p.Name).checkedAt) < pluginProbeTTL {
			continue
		}
		base := s.pluginBase(p)
		if base == "" {
			s.setPluginProbe(p.Name, pluginProbe{conn: "down", checkedAt: time.Now()})
			continue
		}
		wg.Add(1)
		go func(name, base string) {
			defer wg.Done()
			version, ok := probePeer(ctx, pluginClient, base)
			pr := pluginProbe{conn: "down", checkedAt: time.Now()}
			if ok {
				// A peer that stops reporting its version keeps the one it
				// gave at pairing rather than blanking the settings row.
				pr = pluginProbe{conn: "ok", version: version, checkedAt: time.Now()}
			}
			s.setPluginProbe(name, pr)
		}(p.Name, base)
	}
	wg.Wait()
}

// runPluginProbes keeps every peer's connectivity state fresh so the button
// surfaces gate on something current. It costs nothing on an install with no
// plugins: the refresh walks an empty list.
func (s *Server) runPluginProbes() {
	ticker := time.NewTicker(pluginProbeInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), pluginProbeTimeout)
			s.refreshPluginProbes(ctx)
			cancel()
		case <-s.done:
			return
		}
	}
}
