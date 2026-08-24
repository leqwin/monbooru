package web

import (
	"net"

	"github.com/monbooru/monbooru/internal/plugins"
)

// pluginCallbackURL is the address a managed plugin calls monbooru on. It is
// the listener, not server.base_url: base_url is the browser-facing address,
// which behind a reverse proxy or an ingress answers as something else (or not
// at all) from inside the container the child runs in. The listener always
// resolves, because the child shares its host.
func (s *Server) pluginCallbackURL() string {
	s.cfgMu.RLock()
	addr, base := s.cfg.Server.BindAddress, s.cfg.Server.BaseURL
	s.cfgMu.RUnlock()

	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return base
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port)
}

// launchOf is the supervisor's view of one discovered plugin.
func launchOf(p effectivePlugin) plugins.Launch {
	return plugins.Launch{Name: p.Name, Command: p.Command, Args: p.Args, Dir: p.Dir}
}

// startManagedPlugins launches every dropped plugin the operator enabled.
func (s *Server) startManagedPlugins() {
	for _, p := range s.effectivePlugins() {
		if !p.Installed {
			continue
		}
		if !p.Enabled {
			// Nothing is listening on the other end, and a cold probe cache
			// reads optimistic - without this the buttons of a plugin the
			// operator disabled come back on every restart.
			s.markPluginDown(p.Name)
			continue
		}
		s.pluginSupervisor.Start(launchOf(p))
	}
}
