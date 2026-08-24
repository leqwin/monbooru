package galleryio

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/tags"
)

// TransferOneImage copies one source image into the target gallery, mirroring
// applyMergeRecords per image: dedup on sha against the target first (merge tags
// into the existing row when present), otherwise copy the file and ingest a
// fresh row with its full provenance. Tags keep their source / auto-tagger
// attribution. With removeAfter the source image is deleted once the copy
// succeeds.
// onDelete is the relations-graph cleanup gallery.DeleteImage takes, passed
// in for the same reason it is there: internal/relations imports the domain,
// so nothing below it can name the service.
func TransferOneImage(srcCx, dstCx gallery.Handle, id int64, removeAfter bool, maxFileSizeMB int, onDelete func(*sql.Tx, int64) error) error {
	var sha, canonPath, folderPath, origin, note, originalSource string
	var isFav, isMissing int
	if err := srcCx.DB.Read.QueryRow(
		`SELECT sha256, canonical_path, folder_path, origin, note, original_source, is_favorited, is_missing FROM images WHERE id = ?`, id,
	).Scan(&sha, &canonPath, &folderPath, &origin, &note, &originalSource, &isFav, &isMissing); err != nil {
		return fmt.Errorf("load image %d: %w", id, err)
	}
	if isMissing == 1 {
		return fmt.Errorf("file missing from disk")
	}

	groups, err := transferTagGroups(srcCx, dstCx, id)
	if err != nil {
		return err
	}
	generalID := LookupCategoryID(dstCx.DB, "general")

	var dstID int64
	var dstMissing int
	var dstCanon string
	err = dstCx.DB.Read.QueryRow(`SELECT id, is_missing, canonical_path FROM images WHERE sha256 = ?`, sha).Scan(&dstID, &dstMissing, &dstCanon)
	switch err {
	case nil:
		// Already in the target: merge tags and provenance into the existing row.
		// If the target's file went missing, restore the bytes and clear the
		// flag so the merge doesn't report success on a still-unviewable row.
		if dstMissing == 1 {
			if err := os.MkdirAll(filepath.Dir(dstCanon), 0o755); err != nil {
				return fmt.Errorf("mkdir dest: %w", err)
			}
			if err := gallery.CopyFileContents(canonPath, dstCanon); err != nil {
				return fmt.Errorf("restore file: %w", err)
			}
			if _, err := dstCx.DB.Write.Exec(`UPDATE images SET is_missing = 0 WHERE id = ?`, dstID); err != nil {
				return err
			}
		}
		if err := applyTransferTags(dstCx.DB, dstCx.TagSvc, dstID, groups, generalID); err != nil {
			return err
		}
		if err := transferProvenance(srcCx, dstCx, id, dstID, note, originalSource, isFav); err != nil {
			return err
		}
	case sql.ErrNoRows:
		rel := filepath.ToSlash(filepath.Join(folderPath, filepath.Base(canonPath)))
		safeBase, err := SafeArchiveDest(dstCx.GalleryPath, rel)
		if err != nil {
			return err
		}
		dst := gallery.UniqueDestPath(filepath.Dir(safeBase), filepath.Base(safeBase))
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return fmt.Errorf("mkdir dest: %w", err)
		}
		if err := gallery.CopyFileContents(canonPath, dst); err != nil {
			return fmt.Errorf("copy file: %w", err)
		}
		if _, err := gallery.DetectFileType(dst); err != nil {
			_ = os.Remove(dst)
			return fmt.Errorf("unsupported file: %w", err)
		}
		img, _, err := gallery.Ingest(dstCx.DB, dstCx.GalleryPath, dstCx.ThumbnailsPath, dst, origin)
		if err != nil {
			_ = os.Remove(dst)
			return fmt.Errorf("ingest: %w", err)
		}
		if err := applyTransferTags(dstCx.DB, dstCx.TagSvc, img.ID, groups, generalID); err != nil {
			return err
		}
		if err := transferProvenance(srcCx, dstCx, id, img.ID, note, originalSource, isFav); err != nil {
			return err
		}
		// The new row keeps Ingest's inbox default whether this is a copy or a
		// move: relations and collections aren't carried, so it needs re-filing.
	default:
		return fmt.Errorf("target sha lookup: %w", err)
	}

	if removeAfter {
		if _, err := gallery.DeleteImage(srcCx.DB, srcCx.GalleryPath, srcCx.ThumbnailsPath, id,
			tags.RemoveAllTagsFromImageTx, onDelete); err != nil {
			return fmt.Errorf("remove source: %w", err)
		}
	}
	return nil
}

// transferTagGroup is a set of category:name tokens that shared one attribution
// on the source image (is_auto plus tagger_name). Grouping lets a transfer
// re-apply each set with the same origin instead of flattening every tag to a
// manual user tag.
type transferTagGroup struct {
	isAuto     bool
	taggerName string
	tokens     []string
	confs      []*float64 // aligned with tokens; nil entry means no score
}

// transferTagGroups reads the source image's tags grouped by attribution and
// recreates any custom category the target lacks (with the source's color) so a
// tag keeps its category instead of collapsing into general; built-in
// categories are seeded in every gallery. The rating tag rides along like any
// other.
func transferTagGroups(srcCx, dstCx gallery.Handle, id int64) ([]transferTagGroup, error) {
	rows, err := srcCx.DB.Read.Query(
		`SELECT t.name, tc.name, tc.color, tc.is_builtin, it.is_auto, it.tagger_name, it.confidence
		 FROM image_tags it
		 JOIN tags t ON t.id = it.tag_id
		 JOIN tag_categories tc ON tc.id = t.category_id
		 WHERE it.image_id = ? AND t.is_alias = 0`, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	type attrKey struct {
		isAuto     bool
		taggerName string
	}
	idx := map[attrKey]int{}
	var groups []transferTagGroup
	for rows.Next() {
		var name, cat, color string
		var builtin, isAuto int
		var taggerName sql.NullString
		var conf sql.NullFloat64
		if err := rows.Scan(&name, &cat, &color, &builtin, &isAuto, &taggerName, &conf); err != nil {
			return nil, err
		}
		if builtin == 0 && LookupCategoryID(dstCx.DB, cat) == 0 {
			if _, err := dstCx.TagSvc.CreateCategory(cat, color); err != nil {
				logx.Warnf("transfer: create category %q: %v", cat, err)
			}
		}
		k := attrKey{isAuto: isAuto == 1, taggerName: taggerName.String}
		i, ok := idx[k]
		if !ok {
			i = len(groups)
			idx[k] = i
			groups = append(groups, transferTagGroup{isAuto: k.isAuto, taggerName: k.taggerName})
		}
		var c *float64
		if conf.Valid {
			v := conf.Float64
			c = &v
		}
		groups[i].tokens = append(groups[i].tokens, tagToken(cat, name))
		groups[i].confs = append(groups[i].confs, c)
	}
	return groups, rows.Err()
}

// transferProvenance copies the sources, per-source commentary and annotations
// onto the target row, then fills an empty note / original source and raises the
// favorite flag so a row that already lived in the target keeps the note,
// original source and favorite it curated.
func transferProvenance(srcCx, dstCx gallery.Handle, srcID, dstID int64, note, originalSource string, isFav int) error {
	sources, err := gallery.SourcesForImage(srcCx.DB, srcID)
	if err != nil {
		return err
	}
	for _, src := range sources {
		if src.Site == "" && src.URL == "" {
			continue
		}
		if err := gallery.AddSourceMembership(dstCx.DB, dstID, src.Site, src.PostID, src.URL); err != nil {
			return err
		}
		if err := gallery.SetSourceSimilarity(dstCx.DB, dstID, src.Site, src.PostID, src.Similarity); err != nil {
			return err
		}
		if src.Commentary != "" && src.Site != "" {
			if err := gallery.SetSourceCommentary(dstCx.DB, dstID, src.Site, src.PostID, src.Commentary); err != nil {
				return err
			}
		}
		if src.Original != "" && src.Site != "" {
			if err := gallery.SetSourceOriginal(dstCx.DB, dstID, src.Site, src.PostID, src.Original); err != nil {
				return err
			}
		}
	}

	anns, err := gallery.AnnotationsForImage(srcCx.DB, srcID)
	if err != nil {
		return err
	}
	// Operator-drawn boxes carry no source identity, so AddManualAnnotation
	// can't key on one; skip any the target already holds so a re-transfer
	// stays idempotent instead of stacking duplicate boxes.
	dstAnns, err := gallery.AnnotationsForImage(dstCx.DB, dstID)
	if err != nil {
		return err
	}
	type manualBox struct {
		x, y, w, h int
		body       string
	}
	haveManual := map[manualBox]bool{}
	for _, a := range dstAnns {
		if a.Manual {
			haveManual[manualBox{a.X, a.Y, a.W, a.H, a.Body}] = true
		}
	}
	byKey := map[[2]string][]models.Annotation{}
	var order [][2]string
	for _, a := range anns {
		if a.Manual {
			box := manualBox{a.X, a.Y, a.W, a.H, a.Body}
			if haveManual[box] {
				continue
			}
			if err := gallery.AddManualAnnotation(dstCx.DB, dstID, a.X, a.Y, a.W, a.H, a.Body); err != nil {
				return err
			}
			haveManual[box] = true
			continue
		}
		k := [2]string{a.Site, a.PostID}
		if _, seen := byKey[k]; !seen {
			order = append(order, k)
		}
		byKey[k] = append(byKey[k], a)
	}
	for _, k := range order {
		if err := gallery.ReplaceSourceAnnotations(dstCx.DB, dstID, k[0], k[1], byKey[k]); err != nil {
			return err
		}
	}

	if _, err := dstCx.DB.Write.Exec(
		`UPDATE images SET note = CASE WHEN note = '' THEN ? ELSE note END,
		                   original_source = CASE WHEN original_source = '' THEN ? ELSE original_source END,
		                   is_favorited = MAX(is_favorited, ?) WHERE id = ?`, note, originalSource, isFav, dstID); err != nil {
		return err
	}
	return nil
}
