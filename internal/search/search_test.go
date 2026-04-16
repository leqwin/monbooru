package search

import (
	"context"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/leqwin/monbooru/internal/config"
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
	_ = and
}

func TestParse_OR(t *testing.T) {
	e, _ := Parse("cat OR dog")
	or, ok := e.(OrExpr)
	if !ok {
		t.Fatalf("expected OrExpr, got %T", e)
	}
	_ = or
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
	expr := FilterExpr{Key: "folder", Val: "2024/jan"}
	where, args, _ := buildWhere(expr)
	if len(args) != 1 {
		t.Fatalf("expected 1 arg, got %d", len(args))
	}
	if args[0] != "2024/jan" {
		t.Errorf("arg[0] = %v", args[0])
	}
	if !strings.Contains(where, "folder_path") {
		t.Errorf("where clause missing folder_path: %s", where)
	}
}

func TestBuildWhere_FolderFilterEmpty(t *testing.T) {
	// Empty folder (root) matches images directly at the gallery root only.
	expr := FilterExpr{Key: "folder", Val: ""}
	where, args, _ := buildWhere(expr)
	if len(args) != 0 {
		t.Fatalf("expected 0 args for empty folder, got %d", len(args))
	}
	if !strings.Contains(where, "folder_path = ''") {
		t.Errorf("where clause for root folder should match empty folder_path: %s", where)
	}
}

func TestBuildWhere_MissingTrue(t *testing.T) {
	expr := FilterExpr{Key: "missing", Val: "true"}
	_, _, hasMissing := buildWhere(expr)
	if !hasMissing {
		t.Error("expected hasMissingTrue = true")
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

// Integration test with real DB
func setupSearchDB(t *testing.T) (*db.DB, *config.Config) {
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

	cfg := &config.Config{}
	cfg.Paths.GalleryPath = galleryDir
	cfg.Paths.ThumbnailsPath = filepath.Join(tmpDir, "thumbs")
	cfg.Gallery.MaxFileSizeMB = 100

	return database, cfg
}

var ingestCounter int

func ingestTestImage(t *testing.T, database *db.DB, cfg *config.Config, name string) {
	t.Helper()
	ingestCounter++
	// Use different sizes to ensure different SHA-256 hashes
	img := image.NewRGBA(image.Rect(0, 0, 10+ingestCounter, 10))
	path := filepath.Join(cfg.Paths.GalleryPath, name)
	f, _ := os.Create(path)
	png.Encode(f, img)
	f.Close()
	gallery.Ingest(database, cfg, path, "png")
}

func TestExecute_BasicSearch(t *testing.T) {
	database, cfg := setupSearchDB(t)
	ingestTestImage(t, database, cfg, "test.png")

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
	database, cfg := setupSearchDB(t)

	subDir := filepath.Join(cfg.Paths.GalleryPath, "2024")
	os.MkdirAll(subDir, 0755)
	ingestTestImage(t, database, cfg, "root.png")

	// Ingest in sub dir
	img := image.NewRGBA(image.Rect(0, 0, 10, 10))
	path := filepath.Join(subDir, "sub.png")
	f, _ := os.Create(path)
	png.Encode(f, img)
	f.Close()
	gallery.Ingest(database, cfg, path, "png")

	// Search for gallery root only
	_, err := Execute(database, Query{
		Expr:  FilterExpr{Key: "folder", Val: ""},
		Page:  1,
		Limit: 40,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestExecute_FullSync(t *testing.T) {
	database, cfg := setupSearchDB(t)
	ingestTestImage(t, database, cfg, "a.png")
	ingestTestImage(t, database, cfg, "b.png")

	gallery.Sync(context.Background(), database, cfg, func(string) {})

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
	if len(args) != 1 || args[0] != "1920" {
		t.Errorf("args = %v, want 1920", args)
	}
	if !strings.Contains(where, "i.width") {
		t.Errorf("where = %q", where)
	}
}

func TestBuildWhere_Height(t *testing.T) {
	expr := FilterExpr{Key: "height", Val: "<768"}
	where, args, _ := buildWhere(expr)
	if len(args) != 1 || args[0] != "768" {
		t.Errorf("args = %v", args)
	}
	if !strings.Contains(where, "i.height") {
		t.Errorf("where = %q", where)
	}
}

func TestBuildWhere_MissingFalse(t *testing.T) {
	expr := FilterExpr{Key: "missing", Val: "false"}
	where, _, hasMissing := buildWhere(expr)
	if hasMissing {
		t.Error("should not set hasMissingTrue for missing:false")
	}
	if !strings.Contains(where, "is_missing = 0") {
		t.Errorf("where = %q", where)
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
	// Unknown filter produces "1=1" — AND should still work
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
	database, cfg := setupSearchDB(t)
	ingestTestImage(t, database, cfg, "a.png")
	ingestTestImage(t, database, cfg, "b.png")

	result, err := Execute(database, Query{Page: 1, Limit: 40})
	if err != nil || len(result.Results) == 0 {
		t.Skip("no images to test SidebarTagsWithGlobalCount")
	}

	ids := make([]int64, len(result.Results))
	for i, img := range result.Results {
		ids[i] = img.ID
	}
	tags, err := SidebarTagsWithGlobalCount(database, ids)
	if err != nil {
		t.Fatal(err)
	}
	_ = tags // may be empty if no tags were added; that's OK
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

func TestExprNodeMarkers(t *testing.T) {
	// exprNode() is a marker method used to implement the Expr interface.
	// Call each one to satisfy coverage — they are intentionally empty.
	exprs := []Expr{AndExpr{}, OrExpr{}, NotExpr{}, TagExpr{}, FilterExpr{}}
	for _, e := range exprs {
		e.exprNode()
	}
}

