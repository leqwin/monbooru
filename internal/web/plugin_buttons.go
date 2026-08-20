package web

import (
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/monbooru/monbooru/internal/config"
)

// pluginButtonView is one rendered button. Href is the substituted target of
// an open-mode link; Index locates a relay button in its peer's block. Off
// marks a button whose peer is paired but paused or unreachable: it renders
// inert rather than vanishing, so a pause reads as a pause.
type pluginButtonView struct {
	Label string
	Mode  string
	Href  string
	Index int
	Off   bool
	Why   string
}

// pluginGroup is one peer's buttons in a slot, under its name.
type pluginGroup struct {
	Peer    string
	Buttons []pluginButtonView
}

// pluginSlotView is what a mount point renders: the peer groups plus the
// image a relay click there acts on (0 on the gallery, where the scope is the
// live selection instead).
type pluginSlotView struct {
	ImageID int64
	Groups  []pluginGroup
}

// Any reports whether the surface has something to render. A slot with no
// buttons is absent entirely, like the monloader-gated surfaces.
func (v pluginSlotView) Any() bool { return len(v.Groups) > 0 }

// AnyOpen reports whether any button here opens a page, so a surface of
// relay buttons carries no pop-in it would never show.
func (v pluginSlotView) AnyOpen() bool {
	for _, g := range v.Groups {
		for _, b := range g.Buttons {
			if b.Mode == config.ModeOpen {
				return true
			}
		}
	}
	return false
}

// pluginSlot collects the buttons every paired peer declared for a slot,
// grouped by peer in name order and declaration order within a peer. A peer
// that is paused or unreachable still contributes its group, inert; one with
// no address to reach contributes nothing, since there is no pairing to
// resume. fileType is the medium the surface holds, empty where it holds
// several (the gallery).
func (s *Server) pluginSlot(r *http.Request, slot string, imageID int64, fileType string) pluginSlotView {
	peers := s.plugins()
	slices.SortFunc(peers, func(a, b config.PluginConfig) int { return strings.Compare(a.Name, b.Name) })
	back, gallery := s.pageURL(r), s.activeName
	view := pluginSlotView{ImageID: imageID}
	for _, p := range peers {
		// pluginAddress rather than pluginBase: the latter reports a paused
		// peer as unreachable, which is the state that should render inert.
		if s.pluginAddress(p) == "" {
			continue
		}
		off := !s.pluginUsable(p)
		g := pluginGroup{Peer: p.Name}
		for i, b := range p.Buttons {
			if b.Slot != slot {
				continue
			}
			if fileType != "" && !b.AppliesTo(fileType) {
				continue
			}
			v := pluginButtonView{Label: b.Label, Mode: b.Mode, Index: i, Off: off}
			if off {
				v.Why = p.Name + " is " + pluginOffState(p)
			}
			if b.Mode == config.ModeOpen {
				// A peer's own pages ride monbooru's mount, not the address
				// it pairs on: that one answers from the server, not from
				// the browser (pluginMount).
				v.Href = substitutePluginVars(pluginMountBase(p.Name)+b.Path, imageID, gallery, back)
			}
			g.Buttons = append(g.Buttons, v)
		}
		if len(g.Buttons) > 0 {
			view.Groups = append(view.Groups, g)
		}
	}
	return view
}

// substitutePluginVars fills an open-mode target's variables, escaped so a
// gallery name or a back url with reserved characters survives the trip.
func substitutePluginVars(target string, imageID int64, gallery, backURL string) string {
	return strings.NewReplacer(
		"{image_id}", url.QueryEscape(strconv.FormatInt(imageID, 10)),
		"{gallery}", url.QueryEscape(gallery),
		"{back_url}", url.QueryEscape(backURL),
	).Replace(target)
}

// pageURL is the absolute address of the page being rendered, for {back_url}.
// The host is the one the browser actually reached monbooru on, since
// server.base_url defaults to localhost and would send a LAN browser nowhere;
// only the scheme comes from the configured base.
func (s *Server) pageURL(r *http.Request) string {
	scheme := "http"
	s.cfgMu.RLock()
	configured := s.cfg.Server.BaseURL
	s.cfgMu.RUnlock()
	if strings.HasPrefix(configured, "https://") {
		scheme = "https"
	}
	return scheme + "://" + r.Host + r.URL.RequestURI()
}
