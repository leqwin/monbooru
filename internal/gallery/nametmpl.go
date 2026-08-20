package gallery

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/logx"
)

// maxNameBytes leaves room under the usual 255-byte filename limit for an
// extension and a collision counter.
const maxNameBytes = 180

// Scope is the call site a template is parsed for. It decides which
// separators and which tokens that site can honour.
type Scope int

const (
	ScopeRename Scope = iota
	ScopeRenameBatch
	ScopeMove
	ScopeMoveBatch
	ScopeUploadFolder
	ScopeUploadName
)

// Folder reports whether the scope names a directory rather than a file:
// "/" separates instead of being refused, and an empty render is the
// gallery root instead of a name that has to fall back.
func (s Scope) Folder() bool {
	return s == ScopeMove || s == ScopeMoveBatch || s == ScopeUploadFolder
}

func (s Scope) sequence() bool { return s == ScopeRenameBatch || s == ScopeMoveBatch }

func (s Scope) source() bool { return s == ScopeUploadFolder || s == ScopeUploadName }

func (s Scope) slashRefusal() string {
	if s == ScopeUploadName {
		return "a name carries no folder - set that in the folder field"
	}
	return "a rename stays in the folder - use Move to file it"
}

type nameToken int

const (
	tokLiteral nameToken = iota
	tokName
	tokExt
	tokType
	tokID
	tokHash
	tokMD5
	tokGallery
	tokDate
	tokYear
	tokMonth
	tokDay
	tokTime
	tokImgWidth
	tokImgHeight
	tokSize
	tokOrigin
	tokSeq
	tokSource
	tokPostID
)

var nameTokens = map[string]nameToken{
	"name": tokName, "ext": tokExt, "type": tokType, "id": tokID,
	"hash": tokHash, "md5": tokMD5, "gallery": tokGallery, "date": tokDate,
	"year": tokYear, "month": tokMonth, "day": tokDay, "time": tokTime,
	"w": tokImgWidth, "h": tokImgHeight, "size": tokSize, "origin": tokOrigin,
	"n": tokSeq, "source": tokSource, "post_id": tokPostID,
}

// refusedNameTokens are the spellings people reach for that name something
// no rename can know. Answering them by name is cheaper than a help page.
var refusedNameTokens = map[string]string{
	"tag":        "tags are not known at ingest time",
	"artist":     "tags are not known at ingest time",
	"collection": "a collection membership is not a fact about the file",
	"rating":     "a rating is a tag by another name",
	"inbox":      "the inbox is a triage state, not an identity",
}

type namePart struct {
	lit   string
	tok   nameToken
	width int
}

// NameTemplate is a compiled filename template: literal runs and the
// tokens between them.
type NameTemplate struct {
	parts  []namePart
	scope  Scope
	tokens bool
}

// ParseNameTemplate compiles s for the call site sc, refusing anything sc
// cannot honour. A blank template compiles to nil, which every caller
// reads as "leave the name alone".
func ParseNameTemplate(s string, sc Scope) (*NameTemplate, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	t := &NameTemplate{scope: sc}
	var lit strings.Builder
	flush := func() {
		if lit.Len() > 0 {
			t.parts = append(t.parts, namePart{lit: lit.String()})
			lit.Reset()
		}
	}
	for i := 0; i < len(s); {
		switch {
		case strings.HasPrefix(s[i:], "{{"):
			lit.WriteByte('{')
			i += 2
		case strings.HasPrefix(s[i:], "}}"):
			lit.WriteByte('}')
			i += 2
		case s[i] == '{':
			end := strings.IndexByte(s[i:], '}')
			if end < 0 {
				return nil, fmt.Errorf("unclosed { in the template")
			}
			part, err := parseNameToken(s[i+1:i+end], sc)
			if err != nil {
				return nil, err
			}
			flush()
			t.parts = append(t.parts, part)
			t.tokens = true
			i += end + 1
		case s[i] == '/' && !sc.Folder():
			return nil, fmt.Errorf("%s", sc.slashRefusal())
		default:
			lit.WriteByte(s[i])
			i++
		}
	}
	flush()
	return t, nil
}

func parseNameToken(inner string, sc Scope) (namePart, error) {
	name, arg, hasArg := strings.Cut(inner, ":")
	name = strings.TrimSpace(name)
	tok, known := nameTokens[name]
	if !known {
		if why, refused := refusedNameTokens[name]; refused {
			return namePart{}, fmt.Errorf("unknown token {%s}: %s", inner, why)
		}
		return namePart{}, fmt.Errorf("unknown token {%s}", inner)
	}
	if tok == tokSeq && !sc.sequence() {
		return namePart{}, fmt.Errorf("{n} needs a batch - use {id} to name one file")
	}
	if (tok == tokSource || tok == tokPostID) && !sc.source() {
		return namePart{}, fmt.Errorf("{%s} is only available for received files", name)
	}
	p := namePart{tok: tok}
	if hasArg {
		n, convErr := strconv.Atoi(strings.TrimSpace(arg))
		switch tok {
		case tokHash:
			if convErr != nil || n < 4 || n > 64 {
				return namePart{}, fmt.Errorf("{hash:N} takes a width between 4 and 64")
			}
		case tokSeq:
			if convErr != nil || n < 1 || n > 12 {
				return namePart{}, fmt.Errorf("{n:N} takes a width between 1 and 12")
			}
		default:
			return namePart{}, fmt.Errorf("{%s} takes no width", name)
		}
		p.width = n
	}
	return p, nil
}

// HasTokens reports whether the template varies per image. Batch rename
// numbers a plain string itself so a whole run cannot collide on one name.
func (t *NameTemplate) HasTokens() bool { return t != nil && t.tokens }

// NameFacts is everything a template can substitute: the row's own
// columns, the batch position, and the origin a push carried.
type NameFacts struct {
	Name string
	// Ext is lower-cased, which is what makes {name}.{ext} the way to
	// normalise a shouting extension. Base keeps the on-disk spelling,
	// since that is the name a rename actually starts from.
	Ext        string
	Base       string
	Type       string
	Gallery    string
	ID         int64
	SHA256     string
	MD5        string
	IngestedAt time.Time
	Width      int
	Height     int
	Size       int64
	Origin     string
	Source     string
	PostID     string
	N          int
	NWidth     int
}

// LoadNameFacts reads the row's half of a render. Source, PostID and the
// batch position are the caller's to fill.
func LoadNameFacts(database *db.DB, galleryName string, id int64) (NameFacts, error) {
	f := NameFacts{ID: id, Gallery: galleryName}
	var canonical, ingestedAt string
	if err := database.Read.QueryRow(
		`SELECT canonical_path, file_type, sha256, md5, origin, file_size,
		        COALESCE(width, 0), COALESCE(height, 0), ingested_at
		 FROM images WHERE id = ?`, id,
	).Scan(&canonical, &f.Type, &f.SHA256, &f.MD5, &f.Origin, &f.Size, &f.Width, &f.Height, &ingestedAt); err != nil {
		return f, fmt.Errorf("name facts for image %d: %w", id, err)
	}
	ext := filepath.Ext(canonical)
	f.Base = filepath.Base(canonical)
	f.Name = strings.TrimSuffix(f.Base, ext)
	f.Ext = strings.ToLower(strings.TrimPrefix(ext, "."))
	f.IngestedAt, _ = time.Parse(time.RFC3339, ingestedAt)
	return f, nil
}

// Render substitutes f into the template. A column the row does not carry
// renders empty and the separators around it close up, so a template
// written for a push still reads right on a file that arrived without one.
// A path renders empty as the gallery root; a filename falls back to the
// id, which is the one value every row has.
func (t *NameTemplate) Render(f NameFacts) string {
	var b strings.Builder
	for _, p := range t.parts {
		if p.tok == tokLiteral {
			b.WriteString(p.lit)
			continue
		}
		b.WriteString(SanitizeFilename(p.value(f)))
	}
	out := tidyNamePath(b.String())
	if out == "" && !t.scope.Folder() {
		return strconv.FormatInt(f.ID, 10)
	}
	return out
}

func (p namePart) value(f NameFacts) string {
	switch p.tok {
	case tokName:
		return f.Name
	case tokExt:
		return f.Ext
	case tokType:
		return f.Type
	case tokID:
		return strconv.FormatInt(f.ID, 10)
	case tokHash:
		if p.width > 0 && p.width < len(f.SHA256) {
			return f.SHA256[:p.width]
		}
		return f.SHA256
	case tokMD5:
		return f.MD5
	case tokGallery:
		return f.Gallery
	case tokDate:
		return f.IngestedAt.Format("2006-01-02")
	case tokYear:
		return f.IngestedAt.Format("2006")
	case tokMonth:
		return f.IngestedAt.Format("01")
	case tokDay:
		return f.IngestedAt.Format("02")
	case tokTime:
		return f.IngestedAt.Format("150405")
	case tokImgWidth:
		return sizeOrEmpty(f.Width)
	case tokImgHeight:
		return sizeOrEmpty(f.Height)
	case tokSize:
		return strconv.FormatInt(f.Size, 10)
	case tokOrigin:
		return f.Origin
	case tokSeq:
		width := p.width
		if width == 0 {
			width = f.NWidth
		}
		return fmt.Sprintf("%0*d", max(width, 1), f.N)
	case tokSource:
		return f.Source
	case tokPostID:
		return f.PostID
	}
	return ""
}

// sizeOrEmpty renders a dimension the decode never filled as nothing, so
// {w}x{h} on a video collapses instead of reading 0x0.
func sizeOrEmpty(v int) string {
	if v <= 0 {
		return ""
	}
	return strconv.Itoa(v)
}

// tidyNamePath cleans each segment and drops the empty ones, so a token
// that rendered nothing cannot leave a bare separator - or, at the front,
// a leading slash the destination resolver would read as absolute.
func tidyNamePath(s string) string {
	segs := strings.Split(s, "/")
	kept := segs[:0]
	for _, seg := range segs {
		// Per segment, not per token: a template's own literal text reaches
		// the path too, and the filesystem refuses it for the same reasons.
		if seg = TruncateFilename(tidyNameSegment(SanitizeFilename(seg)), maxNameBytes); seg != "" {
			kept = append(kept, seg)
		}
	}
	return strings.Join(kept, "/")
}

func tidyNameSegment(s string) string {
	var b strings.Builder
	prevSep := false
	for _, r := range s {
		if r == '-' || r == '_' || r == ' ' {
			if !prevSep {
				b.WriteRune(r)
				prevSep = true
			}
			continue
		}
		prevSep = false
		b.WriteRune(r)
	}
	return strings.Trim(b.String(), "-_ ")
}

// SanitizeFilename folds what a filesystem would refuse in one path
// segment to _. Only that - a name in any script still names its own file
// instead of collapsing to underscores. The Windows reserved set covers
// POSIX's, and a gallery is often shared over SMB; trailing dots and
// spaces go too, since Windows drops them silently and the path would
// stop round-tripping.
func SanitizeFilename(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(`<>:"/\|?*`, r) {
			b.WriteRune('_')
			continue
		}
		b.WriteRune(r)
	}
	return strings.Trim(b.String(), " .")
}

// TruncateFilename cuts s to at most max bytes, backing up to a rune
// boundary so a multi-byte character is never left in half. The budget is
// bytes because the filesystem's is.
func TruncateFilename(s string, max int) string {
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return strings.Trim(s[:cut], " .")
}

// Naming is where a file is filed and what it is called, plus the gallery
// name {gallery} resolves to. A zero value leaves the file alone.
type Naming struct {
	Folder  *NameTemplate
	Name    *NameTemplate
	Gallery string
}

// Empty reports whether the naming would do nothing.
func (n Naming) Empty() bool { return n.Folder == nil && n.Name == nil }

// Apply files image id where the naming says it belongs: the folder
// first, then the name. It runs after ingest, which is what makes {id}
// and {hash} available at all. site and postID carry the origin only a
// push knows; every other caller passes empty strings. The final path
// comes back so a caller tracking paths on disk can follow the file.
func (n Naming) Apply(database *db.DB, galleryPath string, id int64, site, postID string) (string, error) {
	if n.Empty() {
		return "", nil
	}
	facts, err := LoadNameFacts(database, n.Gallery, id)
	if err != nil {
		return "", err
	}
	facts.Source, facts.PostID = site, postID
	var folder, name *string
	if n.Folder != nil {
		rendered := n.Folder.Render(facts)
		folder = &rendered
	}
	if n.Name != nil {
		rendered := n.Name.Render(facts)
		name = &rendered
	}
	res, err := PlaceImage(database, galleryPath, id, folder, name)
	if err != nil {
		return "", err
	}
	return res.NewCanonicalPath, nil
}

// ReceivedNaming compiles the destination settings for a file monbooru is
// about to write: the directory the bytes go into now, and what to apply
// once the row exists. A folder built from tokens cannot be resolved
// before that row, so those bytes land in the gallery root and the move
// follows the ingest. A folder named on the request is the caller's own
// choice and is never a template.
func ReceivedNaming(galleryName, requestFolder, defaultFolder, defaultName string) (writeDir string, n Naming) {
	n.Gallery = galleryName
	n.Name = parseSetting(defaultName, ScopeUploadName, "default_upload_name")
	if requestFolder != "" {
		return requestFolder, n
	}
	folder := parseSetting(defaultFolder, ScopeUploadFolder, "default_upload_folder")
	if folder.HasTokens() {
		n.Folder = folder
		return "", n
	}
	return defaultFolder, n
}

// IngestNaming compiles the destination settings for a file that is
// already on disk. There is nowhere to write it first, so the folder
// applies as a move whether or not it carries tokens; a blank folder
// leaves the file in whatever folder it was dropped in.
func IngestNaming(galleryName, folder, name string) Naming {
	return Naming{
		Folder:  parseSetting(folder, ScopeUploadFolder, "default_upload_folder"),
		Name:    parseSetting(name, ScopeUploadName, "default_upload_name"),
		Gallery: galleryName,
	}
}

// ParseBatchRenameTemplate compiles a batch-rename base. A base carrying no
// token of its own numbers the run, or the whole scope would collide on one
// name and auto-suffix its way out; the preview and the job both parse
// through here so they cannot disagree about what a plain name does.
func ParseBatchRenameTemplate(s string) (*NameTemplate, error) {
	tmpl, err := ParseNameTemplate(s, ScopeRenameBatch)
	if err != nil || tmpl == nil || tmpl.HasTokens() {
		return tmpl, err
	}
	return ParseNameTemplate(s+"{n}", ScopeRenameBatch)
}

// FolderFor renders one row's destination folder, empty when the naming
// names none. Apply is the whole answer for a caller filing a single row;
// this is for the one that hangs a subfolder of its own under the result.
func (n Naming) FolderFor(database *db.DB, id int64) (string, error) {
	if n.Folder == nil {
		return "", nil
	}
	facts, err := LoadNameFacts(database, n.Gallery, id)
	if err != nil {
		return "", err
	}
	return n.Folder.Render(facts), nil
}

// parseSetting compiles a stored setting, reporting a hand-edited value
// that will not parse rather than filing by half of it.
func parseSetting(raw string, sc Scope, key string) *NameTemplate {
	tmpl, err := ParseNameTemplate(raw, sc)
	if err != nil {
		logx.Warnf("%s %q: %v", key, raw, err)
		return nil
	}
	return tmpl
}
