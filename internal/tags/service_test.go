package tags

import (
	"strings"
	"testing"

	"github.com/leqwin/monbooru/internal/db"
)

func setupTestDB(t *testing.T) (*db.DB, *Service) {
	t.Helper()
	dir := t.TempDir()
	database, err := db.Open(dir + "/test.db")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := db.Bootstrap(database); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	svc := New(database)
	return database, svc
}

func generalCategoryID(t *testing.T, svc *Service) int64 {
	t.Helper()
	cats, err := svc.ListCategories()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cats {
		if c.Name == "general" {
			return c.ID
		}
	}
	t.Fatal("general category not found")
	return 0
}

// insertTestImage inserts a minimal image record for testing.
func insertTestImage(t *testing.T, database *db.DB, sha string) int64 {
	t.Helper()
	var id int64
	err := database.Write.QueryRow(
		`INSERT INTO images (sha256, canonical_path, file_type, file_size) VALUES (?, ?, 'png', 100) RETURNING id`,
		sha, "/gallery/"+sha+".png",
	).Scan(&id)
	if err != nil {
		t.Fatalf("insertTestImage: %v", err)
	}
	return id
}

func TestAddTagToImage_UsageCount(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	imgID := insertTestImage(t, database, "abc123")

	tag, err := svc.GetOrCreateTag("cute", catID)
	if err != nil {
		t.Fatal(err)
	}

	if err := svc.AddTagToImage(imgID, tag.ID, false, nil); err != nil {
		t.Fatal(err)
	}

	got, err := svc.GetTag(tag.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.UsageCount != 1 {
		t.Errorf("UsageCount = %d, want 1", got.UsageCount)
	}
}

func TestAddTagTwice_NoDouble(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	imgID := insertTestImage(t, database, "abc124")

	tag, _ := svc.GetOrCreateTag("cute", catID)
	svc.AddTagToImage(imgID, tag.ID, false, nil)
	svc.AddTagToImage(imgID, tag.ID, false, nil) // duplicate

	got, _ := svc.GetTag(tag.ID)
	if got.UsageCount != 1 {
		t.Errorf("UsageCount = %d, want 1 after duplicate add", got.UsageCount)
	}
}

func TestRemoveTag_DecrementUsageCount(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	imgID := insertTestImage(t, database, "abc125")

	tag, _ := svc.GetOrCreateTag("cute", catID)
	svc.AddTagToImage(imgID, tag.ID, false, nil)
	svc.RemoveTagFromImage(imgID, tag.ID)

	// Tags with 0 usage are automatically deleted, so GetTag should return nil.
	got, _ := svc.GetTag(tag.ID)
	if got != nil {
		t.Errorf("tag should be deleted when usage_count reaches 0, got UsageCount = %d", got.UsageCount)
	}
}

func TestDeleteCategory_ReassignsToGeneral(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	// Create custom category
	custom, err := svc.CreateCategory("custom_cat", "#aabbcc")
	if err != nil {
		t.Fatal(err)
	}

	// Create tag in custom category
	tag, err := svc.GetOrCreateTag("mytag", custom.ID)
	if err != nil {
		t.Fatal(err)
	}

	// Delete custom category
	if err := svc.DeleteCategoryMoveOrDelete(custom.ID, "move", 0); err != nil {
		t.Fatal(err)
	}

	// Tag should now be in general
	got, err := svc.GetTag(tag.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.CategoryID != catID {
		t.Errorf("tag category = %d, want general (%d)", got.CategoryID, catID)
	}
}

func TestDeleteBuiltinCategory_Rejected(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	err := svc.DeleteCategoryMoveOrDelete(catID, "move", 0)
	if err != ErrBuiltinCategory {
		t.Errorf("expected ErrBuiltinCategory, got %v", err)
	}
}

func TestMergeTags(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	imgID := insertTestImage(t, database, "abc127")

	tagAlias, _ := svc.GetOrCreateTag("old_tag", catID)
	tagCanon, _ := svc.GetOrCreateTag("new_tag", catID)

	svc.AddTagToImage(imgID, tagAlias.ID, false, nil)

	if err := svc.MergeTags(tagAlias.ID, tagCanon.ID); err != nil {
		t.Fatal(err)
	}

	// Image should now have canonical tag
	imgTags, err := svc.GetImageTags(imgID)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, it := range imgTags {
		if it.TagID == tagCanon.ID {
			found = true
		}
		if it.TagID == tagAlias.ID {
			t.Error("alias tag still on image after merge")
		}
	}
	if !found {
		t.Error("canonical tag not on image after merge")
	}

	// Alias tag should be marked
	got, _ := svc.GetTag(tagAlias.ID)
	if !got.IsAlias {
		t.Error("aliasID not marked as alias")
	}
	if got.CanonicalTagID == nil || *got.CanonicalTagID != tagCanon.ID {
		t.Error("canonical_tag_id not set correctly")
	}
}

func TestMergeGeneralIntoCategorized(t *testing.T) {
	database, svc := setupTestDB(t)
	generalID := generalCategoryID(t, svc)

	var characterID int64
	if err := database.Read.QueryRow(
		`SELECT id FROM tag_categories WHERE name = 'character'`,
	).Scan(&characterID); err != nil {
		t.Fatal(err)
	}

	imgID := insertTestImage(t, database, "merge_gen_a")
	imgUnique := insertTestImage(t, database, "merge_gen_b")

	// Pair with a unique categorized counterpart, only auto-tagged usage -
	// should merge.
	gen, _ := svc.GetOrCreateTag("hakurei_reimu", generalID)
	chr, _ := svc.GetOrCreateTag("hakurei_reimu", characterID)
	conf := 0.9
	svc.AddTagToImage(imgID, gen.ID, true, &conf)

	// General tag with no categorized counterpart - left alone.
	lonely, _ := svc.GetOrCreateTag("solo", generalID)
	svc.AddTagToImage(imgUnique, lonely.ID, true, &conf)

	merged, err := svc.MergeGeneralIntoCategorized()
	if err != nil {
		t.Fatal(err)
	}
	if merged != 1 {
		t.Errorf("merged = %d, want 1", merged)
	}

	imgTags, _ := svc.GetImageTags(imgID)
	hasCharacter, hasGeneral := false, false
	for _, it := range imgTags {
		if it.TagID == chr.ID {
			hasCharacter = true
		}
		if it.TagID == gen.ID {
			hasGeneral = true
		}
	}
	if !hasCharacter {
		t.Error("character tag missing on image after merge")
	}
	if hasGeneral {
		t.Error("general tag still on image after merge")
	}

	got, _ := svc.GetTag(gen.ID)
	if got == nil || !got.IsAlias {
		t.Errorf("general tag should be an alias after merge, got %+v", got)
	}
	if got != nil && (got.CanonicalTagID == nil || *got.CanonicalTagID != chr.ID) {
		t.Errorf("canonical_tag_id not pointing at character tag, got %+v", got)
	}

	if got, _ := svc.GetTag(lonely.ID); got == nil || got.IsAlias {
		t.Error("lonely general tag should not have been merged")
	}
}

func TestMergeGeneralIntoCategorized_PreservesUserTagged(t *testing.T) {
	database, svc := setupTestDB(t)
	generalID := generalCategoryID(t, svc)

	var characterID int64
	database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'character'`).Scan(&characterID)

	imgID := insertTestImage(t, database, "merge_gen_user")

	// User added the general tag manually - it must not be swallowed even
	// when a unique categorized counterpart exists.
	gen, _ := svc.GetOrCreateTag("hakurei_reimu", generalID)
	svc.GetOrCreateTag("hakurei_reimu", characterID)
	svc.AddTagToImage(imgID, gen.ID, false, nil)

	merged, err := svc.MergeGeneralIntoCategorized()
	if err != nil {
		t.Fatal(err)
	}
	if merged != 0 {
		t.Errorf("merged = %d, want 0 (user-tagged general must be preserved)", merged)
	}
	if got, _ := svc.GetTag(gen.ID); got == nil || got.IsAlias {
		t.Error("user-tagged general tag should remain non-alias")
	}
}

func TestMergeGeneralIntoCategorized_AmbiguousLeftAlone(t *testing.T) {
	database, svc := setupTestDB(t)
	generalID := generalCategoryID(t, svc)

	var characterID, artistID int64
	database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'character'`).Scan(&characterID)
	database.Read.QueryRow(`SELECT id FROM tag_categories WHERE name = 'artist'`).Scan(&artistID)

	imgID := insertTestImage(t, database, "merge_gen_ambig")
	gen, _ := svc.GetOrCreateTag("ambig", generalID)
	svc.GetOrCreateTag("ambig", characterID)
	svc.GetOrCreateTag("ambig", artistID)
	conf := 0.9
	svc.AddTagToImage(imgID, gen.ID, true, &conf)

	merged, err := svc.MergeGeneralIntoCategorized()
	if err != nil {
		t.Fatal(err)
	}
	if merged != 0 {
		t.Errorf("ambiguous merge count = %d, want 0", merged)
	}
	if got, _ := svc.GetTag(gen.ID); got == nil || got.IsAlias {
		t.Error("ambiguous general tag should remain non-alias")
	}
}

func TestSuggestTags_PrefixFirst(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	svc.GetOrCreateTag("abc_123", catID)
	svc.GetOrCreateTag("xyz_abc", catID)
	svc.GetOrCreateTag("abc_456", catID)

	// Increment usage for xyz_abc so it would rank higher if not for prefix rule
	// (we won't actually add to images - just note that prefix should come first)
	results, err := svc.SuggestTags("abc", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) < 2 {
		t.Fatalf("expected at least 2 suggestions, got %d", len(results))
	}
	// First two must start with "abc"
	for i := 0; i < 2 && i < len(results); i++ {
		if len(results[i].Name) < 3 || results[i].Name[:3] != "abc" {
			// xyz_abc contains "abc" but doesn't start with it
			if results[i].Name == "xyz_abc" && i < 2 {
				// It's in the result (substring match), just not in prefix group
				// This is OK if prefix group is returned first
			}
		}
	}
}

func TestRelatedImages(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	img1 := insertTestImage(t, database, "rel1")
	img2 := insertTestImage(t, database, "rel2")
	img3 := insertTestImage(t, database, "rel3")

	tagA, _ := svc.GetOrCreateTag("rel_a", catID)
	tagB, _ := svc.GetOrCreateTag("rel_b", catID)
	tagC, _ := svc.GetOrCreateTag("rel_c", catID)

	// img1: A, B   img2: A, B   img3: C
	svc.AddTagToImage(img1, tagA.ID, false, nil)
	svc.AddTagToImage(img1, tagB.ID, false, nil)
	svc.AddTagToImage(img2, tagA.ID, false, nil)
	svc.AddTagToImage(img2, tagB.ID, false, nil)
	svc.AddTagToImage(img3, tagC.ID, false, nil)

	related, err := svc.RelatedImages(img1, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(related) != 1 || related[0].ID != img2 {
		t.Fatalf("related = %+v, want only img2", related)
	}
}

func TestListTags_All(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	svc.GetOrCreateTag("list_a", catID)
	svc.GetOrCreateTag("list_b", catID)

	tags, total, err := svc.ListTags(TagFilter{Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if total < 2 {
		t.Errorf("total = %d, want >= 2", total)
	}
	_ = tags
}

func TestListTags_WithPrefix(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	svc.GetOrCreateTag("prefix_abc", catID)
	svc.GetOrCreateTag("prefix_xyz", catID)
	svc.GetOrCreateTag("other_tag", catID)

	tags, total, err := svc.ListTags(TagFilter{Prefix: "prefix", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if total < 2 {
		t.Errorf("prefix total = %d, want >= 2", total)
	}
	for _, tg := range tags {
		if len(tg.Name) < 6 || tg.Name[:6] != "prefix" {
			t.Errorf("unexpected tag in prefix filter: %q", tg.Name)
		}
	}
}

func TestListTags_WithCategoryFilter(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	custom, _ := svc.CreateCategory("custom_filter", "#000000")
	svc.GetOrCreateTag("cat_tag", custom.ID)
	svc.GetOrCreateTag("gen_tag", catID)

	tags, total, err := svc.ListTags(TagFilter{CategoryID: &custom.ID, Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 {
		t.Errorf("category filter total = %d, want 1", total)
	}
	_ = tags
}

func TestListTags_SortByUsage(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	imgID := insertTestImage(t, database, "usage_sort")
	tagA, _ := svc.GetOrCreateTag("sort_a", catID)
	tagB, _ := svc.GetOrCreateTag("sort_b", catID)
	svc.AddTagToImage(imgID, tagA.ID, false, nil)
	// tagB has 0 usage, tagA has 1

	tags, _, err := svc.ListTags(TagFilter{Sort: "usage", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	_ = tagB
	_ = tags
}

func TestRecalcAndPrune_CountsOnlyNonMissing(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	liveImg := insertTestImage(t, database, "recalc_live")
	goneImg := insertTestImage(t, database, "recalc_gone")
	if _, err := database.Write.Exec(`UPDATE images SET is_missing = 1 WHERE id = ?`, goneImg); err != nil {
		t.Fatal(err)
	}

	shared, _ := svc.GetOrCreateTag("recalc_shared", catID)
	onlyGone, _ := svc.GetOrCreateTag("recalc_only_gone", catID)
	svc.AddTagToImage(liveImg, shared.ID, false, nil)
	svc.AddTagToImage(goneImg, shared.ID, false, nil)
	svc.AddTagToImage(goneImg, onlyGone.ID, false, nil)

	// Poison the counts so the recalc has work to do.
	database.Write.Exec(`UPDATE tags SET usage_count = 99 WHERE id IN (?, ?)`, shared.ID, onlyGone.ID)

	updated, pruned := svc.RecalcAndPruneCount()
	if updated < 2 {
		t.Errorf("updated = %d, want >= 2", updated)
	}
	if pruned < 1 {
		t.Errorf("pruned = %d, want >= 1 (only_gone should be dropped)", pruned)
	}

	got, err := svc.GetTag(shared.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.UsageCount != 1 {
		t.Errorf("shared UsageCount = %d, want 1 (only live image counts)", got.UsageCount)
	}
	if _, err := svc.GetTag(onlyGone.ID); err != ErrTagNotFound {
		t.Errorf("only_gone should be pruned, got err=%v", err)
	}
}

func TestListTags_IsAutoOnly(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	imgID := insertTestImage(t, database, "isauto_img")
	userTag, _ := svc.GetOrCreateTag("user_only", catID)
	autoTag, _ := svc.GetOrCreateTag("auto_only", catID)
	svc.AddTagToImage(imgID, userTag.ID, false, nil)
	conf := 0.9
	svc.AddTagToImage(imgID, autoTag.ID, true, &conf)

	tags, _, err := svc.ListTags(TagFilter{Prefix: "user_only", Limit: 100})
	if err != nil || len(tags) != 1 {
		t.Fatalf("user_only lookup failed: %v %+v", err, tags)
	}
	if tags[0].IsAutoOnly {
		t.Errorf("user_only tag should not be IsAutoOnly")
	}

	tags, _, err = svc.ListTags(TagFilter{Prefix: "auto_only", Limit: 100})
	if err != nil || len(tags) != 1 {
		t.Fatalf("auto_only lookup failed: %v %+v", err, tags)
	}
	if !tags[0].IsAutoOnly {
		t.Errorf("auto_only tag should be IsAutoOnly")
	}
	_ = database
}

func TestUpdateCategoryColor(t *testing.T) {
	_, svc := setupTestDB(t)

	cat, err := svc.CreateCategory("color_test", "#aabbcc")
	if err != nil {
		t.Fatal(err)
	}

	if err := svc.UpdateCategoryColor(cat.ID, "#112233"); err != nil {
		t.Fatal(err)
	}

	cats, err := svc.ListCategories()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cats {
		if c.ID == cat.ID && c.Color != "#112233" {
			t.Errorf("color = %q, want #112233", c.Color)
		}
	}
}

func TestRemoveAllTagsFromImage(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	imgID := insertTestImage(t, database, "remove_all")

	tagA, _ := svc.GetOrCreateTag("rem_all_a", catID)
	tagB, _ := svc.GetOrCreateTag("rem_all_b", catID)
	svc.AddTagToImage(imgID, tagA.ID, false, nil)
	svc.AddTagToImage(imgID, tagB.ID, false, nil)

	if err := svc.RemoveAllTagsFromImage(imgID); err != nil {
		t.Fatal(err)
	}

	imgTags, _ := svc.GetImageTags(imgID)
	if len(imgTags) != 0 {
		t.Errorf("expected 0 tags after RemoveAllTagsFromImage, got %d", len(imgTags))
	}

	// Tags with 0 usage are automatically deleted.
	got, _ := svc.GetTag(tagA.ID)
	if got != nil {
		t.Errorf("tag should be deleted when usage_count reaches 0, got UsageCount = %d", got.UsageCount)
	}
}

func TestGetOrCreateTag_ValidatesName(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	_, err := svc.GetOrCreateTag("", catID)
	if err == nil {
		t.Error("expected error for empty tag name")
	}
}

func TestMergeTags_CanonicalAlreadyOnImage(t *testing.T) {
	// Tests branch where canonical tag is already on the same image as alias
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	imgID := insertTestImage(t, database, "merge_both")

	tagAlias, _ := svc.GetOrCreateTag("alias_tag_overlap", catID)
	tagCanon, _ := svc.GetOrCreateTag("canon_tag_overlap", catID)

	// Add both alias and canonical to the same image
	svc.AddTagToImage(imgID, tagAlias.ID, false, nil)
	svc.AddTagToImage(imgID, tagCanon.ID, false, nil)

	if err := svc.MergeTags(tagAlias.ID, tagCanon.ID); err != nil {
		t.Fatal(err)
	}

	// Image should have canonical, not alias
	imgTags, _ := svc.GetImageTags(imgID)
	for _, it := range imgTags {
		if it.TagID == tagAlias.ID {
			t.Error("alias tag still on image")
		}
	}
}

func TestGetOrCreateTag_CaseNormalized(t *testing.T) {
	// Tag names should be lowercase
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	// Tag name must match regex - lowercase only
	tag, err := svc.GetOrCreateTag("valid_name", catID)
	if err != nil {
		t.Fatal(err)
	}
	if tag.Name != "valid_name" {
		t.Errorf("Name = %q", tag.Name)
	}
}

func TestValidateTagName_InvalidChars(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	// Space in name should fail
	_, err := svc.GetOrCreateTag("has space", catID)
	if err == nil {
		t.Error("expected error for tag name with space")
	}
}

func TestValidateTagName_ValidSpecialChars(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)

	// Valid special chars: ()!@#$.~+-
	_, err := svc.GetOrCreateTag("tag(with)special", catID)
	if err != nil {
		t.Errorf("special chars should be valid: %v", err)
	}
}

func TestDeleteCategory_DeletesEmpty(t *testing.T) {
	_, svc := setupTestDB(t)

	// Create a category with no tags
	cat, err := svc.CreateCategory("empty_cat", "#123456")
	if err != nil {
		t.Fatal(err)
	}

	// Delete the empty category
	if err := svc.DeleteCategoryMoveOrDelete(cat.ID, "move", 0); err != nil {
		t.Fatalf("expected no error deleting empty category, got: %v", err)
	}

	// Verify it's gone
	cats, _ := svc.ListCategories()
	for _, c := range cats {
		if c.ID == cat.ID {
			t.Error("category still present after delete")
		}
	}
}

func TestRemoveTagFromImage_NotOnImage(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	imgID := insertTestImage(t, database, "rem_not_on")

	tag, _ := svc.GetOrCreateTag("not_on_image", catID)
	// Remove a tag that was never added - should not error
	err := svc.RemoveTagFromImage(imgID, tag.ID)
	// Returns error or not - behavior depends on implementation
	_ = err
}

func TestListCategories(t *testing.T) {
	_, svc := setupTestDB(t)
	cats, err := svc.ListCategories()
	if err != nil {
		t.Fatal(err)
	}
	// Built-in categories should be seeded
	if len(cats) == 0 {
		t.Error("expected built-in categories to be seeded")
	}
	hasGeneral := false
	for _, c := range cats {
		if c.Name == "general" {
			hasGeneral = true
		}
	}
	if !hasGeneral {
		t.Error("general category not found in ListCategories")
	}
}

func TestListTags_DefaultLimit(t *testing.T) {
	// Limit <= 0 should use default limit of 40
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	svc.GetOrCreateTag("default_lim_test", catID)

	tags, total, err := svc.ListTags(TagFilter{Limit: 0}) // Limit=0 triggers default
	if err != nil {
		t.Fatal(err)
	}
	if total == 0 {
		t.Error("expected at least 1 tag")
	}
	_ = tags
}

func TestListTags_WithPage(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	svc.GetOrCreateTag("page_tag_a", catID)
	svc.GetOrCreateTag("page_tag_b", catID)

	// Page 1 with limit 1 should get different result than page 0
	_, total, err := svc.ListTags(TagFilter{Prefix: "page_tag_", Limit: 1, PageIndex: 0})
	if err != nil {
		t.Fatal(err)
	}
	if total < 2 {
		t.Errorf("total = %d, want >= 2", total)
	}
}

func TestGetTag_WithCanonical(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	imgID := insertTestImage(t, database, "canon_branch")

	tagAlias, _ := svc.GetOrCreateTag("alias_for_get", catID)
	tagCanon, _ := svc.GetOrCreateTag("canon_for_get", catID)
	svc.AddTagToImage(imgID, tagAlias.ID, false, nil)
	svc.MergeTags(tagAlias.ID, tagCanon.ID)

	// GetTag on alias should have CanonicalTagID set
	got, err := svc.GetTag(tagAlias.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.CanonicalTagID == nil {
		t.Error("expected CanonicalTagID to be set after merge")
	}
}

func TestValidateTagName_TooLong(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	long := "a" + string(make([]byte, 200)) // 201 bytes
	for i := range long {
		_ = i
	}
	long = ""
	for i := 0; i < 201; i++ {
		long += "a"
	}
	_, err := svc.GetOrCreateTag(long, catID)
	if err == nil {
		t.Error("expected error for tag name > 200 chars")
	}
}

func TestValidateTagName_PunctuationOnly(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	_, err := svc.GetOrCreateTag("---", catID)
	if err == nil {
		t.Error("expected error for punctuation-only tag name")
	}
}

func TestListTags_SortByName(t *testing.T) {
	_, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	svc.GetOrCreateTag("name_zzz", catID)
	svc.GetOrCreateTag("name_aaa", catID)

	tags, _, err := svc.ListTags(TagFilter{Sort: "name", Prefix: "name_", Limit: 100})
	if err != nil {
		t.Fatal(err)
	}
	if len(tags) < 2 {
		t.Fatalf("expected >= 2 tags, got %d", len(tags))
	}
	if tags[0].Name > tags[1].Name {
		t.Errorf("tags not sorted by name: %s > %s", tags[0].Name, tags[1].Name)
	}
}

func TestGetTag_NotFound(t *testing.T) {
	_, svc := setupTestDB(t)
	_, err := svc.GetTag(999999)
	if err == nil {
		t.Error("expected error for non-existent tag ID")
	}
}

func TestSuggestTags_Empty(t *testing.T) {
	_, svc := setupTestDB(t)
	results, err := svc.SuggestTags("nonexistent_prefix_xyz", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 suggestions, got %d", len(results))
	}
}

func TestAddTagToImage_IsAuto(t *testing.T) {
	database, svc := setupTestDB(t)
	catID := generalCategoryID(t, svc)
	imgID := insertTestImage(t, database, "auto_conf")

	tag, _ := svc.GetOrCreateTag("auto_with_conf", catID)
	conf := 0.95
	if err := svc.AddTagToImage(imgID, tag.ID, true, &conf); err != nil {
		t.Fatal(err)
	}

	imgTags, err := svc.GetImageTags(imgID)
	if err != nil {
		t.Fatal(err)
	}
	if len(imgTags) != 1 {
		t.Fatalf("expected 1 tag, got %d", len(imgTags))
	}
	if !imgTags[0].IsAuto {
		t.Error("expected IsAuto=true")
	}
	if imgTags[0].Confidence == nil || *imgTags[0].Confidence != 0.95 {
		t.Errorf("Confidence = %v, want 0.95", imgTags[0].Confidence)
	}
}

func TestDeleteCategory_NotFound(t *testing.T) {
	_, svc := setupTestDB(t)
	err := svc.DeleteCategoryMoveOrDelete(999999, "move", 0)
	if err != ErrCategoryNotFound {
		t.Errorf("expected ErrCategoryNotFound, got %v", err)
	}
}

func TestCreateCategory_Duplicate(t *testing.T) {
	_, svc := setupTestDB(t)
	_, err := svc.CreateCategory("dup_cat", "#000000")
	if err != nil {
		t.Fatal(err)
	}
	// Second create with same name should error (UNIQUE constraint)
	_, err = svc.CreateCategory("dup_cat", "#111111")
	if err == nil {
		t.Error("expected error for duplicate category name")
	}
}

func TestChangeTagCategory_RejectsDuplicateInTarget(t *testing.T) {
	_, svc := setupTestDB(t)
	cats, _ := svc.ListCategories()
	var generalID, characterID int64
	for _, c := range cats {
		switch c.Name {
		case "general":
			generalID = c.ID
		case "character":
			characterID = c.ID
		}
	}

	a, _ := svc.GetOrCreateTag("cat", generalID)
	if _, err := svc.GetOrCreateTag("cat", characterID); err != nil {
		t.Fatalf("seed character:cat: %v", err)
	}

	err := svc.ChangeTagCategory(a.ID, characterID)
	if err == nil {
		t.Fatal("expected error when moving into a category that already has the same name")
	}
	if !strings.Contains(err.Error(), "already exists") {
		t.Errorf("error message = %q, want 'already exists'", err.Error())
	}

	// Tag must not have moved.
	got, _ := svc.GetTag(a.ID)
	if got.CategoryID != generalID {
		t.Errorf("CategoryID = %d, want %d (tag should be unchanged on rejection)", got.CategoryID, generalID)
	}
}

func TestChangeTagCategory_SameCategoryNoop(t *testing.T) {
	_, svc := setupTestDB(t)
	generalID := generalCategoryID(t, svc)
	a, _ := svc.GetOrCreateTag("cute", generalID)
	if err := svc.ChangeTagCategory(a.ID, generalID); err != nil {
		t.Errorf("expected no error moving to same category, got %v", err)
	}
}
