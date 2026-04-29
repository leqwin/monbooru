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
	// `a OR b OR c` must produce three leaves, not two; chained ORs past
	// the first pair must not be silently dropped.
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
	if _, _, err := gallery.Ingest(database, env.galleryDir, env.thumbnailsDir, path, "png", ""); err != nil {
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
	if _, _, err := gallery.Ingest(database, env.galleryDir, env.thumbnailsDir, path, "png", ""); err != nil {
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

func TestExecute_TagAliasResolvesToCanonical(t *testing.T) {
	// After a merge the image_tags row lives on the canonical; searching
	// for the alias name must still surface the image.
	database, env := setupSearchDB(t)
	ingestTestImage(t, database, env, "alias_search.png")

	var imgID, generalID int64
	database.Read.QueryRow(`SELECT id FROM images LIMIT 1`).Scan(&imgID)
	database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'general'`).Scan(&generalID)

	var canonID, aliasID int64
	database.Write.QueryRow(`INSERT INTO tags (name, category_id) VALUES (?, ?) RETURNING id`, "feline", generalID).Scan(&canonID)
	database.Write.QueryRow(
		`INSERT INTO tags (name, category_id, is_alias, canonical_tag_id) VALUES (?, ?, 1, ?) RETURNING id`,
		"cat", generalID, canonID,
	).Scan(&aliasID)
	if _, err := database.Write.Exec(
		`INSERT INTO image_tags (image_id, tag_id, is_auto) VALUES (?, ?, 0)`, imgID, canonID,
	); err != nil {
		t.Fatal(err)
	}
	// match the AddTagToImage invariant fastTagTotal trusts.
	if _, err := database.Write.Exec(
		`UPDATE tags SET usage_count = 1 WHERE id = ?`, canonID,
	); err != nil {
		t.Fatal(err)
	}

	result, err := Execute(database, Query{
		Expr:  TagExpr{Tag: "cat"},
		Page:  1,
		Limit: 40,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 1 {
		t.Errorf("Total = %d, want 1 (alias should resolve to canonical)", result.Total)
	}
}

func TestIsPureTagExpr(t *testing.T) {
	tests := []struct {
		name string
		expr Expr
		want bool
	}{
		{"literal tag", TagExpr{Tag: "1girl"}, true},
		{"prefix tag", TagExpr{Tag: "blue", Wildcard: "prefix"}, true},
		{"substring tag", TagExpr{Tag: "blue", Wildcard: "substring"}, true},
		{"and of tags", AndExpr{Left: TagExpr{Tag: "a"}, Right: TagExpr{Tag: "b"}}, true},
		{"or of tags", OrExpr{Left: TagExpr{Tag: "a"}, Right: TagExpr{Tag: "b"}}, true},
		{"not tag", NotExpr{Expr: TagExpr{Tag: "a"}}, true},
		{"cat: filter", FilterExpr{Key: "cat", Val: "character"}, true},
		{"category-qualified (unknown key)", FilterExpr{Key: "character", Val: "miku"}, true},
		{"colon tag fallback (unknown key)", FilterExpr{Key: "nier", Val: "automata"}, true},
		{"fav filter", FilterExpr{Key: "fav", Val: "true"}, false},
		{"source filter", FilterExpr{Key: "source", Val: "ai"}, false},
		{"folder filter", FilterExpr{Key: "folder", Val: "anime"}, false},
		{"tag and fav", AndExpr{Left: TagExpr{Tag: "a"}, Right: FilterExpr{Key: "fav", Val: "true"}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isPureTagExpr(tt.expr); got != tt.want {
				t.Errorf("isPureTagExpr(%+v) = %v, want %v", tt.expr, got, tt.want)
			}
		})
	}
}

func TestFastTagTotal_NonexistentTag(t *testing.T) {
	database, _ := setupSearchDB(t)
	n, ok := fastTagTotal(database, TagExpr{Tag: "no_such_tag"})
	if !ok {
		t.Fatal("nonexistent tag should resolve as confirmed-empty (ok=true)")
	}
	if n != 0 {
		t.Errorf("count = %d, want 0", n)
	}
}

func TestFastTagTotal_SingleCanonical(t *testing.T) {
	database, _ := setupSearchDB(t)
	var generalID int64
	database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'general'`).Scan(&generalID)

	if _, err := database.Write.Exec(
		`INSERT INTO tags (name, category_id, usage_count) VALUES ('hot', ?, 549514)`,
		generalID,
	); err != nil {
		t.Fatal(err)
	}

	n, ok := fastTagTotal(database, TagExpr{Tag: "hot"})
	if !ok {
		t.Fatal("single-canonical tag should hit fast path (ok=true)")
	}
	if n != 549514 {
		t.Errorf("count = %d, want 549514", n)
	}
}

func TestFastTagTotal_AliasFollowsCanonical(t *testing.T) {
	database, _ := setupSearchDB(t)
	var generalID int64
	database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'general'`).Scan(&generalID)

	var canonID int64
	database.Write.QueryRow(
		`INSERT INTO tags (name, category_id, usage_count) VALUES ('feline', ?, 7) RETURNING id`,
		generalID,
	).Scan(&canonID)
	if _, err := database.Write.Exec(
		`INSERT INTO tags (name, category_id, is_alias, canonical_tag_id) VALUES ('cat', ?, 1, ?)`,
		generalID, canonID,
	); err != nil {
		t.Fatal(err)
	}

	n, ok := fastTagTotal(database, TagExpr{Tag: "cat"})
	if !ok {
		t.Fatal("alias should resolve via canonical (ok=true)")
	}
	if n != 7 {
		t.Errorf("count = %d, want 7 (canonical's usage_count)", n)
	}
}

func TestFastTagTotal_MultipleCanonicalsFallthrough(t *testing.T) {
	database, _ := setupSearchDB(t)
	var generalID, charID int64
	database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'general'`).Scan(&generalID)
	database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'character'`).Scan(&charID)

	if _, err := database.Write.Exec(
		`INSERT INTO tags (name, category_id, usage_count) VALUES ('cat', ?, 3)`, generalID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := database.Write.Exec(
		`INSERT INTO tags (name, category_id, usage_count) VALUES ('cat', ?, 5)`, charID,
	); err != nil {
		t.Fatal(err)
	}

	if _, ok := fastTagTotal(database, TagExpr{Tag: "cat"}); ok {
		t.Error("multi-canonical name must fall through to slow path (ok=false)")
	}
}

func TestFastTagTotal_WildcardPrefix(t *testing.T) {
	database, _ := setupSearchDB(t)
	var generalID int64
	database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'general'`).Scan(&generalID)

	// Usages above fastApproxThreshold so the upper-bound short-circuit
	// engages instead of falling through to the slow exact COUNT.
	if _, err := database.Write.Exec(
		`INSERT INTO tags (name, category_id, usage_count) VALUES
		    ('blue_eyes',  ?, 40000),
		    ('blue_hair',  ?, 20000),
		    ('green_eyes', ?, 30)`,
		generalID, generalID, generalID,
	); err != nil {
		t.Fatal(err)
	}

	n, ok := fastTagTotal(database, TagExpr{Tag: "blue", Wildcard: "prefix"})
	if !ok {
		t.Fatal("wildcard prefix should hit fast path")
	}
	if n != 60000 {
		t.Errorf("count = %d, want 60000 (sum over name LIKE 'blue%%')", n)
	}
}

func TestFastTagTotal_WildcardSubstring(t *testing.T) {
	database, _ := setupSearchDB(t)
	var generalID int64
	database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'general'`).Scan(&generalID)

	if _, err := database.Write.Exec(
		`INSERT INTO tags (name, category_id, usage_count) VALUES
		    ('blue_eyes',  ?, 40000),
		    ('light_blue', ?, 20000),
		    ('green_eyes', ?, 30)`,
		generalID, generalID, generalID,
	); err != nil {
		t.Fatal(err)
	}

	n, ok := fastTagTotal(database, TagExpr{Tag: "blue", Wildcard: "substring"})
	if !ok {
		t.Fatal("wildcard substring should hit fast path")
	}
	if n != 60000 {
		t.Errorf("count = %d, want 60000 (sum over name LIKE '%%blue%%')", n)
	}
}

func TestFastTagTotal_WildcardBelowThresholdFallsThrough(t *testing.T) {
	// Multi-canonical wildcard with small usages: the slow path is fast
	// and exact, so the fast path bails to keep displayed totals exact.
	database, _ := setupSearchDB(t)
	var generalID int64
	database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'general'`).Scan(&generalID)

	if _, err := database.Write.Exec(
		`INSERT INTO tags (name, category_id, usage_count) VALUES
		    ('blue_eyes', ?, 5),
		    ('blue_hair', ?, 3)`,
		generalID, generalID,
	); err != nil {
		t.Fatal(err)
	}

	if _, ok := fastTagTotal(database, TagExpr{Tag: "blue", Wildcard: "prefix"}); ok {
		t.Error("sub-threshold wildcard should fall through to the exact slow path")
	}
}

func TestFastTagTotal_WildcardCollapsesAlias(t *testing.T) {
	database, _ := setupSearchDB(t)
	var generalID int64
	database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'general'`).Scan(&generalID)

	var canonID int64
	database.Write.QueryRow(
		`INSERT INTO tags (name, category_id, usage_count) VALUES ('blueberry', ?, 7) RETURNING id`,
		generalID,
	).Scan(&canonID)
	if _, err := database.Write.Exec(
		`INSERT INTO tags (name, category_id, is_alias, canonical_tag_id) VALUES ('bluebell', ?, 1, ?)`,
		generalID, canonID,
	); err != nil {
		t.Fatal(err)
	}

	n, ok := fastTagTotal(database, TagExpr{Tag: "blue", Wildcard: "prefix"})
	if !ok {
		t.Fatal("wildcard with alias should hit fast path")
	}
	if n != 7 {
		t.Errorf("count = %d, want 7 (alias and canonical collapse via DISTINCT COALESCE)", n)
	}
}

func TestFastTagTotal_WildcardEmpty(t *testing.T) {
	database, _ := setupSearchDB(t)
	n, ok := fastTagTotal(database, TagExpr{Tag: "nomatch_zzzzz", Wildcard: "prefix"})
	if !ok {
		t.Fatal("wildcard with no matches should resolve as confirmed-empty")
	}
	if n != 0 {
		t.Errorf("count = %d, want 0", n)
	}
}

func TestFastTagTotal_WildcardEscapesMetacharacters(t *testing.T) {
	// A wildcard like `foo_*` must match `foo_bar` literally (the underscore
	// is part of the tag name) and NOT every name with any character at
	// position 4. escapeLike + ESCAPE '\' carries that through.
	database, _ := setupSearchDB(t)
	var generalID int64
	database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'general'`).Scan(&generalID)

	if _, err := database.Write.Exec(
		`INSERT INTO tags (name, category_id, usage_count) VALUES
		    ('foo_bar',  ?, 10),
		    ('fooXbar', ?, 99)`,
		generalID, generalID,
	); err != nil {
		t.Fatal(err)
	}

	n, ok := fastTagTotal(database, TagExpr{Tag: "foo_b", Wildcard: "prefix"})
	if !ok {
		t.Fatal("wildcard with literal underscore should hit fast path")
	}
	if n != 10 {
		t.Errorf("count = %d, want 10 (only foo_bar, not fooXbar)", n)
	}
}

func TestFastTagTotal_RejectsNonRecognisedShapes(t *testing.T) {
	database, _ := setupSearchDB(t)
	// Filter keywords with their own selective indexes still fall through.
	for _, key := range []string{"fav", "source", "folder", "width", "date"} {
		if _, ok := fastTagTotal(database, FilterExpr{Key: key, Val: "true"}); ok {
			t.Errorf("FilterExpr{%q} should fall through to slow path", key)
		}
	}
	// AND/OR with a non-fast-pathable leaf falls through.
	mixed := AndExpr{Left: TagExpr{Tag: "a"}, Right: FilterExpr{Key: "fav", Val: "true"}}
	if _, ok := fastTagTotal(database, mixed); ok {
		t.Error("AND with FilterExpr{fav} leaf should fall through")
	}
	mixedOr := OrExpr{Left: TagExpr{Tag: "a"}, Right: FilterExpr{Key: "fav", Val: "true"}}
	if _, ok := fastTagTotal(database, mixedOr); ok {
		t.Error("OR with FilterExpr{fav} leaf should fall through")
	}
	// NotExpr with non-literal inner falls through.
	if _, ok := fastTagTotal(database, NotExpr{Expr: TagExpr{Tag: "blue", Wildcard: "prefix"}}); ok {
		t.Error("NOT with wildcard inner should fall through")
	}
	if _, ok := fastTagTotal(database, NotExpr{Expr: FilterExpr{Key: "fav", Val: "true"}}); ok {
		t.Error("NOT with non-tag inner should fall through")
	}
}

func TestFastTagTotal_NotSingleTag(t *testing.T) {
	database, env := setupSearchDB(t)
	ingestTestImage(t, database, env, "n_a.png")
	ingestTestImage(t, database, env, "n_b.png")
	ingestTestImage(t, database, env, "n_c.png")

	var generalID int64
	database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'general'`).Scan(&generalID)
	if _, err := database.Write.Exec(
		`INSERT INTO tags (name, category_id, usage_count) VALUES ('hot', ?, 2)`, generalID,
	); err != nil {
		t.Fatal(err)
	}

	n, ok := fastTagTotal(database, NotExpr{Expr: TagExpr{Tag: "hot"}})
	if !ok {
		t.Fatal("NOT single tag should hit fast path")
	}
	if n != 1 {
		t.Errorf("count = %d, want 1 (3 visible - 2 hot)", n)
	}
}

func TestFastTagTotal_NotMissingTag(t *testing.T) {
	// NotExpr{tag} where the tag doesn't exist should report all visible.
	database, env := setupSearchDB(t)
	ingestTestImage(t, database, env, "nm_a.png")
	ingestTestImage(t, database, env, "nm_b.png")

	n, ok := fastTagTotal(database, NotExpr{Expr: TagExpr{Tag: "no_such_tag"}})
	if !ok {
		t.Fatal("NOT of missing tag should hit fast path")
	}
	if n != 2 {
		t.Errorf("count = %d, want 2 (all visible)", n)
	}
}

func TestFastTagTotal_AndPositiveTags(t *testing.T) {
	database, _ := setupSearchDB(t)
	var generalID int64
	database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'general'`).Scan(&generalID)
	// Each tag's usage above fastApproxThreshold so min(...) clears the
	// gate and the upper-bound short-circuit engages.
	if _, err := database.Write.Exec(
		`INSERT INTO tags (name, category_id, usage_count) VALUES
		    ('a', ?, 100000),
		    ('b', ?, 60000),
		    ('c', ?, 200000)`,
		generalID, generalID, generalID,
	); err != nil {
		t.Fatal(err)
	}

	expr := AndExpr{
		Left: TagExpr{Tag: "a"},
		Right: AndExpr{
			Left:  TagExpr{Tag: "b"},
			Right: TagExpr{Tag: "c"},
		},
	}
	n, ok := fastTagTotal(database, expr)
	if !ok {
		t.Fatal("AND of high-usage positive tags should hit fast path")
	}
	if n != 60000 {
		t.Errorf("count = %d, want 60000 (min upper bound)", n)
	}
}

func TestFastTagTotal_AndBelowThresholdFallsThrough(t *testing.T) {
	// Small AND queries fall through to the exact slow COUNT so totals
	// like `cute dog` (no overlap → 0) stay exact in tests and small
	// libraries.
	database, _ := setupSearchDB(t)
	var generalID int64
	database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'general'`).Scan(&generalID)
	if _, err := database.Write.Exec(
		`INSERT INTO tags (name, category_id, usage_count) VALUES ('cute', ?, 3), ('dog', ?, 2)`,
		generalID, generalID,
	); err != nil {
		t.Fatal(err)
	}

	expr := AndExpr{Left: TagExpr{Tag: "cute"}, Right: TagExpr{Tag: "dog"}}
	if _, ok := fastTagTotal(database, expr); ok {
		t.Error("sub-threshold AND should fall through to the exact slow path")
	}
}

func TestFastTagTotal_OrCappedAtVisibleCount(t *testing.T) {
	database, env := setupSearchDB(t)
	ingestTestImage(t, database, env, "or_cap.png") // 1 visible image total

	var generalID int64
	database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'general'`).Scan(&generalID)
	// Drift-style usage_counts above what the (1) visible image
	// supports; the cap should clamp the upper bound to visible_count.
	if _, err := database.Write.Exec(
		`INSERT INTO tags (name, category_id, usage_count) VALUES ('a', ?, 30000), ('b', ?, 30000)`,
		generalID, generalID,
	); err != nil {
		t.Fatal(err)
	}

	expr := OrExpr{Left: TagExpr{Tag: "a"}, Right: TagExpr{Tag: "b"}}
	n, ok := fastTagTotal(database, expr)
	if !ok {
		t.Fatal("OR fast path expected")
	}
	if n != 1 {
		t.Errorf("count = %d, want 1 (sum=60000 capped at visible=1)", n)
	}
}

func TestFastTagTotal_OrBelowThresholdFallsThrough(t *testing.T) {
	database, _ := setupSearchDB(t)
	var generalID int64
	database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'general'`).Scan(&generalID)
	if _, err := database.Write.Exec(
		`INSERT INTO tags (name, category_id, usage_count) VALUES ('a', ?, 5), ('b', ?, 3)`,
		generalID, generalID,
	); err != nil {
		t.Fatal(err)
	}
	expr := OrExpr{Left: TagExpr{Tag: "a"}, Right: TagExpr{Tag: "b"}}
	if _, ok := fastTagTotal(database, expr); ok {
		t.Error("sub-threshold OR should fall through to the exact slow path")
	}
}

func TestFastTagTotal_CatFilter(t *testing.T) {
	database, _ := setupSearchDB(t)
	var generalID, charID int64
	database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'general'`).Scan(&generalID)
	database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'character'`).Scan(&charID)
	// Sum within character above fastApproxThreshold so the upper-bound
	// short-circuit engages.
	if _, err := database.Write.Exec(
		`INSERT INTO tags (name, category_id, usage_count) VALUES
		    ('miku',  ?, 40000),
		    ('haku',  ?, 20000),
		    ('1girl', ?, 100000)`,
		charID, charID, generalID,
	); err != nil {
		t.Fatal(err)
	}

	n, ok := fastTagTotal(database, FilterExpr{Key: "cat", Val: "character"})
	if !ok {
		t.Fatal("cat: filter should hit fast path")
	}
	if n != 60000 {
		t.Errorf("count = %d, want 60000 (sum within character, general excluded)", n)
	}
}

func TestFastTagTotal_CatFilterBelowThresholdFallsThrough(t *testing.T) {
	database, _ := setupSearchDB(t)
	var charID int64
	database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'character'`).Scan(&charID)
	if _, err := database.Write.Exec(
		`INSERT INTO tags (name, category_id, usage_count) VALUES ('miku', ?, 5), ('haku', ?, 3)`,
		charID, charID,
	); err != nil {
		t.Fatal(err)
	}
	if _, ok := fastTagTotal(database, FilterExpr{Key: "cat", Val: "character"}); ok {
		t.Error("sub-threshold cat: should fall through to the exact slow path")
	}
}

func TestFastTagTotal_CategoryQualifiedTag(t *testing.T) {
	database, _ := setupSearchDB(t)
	var charID int64
	database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'character'`).Scan(&charID)
	if _, err := database.Write.Exec(
		`INSERT INTO tags (name, category_id, usage_count) VALUES ('miku', ?, 5)`, charID,
	); err != nil {
		t.Fatal(err)
	}

	n, ok := fastTagTotal(database, FilterExpr{Key: "character", Val: "miku"})
	if !ok {
		t.Fatal("character:miku should hit fast path")
	}
	if n != 5 {
		t.Errorf("count = %d, want 5", n)
	}
}

func TestFastTagTotal_CategoryQualifiedFollowsAlias(t *testing.T) {
	// character:cat aliased to character:feline - querying the alias
	// should report the canonical's usage_count.
	database, _ := setupSearchDB(t)
	var charID int64
	database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'character'`).Scan(&charID)

	var canonID int64
	database.Write.QueryRow(
		`INSERT INTO tags (name, category_id, usage_count) VALUES ('feline', ?, 9) RETURNING id`,
		charID,
	).Scan(&canonID)
	if _, err := database.Write.Exec(
		`INSERT INTO tags (name, category_id, is_alias, canonical_tag_id) VALUES ('cat', ?, 1, ?)`,
		charID, canonID,
	); err != nil {
		t.Fatal(err)
	}

	n, ok := fastTagTotal(database, FilterExpr{Key: "character", Val: "cat"})
	if !ok {
		t.Fatal("alias should resolve via canonical")
	}
	if n != 9 {
		t.Errorf("count = %d, want 9 (canonical's usage_count)", n)
	}
}

func TestFastTagTotal_CategoryQualifiedMissingTag(t *testing.T) {
	// (name, cat) pair doesn't exist but the category does - exact 0.
	database, _ := setupSearchDB(t)
	n, ok := fastTagTotal(database, FilterExpr{Key: "character", Val: "no_such_char"})
	if !ok {
		t.Fatal("missing tag in real category should still hit fast path")
	}
	if n != 0 {
		t.Errorf("count = %d, want 0", n)
	}
}

func TestFastTagTotal_CategoryQualifiedUnknownCategory(t *testing.T) {
	// "nier:automata" - "nier" is not a real category. Slow path falls
	// back to a literal-tag search; fast path bails so that runs.
	database, _ := setupSearchDB(t)
	if _, ok := fastTagTotal(database, FilterExpr{Key: "nier", Val: "automata"}); ok {
		t.Error("unknown category must fall through")
	}
}

func TestExecute_FastTagTotal_EmptyShortCircuits(t *testing.T) {
	database, env := setupSearchDB(t)
	ingestTestImage(t, database, env, "fast_empty.png")

	result, err := Execute(database, Query{
		Expr:  TagExpr{Tag: "tag_that_does_not_exist"},
		Page:  1,
		Limit: 40,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Total != 0 || len(result.Results) != 0 {
		t.Errorf("Total = %d, Results = %d, want both 0", result.Total, len(result.Results))
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

func TestBuildWhere_TagExact(t *testing.T) {
	expr := TagExpr{Tag: "cute", Wildcard: ""}
	where, args, _ := buildWhere(expr)
	if len(args) != 1 || args[0] != "cute" {
		t.Errorf("args = %v", args)
	}
	if !strings.Contains(where, "name = ?") {
		t.Errorf("where = %q", where)
	}
	// Alias-aware: the tag_id lookup goes through COALESCE so a name
	// matching an alias row resolves to its canonical.
	if !strings.Contains(where, "COALESCE(canonical_tag_id, id)") {
		t.Errorf("where missing alias resolution: %q", where)
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
	// A non-numeric width comparand must produce `1=0`, not bind the string
	// into the SQL — SQLite would coerce it to 0 and match every row with
	// width >= 0, which is worse than returning nothing.
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
	// `-missing:false` should mean "show me missing images", equivalent to
	// `missing:true`. The auto-injected `AND is_missing = 0` clause must
	// not be layered on top of the negation, or the query collapses to
	// zero results.
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

func TestExecuteAdjacent_WithTagPredicate(t *testing.T) {
	// Tuple cursor with a tag-predicate WHERE: LIMIT 1 must walk past
	// untagged neighbours and land on the next tagged one.
	database, env := setupSearchDB(t)
	ingestTestImage(t, database, env, "adj_tag_a.png")
	ingestTestImage(t, database, env, "adj_tag_b.png")
	ingestTestImage(t, database, env, "adj_tag_c.png")

	result, err := Execute(database, Query{Sort: "newest", Order: "desc", Page: 1, Limit: 40})
	if err != nil || len(result.Results) != 3 {
		t.Fatalf("setup Execute: err=%v len=%d", err, len(result.Results))
	}
	newest, _, oldest := result.Results[0].ID, result.Results[1].ID, result.Results[2].ID

	var generalID int64
	database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'general'`).Scan(&generalID)
	var tagID int64
	if err := database.Write.QueryRow(
		`INSERT INTO tags (name, category_id, usage_count) VALUES ('blue', ?, 2) RETURNING id`,
		generalID,
	).Scan(&tagID); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{newest, oldest} {
		if _, err := database.Write.Exec(
			`INSERT INTO image_tags (image_id, tag_id, is_auto) VALUES (?, ?, 0)`, id, tagID,
		); err != nil {
			t.Fatal(err)
		}
	}

	q := Query{Expr: TagExpr{Tag: "blue"}, Sort: "newest", Order: "desc"}

	prev, next, err := ExecuteAdjacent(database, q, newest)
	if err != nil {
		t.Fatal(err)
	}
	if prev != nil {
		t.Errorf("newest prev = %v, want nil", prev)
	}
	if next == nil || *next != oldest {
		t.Errorf("newest next = %v, want %d", next, oldest)
	}

	prev, next, _ = ExecuteAdjacent(database, q, oldest)
	if prev == nil || *prev != newest {
		t.Errorf("oldest prev = %v, want %d", prev, newest)
	}
	if next != nil {
		t.Errorf("oldest next = %v, want nil", next)
	}
}

func TestExecuteAdjacent_RandomBucketBound(t *testing.T) {
	// Random sort + tag predicate bounds adjacency to a fixed id-range
	// bucket. Images outside the current image's bucket must not appear
	// as prev/next, even if they match the predicate.
	database, env := setupSearchDB(t)
	ingestTestImage(t, database, env, "rb_a.png")
	ingestTestImage(t, database, env, "rb_b.png")
	ingestTestImage(t, database, env, "rb_c.png")

	var nearA, nearB, far int64
	rows, err := database.Read.Query(`SELECT id FROM images ORDER BY id ASC`)
	if err != nil {
		t.Fatal(err)
	}
	ids := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if len(ids) != 3 {
		t.Fatalf("expected 3 images, got %d", len(ids))
	}
	nearA, nearB = ids[0], ids[1]
	far = int64(randomAdjacencyBucketSize) + 1
	tx, err := database.Write.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`PRAGMA defer_foreign_keys = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`UPDATE images SET id = ? WHERE id = ?`, far, ids[2]); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`UPDATE image_paths SET image_id = ? WHERE image_id = ?`, far, ids[2]); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	var generalID int64
	if err := database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'general'`).Scan(&generalID); err != nil {
		t.Fatal(err)
	}
	var tagID int64
	if err := database.Write.QueryRow(
		`INSERT INTO tags (name, category_id, usage_count) VALUES ('rndtag', ?, 3) RETURNING id`,
		generalID,
	).Scan(&tagID); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{nearA, nearB, far} {
		if _, err := database.Write.Exec(
			`INSERT INTO image_tags (image_id, tag_id, is_auto) VALUES (?, ?, 0)`, id, tagID,
		); err != nil {
			t.Fatal(err)
		}
	}

	q := Query{Expr: TagExpr{Tag: "rndtag"}, Sort: "random", RandomSeed: 1234567}

	// nearA's bucket holds nearA + nearB; far is in a different bucket.
	prev, next, err := ExecuteAdjacent(database, q, nearA)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []*int64{prev, next} {
		if p != nil && *p == far {
			t.Errorf("nearA reached far image %d across bucket boundary", far)
		}
	}
	reachedNearB := (prev != nil && *prev == nearB) || (next != nil && *next == nearB)
	if !reachedNearB {
		t.Errorf("nearA did not reach in-bucket peer %d (prev=%v next=%v)", nearB, prev, next)
	}

	// far is alone in its bucket.
	prev, next, _ = ExecuteAdjacent(database, q, far)
	if prev != nil || next != nil {
		t.Errorf("far alone in bucket: want nil/nil, got prev=%v next=%v", prev, next)
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

func TestExecutePosition_TagPredicateSkipped(t *testing.T) {
	database, cfg := setupSearchDB(t)
	ingestTestImage(t, database, cfg, "pos_tag_a.png")
	ingestTestImage(t, database, cfg, "pos_tag_b.png")

	var imgID, generalID int64
	database.Read.QueryRow(`SELECT id FROM images LIMIT 1`).Scan(&imgID)
	database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'general'`).Scan(&generalID)
	var tagID int64
	database.Write.QueryRow(
		`INSERT INTO tags (name, category_id, usage_count) VALUES ('blue', ?, 1) RETURNING id`,
		generalID,
	).Scan(&tagID)
	if _, err := database.Write.Exec(
		`INSERT INTO image_tags (image_id, tag_id, is_auto) VALUES (?, ?, 0)`, imgID, tagID,
	); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name string
		expr Expr
	}{
		{"literal tag", TagExpr{Tag: "blue"}},
		{"wildcard tag", TagExpr{Tag: "blue", Wildcard: "prefix"}},
		{"NOT tag", NotExpr{Expr: TagExpr{Tag: "blue"}}},
		{"AND of tags", AndExpr{Left: TagExpr{Tag: "blue"}, Right: TagExpr{Tag: "red"}}},
		{"cat filter", FilterExpr{Key: "cat", Val: "general"}},
		{"category-qualified tag", FilterExpr{Key: "general", Val: "blue"}},
		{"tagged filter", FilterExpr{Key: "tagged", Val: "true"}},
		{"autotagged filter", FilterExpr{Key: "autotagged", Val: "false"}},
		{"folder filter", FilterExpr{Key: "folder", Val: "anime"}},
		{"folderonly filter", FilterExpr{Key: "folderonly", Val: "anime"}},
	}
	for _, c := range cases {
		pos, total, err := ExecutePosition(database, Query{Expr: c.expr, Sort: "newest", Order: "desc"}, imgID)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if pos != 0 || total != 0 {
			t.Errorf("%s: pos=%d total=%d, want 0/0", c.name, pos, total)
		}
	}

	// Sanity: a non-tag-predicate expression still returns a real rank
	// so the early-return is scoped to the slow shape only.
	pos, total, err := ExecutePosition(database, Query{Expr: FilterExpr{Key: "fav", Val: "false"}, Sort: "newest", Order: "desc"}, imgID)
	if err != nil {
		t.Fatal(err)
	}
	if pos == 0 || total == 0 {
		t.Errorf("non-tag predicate skipped: pos=%d total=%d", pos, total)
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

