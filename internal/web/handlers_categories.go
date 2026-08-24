package web

import (
	"cmp"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/tags"
)

// categoriesData is the /categories page. Galleries shadows the layout's
// own list with the config rows this page's picker renders.
type categoriesData struct {
	baseData
	Galleries  []config.Gallery
	Categories []models.TagCategory
}

func (s *Server) categoriesHandler(w http.ResponseWriter, r *http.Request) {
	cats, err := s.tagSvc().ListCategories()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	s.renderTemplate(w, "categories.html", categoriesData{
		baseData:   s.base(r, "categories", "Categories - "+s.booruName()),
		Galleries:  s.galleryList(),
		Categories: cats,
	})
}

// categoryColors returns a name → color map for every row in
// tag_categories on the active gallery. Used by the tagger config
// dialog so each category label renders in its own colour. Database
// errors yield an empty map so the dialog still renders without colour.
func (s *Server) categoryColors() map[string]string {
	cx := s.Active()
	if cx == nil {
		return nil
	}
	rows, err := cx.DB.Read.Query(`SELECT name, color FROM tag_categories`)
	if err != nil {
		return nil
	}
	defer func() { _ = rows.Close() }()
	out := map[string]string{}
	for rows.Next() {
		var name, color string
		if err := rows.Scan(&name, &color); err != nil {
			return nil
		}
		out[name] = tags.SafeCategoryColor(color)
	}
	if rows.Err() != nil {
		return nil
	}
	return out
}

func (s *Server) categoryCountHandler(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	count, err := s.tagSvc().GetCategoryTagCount(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = fmt.Fprintf(w, `{"count":%d}`, count)
}

func (s *Server) createCategoryPost(w http.ResponseWriter, r *http.Request) {
	if !parseFormOK(w, r) {
		return
	}
	name := r.FormValue("name")
	color := r.FormValue("color")
	color = cmp.Or(color, "#888888")
	if _, err := s.tagSvc().CreateCategory(name, color); err != nil {
		externalErr(w, r, err.Error(), http.StatusBadRequest)
		return
	}
	hxDone(w, r, "Category "+name+" created.", "/categories", "/categories")
}

func (s *Server) updateCategoryPatch(w http.ResponseWriter, r *http.Request) {
	id, ok := idAndForm(w, r)
	if !ok {
		return
	}
	err := s.tagSvc().UpdateCategoryColor(id, r.FormValue("color"))
	if err != nil {
		logx.Warnf("update category %d color: %v", id, err)
	}
	if !isHTMXRequest(r) {
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		http.Redirect(w, r, "/categories", http.StatusSeeOther)
		return
	}
	// The row is the answer: it carries the new colour, the two inputs
	// that just disagreed, and whether a reset is still on offer. A
	// refused colour rides the flash channel and the row swaps back to
	// what storage holds.
	if err != nil {
		setFlashHeader(w, err.Error(), "err", nil)
	}
	cat, err := s.tagSvc().GetCategory(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	s.renderTemplate(w, "partials/category_row.html", cat)
}

func (s *Server) deleteCategoryDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := idAndForm(w, r)
	if !ok {
		return
	}
	action := r.FormValue("action") // "move" | "delete_all"
	action = cmp.Or(action, "move")
	var targetID int64
	if ts := r.FormValue("target_id"); ts != "" {
		targetID, _ = strconv.ParseInt(ts, 10, 64)
	}
	if err := s.tagSvc().DeleteCategoryMoveOrDelete(id, action, targetID); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Surface on /tags (the redirect target), not /categories - the
	// flash rides the shared monbooru:flash channel which lands in
	// whichever flash slot the destination page exposes.
	hxDone(w, r, "Category deleted.", "/tags", "/tags")
}

func (s *Server) renameCategoryPost(w http.ResponseWriter, r *http.Request) {
	id, ok := idAndForm(w, r)
	if !ok {
		return
	}
	newName := strings.TrimSpace(r.FormValue("name"))
	if newName == "" {
		externalErr(w, r, "Name required.", http.StatusBadRequest)
		return
	}
	if err := s.tagSvc().RenameCategory(id, newName); err != nil {
		externalErr(w, r, err.Error(), http.StatusBadRequest)
		return
	}
	hxDone(w, r, "Category renamed to "+newName+".", "/categories", "/categories")
}
