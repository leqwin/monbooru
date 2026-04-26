package compatibility

import (
	"archive/zip"
	"bytes"
	"reflect"
	"sort"
	"testing"
)

// buildZip constructs an in-memory archive from {name → contents}. The
// per-test fixtures are tiny (a `backup.json` + a few media bytes), so
// streaming through a *bytes.Buffer keeps them readable inline.
func buildZip(t *testing.T, entries map[string]string) *zip.Reader {
	t.Helper()
	// Stable iteration order so detection that depends on entry order
	// (NormalizeEntries' shared-prefix scan, for instance) is testable.
	keys := make([]string, 0, len(entries))
	for k := range entries {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, k := range keys {
		w, err := zw.Create(k)
		if err != nil {
			t.Fatalf("zip.Create %q: %v", k, err)
		}
		if _, err := w.Write([]byte(entries[k])); err != nil {
			t.Fatalf("zip.Write %q: %v", k, err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip.Close: %v", err)
	}
	zr, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("zip.NewReader: %v", err)
	}
	return zr
}

func TestPickValidSHA256(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855", "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"},
		{"E3B0C44298FC1C149AFBF4C8996FB92427AE41E4649B934CA495991B7852B855", ""}, // uppercase rejected
		{"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b85", ""},  // 63 chars
		{"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b8550", ""}, // 65 chars
		{"e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b85g", ""},  // non-hex char
		{"", ""},
	}
	for _, c := range cases {
		if got := PickValidSHA256(c.in); got != c.want {
			t.Errorf("PickValidSHA256(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestHasMediaExt(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"foo.jpg", true},
		{"foo.JPEG", true},
		{"foo.png", true},
		{"foo.WebP", true},
		{"foo.gif", true},
		{"foo.mp4", true},
		{"foo.webm", true},
		{"foo.txt", false},
		{"backup.json", false},
		{"folder/", false}, // trailing slash → directory entry
		{"foo", false},
	}
	for _, c := range cases {
		if got := HasMediaExt(c.name); got != c.want {
			t.Errorf("HasMediaExt(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestNormalizeEntries_StripsSharedPrefix(t *testing.T) {
	zr := buildZip(t, map[string]string{
		"hydrus_export/a.png":         "x",
		"hydrus_export/a.png.txt":     "tag\n",
		"hydrus_export/sub/b.png":     "y",
		"hydrus_export/sub/b.png.txt": "tag2\n",
	})
	got := NormalizeEntries(zr.File)
	wantRels := []string{"a.png", "a.png.txt", "sub/b.png", "sub/b.png.txt"}
	gotRels := make([]string, len(got))
	for i, e := range got {
		gotRels[i] = e.Rel
	}
	sort.Strings(gotRels)
	if !reflect.DeepEqual(gotRels, wantRels) {
		t.Errorf("NormalizeEntries rels = %v, want %v", gotRels, wantRels)
	}
}

func TestNormalizeEntries_FlatNoPrefix(t *testing.T) {
	zr := buildZip(t, map[string]string{
		"backup.json":  "{}",
		"media/a.png":  "x",
	})
	got := NormalizeEntries(zr.File)
	wantRels := []string{"backup.json", "media/a.png"}
	gotRels := make([]string, 0, len(got))
	for _, e := range got {
		gotRels = append(gotRels, e.Rel)
	}
	sort.Strings(gotRels)
	if !reflect.DeepEqual(gotRels, wantRels) {
		t.Errorf("NormalizeEntries rels = %v, want %v", gotRels, wantRels)
	}
}

func TestNormalizeEntries_MixedPrefixesNotStripped(t *testing.T) {
	zr := buildZip(t, map[string]string{
		"a/x.png":     "1",
		"b/y.png":     "2",
		"backup.json": "{}",
	})
	got := NormalizeEntries(zr.File)
	rels := make([]string, len(got))
	for i, e := range got {
		rels[i] = e.Rel
	}
	sort.Strings(rels)
	want := []string{"a/x.png", "b/y.png", "backup.json"}
	sort.Strings(want)
	if !reflect.DeepEqual(rels, want) {
		t.Errorf("NormalizeEntries with mixed prefixes = %v, want %v (no stripping)", rels, want)
	}
}

func TestDetect_Blombooru(t *testing.T) {
	zr := buildZip(t, map[string]string{
		"backup.json": `{"version":1,"type":"full","media":[]}`,
		"media/a.png": "x",
	})
	if got := Detect(zr.File); got != "blombooru" {
		t.Errorf("Detect = %q, want blombooru", got)
	}
}

func TestDetect_Hydrus(t *testing.T) {
	zr := buildZip(t, map[string]string{
		"a.png":     "x",
		"a.png.txt": "tag\n",
	})
	if got := Detect(zr.File); got != "hydrus" {
		t.Errorf("Detect = %q, want hydrus", got)
	}
}

func TestDetect_Unknown(t *testing.T) {
	zr := buildZip(t, map[string]string{
		"random.txt": "nothing",
	})
	if got := Detect(zr.File); got != "" {
		t.Errorf("Detect = %q, want empty (unknown)", got)
	}
}

func TestTranslate_BlombooruHappyPath(t *testing.T) {
	backup := `{
		"version":1, "type":"full",
		"media":[
			{"filename":"cat.png","hash":"abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789",
			 "tags":["cat","sky","artist:foo"], "archive_path":"media/cat.png"}
		]
	}`
	csvText := "cat,0\nsky,0\nfoo,1\n" // foo → artist (id 1) per blombooruCategoryByID
	zr := buildZip(t, map[string]string{
		"backup.json": backup,
		"tags.csv":    csvText,
		"media/cat.png": "PNGBYTES",
	})
	res, err := Translate(zr.File, "blombooru")
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if len(res.Manifest.Images) != 1 {
		t.Fatalf("Manifest.Images = %d, want 1", len(res.Manifest.Images))
	}
	got := res.Manifest.Images[0]
	if got.Path != "cat.png" {
		t.Errorf("Path = %q, want cat.png", got.Path)
	}
	if got.SHA256 != "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789" {
		t.Errorf("SHA256 = %q, want full hash", got.SHA256)
	}
	// `cat` and `sky` are general (no prefix); `artist:foo` from tags.csv.
	wantTags := []string{"cat", "sky", "artist:foo"}
	sort.Strings(got.Tags)
	sort.Strings(wantTags)
	if !reflect.DeepEqual(got.Tags, wantTags) {
		t.Errorf("Tags = %v, want %v", got.Tags, wantTags)
	}
	if _, ok := res.Files["cat.png"]; !ok {
		t.Error("Files missing cat.png entry")
	}
}

func TestTranslate_BlombooruRejectsBadHash(t *testing.T) {
	// hash field is not 64 hex chars → SHA256 should come back empty so
	// the apply path computes the real sha from file bytes.
	backup := `{
		"version":1, "type":"full",
		"media":[
			{"filename":"x.png","hash":"NOT-A-HASH","tags":[],"archive_path":"media/x.png"}
		]
	}`
	zr := buildZip(t, map[string]string{
		"backup.json": backup,
		"media/x.png": "x",
	})
	res, err := Translate(zr.File, "blombooru")
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if got := res.Manifest.Images[0].SHA256; got != "" {
		t.Errorf("SHA256 = %q, want empty", got)
	}
}

func TestTranslate_HydrusHappyPath(t *testing.T) {
	// Hash-shaped basename → SHA256 propagates. Sidecar contributes tags.
	const sha = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789"
	zr := buildZip(t, map[string]string{
		sha + ".png":     "PNGBYTES",
		sha + ".png.txt": "cat\n# comment line\n\nartist:foo\n",
	})
	res, err := Translate(zr.File, "hydrus")
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if len(res.Manifest.Images) != 1 {
		t.Fatalf("Manifest.Images = %d, want 1", len(res.Manifest.Images))
	}
	got := res.Manifest.Images[0]
	if got.SHA256 != sha {
		t.Errorf("SHA256 = %q, want hydrus-style basename hash", got.SHA256)
	}
	wantTags := []string{"cat", "artist:foo"}
	if !reflect.DeepEqual(got.Tags, wantTags) {
		t.Errorf("Tags = %v, want %v (blank lines and # comments stripped)", got.Tags, wantTags)
	}
}

func TestTranslate_HydrusNoSidecar(t *testing.T) {
	zr := buildZip(t, map[string]string{
		"plain.png": "x",
	})
	// Hydrus detect requires at least one sidecar somewhere; this zip
	// would not detect, but Translate is still callable directly.
	res, err := Translate(zr.File, "hydrus")
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if len(res.Manifest.Images) != 1 {
		t.Fatalf("Manifest.Images = %d, want 1", len(res.Manifest.Images))
	}
	got := res.Manifest.Images[0]
	if got.SHA256 != "" {
		t.Errorf("SHA256 = %q, want empty (basename is not 64 hex chars)", got.SHA256)
	}
	if len(got.Tags) != 0 {
		t.Errorf("Tags = %v, want empty (no sidecar)", got.Tags)
	}
}

func TestTranslate_UnknownFormat(t *testing.T) {
	zr := buildZip(t, map[string]string{"x": "y"})
	if _, err := Translate(zr.File, "wat"); err == nil {
		t.Error("Translate(unknown) returned nil error")
	}
}
