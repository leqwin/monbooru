// Package markup renders the bracket vocabulary that annotation bodies,
// operator notes and artist commentary carry: a few inline marks plus links to
// a tag search, to another image, or off-site. Bodies are stored as this
// vocabulary and nothing else - a source's HTML is converted by FromHTML on
// the way in - so the renderer only ever emits elements it picked itself, with
// every byte of stored text escaped and every attribute assembled from a
// resolved value.
package markup

import (
	"html"
	"html/template"
	"strconv"
	"strings"
)

// maxDepth caps nesting. Past it a construct renders as the characters it is,
// like any other thing the parser does not recognise.
const maxDepth = 4

type nodeKind uint8

const (
	nodeText nodeKind = iota
	nodeMark
	nodeTag
	nodeURL
	nodeImage
)

type node struct {
	kind  nodeKind
	name  string // mark name, tag reference, or URL
	text  string
	id    int64
	child []node
}

// Doc is a parsed body.
type Doc struct {
	nodes []node
}

// Parse scans src. Anything that is not a complete, known construct stays the
// characters it is, so a body written before the vocabulary existed renders
// unchanged and a body truncated mid-construct degrades to visible text.
func Parse(src string) Doc {
	if !strings.ContainsRune(src, '[') {
		if src == "" {
			return Doc{}
		}
		return Doc{nodes: []node{{kind: nodeText, text: src}}}
	}
	return Doc{nodes: parseNodes(src, 0, false)}
}

func parseNodes(src string, depth int, inLink bool) []node {
	var out []node
	var lit strings.Builder
	flush := func() {
		if lit.Len() > 0 {
			out = append(out, node{kind: nodeText, text: lit.String()})
			lit.Reset()
		}
	}
	for i := 0; i < len(src); {
		if src[i] != '[' {
			lit.WriteByte(src[i])
			i++
			continue
		}
		n, width := parseConstruct(src[i:], depth, inLink)
		if width == 0 {
			lit.WriteByte(src[i])
			i++
			continue
		}
		flush()
		out = append(out, n)
		i += width
	}
	flush()
	return out
}

// parseConstruct reads one construct at the head of s, which starts with '['.
// A zero width means s does not open one and the '[' is literal text. Names are
// lowercase and carry no whitespace: one spelling per construct keeps the
// parser a scanner and keeps ordinary prose in brackets from becoming markup.
func parseConstruct(s string, depth int, inLink bool) (node, int) {
	if depth >= maxDepth {
		return node{}, 0
	}
	head := strings.IndexByte(s, ']')
	if head < 2 {
		return node{}, 0
	}
	inner := s[1:head]
	if strings.ContainsAny(inner, " \t\r\n") {
		return node{}, 0
	}
	if rest, ok := strings.CutPrefix(inner, "image:"); ok {
		id, err := strconv.ParseInt(rest, 10, 64)
		if err != nil || id <= 0 {
			return node{}, 0
		}
		return node{kind: nodeImage, id: id}, head + 1
	}

	name, value, hasValue := strings.Cut(inner, "=")
	kind := nodeMark
	switch {
	case marks.has(name):
		if hasValue {
			return node{}, 0
		}
	case name == "tag":
		kind = nodeTag
	case name == "url":
		kind = nodeURL
		if !validURL(value) {
			return node{}, 0
		}
	default:
		return node{}, 0
	}
	if kind != nodeMark && inLink {
		return node{}, 0
	}

	// First close wins: nesting a mark inside itself carries no meaning, and
	// counting pairs would cost a scan per candidate for nothing.
	closing := "[/" + name + "]"
	rel := strings.Index(s[head+1:], closing)
	if rel < 0 {
		return node{}, 0
	}
	body := s[head+1 : head+1+rel]
	width := head + 1 + rel + len(closing)

	switch kind {
	case nodeTag:
		ref := strings.TrimSpace(value)
		if !hasValue {
			ref = strings.TrimSpace(body)
		}
		if ref == "" || strings.ContainsAny(ref, "[]") {
			return node{}, 0
		}
		return node{kind: nodeTag, name: ref, child: label(body, ref, depth)}, width
	case nodeURL:
		return node{kind: nodeURL, name: value, child: label(body, value, depth)}, width
	default:
		return node{kind: nodeMark, name: name, child: parseNodes(body, depth+1, inLink)}, width
	}
}

// label parses a link's visible text, falling back to the link's own value so
// an empty label still shows what it points at.
func label(body, fallback string, depth int) []node {
	if strings.TrimSpace(body) == "" {
		return []node{{kind: nodeText, text: fallback}}
	}
	return parseNodes(body, depth+1, true)
}

func validURL(s string) bool {
	lower := strings.ToLower(s)
	return strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://")
}

// Refs is the set of references a body points at, accumulated across every
// body on a page so the gallery resolves them in one batch.
type Refs struct {
	Tags   map[string]bool
	Images map[int64]bool
	URLs   map[string]bool
}

func NewRefs() Refs {
	return Refs{Tags: map[string]bool{}, Images: map[int64]bool{}, URLs: map[string]bool{}}
}

// Collect adds d's references to r.
func (d Doc) Collect(r Refs) {
	collect(d.nodes, r)
}

func collect(nodes []node, r Refs) {
	for _, n := range nodes {
		switch n.kind {
		case nodeTag:
			r.Tags[n.name] = true
		case nodeURL:
			r.URLs[n.name] = true
		case nodeImage:
			r.Images[n.id] = true
		}
		collect(n.child, r)
	}
}

// TagRef is a tag reference resolved against the gallery's catalog.
type TagRef struct {
	Href  string
	Color string
	Known bool
}

// Resolver carries what the gallery knows about the references a render is
// about to emit. Every href and colour the renderer writes comes from here,
// never from the body.
type Resolver struct {
	Tags   map[string]TagRef
	Images map[int64]bool
	Links  map[string]int64 // external URL -> the image whose origin serves it
}

// Render emits the document. Text is escaped, elements come from the table
// below, and attribute values are either a resolved value or digits.
func (d Doc) Render(res Resolver) template.HTML {
	var b strings.Builder
	renderNodes(&b, d.nodes, res)
	return template.HTML(b.String())
}

func renderNodes(b *strings.Builder, nodes []node, res Resolver) {
	for _, n := range nodes {
		switch n.kind {
		case nodeText:
			b.WriteString(html.EscapeString(n.text))
		case nodeMark:
			open, closing := markElement(n.name)
			b.WriteString(open)
			renderNodes(b, n.child, res)
			b.WriteString(closing)
		case nodeTag:
			renderTag(b, n, res)
		case nodeURL:
			renderURL(b, n, res)
		case nodeImage:
			renderImage(b, n, res)
		}
	}
}

// markVocabulary is the whole mark vocabulary: which names a construct
// accepts and what each renders as. One table, so adding a mark cannot
// half-land - a name the parser takes but the renderer does not know would
// come out as an unstyled span with no sign anything was missed.
type markVocabulary map[string]struct{ open, closing string }

func (m markVocabulary) has(name string) bool { _, ok := m[name]; return ok }

var marks = markVocabulary{
	"b":    {"<strong>", "</strong>"},
	"i":    {"<em>", "</em>"},
	"u":    {`<span class="mk-u">`, "</span>"},
	"s":    {"<s>", "</s>"},
	"code": {"<code>", "</code>"},
	"tn":   {`<span class="mk-tn">`, "</span>"},
}

func markElement(name string) (open, closing string) {
	m := marks[name]
	return m.open, m.closing
}

func renderTag(b *strings.Builder, n node, res Resolver) {
	ref, ok := res.Tags[n.name]
	if !ok || ref.Href == "" {
		renderNodes(b, n.child, res)
		return
	}
	b.WriteString(`<a class="mk-tag`)
	if !ref.Known {
		b.WriteString(" mk-unknown")
	}
	b.WriteString(`" href="` + html.EscapeString(ref.Href) + `"`)
	if ref.Known {
		if ref.Color != "" {
			b.WriteString(` style="color:` + html.EscapeString(ref.Color) + `"`)
		}
	} else {
		b.WriteString(` title="no such tag in this gallery"`)
	}
	b.WriteString(">")
	renderNodes(b, n.child, res)
	b.WriteString("</a>")
}

func renderURL(b *strings.Builder, n node, res Resolver) {
	if id, ok := res.Links[n.name]; ok {
		b.WriteString(`<a class="mk-local" href="/images/`)
		b.WriteString(strconv.FormatInt(id, 10))
		b.WriteString(`" title="`)
		b.WriteString(html.EscapeString(n.name))
		b.WriteString(`">`)
	} else {
		b.WriteString(`<a class="mk-url" target="_blank" rel="noopener noreferrer" href="`)
		b.WriteString(html.EscapeString(n.name))
		b.WriteString(`">`)
	}
	renderNodes(b, n.child, res)
	b.WriteString("</a>")
}

func renderImage(b *strings.Builder, n node, res Resolver) {
	num := strconv.FormatInt(n.id, 10)
	if !res.Images[n.id] {
		b.WriteString("#" + num)
		return
	}
	b.WriteString(`<a class="mk-local" href="/images/` + num + `">#` + num + `</a>`)
}

// Text flattens the document, for the list entries and title attributes that
// want one scannable line rather than a render.
func (d Doc) Text() string {
	var b strings.Builder
	flatten(&b, d.nodes)
	return b.String()
}

func flatten(b *strings.Builder, nodes []node) {
	for _, n := range nodes {
		switch n.kind {
		case nodeText:
			b.WriteString(n.text)
		case nodeImage:
			b.WriteString("#" + strconv.FormatInt(n.id, 10))
		default:
			flatten(b, n.child)
		}
	}
}
