package tags

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/monbooru/monbooru/internal/db"
	"github.com/monbooru/monbooru/internal/models"
)

// Tag-category vocabulary: listing, create/rename/recolor, and the
// move-or-delete teardown.

func (s *Service) ListCategories() ([]models.TagCategory, error) {
	return db.QueryAll(s.db.Read, func(rows *sql.Rows) (models.TagCategory, error) {
		var c models.TagCategory
		var isBuiltin int
		err := rows.Scan(&c.ID, &c.Name, &c.Color, &isBuiltin)
		c.IsBuiltin = isBuiltin == 1
		return c, err
	}, `SELECT id, name, color, is_builtin FROM tag_categories ORDER BY id`)
}

func (s *Service) GetCategory(id int64) (models.TagCategory, error) {
	var c models.TagCategory
	var isBuiltin int
	err := s.db.Read.QueryRow(
		`SELECT id, name, color, is_builtin FROM tag_categories WHERE id = ?`, id,
	).Scan(&c.ID, &c.Name, &c.Color, &isBuiltin)
	if err == sql.ErrNoRows {
		return c, ErrCategoryNotFound
	}
	c.IsBuiltin = isBuiltin == 1
	return c, err
}

func (s *Service) CreateCategory(name, color string) (*models.TagCategory, error) {
	name = strings.TrimSpace(strings.ToLower(name))
	if name == "" {
		return nil, fmt.Errorf("category name must not be empty")
	}
	if !categoryNameRe.MatchString(name) {
		return nil, ErrInvalidCategoryName
	}
	if isReservedCategoryName(name) {
		return nil, ErrReservedCategoryName
	}
	color = strings.TrimSpace(color)
	if !categoryColorRe.MatchString(color) {
		return nil, ErrInvalidCategoryColor
	}
	color = normalizeCategoryColor(color)
	var id int64
	err := s.db.Write.QueryRow(
		`INSERT INTO tag_categories (name, color) VALUES (?, ?) RETURNING id`,
		name, color,
	).Scan(&id)
	if err != nil {
		if isUniqueConstraintErr(err) {
			return nil, ErrCategoryExists
		}
		return nil, fmt.Errorf("creating category: %w", err)
	}
	return &models.TagCategory{ID: id, Name: name, Color: color}, nil
}

// builtinCategoryColors mirrors the tag_categories seed in schema.sql; a
// theme's --cat-<rrggbb> variables name these values, so returning to one
// is what puts a recoloured category back under the theme.
var builtinCategoryColors = map[string]string{
	"general":   "#3d90e3",
	"character": "#00aa00",
	"artist":    "#cc0000",
	"copyright": "#aa00aa",
	"meta":      "#ffaa00",
	"rating":    "#996666",
	"medium":    "#7d4fbf",
	"person":    "#b85c9e",
	"year":      "#4a8fa8",
	"species":   "#ed5d1f",
}

// DefaultCategoryColor returns the seeded colour of a built-in category,
// or "" for one the operator created.
func DefaultCategoryColor(name string) string { return builtinCategoryColors[name] }

func (s *Service) UpdateCategoryColor(id int64, color string) error {
	color = strings.TrimSpace(color)
	if !categoryColorRe.MatchString(color) {
		return ErrInvalidCategoryColor
	}
	_, err := s.db.Write.Exec(
		`UPDATE tag_categories SET color = ? WHERE id = ?`, normalizeCategoryColor(color), id,
	)
	return err
}

func (s *Service) RenameCategory(id int64, newName string) error {
	newName = strings.TrimSpace(strings.ToLower(newName))
	if newName == "" {
		return fmt.Errorf("category name must not be empty")
	}
	if !categoryNameRe.MatchString(newName) {
		return ErrInvalidCategoryName
	}
	if isReservedCategoryName(newName) {
		return ErrReservedCategoryName
	}
	var isBuiltin int
	if err := s.db.Read.QueryRow(
		`SELECT is_builtin FROM tag_categories WHERE id = ?`, id,
	).Scan(&isBuiltin); err != nil {
		return ErrCategoryNotFound
	}
	if isBuiltin == 1 {
		return ErrBuiltinCategoryName
	}
	_, err := s.db.Write.Exec(
		`UPDATE tag_categories SET name = ? WHERE id = ?`, newName, id,
	)
	if err != nil && isUniqueConstraintErr(err) {
		return ErrCategoryExists
	}
	return err
}

// collideNamesShown caps how many names a collision reports; the point is
// to name what to fix, not to print the whole category.
const collideNamesShown = 5

// ErrCategoryMoveCollision reports the names a category-delete move cannot
// reparent because the destination already holds a tag under each.
type ErrCategoryMoveCollision struct {
	Names []string
	More  int
}

func (e *ErrCategoryMoveCollision) Error() string {
	msg := "tags named " + strings.Join(e.Names, ", ") + " already exist in the target category"
	if e.More > 0 {
		msg = fmt.Sprintf("%s (and %d more)", msg, e.More)
	}
	return msg
}

// collidingNames lists the tag names the category holds that the target
// already has, capped for the message.
func collidingNames(tx *sql.Tx, id, targetID int64) ([]string, error) {
	return db.QueryStrings(tx,
		`SELECT t.name FROM tags t
		 WHERE t.category_id = ?
		   AND EXISTS (SELECT 1 FROM tags o WHERE o.category_id = ? AND o.name = t.name)
		 ORDER BY t.name`, id, targetID)
}

// isUniqueConstraintErr reports whether err is the SQLite UNIQUE
// constraint violation (raw error code 2067). Detecting it via the
// stringified message lets the handlers map a clean "name already
// exists" to the user without exposing the column or the error code.
func isUniqueConstraintErr(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// GetCategoryTagCount returns the number of tags in a category.
func (s *Service) GetCategoryTagCount(id int64) (int, error) {
	var count int
	err := s.db.Read.QueryRow(
		`SELECT COUNT(*) FROM tags WHERE category_id = ? AND is_alias = 0`, id,
	).Scan(&count)
	return count, err
}

// DeleteCategoryMoveOrDelete deletes a category. action="delete_all"
// deletes all tags in the category; "move" reparents them to targetID.
func (s *Service) DeleteCategoryMoveOrDelete(id int64, action string, targetID int64) error {
	var closure []int64
	err := s.inWriteTx(func(tx *sql.Tx) error {
		var isBuiltin int
		if err := tx.QueryRow(
			`SELECT is_builtin FROM tag_categories WHERE id = ?`, id,
		).Scan(&isBuiltin); err == sql.ErrNoRows {
			return ErrCategoryNotFound
		} else if err != nil {
			return err
		}
		if isBuiltin == 1 {
			return ErrBuiltinCategory
		}

		switch action {
		case "delete_all":
			tagIDs, err := db.QueryIDs(tx, `SELECT id FROM tags WHERE category_id = ?`, id)
			if err != nil {
				return err
			}
			// Route through the same closure sweep DeleteTag uses so an implied
			// child in a surviving category isn't orphaned when its only parent
			// here is deleted.
			closure, err = deleteTagsTx(tx, tagIDs)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(`DELETE FROM tags WHERE category_id = ?`, id); err != nil {
				return err
			}
		default: // "move"
			// The rating category holds its four canonical rows and nothing
			// else, the same refusal the single-tag move makes.
			if s.ratingCatID != 0 && targetID == s.ratingCatID {
				return ErrRatingTagImmutable
			}
			switch targetID {
			case 0:
				if err := tx.QueryRow(
					`SELECT id FROM tag_categories WHERE name = 'general'`,
				).Scan(&targetID); err != nil {
					return fmt.Errorf("finding general category: %w", err)
				}
			case id:
				return ErrInvalidMoveTarget
			default:
				// Reparenting onto a row that is about to go, or was never
				// there, trips the foreign key; answer in our own words.
				var exists int
				switch err := tx.QueryRow(
					`SELECT 1 FROM tag_categories WHERE id = ?`, targetID,
				).Scan(&exists); {
				case err == sql.ErrNoRows:
					return ErrInvalidMoveTarget
				case err != nil:
					return err
				}
			}
			// UNIQUE(name, category_id) makes the bulk reparent all-or-nothing,
			// so the names that would collide are named before it runs rather
			// than surfacing as the constraint's own error text.
			clash, err := collidingNames(tx, id, targetID)
			if err != nil {
				return err
			}
			if len(clash) > 0 {
				more := max(len(clash)-collideNamesShown, 0)
				return &ErrCategoryMoveCollision{Names: clash[:min(len(clash), collideNamesShown)], More: more}
			}
			if _, err := tx.Exec(
				`UPDATE tags SET category_id = ? WHERE category_id = ?`, targetID, id,
			); err != nil {
				return err
			}
		}

		if _, err := tx.Exec(`DELETE FROM tag_categories WHERE id = ?`, id); err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return err
	}
	if len(closure) > 0 {
		return s.RecalcIDs(closure)
	}
	return nil
}
