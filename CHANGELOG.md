# Changelog

## [v1.3.0] - 2026-04-25

### Added
- Per-gallery import/export controls in Settings → Galleries. Imports can replace current gallery or merge with it. You can export/import only the database (as full .db or .json or as a minimal .json) or the database+images as .zip.
- Tag aliases: adding or searching a tag under any of its alias names resolves to the canonical tag. Auto-tagger output goes through the same alias resolution before it lands on a row.
- Gallery delete/replace dialog requires typing the gallery name before it confirms.
- Rebuild-thumbs is auto-queued after a gallery import.
- New `tools/blombooru-to-light.py` exporter that converts a blombooru install into a Monbooru light archive ready for the import flow.

### Changed
- Gallery thumbnail focus outline thickened so keyboard focus is easier to spot.
- Aliases now live in the main tags table instead of a separate one, so they share the tag pipeline end to end.

### Fixed
- Gallery: scan errors from Ingest's duplicate-path lookup surface to the caller instead of being swallowed. Re-extract per-image work runs in a transaction.
- Web: per-file size cap is enforced on uploads. Archive entries are checked for path containment with `filepath.Rel`, and the watcher uses the same approach for gallery-path containment. Removing a gallery folder refuses to follow a symlink.
- Search/UI: the favorites filter button stays active across composed queries. The login-rate-limiter shift is clamped to a non-negative range. `warmCaches` nil-deref race and dead `HX-Refresh` flashes resolved.
- Tags: errors from `RecalcAndPruneIDs` propagate and are logged at callers. `folderonly`, `tagged`, and `autotagged` are reserved as category names. Remaining `rows.Scan` and mark-missing loops surface their errors.
- Gallery video probe: ffmpeg invocations terminate option parsing with `--` before output paths so filenames with leading dashes can't be parsed as flags.
- Filter-keyword set is hoisted to a single canonical source shared between search and web.
- Docker Compose example image path on GitHub points at the right repository.

### Internal
- Cleanup pass on stale comments and dead notes; readme updates; assets recompressed.

## [v1.2.3] - 2026-04-21

### Added
- Status bar row under the gallery header and on the detail topbar.
- Tmux-style footer with gallery / tags / saved-search counts;
- Images: per-image `origin` recording how the file entered the gallery (`ingest` for watcher/sync pickups, `upload` for web and multipart-API uploads, or a caller-supplied string via a new `source` field on `POST /api/v1/images`). Surfaced on the detail metadata panel and in the API `Image` response.
- API: `POST /api/v1/images` and `POST /images/{id}/tags` accept a `source` field for manual tags. The detail page splits the user section into "Tags added by the user" plus one bucket per third-party source.
- Detail page action row: **Move image** and **Delete** are grouped together on the right of the Danger zone.
- Some UI style changes

### Changed
- Destructive buttons (delete, delete-all, delete-selected, remove-all) now render as solid red.
- Settings → Run Auto-Tagger: bulk run buttons relabeled to "Auto-tag all untagged images" / "Auto-tag all images"; the three Remove-all buttons move to a new "Bulk tag removal" subsection so destructive actions live apart from autotag-triggering ones.
- Detail page tag chips drop the "auto" badge; chip names are colored by category instead.

### Fixed
- Deleting an image reached through a Similar-images chain walks browser history one hop back (so Escape on `A → B → C` returns to `B`, then `A`) instead of pushing a fresh history entry and dropping the chain. Deleting the chain's source falls back to the referring search, then the gallery.
- Search and sort state is dropped when switching galleries, so the new gallery opens on a clean view instead of inheriting an irrelevant query.
- Detail-page panels and header align with the gallery frame.
- Detail-page tag chip names render in their category color.
- Excessive margin on the topbar sync button.

## [v1.2.2] - 2026-04-20

### Added
- Detail page: gallery-style search bar at the top of the page; submits as a plain GET `/` so the next view is a full gallery render with the chosen query, sort, and order. The input autocompletes against tags the same way the gallery input does.
- Detail page: folder, source, and saved-search sections appear in the sidebar below the image's tag groups, lazy-loaded so the image tags paint first.
- Detail page: position/total counter (e.g. `34/243`) renders between the back link and the filename when the page was opened from a search, computed from the same key-column comparison as the prev/next arrows.
- Detail page: deleting an image moves to the next image in the referring search (falling back to prev, then the gallery) instead of bouncing back to the grid.
- Detail page: videos autoplay muted with `playsinline`, and spacebar toggles play/pause (suppressed while typing in tag/search inputs).
- Similar-image navigation: clicking a related image carries a back-ref so Escape (and the "← Previous image" link) unwinds chains of any depth one hop at a time via the browser history. The gallery-context UI (X/Y counter, prev/next arrows, "← Images" back link) is hidden once you've switched images, since the current image isn't necessarily in the referring result set.
- Keyboard: `s` focuses whichever `#search-input` is on-screen on any page; `t` keeps focusing the tag input on the detail page, and `f` keeps toggling favorite.

### Fixed
- Related-images probe caps only the `general` bucket to the 15 rarest tags. Previously capping every non-meta category could flatten character/artist/copyright signal to the same 15-slot budget as the noisy general bucket; now those categories pass through uncapped while `1girl`-style tags no longer drag tens of thousands of rows into the candidate `GROUP BY`.
- Per-gallery sidebar caches (folder tree, source counts, visible count) pre-warm at startup instead of populating lazily in parallel on the first cold render.
- Sidebar searches skip the count pass; it was a second full filter evaluation for a number the handler never surfaced. A new partial index on `file_size` (visible rows only) turns sort-by-size over large libraries into an index lookup.
- Detail-page header controls (input, buttons, selects) share a single 28 px height and consistent padding, so buttons no longer render taller than the selects next to them.

## [v1.2.1] - 2026-04-19

### Fixed
- Tag names containing `:` (like `:3`) round-trip cleanly through the search parser, the auto-tagger, and the category-qualified API DELETE endpoint, without colliding with the `category:tag` syntax.
- Detail page: filename next to the back link and in the topbar/title; double disclosure marker on the metadata panel dropped; ComfyUI refs scroll to the referenced node; invalid-tag error clears while typing; search autocomplete no longer rewrites the URL.
- Dialogs: move-image dialog shows the current folder; move/delete-selected dialogs pluralize correctly; "1 image" no longer renders as "1 images"; merge-dialog autocomplete anchors below the input.
- Maintenance: destructive and long-running actions confirm before running and use action-named OK buttons.
- Settings: Schedule section shows last/next run; the two General Save buttons are disambiguated; gallery status renders as two distinct badges; login form is disabled when password auth is off.
- Gallery: upload and delete-all are gated on a degraded gallery; gallery add rejects unreadable and absolute folder paths; page-jump dialog clamps out-of-range entries; the toolbar wraps on narrow viewports; the top-nav stays reachable on narrow viewports; sync-missing now labels images "missing" instead of "removed".
- Sidebar: source-filter tree shows per-source counts; the `[·]` shortcut for `folderonly:PATH` is now visible at the same size as the folder name instead of a hover-only middle dot.
- Watcher now watches every configured gallery, not just the active one.
- Auto-tagger: empty subfolders are hidden; unavailable rows are marked n/a; the detail-page Auto-tag button is hidden in the noop build with the real reason surfaced.
- API: `/api/v1/docs` shows a banner when the API is disabled and gets a Back link at the top; category-qualified DELETE falls through to a literal match when the qualified lookup misses.
- Web: missing `_hover.webp` thumbnails return 204; random sort is visible in the gallery sort dropdown; Save-search and Delete-all hide when there's no query or empty result set; job-flash auto-dismiss shortens once a client has seen it.

## [v1.2.0] - 2026-04-18

### Added
- Move images into another folder from the UI: **Move image** in the detail page's action row, **Move selected** in the gallery's batch bar. Folder input autocompletes against existing folders; missing folders are created, filename collisions are auto-suffixed, and empty source folders are cleaned up after the move.

### Changed
- Sidebar folder tree: each node's count now includes images in every descendant folder, so a parent with only subfolder content shows a non-zero figure. `folder:PATH` in the search bar is recursive to match. `folder:` with no value is now a recursive root (matches every non-missing image); use the new `folderonly:` for the old root-only match.
- Each folder row in the sidebar (including `/`) now has a `·` shortcut that runs `folderonly:PATH` to show only the images directly in that folder, without the rolled-up subfolder content.

### Fixed
- Sidebar folder tree: deeply nested folders no longer slide off the right edge of the sidebar. Indent is now a fixed 12 px per level instead of the quadratic `depth × 12` that accumulated through nested `<li>` padding boxes.

## [v1.1.0] - 2026-04-18

### Added
- Multi-gallery support. Galleries are named directories with their own SQLite DB and thumbnails under `<paths.data_path>/<name>/`. Create, rename, and delete galleries from Settings → Galleries; switch at runtime with the topbar button. The REST API targets a specific gallery via `?gallery=<name>` or the `X-Monbooru-Gallery` header; unknown names return 400.
- Daily maintenance schedule: pick a time of day to run sync, auto-tag, recompute counts, merge general tags, and vacuum. All actions enabled by default; toggle per-action in Settings.
- New search filters `tagged:true/false` and `autotagged:true/false` to scope results by tagging state.
- Sync, delete, and re-extract jobs are now cancellable from the status bar, matching the existing auto-tag cancel behavior.
- Sync reconcile reports live progress and the gallery grid refreshes mid-run; delete and re-extract jobs also refresh the grid in-flight, so ingested or removed images appear as jobs run.
- Auto-tag groups on the image detail page are ordered by confidence and show a percentage next to each tag.

### Changed
- Gallery configuration no longer stores `db_path` or `thumbnails_path`. Each gallery lives under `<paths.data_path>/<name>/monbooru.db` + `/thumbnails/`, created on demand. `active_gallery` is renamed to `default_gallery` and only controls the startup pick; the topbar switcher changes the runtime active gallery without persisting. Legacy `[paths]` migration is removed — existing `monbooru.toml` files must be rewritten as `[[galleries]]` entries on a fresh config.
- Settings → Auto-Tagger section now sits above Authentication.
- "Delete all" is hidden while a batch-delete selection is in progress, to avoid two conflicting destructive buttons.
- Sync on large libraries: duplicate hashing and per-file `chown` are skipped; alias lookups and missing-row updates are batched, so idle syncs on 50k+ libraries finish in seconds rather than minutes.
- Gallery page caches the unfiltered visible count and the per-gallery folder tree, cutting redundant SQL scans on every render.

### Fixed
- Search: chained `OR` terms parse correctly (the tail of a 3+ term chain was being dropped). Non-numeric `width:`/`height:` filters are rejected with a clear error. `-missing:false` returns missing images. Autocomplete drops tags already in the query; the suggest swap target is pinned with `HX-Retarget` so results render in the correct place.
- Errors from form parsing, cursor iteration, folder deletion, config save, tagger result storage, and tag prune now surface to the user instead of being silently dropped.
- Tags: add-tag check is atomic with the insert (no read/write-pool race). `MergeTags` counts non-missing rows only. Typed filter input survives a category change on the tags page.
- Tagger: label filenames are sanitised before they become tag rows. Discovered taggers are preserved across a settings save.
- Jobs: scheduled maintenance holds a schedule reservation while running so a manual action cannot start mid-flight. Rebuild-thumbs honors cancellation. Sync respects cancellation during mark-missing and tag recalc. Job-status auto-clear re-arms on every surface event. Watcher ingests surface while a long-running job is in flight. Scheduler cancellation reports a clean summary instead of "context canceled".
- Gallery: out-of-range page numbers clamp to the last valid page. Switching gallery from the image detail page redirects to home. Per-gallery thumbnail URLs render correctly after a switch; switch errors surface in a flash. Sidebar and tag-list search links carry the active category prefix.
- Web: WAL is truncated after vacuum and the total database footprint is reported in the flash. Upload and categories pages receive the gallery switcher data. Settings → Galleries shows the API suffix instead of the full URL. `DeleteImage` no longer runs an unscoped tag prune.
- Sync progress bar no longer double-counts the reconcile phase.

### Removed
- `MONBOORU_PATHS_GALLERY`, `MONBOORU_PATHS_DB`, and `MONBOORU_PATHS_THUMBNAILS` env overrides. Replaced by `MONBOORU_PATHS_DATA_PATH`.

## [v1.0.0] - 2026-04-16

Initial release.
