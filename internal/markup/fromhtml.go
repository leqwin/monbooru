package markup

import (
	"html"
	"net/url"
	"strings"
)

// maxTagRefLen mirrors the tag-name cap: a longer "tags" query is a search, not
// a reference, and stays an ordinary link.
const maxTagRefLen = 200

// FromHTML converts a booru's HTML body into the vocabulary Parse reads. The
// constructs below survive; every other element is dropped and its text kept,
// so nothing a source sends can reach the renderer as markup it did not write.
// base resolves the site-relative hrefs a booru's own links use; with no base
// those links become plain text.
func FromHTML(src string, base *url.URL) string {
	c := converter{base: base}
	c.run(src)
	return strings.TrimSpace(c.out.String())
}

// frame is one construct still to close. A dropped link keeps a frame with no
// closing text so the matching </a> still pops it.
type frame struct {
	closing string
	link    bool
}

type converter struct {
	base  *url.URL
	out   strings.Builder
	stack []frame
}

func (c *converter) run(src string) {
	for i := 0; i < len(src); {
		lt := strings.IndexByte(src[i:], '<')
		if lt < 0 {
			c.text(src[i:])
			break
		}
		c.text(src[i : i+lt])
		i += lt
		if w := skipDecl(src[i:]); w > 0 {
			i += w
			continue
		}
		name, closing, attrs, w := scanTag(src[i:])
		if w == 0 {
			c.text("<")
			i++
			continue
		}
		i += w
		if !closing && (name == "script" || name == "style") {
			i += skipElement(src[i:], name)
			continue
		}
		c.tag(name, closing, attrs)
	}
	for len(c.stack) > 0 {
		c.pop()
	}
}

// htmlMarks maps the HTML a booru serves onto the mark vocabulary
// markup.go defines. Several tags fold onto one mark: a source's <strong>
// and <b> say the same thing here.
var htmlMarks = map[string]string{
	"b": "b", "strong": "b",
	"i": "i", "em": "i",
	"u": "u", "ins": "u",
	"s": "s", "strike": "s", "del": "s",
	"code": "code", "tt": "code", "pre": "code",
	"tn": "tn",
}

func (c *converter) tag(name string, closing bool, attrs string) {
	if mark, ok := htmlMarks[name]; ok {
		c.mark(mark, closing)
		return
	}
	switch name {
	case "br":
		if !closing {
			c.out.WriteString("\n")
		}
	case "p", "div", "blockquote", "ul", "ol", "table", "tr", "h1", "h2", "h3", "h4", "h5", "h6":
		c.newline()
	case "li":
		c.newline()
		if !closing {
			c.out.WriteString("- ")
		}
	case "a":
		if closing {
			c.closeLink()
		} else {
			c.openLink(attrs)
		}
	}
}

func (c *converter) mark(name string, closing bool) {
	if !closing {
		c.out.WriteString("[" + name + "]")
		c.stack = append(c.stack, frame{closing: "[/" + name + "]"})
		return
	}
	want := "[/" + name + "]"
	for i := len(c.stack) - 1; i >= 0; i-- {
		if c.stack[i].closing == want && !c.stack[i].link {
			for len(c.stack) > i {
				c.pop()
			}
			return
		}
	}
}

func (c *converter) openLink(attrs string) {
	kind, value := "", ""
	if !c.inLink() {
		kind, value = c.linkTarget(html.UnescapeString(attrValue(attrs, "href")))
	}
	if kind == "" {
		c.stack = append(c.stack, frame{link: true})
		return
	}
	c.out.WriteString("[" + kind + "=" + value + "]")
	c.stack = append(c.stack, frame{closing: "[/" + kind + "]", link: true})
}

func (c *converter) closeLink() {
	for i := len(c.stack) - 1; i >= 0; i-- {
		if c.stack[i].link {
			for len(c.stack) > i {
				c.pop()
			}
			return
		}
	}
}

// linkTarget classifies an href: a search on the origin's own site becomes a
// tag reference, an absolute http(s) link stays a link, anything else is
// dropped so only its label survives.
func (c *converter) linkTarget(href string) (kind, value string) {
	href = strings.TrimSpace(href)
	if href == "" {
		return "", ""
	}
	u, err := url.Parse(href)
	if err != nil {
		return "", ""
	}
	if c.base != nil {
		u = c.base.ResolveReference(u)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", ""
	}
	if c.base != nil && u.Host == c.base.Host {
		if t := u.Query().Get("tags"); isTagRef(t) {
			return "tag", t
		}
	}
	abs := u.String()
	if strings.ContainsAny(abs, "[]") {
		return "", ""
	}
	return "url", abs
}

func isTagRef(s string) bool {
	return s != "" && len([]rune(s)) <= maxTagRefLen &&
		!strings.ContainsAny(s, " \t\r\n[]")
}

func (c *converter) inLink() bool {
	for _, f := range c.stack {
		if f.link {
			return true
		}
	}
	return false
}

func (c *converter) pop() {
	last := len(c.stack) - 1
	c.out.WriteString(c.stack[last].closing)
	c.stack = c.stack[:last]
}

func (c *converter) text(s string) {
	if s == "" {
		return
	}
	s = html.UnescapeString(s)
	s = strings.ReplaceAll(s, "\r\n", "\n")
	c.out.WriteString(strings.ReplaceAll(s, "\r", "\n"))
}

func (c *converter) newline() {
	if s := c.out.String(); s != "" && !strings.HasSuffix(s, "\n") {
		c.out.WriteString("\n")
	}
}

// scanTag reads one element at the head of s, which starts with '<'. A zero
// width means s opens no element and the '<' is text.
func scanTag(s string) (name string, closing bool, attrs string, width int) {
	i := 1
	if i < len(s) && s[i] == '/' {
		closing = true
		i++
	}
	start := i
	for i < len(s) && isNameByte(s[i]) {
		i++
	}
	if i == start {
		return "", false, "", 0
	}
	name = strings.ToLower(s[start:i])
	attrStart := i
	quote := byte(0)
	for ; i < len(s); i++ {
		switch {
		case quote != 0:
			if s[i] == quote {
				quote = 0
			}
		case s[i] == '"' || s[i] == '\'':
			quote = s[i]
		case s[i] == '>':
			return name, closing, s[attrStart:i], i + 1
		}
	}
	return "", false, "", 0
}

func isNameByte(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z' || b >= '0' && b <= '9'
}

// attrValue pulls one attribute out of an element's attribute text. Bare
// values end at the first space; quoted ones at their quote.
func attrValue(attrs, name string) string {
	for i := 0; i < len(attrs); {
		for i < len(attrs) && isSpace(attrs[i]) {
			i++
		}
		start := i
		for i < len(attrs) && !isSpace(attrs[i]) && attrs[i] != '=' {
			i++
		}
		key := strings.ToLower(attrs[start:i])
		for i < len(attrs) && isSpace(attrs[i]) {
			i++
		}
		if i >= len(attrs) || attrs[i] != '=' {
			continue
		}
		i++
		for i < len(attrs) && isSpace(attrs[i]) {
			i++
		}
		var val string
		if i < len(attrs) && (attrs[i] == '"' || attrs[i] == '\'') {
			quote := attrs[i]
			i++
			start = i
			for i < len(attrs) && attrs[i] != quote {
				i++
			}
			val = attrs[start:i]
			if i < len(attrs) {
				i++
			}
		} else {
			start = i
			for i < len(attrs) && !isSpace(attrs[i]) {
				i++
			}
			val = attrs[start:i]
		}
		if key == name {
			return val
		}
	}
	return ""
}

func isSpace(b byte) bool { return b == ' ' || b == '\t' || b == '\r' || b == '\n' }

// skipDecl consumes a comment or a doctype, which carry no text worth keeping.
func skipDecl(s string) int {
	if strings.HasPrefix(s, "<!--") {
		if end := strings.Index(s[4:], "-->"); end >= 0 {
			return 4 + end + 3
		}
		return len(s)
	}
	if strings.HasPrefix(s, "<!") || strings.HasPrefix(s, "<?") {
		if end := strings.IndexByte(s, '>'); end >= 0 {
			return end + 1
		}
		return len(s)
	}
	return 0
}

// skipElement drops the contents of an element whose text is not prose.
func skipElement(s, name string) int {
	closing := "</" + name
	i := strings.Index(strings.ToLower(s), closing)
	if i < 0 {
		return len(s)
	}
	if end := strings.IndexByte(s[i:], '>'); end >= 0 {
		return i + end + 1
	}
	return len(s)
}
