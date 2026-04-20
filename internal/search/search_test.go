package search

import (
	"context"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leqwin/monbooru/internal/db"
	"github.com/leqwin/monbooru/internal/gallery"
)

func TestParse_BasicTag(t *testing.T) {
	e, err := Parse("cute")
	if err != nil {
		t.Fatal(err)
	}
	tag, ok := e.(TagExpr)
	if !ok {
		t.Fatalf("expected TagExpr, got %T", e)
	}
	if tag.Tag != "cute" {
		t.Errorf("tag = %q", tag.Tag)
	}
}

func TestParse_ImplicitAND(t *testing.T) {
	e, _ := Parse("a b")
	and, ok := e.(AndExpr)
	if !ok {
		t.Fatalf("expected AndExpr, got %T", e)
	}
	left, ok := and.Left.(TagExpr)
	if !ok || left.Tag != "a" {
		t.Errorf("Left = %+v, want TagExpr{a}", and.Left)
	}
	right, ok := and.Right.(TagExpr)
	if !ok || right.Tag != "b" {
		t.Errorf("Right = %+v, want TagExpr{b}", and.Right)
	}
}

func TestParse_OR(t *testing.T) {
	e, _ := Parse("cat OR dog")
	or, ok := e.(OrExpr)
	if !ok {
		t.Fatalf("expected OrExpr, got %T", e)
	}
	_ = or
}

func TestParse_ORChain(t *testing.T) {
	// Chained OR used to silently drop everything past the first pair;
	// `a OR b OR c` should produce three leaves, not two.
	e, _ := Parse("a OR b OR c")
	or, ok := e.(OrExpr)
	if !ok {
		t.Fatalf("expected OrExpr at root, got %T", e)
	}
	leftOr, ok := or.Left.(OrExpr)
	if !ok {
		t.Fatalf("expected nested OrExpr on left, got %T", or.Left)
	}
	if got := leftOr.Left.(TagExpr).Tag; got != "a" {
		t.Errorf("leftOr.Left = %q, want a", got)
	}
	if got := leftOr.Right.(TagExpr).Tag; got != "b" {
		t.Errorf("leftOr.Right = %q, want b", got)
	}
	if got := or.Right.(TagExpr).Tag; got != "c" {
		t.Errorf("or.Right = %q, want c", got)
	}
}

func TestParse_NOT(t *testing.T) {
	e, _ := Parse("-blonde_hair")
	not, ok := e.(NotExpr)
	if !ok {
		t.Fatalf("expected NotExpr, got %T", e)
	}
	_ = not
}

func TestParse_NOT_Keyword(t *testing.T) {
	e, _ := Parse("NOT blonde_hair")
	not, ok := e.(NotExpr)
	if !ok {
		t.Fatalf("expected NotExpr, got %T", e)
	}
	_ = not
}

func TestParse_Filter_Fav(t *testing.T) {
	e, _ := Parse("fav:true")
	f, ok := e.(FilterExpr)
	if !ok {
		t.Fatalf("expected FilterExpr, got %T", e)
	}
	if f.Key != "fav" || f.Val != "true" {
		t.Errorf("filter = {%q, %q}", f.Key, f.Val)
	}
}

func TestParse_Filter_Folder(t *testing.T) {
	e, _ := Parse("folder:2024/jan")
	f, ok := e.(FilterExpr)
	if !ok {
		t.Fatalf("expected FilterExpr, got %T", e)
	}
	if f.Key != "folder" || f.Val != "2024/jan" {
		t.Errorf("filter = {%q, %q}", f.Key, f.Val)
	}
}

func TestParse_Filter_Source(t *testing.T) {
	e, _ := Parse("source:sd")
	f, ok := e.(FilterExpr)
	if !ok || f.Key != "source" || f.Val != "sd" {
		t.Errorf("parse source:sd failed")
	}
}

func TestParse_Wildcard_Prefix(t *testing.T) {
	e, _ := Parse("blue*")
	tag, ok := e.(TagExpr)
	if !ok || tag.Wildcard != "prefix" || tag.Tag != "blue" {
		t.Errorf("expected prefix wildcard, got %+v", e)
	}
}

func TestParse_Wildcard_Substring(t *testing.T) {
	e, _ := Parse("*blue*")
	tag, ok := e.(TagExpr)
	if !ok || tag.Wildcard != "substring" || tag.Tag != "blue" {
		t.Errorf("expected substring wildcard, got %+v", e)
	}
}

func TestParse_Empty(t *testing.T) {
	e, err := Parse("")
	if err != nil {
		t.Fatal(err)
	}
	if e != nil {
		t.Error("expected nil for empty query")
	}
}

func TestBuildWhere_FolderFilter(t *testing.T) {
	// folder:PATH is recursive: images in PATH or any subfolder under it.
	expr := FilterExpr{Key: "folder", Val: "2024/jan"}
	where, args, _ := buildWhere(expr)
	if len(args) != 2 {
		t.Fatalf("expected 2 args, got %d", len(args))
	}
	if args[0] != "2024/jan" {
		t.Errorf("arg[0] = %v", args[0])
	}
	if args[1] != "2024/jan/%" {
		t.Errorf("arg[1] = %v", args[1])
	}
	if !strings.Contains(where, "folder_path") || !strings.Contains(where, "LIKE") {
		t.Errorf("where clause should combine exact and LIKE match: %s", where)
	}
}

func TestBuildWhere_FolderFilterEmpty(t *testing.T) {
	// `folder:` (empty value) is now the recursive-root case: every
	// non-missing image lives at or under the root, so the filter is a
	// no-op (`1=1`). Root-only stays reachable via `folderonly:`.
	expr := FilterExpr{Key: "folder", Val: ""}
	where, args, _ := buildWhere(expr)
	if len(args) != 0 {
		t.Fatalf("expected 0 args for empty folder, got %d", len(args))
	}
	if strings.Contains(where, "folder_path") {
		t.Errorf("empty folder: should no longer constrain folder_path: %s", where)
	}
}

func TestBuildWhere_FolderOnlyFilter(t *testing.T) {
	// folderonly:PATH is exact: only images whose folder_path matches verbatim.
	expr := FilterExpr{Key: "folderonly", Val: "2024/jan"}
	where, args, _ := buildWhere(expr)
	if len(args) != 1 {
		t.Fatalf("expected 1 arg, got %d", len(args))
	}
	if args[0] != "2024/jan" {
		t.Errorf("arg[0] = %v", args[0])
	}
	if !strings.Contains(where, "folder_path = ?") {
		t.Errorf("where clause should match folder_path exactly: %s", where)
	}
}

func TestBuildWhere_FolderOnlyFilterEmpty(t *testing.T) {
	// folderonly: (empty) is the gallery root directly, no recursion.
	expr := FilterExpr{Key: "folderonly", Val: ""}
	where, args, _ := buildWhere(expr)
	if len(args) != 0 {
		t.Fatalf("expected 0 args for empty folderonly, got %d", len(args))
	}
	if !strings.Contains(where, "folder_path = ''") {
		t.Errorf("where clause for root-only should match empty folder_path: %s", where)
	}
}

func TestBuildWhere_MissingTrue(t *testing.T) {
	expr := FilterExpr{Key: "missing", Val: "true"}
	_, _, hasMissing := buildWhere(expr)
	if !hasMissing {
		t.Error("expected hasMissingFilter = true")
	}
}

func TestBuildWhere_AnimatedTrue(t *testing.T) {
	expr := FilterExpr{Key: "animated", Val: "true"}
	where, args, _ := buildWhere(expr)
	if len(args) != 0 {
		t.Fatalf("expected 0 args, got %d", len(args))
	}
	if !strings.Contains(where, "file_type IN ('gif', 'mp4', 'webm')") {
		t.Errorf("where clause missing animated set: %s", where)
	}
}

func TestBuildWhere_AnimatedFalse(t *testing.T) {
	expr := FilterExpr{Key: "animated", Val: "false"}
	where, _, _ := buildWhere(expr)
	if !strings.Contains(where, "file_type NOT IN ('gif', 'mp4', 'webm')") {
		t.Errorf("where clause missing negated animated set: %s", where)
	}
}

func TestBuildWhere_TaggedTrue(t *testing.T) {
	expr := FilterExpr{Key: "tagged", Val: "true"}
	where, args, _ := buildWhere(expr)
	if len(args) != 0 {
		t.Fatalf("expected 0 args, got %d", len(args))
	}
	if !strings.Contains(where, "EXISTS (SELECT 1 FROM image_tags") {
		t.Errorf("where clause missing tagged subselect: %s", where)
	}
	if strings.Contains(where, "NOT EXISTS") {
		t.Errorf("tagged:true should match tagged images, got: %s", where)
	}
}

func TestBuildWhere_TaggedFalse(t *testing.T) {
	expr := FilterExpr{Key: "tagged", Val: "false"}
	where, _, _ := buildWhere(expr)
	if !strings.Contains(where, "NOT EXISTS (SELECT 1 FROM image_tags") {
		t.Errorf("where clause missing untagged subselect: %s", where)
	}
}

func TestBuildWhere_AutotaggedTrue(t *testing.T) {
	expr := FilterExpr{Key: "autotagged", Val: "true"}
	where, args, _ := buildWhere(expr)
	if len(args) != 0 {
		t.Fatalf("expected 0 args, got %d", len(args))
	}
	if !strings.Contains(where, "it.is_auto = 1") {
		t.Errorf("where clause missing is_auto filter: %s", where)
	}
	if strings.Contains(where, "NOT EXISTS") {
		t.Errorf("autotagged:true should match auto-tagged images, got: %s", where)
	}
}

func TestBuildWhere_AutotaggedFalse(t *testing.T) {
	expr := FilterExpr{Key: "autotagged", Val: "false"}
	where, _, _ := buildWhere(expr)
	if !strings.Contains(where, "NOT EXISTS (SELECT 1 FROM image_tags") {
		t.Errorf("where clause missing NOT EXISTS: %s", where)
	}
	if !strings.Contains(where, "it.is_auto = 1") {
		t.Errorf("where clause missing is_auto filter: %s", where)
	}
}

// Integration test with real DB
type searchEnv struct {
	db            *db.DB
	galleryDir    string
	thumbnailsDir string
	maxFileSizeMB int
}

func setupSearchDB(t *testing.T) (*db.DB, *searchEnv) {
	t.Helper()
	tmpDir := t.TempDir()
	galleryDir := filepath.Join(tmpDir, "gallery")
	os.MkdirAll(galleryDir, 0755)

	database, err := db.Open(filepath.Join(tmpDir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Bootstrap(database); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })

	return database, &searchEnv{
		db:            database,
		galleryDir:    galleryDir,
		thumbnailsDir: filepath.Join(tmpDir, "thumbs"),
		maxFileSizeMB: 100,
	}
}

var ingestCounter int

func ingestTestImage(t *testing.T, database *db.DB, env *searchEnv, name string) {
	t.Helper()
	ingestCounter++
	img := image.NewRGBA(image.Rect(0, 0, 10+ingestCounter, 10))
	path := filepath.Join(env.galleryDir, name)
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(f, img); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := gallery.Ingest(database, env.galleryDir, env.thumbnailsDir, path, "png"); err != nil {
		t.Fatalf("ingest %q: %v", name, err)
	}
}

func TestExecute_BasicSearch(t *testing.T) {
	database, env := setupSearchDB(t)
	ingestTestImage(t, database, env, "test.png")

	q := Query{Sort: "newest", Page: 1, Limit: 40}
	result, err := Execute(database, q)
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 1 {
		t.Errorf("Total = %d, want 1", result.Total)
	}
}

func TestExecute_FolderFilter(t *testing.T) {
	database, env := setupSearchDB(t)

	subDir := filepath.Join(env.galleryDir, "2024")
	os.MkdirAll(subDir, 0755)
	ingestTestImage(t, database, env, "root.png")

	img := image.NewRGBA(image.Rect(0, 0, 10, 10))
	path := filepath.Join(subDir, "sub.png")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(f, img); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()
	if _, _, err := gallery.Ingest(database, env.galleryDir, env.thumbnailsDir, path, "png"); err != nil {
		t.Fatalf("ingest sub.png: %v", err)
	}

	// Search for gallery root only
	if _, err := Execute(database, Query{
		Expr:  FilterExpr{Key: "folder", Val: ""},
		Page:  1,
		Limit: 40,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestExecute_FullSync(t *testing.T) {
	database, env := setupSearchDB(t)
	ingestTestImage(t, database, env, "a.png")
	ingestTestImage(t, database, env, "b.png")

	gallery.Sync(context.Background(), database, env.galleryDir, env.thumbnailsDir, env.maxFileSizeMB, func(int, int, string) {})

	result, err := Execute(database, Query{Page: 1, Limit: 40})
	if err != nil {
		t.Fatal(err)
	}
	if result.Total < 2 {
		t.Errorf("Total = %d, want >= 2", result.Total)
	}
}

// --- buildOrder tests ---

func TestBuildOrder_Newest(t *testing.T) {
	got := buildOrder("newest", "", 0)
	if !strings.Contains(got, "DESC") || !strings.Contains(got, "ingested_at") {
		t.Errorf("newest default: %q", got)
	}
}

func TestBuildOrder_NewestAsc(t *testing.T) {
	got := buildOrder("newest", "asc", 0)
	if !strings.Contains(got, "ASC") {
		t.Errorf("newest asc: %q", got)
	}
}

func TestBuildOrder_Filesize(t *testing.T) {
	got := buildOrder("filesize", "", 0)
	if !strings.Contains(got, "file_size") || !strings.Contains(got, "DESC") {
		t.Errorf("filesize: %q", got)
	}
}

func TestBuildOrder_FilesizeAsc(t *testing.T) {
	got := buildOrder("filesize", "asc", 0)
	if !strings.Contains(got, "file_size") || !strings.Contains(got, "ASC") {
		t.Errorf("filesize asc: %q", got)
	}
}

func TestBuildOrder_Unknown(t *testing.T) {
	// Unknown sorts fall back to newest (ingested_at DESC)
	got := buildOrder("unknown_sort", "", 0)
	if !strings.Contains(got, "ingested_at") || !strings.Contains(got, "DESC") {
		t.Errorf("unknown sort: %q", got)
	}
}

func TestBuildOrder_Random(t *testing.T) {
	got := buildOrder("random", "", 12345)
	if !strings.Contains(got, "12345") {
		t.Errorf("random: expected seed in order clause, got %q", got)
	}
}

// --- buildTagExpr tests ---

func TestBuildWhere_TagExact(t *testing.T) {
	expr := TagExpr{Tag: "cute", Wildcard: ""}
	where, args, _ := buildWhere(expr)
	if len(args) != 1 || args[0] != "cute" {
		t.Errorf("args = %v", args)
	}
	if !strings.Contains(where, "t.name = ?") {
		t.Errorf("where = %q", where)
	}
}

func TestBuildWhere_TagPrefix(t *testing.T) {
	expr := TagExpr{Tag: "blue", Wildcard: "prefix"}
	where, args, _ := buildWhere(expr)
	if len(args) != 1 || args[0] != "blue%" {
		t.Errorf("args = %v", args)
	}
	if !strings.Contains(where, "LIKE ?") {
		t.Errorf("where = %q", where)
	}
}

func TestBuildWhere_TagSubstring(t *testing.T) {
	expr := TagExpr{Tag: "hair", Wildcard: "substring"}
	_, args, _ := buildWhere(expr)
	if len(args) != 1 || args[0] != "%hair%" {
		t.Errorf("args = %v", args)
	}
}

// --- buildFilterExpr tests ---

func TestBuildWhere_FavFalse(t *testing.T) {
	expr := FilterExpr{Key: "fav", Val: "false"}
	where, args, _ := buildWhere(expr)
	if len(args) != 0 {
		t.Errorf("expected no args for fav:false")
	}
	if !strings.Contains(where, "is_favorited = 0") {
		t.Errorf("where = %q", where)
	}
}

func TestBuildWhere_Source(t *testing.T) {
	// "sd" is aliased to "a1111"; source filter uses 4 LIKE args for comma-separated types.
	expr := FilterExpr{Key: "source", Val: "sd"}
	where, args, _ := buildWhere(expr)
	if len(args) != 4 {
		t.Errorf("expected 4 args, got %d: %v", len(args), args)
	}
	if len(args) > 0 && args[0] != "a1111" {
		t.Errorf("args[0] = %v, want a1111", args[0])
	}
	if !strings.Contains(where, "source_type") {
		t.Errorf("where = %q, expected source_type", where)
	}
}

func TestBuildWhere_SourceAI(t *testing.T) {
	// "ai" expands inline to match a1111, comfyui, and the combined source type;
	// no bound args are needed because the values are inlined into the SQL.
	expr := FilterExpr{Key: "source", Val: "ai"}
	where, args, _ := buildWhere(expr)
	if len(args) != 0 {
		t.Errorf("expected no args for source:ai, got %v", args)
	}
	for _, want := range []string{"'a1111'", "'comfyui'", "'a1111,comfyui'"} {
		if !strings.Contains(where, want) {
			t.Errorf("where = %q, missing %s", where, want)
		}
	}
}

func TestBuildWhere_Cat(t *testing.T) {
	expr := FilterExpr{Key: "cat", Val: "character"}
	where, args, _ := buildWhere(expr)
	if len(args) != 1 || args[0] != "character" {
		t.Errorf("args = %v", args)
	}
	if !strings.Contains(where, "tc.name = ?") {
		t.Errorf("where = %q", where)
	}
}

func TestBuildWhere_Width(t *testing.T) {
	expr := FilterExpr{Key: "width", Val: ">=1920"}
	where, args, _ := buildWhere(expr)
	if len(args) != 1 || args[0] != int64(1920) {
		t.Errorf("args = %v, want int64(1920)", args)
	}
	if !strings.Contains(where, "i.width") {
		t.Errorf("where = %q", where)
	}
}

func TestBuildWhere_Height(t *testing.T) {
	expr := FilterExpr{Key: "height", Val: "<768"}
	where, args, _ := buildWhere(expr)
	if len(args) != 1 || args[0] != int64(768) {
		t.Errorf("args = %v", args)
	}
	if !strings.Contains(where, "i.height") {
		t.Errorf("where = %q", where)
	}
}

func TestBuildWhere_WidthNonNumericRejected(t *testing.T) {
	// `width:>=abc` used to bind the string "abc" into the SQL and let
	// SQLite silently coerce it to 0, matching every row with width >= 0.
	// Now it produces an explicit `1=0` so the user sees an empty result
	// instead of a too-wide one.
	expr := FilterExpr{Key: "width", Val: ">=abc"}
	where, args, _ := buildWhere(expr)
	if len(args) != 0 {
		t.Errorf("args = %v, want none", args)
	}
	if !strings.Contains(where, "1=0") {
		t.Errorf("where = %q, expected 1=0", where)
	}
}

func TestBuildWhere_MissingFalse(t *testing.T) {
	// `missing:false` (and `missing:true`) opt out of the auto-injected
	// `AND is_missing = 0`; the explicit clause speaks for itself, and
	// without the opt-out a negation like `-missing:false` would
	// collapse to a contradiction and match nothing.
	expr := FilterExpr{Key: "missing", Val: "false"}
	where, _, hasMissing := buildWhere(expr)
	if !hasMissing {
		t.Error("expected hasMissingFilter = true for missing:false")
	}
	if !strings.Contains(where, "is_missing = 0") {
		t.Errorf("where = %q", where)
	}
}

func TestBuildWhere_NegatedMissingFalse(t *testing.T) {
	// `-missing:false` should mean "show me missing images" - equivalent
	// to `missing:true`. Previously the auto-injected
	// `AND is_missing = 0` clause was still appended on top of the
	// negation and the query collapsed to zero results.
	expr := NotExpr{Expr: FilterExpr{Key: "missing", Val: "false"}}
	where, _, _ := buildWhere(expr)
	if !strings.Contains(where, "NOT") {
		t.Errorf("where missing NOT: %q", where)
	}
	// The auto-injection must NOT have been appended; otherwise the
	// caller (Execute) would build a contradictory clause.
	if strings.Contains(where, "AND i.is_missing = 0") {
		t.Errorf("where should not include the auto-clause: %q", where)
	}
}

func TestBuildWhere_UnknownFilter(t *testing.T) {
	// Unknown keys with a non-empty value are treated as category-qualified tag searches.
	expr := FilterExpr{Key: "bogus", Val: "val"}
	where, args, _ := buildWhere(expr)
	if !strings.Contains(where, "EXISTS") {
		t.Errorf("unknown key:val should yield category-qualified EXISTS clause, got %q", where)
	}
	if len(args) != 2 {
		t.Errorf("expected 2 args (tag name + category name), got %d: %v", len(args), args)
	}

	// Unknown key with empty value → 1=1
	expr2 := FilterExpr{Key: "bogus", Val: ""}
	where2, _, _ := buildWhere(expr2)
	if where2 != "1=1" {
		t.Errorf("unknown filter with empty val should yield 1=1, got %q", where2)
	}
}

func TestBuildWhereDB_ColonFallsBackToLiteral(t *testing.T) {
	// When the prefix before `:` is not a real category, the DB-aware
	// builder must match the whole token as a literal tag name so
	// colon-bearing tags like "nier:automata" stay searchable by typing
	// them verbatim.
	database, _ := setupSearchDB(t)

	expr := FilterExpr{Key: "nier", Val: "automata"}
	where, args, _ := buildWhereDB(expr, database)
	if strings.Contains(where, "tc.name") {
		t.Errorf("literal-tag branch should not reference tc.name, got: %q", where)
	}
	if len(args) != 1 || args[0] != "nier:automata" {
		t.Errorf("args = %v, want [nier:automata]", args)
	}
}

func TestBuildWhereDB_ColonUsesCategoryWhenPrefixExists(t *testing.T) {
	// When the prefix IS a real category name, the DB-aware builder must
	// keep the old "category-qualified" behaviour so `artist:foo` still
	// searches for the tag `foo` in the `artist` category.
	database, _ := setupSearchDB(t)

	expr := FilterExpr{Key: "artist", Val: "foo"}
	where, args, _ := buildWhereDB(expr, database)
	if !strings.Contains(where, "tc.name = ?") {
		t.Errorf("category-qualified branch should reference tc.name, got: %q", where)
	}
	if len(args) != 2 || args[0] != "foo" || args[1] != "artist" {
		t.Errorf("args = %v, want [foo artist]", args)
	}
}

// --- buildDateFilter tests ---

func TestBuildDateFilter_After(t *testing.T) {
	b := &whereBuilder{}
	clause := b.buildDateFilter(">2024-01-01")
	if !strings.Contains(clause, "> ?") || b.args[0] != "2024-01-01" {
		t.Errorf("clause = %q, args = %v", clause, b.args)
	}
}

func TestBuildDateFilter_Before(t *testing.T) {
	b := &whereBuilder{}
	clause := b.buildDateFilter("<2024-12-31")
	if !strings.Contains(clause, "< ?") || b.args[0] != "2024-12-31" {
		t.Errorf("clause = %q, args = %v", clause, b.args)
	}
}

func TestBuildDateFilter_Range(t *testing.T) {
	b := &whereBuilder{}
	clause := b.buildDateFilter("2024-01-01..2024-12-31")
	if !strings.Contains(clause, "BETWEEN") {
		t.Errorf("range clause = %q", clause)
	}
	if len(b.args) != 2 {
		t.Errorf("expected 2 args, got %d", len(b.args))
	}
}

func TestBuildDateFilter_Exact(t *testing.T) {
	b := &whereBuilder{}
	clause := b.buildDateFilter("2024-06-15")
	if !strings.Contains(clause, "BETWEEN") {
		t.Errorf("exact date clause = %q", clause)
	}
}

// --- parseCompOp tests ---

func TestParseCompOp_GTE(t *testing.T) {
	op, val := parseCompOp(">=1920")
	if op != ">=" || val != "1920" {
		t.Errorf("op=%q val=%q", op, val)
	}
}

func TestParseCompOp_LTE(t *testing.T) {
	op, val := parseCompOp("<=768")
	if op != "<=" || val != "768" {
		t.Errorf("op=%q val=%q", op, val)
	}
}

func TestParseCompOp_GT(t *testing.T) {
	op, val := parseCompOp(">100")
	if op != ">" || val != "100" {
		t.Errorf("op=%q val=%q", op, val)
	}
}

func TestParseCompOp_LT(t *testing.T) {
	op, val := parseCompOp("<200")
	if op != "<" || val != "200" {
		t.Errorf("op=%q val=%q", op, val)
	}
}

func TestParseCompOp_EQ(t *testing.T) {
	op, val := parseCompOp("=1024")
	if op != "=" || val != "1024" {
		t.Errorf("op=%q val=%q", op, val)
	}
}

func TestParseCompOp_Default(t *testing.T) {
	op, val := parseCompOp("512")
	if op != "=" || val != "512" {
		t.Errorf("default op=%q val=%q", op, val)
	}
}

// --- buildExpr OR/NOT/AND tests ---

func TestBuildWhere_OR(t *testing.T) {
	expr := OrExpr{
		Left:  TagExpr{Tag: "cat"},
		Right: TagExpr{Tag: "dog"},
	}
	where, args, _ := buildWhere(expr)
	if !strings.Contains(where, "OR") {
		t.Errorf("OR clause = %q", where)
	}
	if len(args) != 2 {
		t.Errorf("expected 2 args, got %d", len(args))
	}
}

func TestBuildWhere_NOT(t *testing.T) {
	expr := NotExpr{Expr: TagExpr{Tag: "ugly"}}
	where, _, _ := buildWhere(expr)
	if !strings.Contains(where, "NOT") {
		t.Errorf("NOT clause = %q", where)
	}
}

func TestBuildWhere_AND(t *testing.T) {
	expr := AndExpr{
		Left:  TagExpr{Tag: "cat"},
		Right: TagExpr{Tag: "cute"},
	}
	where, args, _ := buildWhere(expr)
	if !strings.Contains(where, "AND") {
		t.Errorf("AND clause = %q", where)
	}
	if len(args) != 2 {
		t.Errorf("expected 2 args, got %d", len(args))
	}
}

func TestBuildWhere_AND_LeftUnknown(t *testing.T) {
	// Unknown filter produces "1=1" - AND should still work
	expr := AndExpr{
		Left:  FilterExpr{Key: "bogus", Val: ""},
		Right: TagExpr{Tag: "cute"},
	}
	_, args, _ := buildWhere(expr)
	// Should have 1 arg for the tag search
	if len(args) != 1 {
		t.Errorf("expected 1 arg, got %d: %v", len(args), args)
	}
}

// --- SidebarTagsWithGlobalCount test ---

func TestSidebarTagsWithGlobalCount_Empty(t *testing.T) {
	database, _ := setupSearchDB(t)
	tags, err := SidebarTagsWithGlobalCount(database, nil)
	if err != nil {
		t.Fatal(err)
	}
	if tags != nil {
		t.Error("expected nil for empty image IDs")
	}
}

func TestSidebarTagsWithGlobalCount_WithImages(t *testing.T) {
	database, env := setupSearchDB(t)
	ingestTestImage(t, database, env, "a.png")
	ingestTestImage(t, database, env, "b.png")

	// Both images get a shared tag so the sidebar aggregator has work.
	var catID int64
	if err := database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name='general'`).Scan(&catID); err != nil {
		t.Fatal(err)
	}
	var tagID int64
	res, err := database.Write.Exec(`INSERT INTO tags (name, category_id, usage_count) VALUES ('shared', ?, 2)`, catID)
	if err != nil {
		t.Fatal(err)
	}
	tagID, _ = res.LastInsertId()

	result, err := Execute(database, Query{Page: 1, Limit: 40})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Results) != 2 {
		t.Fatalf("expected 2 images, got %d", len(result.Results))
	}
	ids := make([]int64, 0, len(result.Results))
	for _, img := range result.Results {
		ids = append(ids, img.ID)
		if _, err := database.Write.Exec(
			`INSERT INTO image_tags (image_id, tag_id, is_auto) VALUES (?, ?, 0)`, img.ID, tagID,
		); err != nil {
			t.Fatal(err)
		}
	}

	got, err := SidebarTagsWithGlobalCount(database, ids)
	if err != nil {
		t.Fatal(err)
	}
	var found *int
	for i, tag := range got {
		if tag.Name == "shared" {
			i := i
			found = &i
			break
		}
	}
	if found == nil {
		t.Fatalf("expected tag 'shared' in sidebar aggregator, got %+v", got)
	}
	if got[*found].UsageCount != 2 {
		t.Errorf("shared tag usage = %d, want 2", got[*found].UsageCount)
	}
}

// --- Execute with sort/filter integration tests ---

func TestExecute_SortFilesize(t *testing.T) {
	database, cfg := setupSearchDB(t)
	ingestTestImage(t, database, cfg, "s1.png")
	ingestTestImage(t, database, cfg, "s2.png")

	result, err := Execute(database, Query{Sort: "filesize", Page: 1, Limit: 40})
	if err != nil {
		t.Fatal(err)
	}
	if result.Total < 2 {
		t.Errorf("Total = %d, want >= 2", result.Total)
	}
}

func TestExecute_SortRandom(t *testing.T) {
	database, cfg := setupSearchDB(t)
	ingestTestImage(t, database, cfg, "r1.png")

	_, err := Execute(database, Query{Sort: "random", Page: 1, Limit: 40})
	if err != nil {
		t.Fatal(err)
	}
}

func TestExecute_FavFilter(t *testing.T) {
	database, cfg := setupSearchDB(t)
	ingestTestImage(t, database, cfg, "fav1.png")

	expr := FilterExpr{Key: "fav", Val: "true"}
	result, err := Execute(database, Query{Expr: expr, Page: 1, Limit: 40})
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 0 {
		t.Errorf("expected 0 favorited images, got %d", result.Total)
	}
}

func TestExecute_TagSearch(t *testing.T) {
	database, cfg := setupSearchDB(t)
	ingestTestImage(t, database, cfg, "tagged.png")

	// Even without tags, tag search should return correct results
	expr := TagExpr{Tag: "nonexistent_tag_xyz"}
	result, err := Execute(database, Query{Expr: expr, Page: 1, Limit: 40})
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 0 {
		t.Errorf("expected 0 results for nonexistent tag, got %d", result.Total)
	}
}

func TestExecute_SkipCount(t *testing.T) {
	database, cfg := setupSearchDB(t)
	ingestTestImage(t, database, cfg, "sc1.png")
	ingestTestImage(t, database, cfg, "sc2.png")

	result, err := Execute(database, Query{Page: 1, Limit: 40, SkipCount: true})
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 0 {
		t.Errorf("Total = %d, want 0 (skip-count)", result.Total)
	}
	if len(result.Results) != 2 {
		t.Errorf("Results = %d, want 2", len(result.Results))
	}
}

func TestExecute_DefaultPagination(t *testing.T) {
	database, cfg := setupSearchDB(t)
	ingestTestImage(t, database, cfg, "p1.png")

	// page=0 and limit=0 should use defaults
	result, err := Execute(database, Query{Page: 0, Limit: 0})
	if err != nil {
		t.Fatal(err)
	}
	if result.Page != 1 {
		t.Errorf("default page = %d, want 1", result.Page)
	}
	if result.Limit != 40 {
		t.Errorf("default limit = %d, want 40", result.Limit)
	}
}

func TestExecuteAdjacent_Newest(t *testing.T) {
	database, cfg := setupSearchDB(t)
	ingestTestImage(t, database, cfg, "adj_a.png")
	ingestTestImage(t, database, cfg, "adj_b.png")
	ingestTestImage(t, database, cfg, "adj_c.png")

	result, err := Execute(database, Query{Sort: "newest", Order: "desc", Page: 1, Limit: 40})
	if err != nil || len(result.Results) != 3 {
		t.Fatalf("setup Execute: err=%v len=%d", err, len(result.Results))
	}
	// result.Results is sorted newest→oldest: [newest, middle, oldest].
	newest, middle, oldest := result.Results[0].ID, result.Results[1].ID, result.Results[2].ID

	// Middle image: prev is the newer (newest), next is the older (oldest).
	prev, next, err := ExecuteAdjacent(database, Query{Sort: "newest", Order: "desc"}, middle)
	if err != nil {
		t.Fatal(err)
	}
	if prev == nil || *prev != newest {
		t.Errorf("middle prev = %v, want %d", prev, newest)
	}
	if next == nil || *next != oldest {
		t.Errorf("middle next = %v, want %d", next, oldest)
	}

	// Edge: newest has no prev, still has next.
	prev, next, _ = ExecuteAdjacent(database, Query{Sort: "newest", Order: "desc"}, newest)
	if prev != nil {
		t.Errorf("newest prev = %v, want nil", prev)
	}
	if next == nil || *next != middle {
		t.Errorf("newest next = %v, want %d", next, middle)
	}

	// Edge: oldest has no next, still has prev.
	prev, next, _ = ExecuteAdjacent(database, Query{Sort: "newest", Order: "desc"}, oldest)
	if next != nil {
		t.Errorf("oldest next = %v, want nil", next)
	}
	if prev == nil || *prev != middle {
		t.Errorf("oldest prev = %v, want %d", prev, middle)
	}
}

func TestExecuteAdjacent_Random(t *testing.T) {
	database, cfg := setupSearchDB(t)
	ingestTestImage(t, database, cfg, "rnd_a.png")
	ingestTestImage(t, database, cfg, "rnd_b.png")
	ingestTestImage(t, database, cfg, "rnd_c.png")

	const seed int64 = 1234567
	q := Query{Sort: "random", RandomSeed: seed, Page: 1, Limit: 40}
	result, err := Execute(database, q)
	if err != nil || len(result.Results) != 3 {
		t.Fatalf("setup Execute: err=%v len=%d", err, len(result.Results))
	}
	first, second, third := result.Results[0].ID, result.Results[1].ID, result.Results[2].ID

	prev, next, err := ExecuteAdjacent(database, q, second)
	if err != nil {
		t.Fatal(err)
	}
	if prev == nil || *prev != first {
		t.Errorf("middle prev = %v, want %d", prev, first)
	}
	if next == nil || *next != third {
		t.Errorf("middle next = %v, want %d", next, third)
	}

	prev, next, _ = ExecuteAdjacent(database, q, first)
	if prev != nil {
		t.Errorf("first prev = %v, want nil", prev)
	}
	if next == nil || *next != second {
		t.Errorf("first next = %v, want %d", next, second)
	}

	prev, next, _ = ExecuteAdjacent(database, q, third)
	if next != nil {
		t.Errorf("third next = %v, want nil", next)
	}
	if prev == nil || *prev != second {
		t.Errorf("third prev = %v, want %d", prev, second)
	}
}

func TestExecuteAdjacent_RandomNoSeed(t *testing.T) {
	database, cfg := setupSearchDB(t)
	ingestTestImage(t, database, cfg, "rnd_no_seed.png")
	prev, next, err := ExecuteAdjacent(database, Query{Sort: "random"}, 1)
	if err != nil || prev != nil || next != nil {
		t.Errorf("random adjacency without seed must be nil/nil, got prev=%v next=%v err=%v", prev, next, err)
	}
}

func TestExecutePosition_Newest(t *testing.T) {
	database, cfg := setupSearchDB(t)
	ingestTestImage(t, database, cfg, "pos_a.png")
	ingestTestImage(t, database, cfg, "pos_b.png")
	ingestTestImage(t, database, cfg, "pos_c.png")

	result, err := Execute(database, Query{Sort: "newest", Order: "desc", Page: 1, Limit: 40})
	if err != nil || len(result.Results) != 3 {
		t.Fatalf("setup Execute: err=%v len=%d", err, len(result.Results))
	}
	newest, middle, oldest := result.Results[0].ID, result.Results[1].ID, result.Results[2].ID

	cases := []struct {
		name    string
		id      int64
		wantPos int
	}{
		{"newest is #1", newest, 1},
		{"middle is #2", middle, 2},
		{"oldest is #3", oldest, 3},
	}
	for _, c := range cases {
		pos, total, err := ExecutePosition(database, Query{Sort: "newest", Order: "desc"}, c.id)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if pos != c.wantPos {
			t.Errorf("%s: pos = %d, want %d", c.name, pos, c.wantPos)
		}
		if total != 3 {
			t.Errorf("%s: total = %d, want 3", c.name, total)
		}
	}
}

func TestExecutePosition_AscFlipsOrder(t *testing.T) {
	database, cfg := setupSearchDB(t)
	ingestTestImage(t, database, cfg, "pos_asc_a.png")
	ingestTestImage(t, database, cfg, "pos_asc_b.png")
	ingestTestImage(t, database, cfg, "pos_asc_c.png")

	// Ascending ingestion order means the first-ingested image is #1.
	result, _ := Execute(database, Query{Sort: "newest", Order: "asc", Page: 1, Limit: 40})
	if len(result.Results) != 3 {
		t.Fatalf("setup: got %d rows, want 3", len(result.Results))
	}
	first := result.Results[0].ID

	pos, total, err := ExecutePosition(database, Query{Sort: "newest", Order: "asc"}, first)
	if err != nil {
		t.Fatal(err)
	}
	if pos != 1 || total != 3 {
		t.Errorf("asc first: pos=%d total=%d, want 1/3", pos, total)
	}
}

func TestExecutePosition_Random(t *testing.T) {
	database, cfg := setupSearchDB(t)
	ingestTestImage(t, database, cfg, "pos_rnd_a.png")
	ingestTestImage(t, database, cfg, "pos_rnd_b.png")
	ingestTestImage(t, database, cfg, "pos_rnd_c.png")

	const seed int64 = 1234567
	q := Query{Sort: "random", RandomSeed: seed, Page: 1, Limit: 40}
	result, err := Execute(database, q)
	if err != nil || len(result.Results) != 3 {
		t.Fatalf("setup Execute: err=%v len=%d", err, len(result.Results))
	}
	first, second, third := result.Results[0].ID, result.Results[1].ID, result.Results[2].ID

	cases := []struct {
		name    string
		id      int64
		wantPos int
	}{
		{"first", first, 1},
		{"second", second, 2},
		{"third", third, 3},
	}
	for _, c := range cases {
		pos, total, err := ExecutePosition(database, q, c.id)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if pos != c.wantPos || total != 3 {
			t.Errorf("%s: pos=%d total=%d, want %d/3", c.name, pos, total, c.wantPos)
		}
	}
}

func TestExecutePosition_RandomNoSeed(t *testing.T) {
	database, cfg := setupSearchDB(t)
	ingestTestImage(t, database, cfg, "pos_rnd_noseed.png")
	pos, total, err := ExecutePosition(database, Query{Sort: "random"}, 1)
	if err != nil || pos != 0 || total != 0 {
		t.Errorf("random without seed must return 0/0, got pos=%d total=%d err=%v", pos, total, err)
	}
}

func TestExecutePosition_UnknownID(t *testing.T) {
	database, cfg := setupSearchDB(t)
	ingestTestImage(t, database, cfg, "pos_unknown.png")
	pos, total, err := ExecutePosition(database, Query{Sort: "newest", Order: "desc"}, 99999)
	if err != nil || pos != 0 || total != 0 {
		t.Errorf("unknown id must return 0/0, got pos=%d total=%d err=%v", pos, total, err)
	}
}

func TestBuildWhere_DateFilter(t *testing.T) {
	where, args, _ := buildWhere(FilterExpr{Key: "date", Val: ">2024-01-01"})
	if !strings.Contains(where, "ingested_at") {
		t.Errorf("date filter missing ingested_at: %q", where)
	}
	if len(args) == 0 {
		t.Error("expected args for date filter")
	}
}

func TestExecute_WithAutoTaggedAt(t *testing.T) {
	// Test that Execute correctly parses auto_tagged_at when set
	database, cfg := setupSearchDB(t)
	ingestTestImage(t, database, cfg, "autotagged.png")

	// Set auto_tagged_at directly in DB
	database.Write.Exec(`UPDATE images SET auto_tagged_at = '2024-01-15T12:00:00Z'`)

	result, err := Execute(database, Query{Page: 1, Limit: 40})
	if err != nil {
		t.Fatal(err)
	}
	if result.Total == 0 {
		t.Skip("no images in DB")
	}
	// At least one image should have AutoTaggedAt set
	found := false
	for _, img := range result.Results {
		if img.AutoTaggedAt != nil {
			found = true
		}
	}
	if !found {
		t.Error("expected at least one image with AutoTaggedAt set")
	}
}

func TestSuggestTagsWithFilter(t *testing.T) {
	database, cfg := setupSearchDB(t)
	ingestTestImage(t, database, cfg, "combo1.png")
	ingestTestImage(t, database, cfg, "combo2.png")
	ingestTestImage(t, database, cfg, "combo3.png")

	// Grab image IDs in insertion order.
	rows, _ := database.Read.Query(`SELECT id FROM images ORDER BY id`)
	var ids []int64
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		ids = append(ids, id)
	}
	rows.Close()
	if len(ids) != 3 {
		t.Fatalf("expected 3 images, got %d", len(ids))
	}

	var catID int64
	database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name='general'`).Scan(&catID)
	seed := func(name string) int64 {
		res, err := database.Write.Exec(`INSERT INTO tags (name, category_id, usage_count) VALUES (?, ?, 0)`, name, catID)
		if err != nil {
			t.Fatalf("seed tag %q: %v", name, err)
		}
		tid, _ := res.LastInsertId()
		return tid
	}
	tagA := seed("alpha")
	tagB := seed("beta")
	tagBet := seed("betula")

	add := func(img, tag int64) {
		if _, err := database.Write.Exec(
			`INSERT INTO image_tags (image_id, tag_id, is_auto) VALUES (?, ?, 0)`, img, tag,
		); err != nil {
			t.Fatalf("add tag: %v", err)
		}
		database.Write.Exec(`UPDATE tags SET usage_count = usage_count + 1 WHERE id = ?`, tag)
	}
	// img1: alpha, beta   img2: alpha, beta   img3: betula only
	add(ids[0], tagA)
	add(ids[0], tagB)
	add(ids[1], tagA)
	add(ids[1], tagB)
	add(ids[2], tagBet)

	// Typing "be" with no context: both beta (2 images) and betula (1) match.
	got, err := SuggestTagsWithFilter(database, nil, "be", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("plain suggest: expected 2, got %+v", got)
	}
	// Typing "be" with context "alpha": only beta (co-occurs with alpha).
	expr, _ := Parse("alpha")
	got, err = SuggestTagsWithFilter(database, expr, "be", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Name != "beta" {
		t.Fatalf("context suggest: expected only beta, got %+v", got)
	}
	if got[0].UsageCount != 2 {
		t.Errorf("expected combo count 2 for alpha+beta, got %d", got[0].UsageCount)
	}
}

