package web

import (
	"cmp"
	"context"
	"fmt"
	"html/template"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/monbooru/monbooru/internal/config"
	"github.com/monbooru/monbooru/internal/gallery"
	"github.com/monbooru/monbooru/internal/logx"
	"github.com/monbooru/monbooru/internal/markup"
	meta "github.com/monbooru/monbooru/internal/metadata"
	"github.com/monbooru/monbooru/internal/models"
	"github.com/monbooru/monbooru/internal/search"
	"github.com/monbooru/monbooru/internal/tagger"
)

// rankPageBudget caps the cold back-link rank. It resolves which page
// Back returns to when no cached match list settles it, and the render
// blocks on the answer, so it gets a slice of the detail budget rather
// than all of it.
const rankPageBudget = 150 * time.Millisecond

// annotationView is one positional note ready for the overlay: the box
// geometry as CSS percentages of the rendered image so it scales at any size.
// ID ties the overlay box to its list entry so hovering the entry can
// highlight the box.
type annotationView struct {
	ID    int64
	Body  template.HTML
	Style template.CSS
	doc   markup.Doc
}

// annotationEntry is one box in an editable list. The lists are control
// surfaces - one ellipsized line each - so they carry the flattened text while
// the overlay carries the render, and Body stays the stored markup the edit
// dialog reopens.
type annotationEntry struct {
	models.Annotation
	Text string
}

// sourcePanelView is one collapsible per-source provenance panel: the origin
// plus the annotations it pulled. Built only for a source that carries
// commentary, an original source, or at least one box, so a bare origin adds
// no empty panel.
type sourcePanelView struct {
	models.ImageSource
	Annotations    []annotationEntry
	OriginalLines  []originalLine
	CommentaryHTML template.HTML
	doc            markup.Doc
}

// originalLine is one entry of an origin's newline-joined original source,
// rendered as a link when it is an http(s) URL and as plain text otherwise
// (upstream sources are sometimes dead hosts or free text).
type originalLine struct {
	Text  string
	IsURL bool
}

func buildOriginalLines(original string) []originalLine {
	var out []originalLine
	for _, line := range strings.Split(original, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		out = append(out, originalLine{Text: line, IsURL: gallery.ValidExternalURL(line)})
	}
	return out
}

// buildAnnotationViews turns pixel-space note boxes into percentage-positioned
// overlay entries, clamping each box to the image bounds so oversized stored
// geometry can't drive the overlay outside the media frame. Empty when the
// image has no known dimensions to scale by.
func buildAnnotationViews(img *models.Image, anns []models.Annotation, refs markup.Refs) []annotationView {
	if img.Width == nil || img.Height == nil || *img.Width <= 0 || *img.Height <= 0 || len(anns) == 0 {
		return nil
	}
	fw, fh := float64(*img.Width), float64(*img.Height)
	out := make([]annotationView, 0, len(anns))
	for _, a := range anns {
		x := min(max(a.X, 0), *img.Width)
		y := min(max(a.Y, 0), *img.Height)
		w := min(max(a.W, 0), *img.Width-x)
		h := min(max(a.H, 0), *img.Height-y)
		style := fmt.Sprintf("left:%.4f%%;top:%.4f%%;width:%.4f%%;height:%.4f%%",
			float64(x)/fw*100, float64(y)/fh*100, float64(w)/fw*100, float64(h)/fh*100)
		doc := markup.Parse(a.Body)
		doc.Collect(refs)
		out = append(out, annotationView{ID: a.ID, Style: template.CSS(style), doc: doc})
	}
	return out
}

// buildAnnotationEntries flattens boxes for an editable list.
func buildAnnotationEntries(anns []models.Annotation) []annotationEntry {
	if len(anns) == 0 {
		return nil
	}
	out := make([]annotationEntry, 0, len(anns))
	for _, a := range anns {
		out = append(out, annotationEntry{Annotation: a, Text: markup.Parse(a.Body).Text()})
	}
	return out
}

type detailData struct {
	baseData
	Image             models.Image
	Filename          string // basename of the canonical path, shown on the detail page topbar
	ImageTags         []models.ImageTag
	SDMeta            *models.SDMetadata
	ComfyMeta         *models.ComfyUIMetadata
	ComfyNodes        []models.ComfyNode
	GenericMeta       []models.SDParam
	MangaMeta         *models.MangaMetadata // populated for cbz rows when ComicInfo.xml was parsed
	IsManga           bool                  // shorthand for FileType == "cbz" so the template doesn't string-compare
	MisnamedExt       string                // on-disk extension when it contradicts FileType, "" when they agree
	ResumePage        int                   // reader bookmark for manga rows, 0 when unstarted or finished
	MangaHint         string                // generate-collection dialog prefill: ComicInfo Series, else Title, else the archive filename stem
	Collections       []models.Collection   // every collection this image belongs to, ordered for display
	Sources           []models.ImageSource  // every origin this image came from, primary first
	Annotations       []annotationView      // positional note boxes overlaid on the media
	SourcePanels      []sourcePanelView     // per-source panels (commentary + pulled annotations) below the metadata
	ManualAnnotations []annotationEntry     // operator-drawn boxes, edited under the image beside the Note
	NoteHTML          template.HTML         // the operator's note, rendered
	ImagePaths        []models.ImagePath
	ThumbnailURL      string
	PrevID            *int64
	NextID            *int64
	RefURL            string // predecessor detail URL when the user arrived via a Similar-images click; drives the "← Previous image" back link and Escape
	Ref               string // raw ref=<sourceID> value when valid; forwarded on the delete button so the post-delete redirect returns to the source instead of an arbitrary neighbour
	BackQuery         string
	BackSort          string
	BackOrder         string
	BackPage          string
	BackSeed          string
	// PrevBackPage / PrevBackIdx and their Next twins are the listing
	// position the neighbour links hand on, so a walk that steps off the
	// end of a page carries the next page's number with it instead of
	// asking the database which page it landed on.
	PrevBackPage string
	PrevBackIdx  string
	NextBackPage string
	NextBackIdx  string
	// BackQS is the URL-safe `?back_*=...` fragment carrying every back_*
	// the detail handler saw. Forwarded verbatim on the manga Read /
	// Pages anchors so click-through preserves the gallery context;
	// rendered as template.URL so html/template doesn't URL-encode the
	// `&` separators when the fragment is interpolated after `?page=N`.
	BackQS         template.URL
	BackKVQS       template.URL
	EnabledTaggers []tagger.TaggerStatus // enabled+available taggers offered in the auto-tag control
	// TaggersPresent gates the auto-tag control's render where
	// EnabledTaggers gates whether it is live; TaggerReason titles it
	// while it is not.
	TaggersPresent bool
	TaggerReason   string
	ImageTaggers   []string   // distinct tagger names currently on this image's auto-tags
	ImageSources   []string   // distinct source labels currently carrying tags on this image (is_auto=0 with a tagger_name)
	HasUserTags    bool       // true when at least one operator-added manual tag is on this image
	HasStaleTags   bool       // true when at least one tag on this image is stale (a source dropped it)
	TagSidebar     tagSidebar // the sidebar's tag listing: sections, provenance markers, implied nesting
	// Lookup is the image's scheduled-lookup state: the history line under
	// the lookup button and the control in the Sources field.
	Lookup lookupView
	// PhashDistance is the configured Find-pairs Hamming distance used by
	// the phash row's [search near-duplicates] link. Pulled live from
	// Config.Relations.DefaultDistance so a settings tweak is honoured
	// without a restart.
	PhashDistance int
	// NoPreview is true when no thumbnail file exists for this row, which
	// is also why the phash cannot be backfilled - it is hashed from the
	// thumbnail. PreviewNote carries the reason when the decode budget is
	// the one that refused it.
	NoPreview   bool
	PreviewNote string
	// PreviewScaled says the media area is showing the bounded rendition
	// rather than the file, which the operator has no other signal for -
	// the picture just looks smaller than the Resolution row claims.
	PreviewScaled bool
	PreviewMaxDim int
	// PluginSlot is the detail-actions mount point: the paired and static
	// peers' buttons for this image, absent when no peer offers any.
	PluginSlot pluginSlotView
}

// imageByHashHandler redirects /i/{sha} to the detail page of the image with
// that sha256. An image id is reused after deletion, so an external link by id
// can later open the wrong image; the sha is content-addressed and stable, so
// resolving it here keeps such a link pointing at the right content. A monloader
// "view" link can target an image pushed into a gallery other than the active
// one; since every gallery's DB stays open, a miss in the active gallery falls
// back to the others and switches the active gallery to the one holding the
// image before redirecting. 404 when no gallery carries the hash.
func (s *Server) imageByHashHandler(w http.ResponseWriter, r *http.Request) {
	sha := r.PathValue("sha")
	if d := s.db(); d != nil {
		var id int64
		if err := d.Read.QueryRow(`SELECT id FROM images WHERE sha256 = ?`, sha).Scan(&id); err == nil {
			http.Redirect(w, r, "/images/"+strconv.FormatInt(id, 10), http.StatusFound)
			return
		}
	}

	s.ctxMu.RLock()
	ctxs := make([]*galleryCtx, 0, len(s.contexts))
	for _, cx := range s.contexts {
		ctxs = append(ctxs, cx)
	}
	s.ctxMu.RUnlock()

	for _, cx := range ctxs {
		if cx.DB == nil {
			continue
		}
		var id int64
		if err := cx.DB.Read.QueryRow(`SELECT id FROM images WHERE sha256 = ?`, sha).Scan(&id); err != nil {
			continue
		}
		if err := s.SwitchGallery(cx.Name); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
		http.Redirect(w, r, "/images/"+strconv.FormatInt(id, 10), http.StatusFound)
		return
	}
	s.notFoundHandler(w, r)
}

func (s *Server) detailHandler(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		s.notFoundHandler(w, r)
		return
	}

	ctx := r.Context()
	img, err := loadImage(ctx, s.db(), id)
	if err != nil {
		s.notFoundHandler(w, r)
		return
	}

	// Prev/next navigation is only computed when the referring gallery context
	// is carried through via back_* query params. Resolve the values now so
	// the parallel block below can launch the adjacency lookup alongside the
	// other reads instead of after them.
	back := parseBackContext(r)
	backQ, backSort, backOrder, backPage, backSeed := back.Q, back.Sort, back.Order, back.Page, back.Seed
	backIdx := r.URL.Query().Get("back_idx")

	// A "ref" query param points at the detail page the user just came from
	// (a Similar-images click). When set and valid, the gallery-context UI
	// (X/Y counter, prev/next arrows, "← Images" back link) is suppressed
	// because the user just switched contexts - the current image may not
	// even be in the referring search. back_* still flows through so the
	// rebuilt refURL lands the user back on the source with its original
	// gallery context when they click "← Previous image".
	refURL := ""
	refStrValid := ""
	if refStr := r.URL.Query().Get("ref"); refStr != "" {
		if refID, err := strconv.ParseInt(refStr, 10, 64); err == nil && refID != id {
			refURL = back.DetailURL(refID)
			refStrValid = strconv.FormatInt(refID, 10)
		}
	}

	wantAdjacent := refURL == "" && (backSort != "" || backQ != "")
	if wantAdjacent {
		backSort = cmp.Or(backSort, "newest")
		backOrder = cmp.Or(backOrder, "desc")
	}
	ceiling := resolveCeiling(r, s.Active())

	// Resolve back_page so Escape and "← Back" land on the page that
	// actually contains the current image, even after prev/next walked
	// past the page the user arrived from.
	//
	// The listing position the link carried answers it for nothing: the
	// grid stamps each thumbnail with its page and its row on that page,
	// and each prev/next link hands the neighbour's on, rolling over to
	// the next page when it steps off the end. Every other route to the
	// page number costs a query the render has to wait for - a slice
	// scan of the cached match list when there is one, and a rank COUNT
	// when there is not, which is the one that grows with how deep the
	// page sits.
	var rankPage string
	rankReady := make(chan struct{})
	rankFired := false
	// Set only while the page is still unresolved, so the post-adjacency
	// read below knows whether to look again.
	pendingKey := ""
	pageSize := s.pageSize()
	posPage, posIdx, havePos := 0, 0, false
	if p, err := strconv.Atoi(backPage); err == nil && p > 0 && pageSize > 0 {
		if i, err := strconv.Atoi(backIdx); err == nil && i >= 0 && i < pageSize {
			posPage, posIdx, havePos = p, i, true
		}
	}
	if havePos {
		backPage = strconv.Itoa(posPage)
	}
	if wantAdjacent && pageSize > 0 && !havePos {
		var seed int64
		if backSort == "random" && backSeed != "" {
			seed, _ = strconv.ParseInt(backSeed, 10, 64)
		}
		cacheKey := search.BuildAdjacencyCacheKey(s.activeName, backQ, backSort, backOrder, seed, ceiling.Level())
		switch ids, ok := search.AdjacencyCacheGet(cacheKey); {
		case ok:
			// A populated list settles the question either way: an image
			// it does not carry is one the back_q no longer matches, and
			// ranking it would answer a question nobody asked.
			if page, found := s.pageOfMatch(ids, id); found {
				backPage = page
			}
		case backSort == "similarity":
			// A score is not a column, so the rank has no cursor to count
			// against and would resolve it by re-running the same ranked
			// fan the adjacency lookup below already pays. Read the page
			// off that fan's cached list instead.
			pendingKey = cacheKey
		default:
			rankFired = true
			go func() {
				defer close(rankReady)
				sq := adjacentSearchQuery(backQ, backSort, backOrder, backSeed, ceiling)
				// The render waits on this, so the deadline is the page's
				// to spend: it names which page Back returns to, which is
				// worth a fraction of the detail budget and not the whole
				// of it. Overrunning keeps the page the URL carries.
				ctx, cancel := context.WithTimeout(r.Context(), rankPageBudget)
				defer cancel()
				if arrived, err := strconv.Atoi(backPage); err == nil {
					if page, ok := s.pageHoldingMatch(ctx, sq, id, arrived, pageSize); ok {
						rankPage = strconv.Itoa(page)
						return
					}
				}
				rank, err := search.RankInQuery(ctx, s.db(), sq, id)
				if err != nil || rank < 0 {
					return
				}
				rankPage = strconv.Itoa(rank/pageSize + 1)
			}()
		}
	}
	if !rankFired {
		close(rankReady)
	}

	// The remaining reads are independent of each other and target different
	// tables (or the filesystem for ExtractGeneric). Run them in parallel
	// across the read pool. Related images are fetched lazily via
	// /images/{id}/related so the page paints before that join finishes -
	// on libraries with millions of image_tags rows it was the single
	// largest contributor to detail-page latency. comfyNodes parsing stays
	// sequential - it's pure CPU on the comfyMeta payload and only matters
	// once that read returns.
	var (
		imageTags   []models.ImageTag
		sdMeta      *models.SDMetadata
		comfyMeta   *models.ComfyUIMetadata
		genericMeta []models.SDParam
		mangaMeta   *models.MangaMetadata
		imagePaths  []models.ImagePath
		collections []models.Collection
		sources     []models.ImageSource
		annotations []models.Annotation
		sidebar     tagSidebar
		lookupState lookupView
		prevID      *int64
		nextID      *int64
	)
	csrfToken := s.csrfToken(sessionFromContext(ctx))
	tagMode := readTagModeCookie(r)
	isManga := img.FileType == models.FileTypeCBZ
	// Ingest records the type the bytes declare and leaves the file named
	// as the operator named it, so the two can legitimately disagree.
	misnamedExt := ""
	if claimed := gallery.ExtFileType(img.CanonicalPath); claimed != "" && claimed != img.FileType {
		misnamedExt = strings.ToLower(filepath.Ext(img.CanonicalPath))
	}
	var wg sync.WaitGroup
	wg.Add(9)
	go func() {
		defer wg.Done()
		_, imageTags, _ = s.tagSvc().GetImageTags(id)
		sidebar = s.buildTagSidebar(id, csrfToken, tagMode, imageTags)
	}()
	go func() { defer wg.Done(); sdMeta = loadSDMeta(ctx, s.db(), id) }()
	go func() { defer wg.Done(); comfyMeta = loadComfyMeta(ctx, s.db(), id) }()
	go func() { defer wg.Done(); imagePaths = loadImagePaths(ctx, s.db(), id) }()
	go func() { defer wg.Done(); collections, _ = gallery.CollectionsForImage(s.db(), id) }()
	go func() { defer wg.Done(); sources, _ = gallery.SourcesForImage(s.db(), id) }()
	go func() { defer wg.Done(); lookupState = s.lookupViewFor(s.Active(), id) }()
	go func() { defer wg.Done(); annotations, _ = gallery.AnnotationsForImage(s.db(), id) }()
	go func() {
		defer wg.Done()
		// Skip the generic-EXIF/text-chunk extraction for manga - the
		// archive's bytes don't carry per-image metadata in the way SD
		// images do, and the work would just walk the cbz's central
		// directory for nothing. ComicInfo.xml is loaded separately.
		if !isManga {
			genericMeta = meta.ExtractGeneric(img.CanonicalPath, img.FileType)
		}
	}()
	if isManga {
		wg.Add(1)
		go func() { defer wg.Done(); mangaMeta = loadMangaMeta(ctx, s.db(), id) }()
	}
	if wantAdjacent {
		wg.Add(1)
		go func() {
			defer wg.Done()
			prevID, nextID = s.findAdjacentImages(ctx, id, backQ, backSort, backOrder, backSeed, ceiling)
		}()
	}
	wg.Wait()
	<-rankReady
	if rankPage != "" {
		backPage = rankPage
	} else if ids, ok := search.AdjacencyCacheGet(pendingKey); ok {
		if page, found := s.pageOfMatch(ids, id); found {
			backPage = page
		}
	}

	// Hand the neighbours their own listing position so the walk keeps
	// tracking across a page edge. Without one to carry (a bare link, or
	// a page the grid never stamped) the links fall back to the page
	// this render resolved, which is what they carried before.
	prevBackPage, prevBackIdx, nextBackPage, nextBackIdx := backPage, "", backPage, ""
	if havePos {
		if posIdx > 0 {
			prevBackPage, prevBackIdx = strconv.Itoa(posPage), strconv.Itoa(posIdx-1)
		} else if posPage > 1 {
			prevBackPage, prevBackIdx = strconv.Itoa(posPage-1), strconv.Itoa(pageSize-1)
		}
		if posIdx+1 < pageSize {
			nextBackPage, nextBackIdx = strconv.Itoa(posPage), strconv.Itoa(posIdx+1)
		} else {
			nextBackPage, nextBackIdx = strconv.Itoa(posPage+1), "0"
		}
	}

	var comfyNodes []models.ComfyNode
	if comfyMeta != nil && comfyMeta.RawWorkflow != "" {
		comfyNodes = meta.ParseComfyWorkflowNodes(comfyMeta.RawWorkflow)
	}

	taggerCfg := s.cfgSnapshot()
	enabledTaggers := tagger.EnabledTaggersForGallery(taggerCfg, s.activeName)
	imageTaggers := distinctTaggerNames(imageTags, true)
	imageSources := distinctTaggerNames(imageTags, false)
	hasUserTags := false
	hasStaleTags := false
	for _, t := range imageTags {
		if !t.IsAuto && t.TaggerName == "" {
			hasUserTags = true
		}
		if t.Stale {
			hasStaleTags = true
		}
		if hasUserTags && hasStaleTags {
			break
		}
	}

	// Split annotations into operator-drawn boxes (edited under the image, by
	// the Note) and source-pulled boxes grouped onto their origin's panel.
	var manualAnnotations []models.Annotation
	annBySource := map[[2]string][]models.Annotation{}
	for _, a := range annotations {
		if a.Manual {
			manualAnnotations = append(manualAnnotations, a)
			continue
		}
		k := [2]string{a.Site, a.PostID}
		annBySource[k] = append(annBySource[k], a)
	}
	// Every body the page renders is parsed here so the references they carry
	// resolve against the gallery in one batch rather than one lookup each.
	mkRefs := markup.NewRefs()
	noteDoc := markup.Parse(img.Note)
	noteDoc.Collect(mkRefs)
	annotationViews := buildAnnotationViews(img, annotations, mkRefs)
	var sourcePanels []sourcePanelView
	for _, src := range sources {
		boxes := annBySource[[2]string{src.Site, src.PostID}]
		if src.Commentary == "" && src.Original == "" && len(boxes) == 0 {
			continue
		}
		doc := markup.Parse(src.Commentary)
		doc.Collect(mkRefs)
		sourcePanels = append(sourcePanels, sourcePanelView{
			ImageSource:   src,
			Annotations:   buildAnnotationEntries(boxes),
			OriginalLines: buildOriginalLines(src.Original),
			doc:           doc,
		})
	}
	mkRes := s.resolveMarkup(mkRefs)
	for i := range annotationViews {
		annotationViews[i].Body = annotationViews[i].doc.Render(mkRes)
	}
	for i := range sourcePanels {
		sourcePanels[i].CommentaryHTML = sourcePanels[i].doc.Render(mkRes)
	}

	noPreview, previewNote := false, ""
	if _, statErr := os.Stat(gallery.ThumbnailPath(s.thumbnailsPath(), img.ID)); statErr != nil {
		noPreview = true
		if budgetErr := gallery.DecodeBudgetError(img.CanonicalPath); budgetErr != nil {
			previewNote = budgetErr.Error()
		}
	}
	var pxW, pxH int
	if img.Width != nil && img.Height != nil {
		pxW, pxH = *img.Width, *img.Height
	}
	previewScaled := !isManga && !gallery.IsVideoType(img.FileType) && gallery.NeedsViewRendition(pxW, pxH)

	baseName := filepath.Base(img.CanonicalPath)
	// Prefix the immediate parent folder so a tab strip with several
	// generic basenames (file.png, vol2.cbz, ...) stays distinguishable.
	titleName := baseName
	if parent := filepath.Base(filepath.Dir(img.CanonicalPath)); parent != "" && parent != "." && parent != "/" {
		titleName = parent + "/" + baseName
	}
	mangaHint := ""
	if isManga {
		mangaHint = strings.TrimSuffix(baseName, filepath.Ext(baseName))
		if mangaMeta != nil {
			if series := strings.TrimSpace(mangaMeta.Series); series != "" {
				mangaHint = series
			} else if title := strings.TrimSpace(mangaMeta.Title); title != "" {
				mangaHint = title
			}
		}
	}
	data := detailData{
		baseData:          s.base(r, "gallery", fmt.Sprintf("%s - %s", titleName, s.booruName())),
		Image:             *img,
		Filename:          baseName,
		ImageTags:         imageTags,
		SDMeta:            sdMeta,
		ComfyMeta:         comfyMeta,
		ComfyNodes:        comfyNodes,
		GenericMeta:       genericMeta,
		MangaMeta:         mangaMeta,
		IsManga:           isManga,
		MisnamedExt:       misnamedExt,
		MangaHint:         mangaHint,
		ResumePage:        resumePage(img),
		Collections:       collections,
		Sources:           sources,
		Annotations:       annotationViews,
		SourcePanels:      sourcePanels,
		ManualAnnotations: buildAnnotationEntries(manualAnnotations),
		NoteHTML:          noteDoc.Render(mkRes),
		ImagePaths:        imagePaths,
		ThumbnailURL:      fmt.Sprintf("/thumbnails/%s/%d.jpg", s.activeName, id),
		PrevID:            prevID,
		NextID:            nextID,
		RefURL:            refURL,
		Ref:               refStrValid,
		BackQuery:         backQ,
		BackSort:          backSort,
		BackOrder:         backOrder,
		BackPage:          backPage,
		BackSeed:          backSeed,
		PrevBackPage:      prevBackPage,
		PrevBackIdx:       prevBackIdx,
		NextBackPage:      nextBackPage,
		NextBackIdx:       nextBackIdx,
		BackQS:            back.QueryString("?"),
		BackKVQS:          back.QueryString("&"),
		EnabledTaggers:    enabledTaggers,
		TaggersPresent:    tagger.Present(taggerCfg),
		TaggerReason:      tagger.UnavailableReason(taggerCfg),
		ImageTaggers:      imageTaggers,
		ImageSources:      imageSources,
		HasUserTags:       hasUserTags,
		HasStaleTags:      hasStaleTags,
		TagSidebar:        sidebar,
		Lookup:            lookupState,
		PhashDistance:     s.findPairsDistance(),
		NoPreview:         noPreview,
		PreviewNote:       previewNote,
		PreviewScaled:     previewScaled,
		PreviewMaxDim:     gallery.ViewMaxDim,
		PluginSlot:        s.pluginSlot(r, config.SlotDetailActions, id, img.FileType),
	}
	s.renderTemplate(w, "detail.html", data)
}

// relatedImagesHandler returns the Similar-images mini-grid for an image,
// fetched lazily from the detail page so the initial render isn't blocked
// on the shared-tag aggregation over image_tags.
func (s *Server) relatedImagesHandler(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	// Fetched as a fragment by the detail page; a non-htmx caller
	// (refresh, bookmark, shared link) gets the detail page rather than
	// a chrome-less fragment. The back_* params ride along.
	if !isHTMXRequest(r) {
		dst := "/images/" + idStr
		if r.URL.RawQuery != "" {
			dst += "?" + r.URL.RawQuery
		}
		http.Redirect(w, r, dst, http.StatusSeeOther)
		return
	}
	related, _ := s.tagSvc().RelatedImages(id, 6, readRatingCookie(r))
	back := parseBackContext(r)
	s.renderTemplate(w, "partials/related_images.html", map[string]any{
		"Images":        related,
		"ActiveGallery": s.activeName,
		// Each similar-image link carries ref=<current image> so the
		// destination detail page swaps "← Images" for "← Previous image"
		// (pointing back here) and Escape walks browser history. back_*
		// flow through so that "← Previous image" link restores this
		// page's own gallery context when clicked.
		"SourceID":  id,
		"BackQuery": back.Q,
		"BackSort":  back.Sort,
		"BackOrder": back.Order,
		"BackPage":  back.Page,
		"BackSeed":  back.Seed,
	})
}

// distinctTaggerNames returns the unique tagger_name values on the
// image's tag rows, preserving the first-seen order from the sorted tag
// list. auto=true selects the auto-tag rows (the taggers that ran);
// auto=false the source rows, whose tagger_name SyncSourceTags stamps
// with the originating site. Rows with no tagger_name are excluded.
func distinctTaggerNames(tags []models.ImageTag, auto bool) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range tags {
		if t.IsAuto != auto || t.TaggerName == "" || seen[t.TaggerName] {
			continue
		}
		seen[t.TaggerName] = true
		out = append(out, t.TaggerName)
	}
	return out
}

// pageOfMatch returns the 1-indexed gallery page holding id in a sorted
// match list. Not found when the list no longer carries the image (it
// was deleted, or it never matched the back_q), which leaves back_page
// on whatever the URL carried.
func (s *Server) pageOfMatch(ids []int64, id int64) (string, bool) {
	i := slices.Index(ids, id)
	if i < 0 {
		return "", false
	}
	return strconv.Itoa(i/s.pageSize() + 1), true
}

// pageHoldingMatch names the page of q that carries id, looking at the
// page the operator arrived on and its two neighbours - the only pages
// prev/next can have walked to from there. Each look is one page-sized
// read, so it costs the same whether the page is the second or the two
// hundredth, where counting the rows above the image grows with the
// depth and gives up once it is deeper than it will count. Not found
// leaves back_page on whatever the URL carried.
func (s *Server) pageHoldingMatch(ctx context.Context, sq search.Query, id int64, arrived, pageSize int) (int, bool) {
	if arrived < 1 || pageSize < 1 {
		return 0, false
	}
	for _, page := range []int{arrived, arrived + 1, arrived - 1} {
		if page < 1 || ctx.Err() != nil {
			continue
		}
		probe := sq
		probe.Page, probe.Limit = page, pageSize
		// No total to report and no list worth caching: this asks one
		// question about one page.
		probe.SkipCount, probe.CacheKey = true, ""
		res, err := search.Execute(s.db(), probe)
		if err != nil {
			return 0, false
		}
		if slices.ContainsFunc(res.Results, func(img models.Image) bool { return img.ID == id }) {
			return page, true
		}
	}
	return 0, false
}

// findAdjacentImages finds the prev/next image IDs in the given search context
// via cursor-style LIMIT 1 queries - O(log n) per side instead of loading the
// full matching ID list. seedStr carries the random-sort seed forward from the
// referring gallery so the same shuffle resolves to the same neighbours.
// ceiling AND-chains the cookie-ceiling NotExprs onto the parsed back_q so
// adjacency walks the same set the gallery shows.
func (s *Server) findAdjacentImages(ctx context.Context, currentID int64, queryStr, sortStr, orderStr, seedStr string, ceiling *Ceiling) (prevID, nextID *int64) {
	sq := adjacentSearchQuery(queryStr, sortStr, orderStr, seedStr, ceiling)
	sq.CacheKey = search.BuildAdjacencyCacheKey(s.activeName, queryStr, sortStr, orderStr, sq.RandomSeed, ceiling.Level())
	prevID, nextID, err := search.ExecuteAdjacent(ctx, s.db(), sq, currentID)
	if err != nil {
		logx.Warnf("findAdjacentImages: %v", err)
	}
	return
}

func adjacentSearchQuery(queryStr, sortStr, orderStr, seedStr string, ceiling *Ceiling) search.Query {
	expr, _ := search.Parse(queryStr)
	pinnedCollection := search.PinnedCollectionName(expr)
	expr = ceiling.Apply(expr)
	sq := search.Query{
		Expr:  expr,
		Sort:  sortStr,
		Order: orderStr,
	}
	if sortStr == "order" {
		sq.OrderCollection = pinnedCollection
	}
	if sortStr == "random" && seedStr != "" {
		if seed, err := strconv.ParseInt(seedStr, 10, 64); err == nil {
			sq.RandomSeed = seed
		}
	}
	return sq
}

// md5CellGet computes and stores the image's md5, then renders the
// metadata row's cell. The detail page mounts it on load for rows that
// predate the column, so an operator never has to run the backfill just
// to read one digest. A row over the ingest cap is left to the backfill
// job instead: reading it here would tie up a request per view for a file
// the operator only opened.
func (s *Server) md5CellGet(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(w, r, "id")
	if !ok {
		return
	}
	cx := s.Active()
	if cx == nil || cx.DB == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if maxMB := s.maxFileSizeMB(); maxMB > 0 {
		var size int64
		if err := cx.DB.Read.QueryRow(`SELECT file_size FROM images WHERE id = ?`, id).Scan(&size); err == nil &&
			size > int64(maxMB)*1024*1024 {
			s.renderTemplate(w, "partials/md5_cell.html", map[string]any{"Deferred": true})
			return
		}
	}
	sum, err := gallery.ComputeAndStoreMD5(r.Context(), cx.DB, id)
	if err != nil {
		logx.Debugf("md5 cell %d: %v", id, err)
	}
	s.renderTemplate(w, "partials/md5_cell.html", map[string]any{"MD5": sum})
}
