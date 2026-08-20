package web

import (
	"html/template"
	"maps"
	"net/url"
	"slices"

	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/markup"
)

// resolveMarkup turns the references a page's bodies carry into the hrefs and
// colours the renderer writes. Every kind resolves in one batched read, so an
// image labelled with two hundred boxes costs three queries rather than six
// hundred, and a page whose bodies carry no reference costs none.
func (s *Server) resolveMarkup(refs markup.Refs) markup.Resolver {
	var res markup.Resolver
	if len(refs.Tags) > 0 {
		names := slices.Collect(maps.Keys(refs.Tags))
		res.Tags = make(map[string]markup.TagRef, len(names))
		for name, target := range gallery.ResolveTagRefs(s.db(), names) {
			ref := markup.TagRef{
				Href:  "/?q=" + uppercasePercentEscapes(url.QueryEscape(name)),
				Known: target.Found,
			}
			if target.Found {
				ref.Color = categoryColor(target.Color)
			}
			res.Tags[name] = ref
		}
	}
	if len(refs.Images) > 0 {
		res.Images = gallery.ExistingImageIDs(s.db(), slices.Collect(maps.Keys(refs.Images)))
	}
	if len(refs.URLs) > 0 {
		res.Links = gallery.ImageIDsBySourceURL(s.db(), slices.Collect(maps.Keys(refs.URLs)))
	}
	return res
}

// renderMarkup parses and renders one standalone body, for the surfaces that
// carry a single one rather than a page full.
func (s *Server) renderMarkup(body string) template.HTML {
	doc := markup.Parse(body)
	refs := markup.NewRefs()
	doc.Collect(refs)
	return doc.Render(s.resolveMarkup(refs))
}
