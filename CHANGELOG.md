# Changelog

## [v1.20.0] - 2026-08-29
### Added
- Add Windows installer, Flatpak, and binary tarballs. ([#114](https://github.com/monbooru/monbooru/issues/114))
- A desktop install adds a first-run setup page to set monbooru's gallery folder and who can reach it, opens a browser, adds a tray icon (on supported systems), can start at login, adds buttons to restart/stop monbooru, adds buttons to open its folders. ([#114](https://github.com/monbooru/monbooru/issues/114))
- Scheduled actions can catch up a missed day or run at startup, plus a Run now button. ([#114](https://github.com/monbooru/monbooru/issues/114))
- Tagger models install from the browser on a desktop install, instead of a shell snippet.

### Changed
- BREAKING: new configs and the container images serve 8455 by default, not 8080; a compose that sets MONBOORU_SERVER_BIND_ADDRESS like the default one is unaffected.
- The container images and bundled downloads ship a trimmed ffmpeg build: far smaller, documented formats only.
- A tagger row reports a missing ONNX Runtime instead of failing when a job runs.

### Fixed
- Switching, renaming or importing a gallery no longer blocks every other request.
- A gallery repoint or rename that fails leaves the gallery open and where it was.
- A finished job releases its memory immediately, and a sync of large images peaks lower.
- A gallery added or imported at runtime no longer holds every manga page it opened.
- A JSON export round trip keeps every tag's source, not just the first writer.
- A batch lookup that fails part way no longer leaves stale counts behind.
- A batch tag add names the tags it refused instead of reporting a clean run.
- An implied tag whose parent is gone shows on the detail page and can be removed.
- A merge import's summary survives the reload that follows it.
- Adding a tag with an unknown prefix says the prefix was kept as part of the name.
- The login page carries the user's theme and custom CSS.
- A long watcher filename no longer pushes the nav out of the top bar.
- The detail page's Prev button no longer hangs over the image.
- Disabling a managed plugin no longer races a relaunch that leaves it running.

Thanks to @gary-host-laptop for the suggestions (https://github.com/monbooru/monbooru/issues/114).

## [v1.19.1] - 2026-08-24
### Added
- The PTR look-up for tags can pull in either direction, keeping the local spelling or the repository's.

### Changed
- The sidebar toggle above 768px is now a rail instead of a topbar button. ([#118](https://github.com/monbooru/monbooru/issues/118))
- A gallery import in merge mode reports what it added and tagged, not a bare success.
- The tag detail page opens faster in a large catalog.

### Fixed
- Deleting one copy of a file the gallery holds twice no longer hides the image.
- A crash while saving settings can no longer leave a truncated config file.
- The gallery JSON export carries perceptual hashes, stale flags, bookmarks and the lookup ledger.
- A rename or move whose template renders nothing is refused, not filed under bare ids.
- `{md5}` renders for an image whose digest was never computed.
- A separator a rename/move template asked for between two tokens survives instead of collapsing.
- The name preview uses the field's full width and clears when the template does.
- A move preview pairs whole paths and shows the destination the job will use.
- The file-duplicates listing honours the rating ceiling.
- A batch category move counts a tag already in the target as skipped, not moved.
- The footer's tag count updates when adding a tag creates a catalog row.
- The Tags page Conflicts badge counts rows the way its listing does.
- A PTR contribution goes up under the spelling the repository knows the tag by.
- A tag carried under a pulled alias no longer offers both an upload and a petition.
- The look-up's match-anywhere option works on a general tag instead of sitting disabled.

Thanks to @CeareDelafont for the suggestion (https://github.com/monbooru/monbooru/issues/118).

## [v1.19.0] - 2026-08-20
### Added
- Filename template for rename, move and upload destinations. ([#75](https://github.com/monbooru/monbooru/issues/75))
- Files arriving by upload, API, watcher or sync can be named from the upload settings. ([#75](https://github.com/monbooru/monbooru/issues/75))
- An image's MD5 shows beside its SHA-256, is searchable via `md5:`. ([#76](https://github.com/monbooru/monbooru/issues/76))
- `upgrade:` finds images whose source serves a different file. ([#80](https://github.com/monbooru/monbooru/issues/80))
- A source row shows the size, dimensions and type the post declares. ([#80](https://github.com/monbooru/monbooru/issues/80))
- `[keep local]` allows to decline [upgrade] until the post changes. ([#80](https://github.com/monbooru/monbooru/issues/80))
- Look a tag up in the PTR under another spelling, or search the PTR's spellings. ([#82](https://github.com/monbooru/monbooru/issues/82))
- Annotations, the image note and per-source commentary carry links and formatting. ([#84](https://github.com/monbooru/monbooru/issues/84))
- The sidebar can be hidden at any width, from the topbar toggle or the `b` key. ([#118](https://github.com/monbooru/monbooru/issues/118))
- An OpenVINO Docker image for auto-tagging on Intel GPUs. ([#119](https://github.com/monbooru/monbooru/issues/119))

### Changed
- More space and better indentation for long tags in the sidebar. ([#111](https://github.com/monbooru/monbooru/issues/111))
- The detail page's metadata column is narrower, with hashes elided in the middle.
- The scheduled lookup gets its own detail page row, one line per backend.
- A paused companion or plugin controls are dimmed with the reason instead of disappearing.
- Compute perceptual hashes becomes Compute hashes and fills the MD5 column too.
- An API push that names no folder now honours the default upload folder.

### Fixed
- Dialogs, wide tables, the detail page and tag columns have better responsive design for phone or narrow windows.
- The top nav takes its own row at narrow widths instead of overflowing the bar.
- A sync reports a file edited into bytes another image holds instead of retrying.
- The duplicate walkers paginate instead of rendering every match in one response.
- Saving the schedule no longer switches the PTR lookup phase back off.
- The Tags page Origin badge counts match the listing they open.
- The PTR tag card no longer offers a pull or a petition that cannot apply.
- An upload rescued into bytes the gallery already holds reports the duplicate, not an error.

Thanks to @CeareDelafont for the suggestions (https://github.com/monbooru/monbooru/issues/75, https://github.com/monbooru/monbooru/issues/76, https://github.com/monbooru/monbooru/issues/82, https://github.com/monbooru/monbooru/issues/118).
Thanks to @gary-host-laptop for the suggestions (https://github.com/monbooru/monbooru/issues/80, https://github.com/monbooru/monbooru/issues/84, https://github.com/monbooru/monbooru/issues/111), (https://github.com/monbooru/monbooru/issues/75).
Thanks to @XEarthlydust for the OpenVINO dockerfile (https://github.com/monbooru/monbooru/issues/119).

Co-authored-by: Klein <87592342+XEarthlydust@users.noreply.github.com>

## [v1.18.1] - 2026-08-13
### Added
- Add a reset button for category colors. ([#113](https://github.com/monbooru/monbooru/issues/113))
- The CPU Docker image is published for arm64 alongside amd64. ([#96](https://github.com/monbooru/monbooru/issues/96))

### Changed
- Category colors are set in the row by typing hex. ([#109](https://github.com/monbooru/monbooru/issues/109))
- The detail page opens faster from deep pages of a large result set.

### Fixed
- The footer's help link points at the current documentation site for non docker builds. ([#112](https://github.com/monbooru/monbooru/issues/112))

Thanks to @gary-host-laptop for the suggestion (https://github.com/monbooru/monbooru/issues/113).
Thanks to @283375 for the suggestion (https://github.com/monbooru/monbooru/issues/96).
Thanks to @CeareDelafont for the suggestion (https://github.com/monbooru/monbooru/issues/109).
Thanks to @PROP65 for the fix (https://github.com/monbooru/monbooru/issues/112).

Co-authored-by: PROP65 <132837123+PROP65@users.noreply.github.com>

## [v1.18.0] - 2026-08-10
### Added
- Plugin support with buttons on the image and batch surfaces. Drop a plugin folder in config/plugins/ and monbooru launches and supervises it.   ([#78](https://github.com/monbooru/monbooru/issues/78))
- Switchable themes in the settings, including a default light theme. Drop a theme folder in config/themes/ to add a new one. ([#102](https://github.com/monbooru/monbooru/issues/102))
- Scheduled job to lookup unsourced images up against the PTR and the online boorus lookup list via monloader. ([#65](https://github.com/monbooru/monbooru/issues/65))
- Pack a collection into a cbz archive, or unpack an archive back into a collection. ([#77](https://github.com/monbooru/monbooru/issues/77))
- `lookup:` filters by what the nightly run has tried, with a per-image switch.
- The file watcher catches a file edited in place, keeping the image's tags.

### Changed
- An image's tags now list in the detail sidebar, grouped by category or by source. ([#106](https://github.com/monbooru/monbooru/issues/106))
- Removing a tag in the sidebar's sources view withdraws that source's claim.
- The detail image is sized so the tag input stays visible without scrolling on medium sized screens.
- Batch Find-tags dialog overhaul.
- The Tags page and tag autocomplete are faster on large catalogs.
- A first search on a large library no longer stalls caching the whole match set.

### Fixed
- A wildcard search no longer reports more matches than the library holds.
- Downloads keep the file's name on disk instead of naming it after its type. ([#75](https://github.com/monbooru/monbooru/issues/75))
- An image too large for a browser to decode is served as a bounded rendition. ([#99](https://github.com/monbooru/monbooru/issues/99))
- A batch action keeps the selection across the reload it ends in.
- Autocomplete dropdowns close on a cleared input instead of reappearing.
- The batch bar keeps its layout when it wraps to a second row.
- The login page loads the custom stylesheet and logo.
- Deleting a tag category names the tags that collide and refuses rating as the target.
- A peer that lost its credentials can pair again instead of being refused.

See https://github.com/monbooru/monbooru-plugins for the plugins and theme repository.

Thanks to @QiE2035 for the suggestions (https://github.com/monbooru/monbooru/issues/78, https://github.com/monbooru/monbooru/issues/102).
Thanks to @gary-host-laptop for the feature (https://github.com/monbooru/monbooru/issues/77), the fix (https://github.com/monbooru/monbooru/issues/98), the suggestions (https://github.com/monbooru/monbooru/issues/65, https://github.com/monbooru/monbooru/issues/106) and bug report (https://github.com/monbooru/monbooru/issues/99).
Thanks to @CeareDelafont for the suggestions (https://github.com/monbooru/monbooru/issues/77, https://github.com/monbooru/monbooru/issues/75).

Co-authored-by: gary-host-laptop <github.striven@aleeas.com>

## [v1.17.1] - 2026-08-04
### Added
- `[review again]` appears on every edge of a relation chain or tree, not just on a pair.

### Changed
- Rating tags accept aliases and merges; only the general/sensitive/questionable/explicit tags stay locked. ([#94](https://github.com/monbooru/monbooru/issues/94))
- The upload drop summary says why files were rejected.
- Review sessions name the nearest shared ancestor, not only a direct parent. ([#71](https://github.com/monbooru/monbooru/issues/71))
- A batch tag add that replaces ratings now reports what it replaced.
- Batch lookup reports progress instead of leaving the status bar empty.
- Some dependencies updates.

### Fixed
- A file's type comes from its bytes, so a misnamed image searches and serves correctly. ([#98](https://github.com/monbooru/monbooru/issues/98))
- MP4 files branded iso5 no longer land as an unsupported file type. ([#100](https://github.com/monbooru/monbooru/issues/100))
- Very large images generate thumbnail instead of failing, and a refused preview says why. ([#99](https://github.com/monbooru/monbooru/issues/99))
- A file moved out of the gallery is marked missing instead of staying listed.
- A malformed EXIF block no longer hangs ingest and the detail page.
- Relation groups emptied by deleted images are cleared instead of listing as 0 members. ([#95](https://github.com/monbooru/monbooru/issues/95))
- Review sessions no longer offer pairs already related through a chain or tree. ([#71](https://github.com/monbooru/monbooru/issues/71))
- Find pairs better calibrates rare tags on small libraries. ([#88](https://github.com/monbooru/monbooru/issues/88))
- The DirectML auto-tagger no longer crashes or fails to load under concurrent inference. ([#97](https://github.com/monbooru/monbooru/issues/97))
- Auto-tagging a missing image no longer inflates its tags' usage counts.
- A folded-duplicate merge that refused every pair now says so instead of reporting success.
- Rebuild thumbnails no longer counts refused rows as rebuilt.
- A failed job's error stays in the status bar for the full dismiss window.
- Deleting a tag category no longer offers itself as the destination for its tags.

Thanks to @QiE2035 for the DirectML fix (https://github.com/monbooru/monbooru/issues/97) and the report (https://github.com/monbooru/monbooru/issues/98).
Thanks to @gary-host-laptop for the reports (https://github.com/monbooru/monbooru/issues/71, https://github.com/monbooru/monbooru/issues/95, https://github.com/monbooru/monbooru/issues/99, https://github.com/monbooru/monbooru/issues/100).
Thanks to @CeareDelafont for the reports (https://github.com/monbooru/monbooru/issues/88, https://github.com/monbooru/monbooru/issues/94).

Co-authored-by: QiE2035 <18079122+QiE2035@users.noreply.github.com>

## [v1.17.0] - 2026-08-01
### Added
- New dialog for configuring each tagger: browse a tagger's labels, rewrite their mappings, and export the merged rules. ([#59](https://github.com/monbooru/monbooru/issues/59))
- More auto-tagger's ONNX Runtime provider: CUDA, DirectML, TensorRT, OpenVINO or CoreML. ([#70](https://github.com/monbooru/monbooru/issues/70))
- The auto-tagger worker runs on Windows. ([#70](https://github.com/monbooru/monbooru/issues/70))
- Add animetimm EVA02 model to the tagger catalog. ([#68](https://github.com/monbooru/monbooru/issues/68))
- The Tags page filter and its API parameter accept `*` wildcards. ([#81](https://github.com/monbooru/monbooru/issues/81))
- Add PWA support for home-screen web app. ([#74](https://github.com/monbooru/monbooru/issues/74))
- Review sessions show the parent two sibling images share. ([#71](https://github.com/monbooru/monbooru/issues/71))

### Changed
- `similar:` and the thumbnail scores rank by shared tags, not by tag rarity. (Find relations pair keeps tag rarity)
- The auto-tagger caps medium, person, species and year tags like the other categories.
- Similarity search and the Find pairs tag pass are much faster on large libraries.
- Steady-state memory drops on large libraries as idle indexes are released.

### Fixed
- Find pairs no longer floods the queue with images that only share a character. ([#88](https://github.com/monbooru/monbooru/issues/88))
- Changing the pair detector settings clears the pairs the old settings queued. ([#88](https://github.com/monbooru/monbooru/issues/88))
- Skipped and already-linked pairs no longer reappear in a review session.
- Galleries created on Windows now import, sync and search correctly on any platform. ([#72](https://github.com/monbooru/monbooru/issues/72))
- A tag batch that changed nothing now fails and says why.
- Relabelling a URL-only source renames it instead of leaving a duplicate. ([#79](https://github.com/monbooru/monbooru/issues/79))
- `a OR -b` is read as a union instead of silently intersecting.
- Syncing an unreadable gallery root no longer marks the whole library missing.
- Tag counts are correct after a metadata-only import.
- A failed export reports an error instead of writing a truncated archive.

Thanks to @QiE2035 for the execution provider support (https://github.com/monbooru/monbooru/issues/70), the Windows path fix (https://github.com/monbooru/monbooru/issues/72) and the web app manifest (https://github.com/monbooru/monbooru/issues/74).
Thanks to @gary-host-laptop for the report (https://github.com/monbooru/monbooru/issues/71), the report and fix (https://github.com/monbooru/monbooru/issues/79) and the fix (https://github.com/monbooru/monbooru/issues/81).
Thanks to @CeareDelafont for the reports (https://github.com/monbooru/monbooru/issues/81, https://github.com/monbooru/monbooru/issues/88).
Thanks to @JustRoxy for the feature request (https://github.com/monbooru/monbooru/issues/68).

Co-authored-by: QiE2035 <18079122+QiE2035@users.noreply.github.com>
Co-authored-by: gary-host-laptop <github.striven@aleeas.com>

## [v1.16.0] - 2026-07-26
### Added
- `similar:<id>` search ranks images by shared tags, with scores on the thumbnails. ([#63](https://github.com/monbooru/monbooru/issues/63))
- Find pairs can queue images sharing rare tags; sessions show and filter by detector. ([#63](https://github.com/monbooru/monbooru/issues/63))
- The manga reader resumes at the page the last session stopped on. ([#56](https://github.com/monbooru/monbooru/issues/56))
- Extract the page being read as its own image, related to its archive. ([#55](https://github.com/monbooru/monbooru/issues/55))
- Replace an image's file in place when a source serves a different version. ([#58](https://github.com/monbooru/monbooru/issues/58))
- Delete a whole source, alias, or implication group of tags at once.
- The Tags page shows and filters by every source that has applied a tag.

### Changed
- Archives are paired by shared tags instead of their cover's pHash.
- Changing the gallery sort or order applies immediately.
- Dense multi-tag and wildcard searches are faster on large libraries.
- The relations hub and sessions load faster on large pair queues.
- PTR controls wait for the index sync instead of erroring or closing the panel.

### Fixed
- Pairs already linked or marked not-related no longer return to the pair queue.  ([#71](https://github.com/monbooru/monbooru/issues/71))
- The copy-tags choice is honoured when a duplicate decision keeps the file.
- Renaming a collection onto another no longer interleaves the merged reading order.
- Removing tags or implications no longer strands implied tags without a justifying parent.
- A search with no matches no longer redirect-loops on page 0.
- A crafted GIF or EXIF block no longer crashes the server during ingest.

### Removed
- The image-level Original source field; per-source artist links replace it.

Thanks to @gary-host-laptop for the suggestions (https://github.com/monbooru/monbooru/issues/55, https://github.com/monbooru/monbooru/issues/56) and bug report (https://github.com/monbooru/monbooru/issues/71)
Thanks to @JustRoxy for the suggestions (https://github.com/monbooru/monbooru/issues/63, https://github.com/monbooru/monbooru/issues/58).

## [v1.15.0] - 2026-07-21
### Added
- Rename image files from the detail page, in batch, or on the collection order tiles.  ([#60](https://github.com/monbooru/monbooru/issues/60))
- Find and merge tags whose spelling was folded before the tag charset widened. ([#64](https://github.com/monbooru/monbooru/issues/64))
- Contribute tags, aliases, and implications to the Public Tag Repository, and pull what it holds.
- Tags dropped by a source's last refresh are flagged stale, filterable and removable in bulk.
- Prev/next arrows and keyboard navigation on the tag detail page.
- Pause and resume the monloader link from the footer connection light.
- Built-in `species` category so hydrus and e621 species tags land typed.
- `source:none` and `source:any` match images with no source, or with any.
- Expand a source group on the image detail to see every tag that source supplied.
- The single-image API response lists every source that supplied each tag.
- Tag detail groups aliases and implications by the source that declared them.

### Changed
- Tag names accept accented, CJK, and punctuated spellings instead of folding to underscores. ([#64](https://github.com/monbooru/monbooru/issues/64))
- Docker images are now `ghcr.io/monbooru/monbooru`.
- The detail page's lookup button covers online boorus; PTR pulls moved to the contribution panel.
- Batch source lookup refreshes every source on an image, not just the first.
- Removing an alias or an implication asks for confirmation first.
- Source, metadata, and wildcard searches are much faster on large libraries.

### Fixed
- `date:` searches match the displayed timezone instead of UTC. ([#61](https://github.com/monbooru/monbooru/issues/61))
- A Tags-page batch with nothing selected no longer applies to every tag in the catalog.
- `relation:` searches refresh after a relation is added or removed.
- Tag autocomplete no longer treats `_` in the typed prefix as a wildcard.
- Remove user tags no longer deletes tags implied by a source's tags.
- An alias chain is flattened when its target becomes an alias itself.
- Tag counts stay correct after a duplicate's tags are copied to the original.
- One refused row no longer aborts a whole batch lookup.
- The API rejects a malformed `tags` field instead of creating the image untagged.
- Watched-folder ingestion no longer races with shutdown or a running job.

### Removed
- The batch "Set source" action.

### Internal
- Contributing to the Public Tag Repository requires monloader v1.5.0 or newer.

## [v1.14.1] - 2026-07-14
### Added
- Uploads show a progress percentage while the files are sent. ([#54](https://github.com/monbooru/monbooru/issues/54))

### Changed
- Max file size defaults to 2 GB and can be raised to 5 GB in Settings. ([#54](https://github.com/monbooru/monbooru/issues/54))

### Fixed
- The auto-tagger no longer files copyright and meta tags under the wrong category.
- Auto-tagged tags no longer land in the wrong category after switching galleries. ([#57](https://github.com/monbooru/monbooru/issues/57))
- Adding a cbz no longer blocks while every page thumbnail is generated. ([#53](https://github.com/monbooru/monbooru/issues/53))
- Uploading a file already in the gallery no longer leaves a second copy on disk.

### Removed
- Moved the help page to the online documentation.

## [v1.14.0] - 2026-07-13
Please update monloader to >=v1.4.0.

### Added
- Sort a collection by filename order from the reorder dialog. ([#37](https://github.com/monbooru/monbooru/issues/37))
- Transfer an image, or a whole search or selection, to another gallery. ([#45](https://github.com/monbooru/monbooru/issues/45))
- Draw your own annotation boxes on an image from the detail page.  ([#48](https://github.com/monbooru/monbooru/issues/48))
- Remove tags scoped to a specific source or auto-tagger.  ([#47](https://github.com/monbooru/monbooru/issues/47))
- Per-image original-source field, plus per-source original artist links. ([#49](https://github.com/monbooru/monbooru/issues/49))
- Per-tag detail page with a usage graph, alias and implication editing, and recent images. ([#52](https://github.com/monbooru/monbooru/issues/52))
- Batch selection bar on the Tags page to tag, merge, or delete many tags at once. ([#52](https://github.com/monbooru/monbooru/issues/52))
- Tags record their creation origin and last use, as sortable columns and filters. ([#52](https://github.com/monbooru/monbooru/issues/52))
- Look up an image against booru, similarity, and Public Tag Repository backends via monloader.
- Public Tag Repository lookups also import tag aliases and implications.
- Set which of an image's sources is the primary one. (used for batch refresh)
- Booru parent/child posts are linked as derivative relations.
- Keyboard shortcuts: `L` opens monloader lookup; `e o` and `e n` edit the original source and note.

### Changed
- Tags page sorts from column headers and filters in place, and hides aliases by default.
- Renaming a tag can keep the old name as an alias pointing to it.
- Each source's commentary and annotations move into their own collapsible panel.
- The per-source "Fetch tags" button is now "Refresh"; source lookups moved to Lookup.
- Timestamps display in the operator's timezone (set `TZ`); stored times stay UTC. ([#50](https://github.com/monbooru/monbooru/issues/50))

### Fixed
- Hydrus and Blombooru imports handle multi-word and namespaced tags correctly.
- A fully failed auto-tag run no longer wipes an image's existing auto-tags.
- Files already present in a newly-watched subdirectory are now ingested.
- `max_file_size_mb = 0` no longer rejects multipart and monloader pushes.
- Same-site posts with different IDs are kept as distinct sources.
- Deleting a tag clears alias chains and cycles that pointed through it.

## [v1.13.0] - 2026-07-04
### Added
- Click-to-order dialog for arranging the images in a collection. ([#37](https://github.com/monbooru/monbooru/issues/37))
- Per-collection find-relations switch to queue its images for the session. ([#39](https://github.com/monbooru/monbooru/issues/39))
- Button to re-fetch a source's tags, commentary and notes from monloader. ([#42](https://github.com/monbooru/monbooru/issues/42))
- Add support for commentaries and note boxes + a per-image own user's note. ([#43](https://github.com/monbooru/monbooru/issues/43))
- Images can carry multiple sources, each with its own tags/commentary/notes.
- `collection:any` search matches images in at least one collection.

### Changed
- Gallery grid defaults to natural aspect-ratio thumbnails instead of square-cropped.
- A duplicate image push merges its tags and source instead of discarding them.
- The monloader link moved from the top bar to the footer.

### Fixed
- A failed JSON gallery import leaves the target gallery intact.

## [v1.12.0] - 2026-07-01
This update include a breaking change with the monbooru and monsender pairing mechanism. Update monloader to >v1.2.0 and use the pairing mechanism.

### Added
- Named, scoped API tokens managed in Settings, each with its own privileges.
- Pairing with monloader from Settings: approval flow, connectivity light.

### Changed
- Monloader now connects via pairing; update monloader past v1.2.0 and re-pair (breaking).
- `type:image` search now excludes gif, mp4, and webm.
- Search flags a filter value that isn't in the filter's allowed set.
- Relations browse offers [review again] on 2-member derivative trees.

### Removed
- The `auth.api_token` config value and `MONBOORU_AUTH_API_TOKEN` env var; use named tokens.

### Fixed
- Relations review-again reopens the exact pair instead of a different image.
- Full-archive import clears the missing flag on the files it extracts.

## [v1.11.1] - 2026-06-24
### Added
- Collapsible Tags header in the gallery and image-detail sidebars.
- Keyboard shortcuts `g c o` and `g c a` to open Collections and Categories.
- Keyboard shortcut `e` + digit to edit a collection on the detail page.

### Fixed
- Tag and source suggestion dropdowns no longer shift the add button when open.

## [v1.11.0] - 2026-06-23
### Added
- An image can belong to multiple collections, each with its own order.
- A Collections page at `/collections` to browse, rename, or dissolve them.

### Changed
- Docker: ffmpeg and ffprobe now come from BtbN's maintained static builds.
- The top-bar "Go to monloader" link opens monloader's queue in the same tab.

### Fixed
- Config: `page_size` is capped so a large value can't break the gallery page.

### Removed
- The Favorites, Collections, and AI sidebar sections.

## [v1.10.2] - 2026-06-13
### Added
- Settings: upload folder applied to web-UI uploads.
- API: `DELETE /api/v1/relations` accepts `promote_original` to promote a new original on removal.

### Changed
- Emptied folders are no longer removed when their last image is moved or deleted.
- Inbox: each web-UI upload forms its own cluster, even when drops land minutes apart.

### Fixed
- Manga pages and image moves refuse a stored path that resolves outside the gallery root.
- ffmpeg/ffprobe runs are bounded by a timeout so a file can't wedge ingestion.
- Custom CSS and logo revalidate on each load, so edits show without disabling the cache.
- Tags page: the list filter treats `_` and `%` as literal characters, not wildcards.
- Bulk category delete now also removes the implied tags those tags had added to images.
- Config: environment-variable overrides are re-validated, warning on invalid values.
- Relations: set-canonical and phash-recompute failures now surface an inline error.

## [v1.10.1] - 2026-06-11
### Fixed
- Natural-fit thumbnails now sit in a uniform square cell.

## [v1.10.0] - 2026-06-11
### Added
- Settings: gallery thumbnails can render at their natural aspect ratio instead of the default cropped square.
- Built-in tag category colors can now be recolored from the Categories settings page.
- A "Go to monloader" top-bar link, set via `server.monloader_url` in a new settings section (hidden when unset).
- `/i/{sha256}` permalink resolves an image by its content hash, stable across id reuse and available in any gallery.
- API: the `/api/v1/` root response now reports the running server version.
- `monbooru healthcheck` subcommand and a baked-in container `HEALTHCHECK`.

### Changed
- Collection links now sort by collection order (ascending) by default; an explicit sort still wins.
- UI palette refreshed for higher contrast.
- Custom CSS: link text now uses a new `--link` variable, while `--accent` stays structural (borders, focus, buttons, badges, fills). A stylesheet that recolored `--accent` for link text should also set `--link`.
- Docker: the CPU image is now distroless and non-root (`distroless/cc`, Debian 13, uid 1000), with ffmpeg bundled as a static binary. The CUDA image is now non-root (uid 1000) and uses static ffmpeg instead of apt's. Both drop the `setcap` file capabilities.

### Fixed
- Folder moves no longer leave phantom duplicate paths in the detail page's Duplicates panel.
- `/favicon.ico` now honors the `server.logo` override.
- Quadlet (`docker/monbooru.container`): the healthcheck probed the wrong port (8081) and needed a shell; it now calls the binary directly.

## [v1.9.4] - 2026-06-06
### Fixed
- Upload: a valid JPEG the image decoder rejected is now re-encoded and accepted.

## [v1.9.3] - 2026-05-31
### Added
- Tags page: API-applied tags show an "api" origin badge.

### Changed
- Auto-tagger: the Configure dialog's per-category column is now Enable, ticked by default, instead of Disable.

### Fixed
- Detail page: confirm before deleting a duplicate file or removing all of an image's user tags.
- API: manga-page serving is restricted to images inside the active gallery.
- Search: the rating-ceiling hidden count is exact for images carrying multiple ratings.
- Relations: declaring two images as duplicates no longer also records them as alternates.
- Collection and source chips with a quote in the name now link to the right search.
- Reader: editing a CBZ in place drops its cached manga pages.
- Relations: find-pairs computes every perceptual hash before probing, so no near-duplicate is missed.
- Tag-removal and batch-delete failures surface an error instead of silently succeeding.
- Settings: the page-size slider can select values below 20.
- Auto-tagger: the per-row threshold Reset shows only when a row is off its default.
- Relations session: swapping the pair swaps the comparison-table columns too.

## [v1.9.2] - 2026-05-28
### Added
- API: create, edit, delete, alias, and merge tags.
- API: manage tag categories and implications.
- API: edit image fields and set source/url/collection on upload.
- API: serve image, thumbnail, and manga-page bytes under token auth.
- API: list galleries with image and tag counts.
- Auto-tagger: Configure checkbox to mute categories.

### Changed
- Upload summary links each duplicate to the existing image it collided with.

### Fixed
- Topbar logo falls back to the bundled logo, not the favicon, when `server.logo` is unset.
- Auto-tagger: per-row Reset restores the catalog default, not a blank cell.

## [v1.9.1] - 2026-05-26
### Added
- Relations browse: [Remove selected] selection toolbar.

### Changed
- Relations session: pair cells render the original image bytes, with `<video>` for mp4 / webm sides instead of the stretched 360px thumbnail.
- Search faster on `mime:` / `type:` filters and short-circuited `relation:any` / `relation:none` queries; merge imports apply tags per image in one transaction.
- Inbox: "Reset" relabelled to "Clear staged".

### Fixed
- Relations browse: card thumbs link to `/images/{id}`, matching every other relations thumb.
- Relations: [unlink] surfaces under self when self is the dup original; Same Collection cards render every sibling without shadowing.
- Relations session: compare slider guards against a stale prior-pair id.
- Search: `date:` accepts bare `T HH` hour precision; backslash escapes inside quoted filter values pass through; pagination and back-link hrefs URL-encode `q` and `back_q`.
- Inbox: cluster header count clarified as the page-local slice; Upload / Clear staged disable on empty staged set; clusters split at exactly 15 minutes; cluster range link drops its hard-coded sort/order.
- Settings: gallery name input hints the allowed characters.
- Image delete: 404 distinguished from generic 500.
- Ingest: video dimensions probed with ffprobe and backfilled by Rebuild thumbnails.
- `.btn-danger` disabled state readable on the red fill; sidebar `+/-` column aligns across tag rows.

### Removed
- Sidebar Inbox section (the top-nav Inbox link still carries the count).

## [v1.9.0] - 2026-05-24
### Added
- Server `name` and `logo` configurable in `monbooru.toml`, swapping the title, topbar wordmark, login screen, and favicon at render.
- Inbox groups newly ingested rows under time-bucketed cluster headers with [Select] / [Unselect] and a `date:T1..T2` time-range link.
- Search `date:` accepts `T HH[:MM]` precision; inbox-cluster links emit the minute form.
- Relations: [dissolve] action on version chains and derivative trees.
- Relations: [original] action under the self cell in dup-group sections.
- Relations: [unlink earlier/newer revision] under the matching chain thumb.
- Relations: [review again] on a 2-member group requeues the pair at the front of the session.
- Relations: Same Collection section includes the current image and numbers every card by position.
- Relations: current-image cell carries a thicker outline and "^ This image" label across dup, alt, and derivative sections.
- Relations browse: per-kind sort pills, 60-cards-per-page pagination, and chain / tree summaries wrapped in collapsed `<details>`.
- Relations session: compare table grows a Collection row that highlights when both sides share the same collection.

### Changed
- Inbox replaces the standalone Upload page: a new "Inbox (N)" menu entry leads to the inbox view with an inline drop zone, the toolbar inbox toggle is gone, the keyboard chord moves from `g u` to `g i`, and the thumbnail badge reads "Inbox" instead of an asterisk.
- Detail page: Related panel reorders to lead with collection siblings and renames its mini-list to "Images with similar tags".
- Detail page: collection chip floats top-right of the image and "Same collection" sits bottom-right.
- Per-image relations page leads with Same Collection, matching the detail-panel ordering.
- Small buttons across cluster headers, inbox actions, and drop-zone footers render at full contrast.

### Fixed
- Thumbnails: reusing an image id sets a new ETag so the browser refreshes its cache instead of serving the stale bytes under heuristic freshness.
- Search: `size:`, `width:`, `height:`, `ratio:`, `tagcount:`, `duration:`, and `pages:` accept the `X..Y` range and the open-ended `..Y` / `X..` forms (previously matched nothing silently).
- Search: `-"tag with spaces"` parses as a negated quoted tag; quoted tokens collapse internal whitespace to underscores.
- Relations: long collection names clip on the Related-images series chip; Same Collection cards show the real `series_order`.
- Relations session: thumbnails scale to fill the cell instead of rendering at natural pixel size.
- `/images/{id}/relations` browser tab title includes the filename so two tabs are distinguishable.

### Removed
- BREAKING: Upload page (folded into the inbox view) and the `g u` chord (use `g i` instead).
- `/relations/browse` [edit chain] / [edit tree] links (use the per-image relations page).

## [v1.8.3] - 2026-05-22
### Added
- Sidebar category headers collapse on a click anywhere across the row.
- Sidebar tag buttons show whether a tag is already in the active search; clicking removes the matching term instead of duplicating it.
- Settings: Copy buttons next to the tagger install commands.

### Changed
- Search and navigation faster across `name:`, `folder:`, category-qualified tags, rating ceiling, `fav:`, `tagcount:`, `relation:none`, `type:`, `mime:`, and popular-tag random sort.
- Steady-state memory drops after rebalancing the SQLite per-connection cache and mmap window.

### Fixed
- Search: `folder:`, `via:`, category-qualified tags, and folder/source/collection autocomplete match case-insensitively.
- Search: boolean filters (`fav:`, `inbox:`, `missing:`, `tagged:`, `autotagged:`) accept yes/no, y/n, on/off, 1/0; unknown values surface as empty.
- Search: `date:` accepts `=`; half-open `..X` and `X..` honour the inclusive bound.
- Search: literal `AND` keyword parses as space-AND so `1girl AND scenery` matches `1girl scenery`.
- Search: random sort actually shuffles on small libraries.
- Search API: response carries the `phash` field to match the single-image surface.
- Lightbox: close-on-background-click ignores empty area inside the element rect; small images scale up to fit the stage.
- Settings: tagger Delete header stops clipping with no taggers installed; new-password input is marked required; inactive gallery action cells render a dash placeholder.
- Relations: find-pairs `replace=true` requires `confirm=REBUILD`; remove-edge handlers behave symmetrically in either direction; find-similar link gated on a non-null phash; not-related browse cards drop the inline compare table.
- API: REST version edge direction matches the session and web handlers.

## [v1.8.2] - 2026-05-20
### Added
- Detail page: in-page zoomable image viewer with shortcut `v`, dimmed backdrop, click-outside-to-close, and 0.5x-4x zoom range.
- Reader: clicking a page opens the in-page image viewer.
- Footer shows "N hidden" next to the match count under an active rating ceiling.

### Changed
- Rating ceiling extends to sidebar counts, relations hub and browse counters, relations bulk-delete, and batch / auto-tag.

### Fixed
- Relations hub and browse counters count chains and trees, not edges.

## [v1.8.1] - 2026-05-19
### Added
- Unified `/relations/browse?kind=` with tabs per kind; group tabs carry per-card checkboxes for multi-select merge.
- `/images/{id}/relations` shows the full version chain (left-to-right with arrows) and derivative tree; current image accented.
- Comparison table on pair-shape views (session, browse, per-image): resolution, file size, added-on, tag count, tag delta, format. 
- Session Duplicate decision opens a Delete-now / Keep modal with an optional copy-tags pre-step; Esc and Enter default to Keep, D triggers delete.
- Add-relation dialog: preview tile per side; pasting `/images/{id}` or a full URL fills the id.
- Browse cards: declared-at line under the toolbar and an ingest-date pill on every thumbnail.
- Two-column hub (Find / Browse) with state-aware accent; Start a session disables with a tooltip when the queue is empty.
- Marked-duplicates walker grows a Marked-at column.

### Changed
- Plain-language relations labels on user-facing surfaces: Same image, Variant, Newer revision, Based on, Not related. Search keywords, schema, REST API, and CSS classes stay on technical names.
- Session UI rebuild: Swap and Compare on a centred bridge between thumbs, accent per primary decision button, button hover writes the committed `{left}/{right}` sentence to the prompt; remaining-pair counter moves into the topbar; compare slider gains a chevron handle and Left/Right caption strip.
- Marked-duplicates walker sorts rows newest-first and badges the Original column.

### Fixed
- Version chain: extending an existing chain (X -> Y, then Y -> Z) is allowed; only schema-enforced collisions and cycle closures reject.
- Derivative edge: two-node loops (A -> B then B -> A) and longer cycles caught by walking the source chain.
- Chain edge replace drops only the rows that collide with the new pair, so other chain entries survive when overwriting one step.
- Browse cards, duplicates walkers, session pair feeder, and hub queue counters honour the rating ceiling; empty-queue branch offers a one-click ceiling raise.

### Removed
- Session inline 1-6 legend (the `?` cheat-sheet still documents the shortcuts).

## [v1.8.0] - 2026-05-16
### Added
- Relations system. New `/relations` top-level page (between Tags and Settings) with Find / Duplicates / Browse sections. A 64-bit perceptual hash (pHash) computed at ingest, with a backfill job for existing libraries, feeds a background find-pairs job that fills a queue of near-duplicate candidates. Sessions step through the queue in a swipe-style UI: decide Duplicates / Alternates / Version / Derivative / Not related / Skip with keyboard shortcuts; Compare opens a draggable before/after slider; Swap-sides flips parent/child for directional types. Browse-pair (with a Not-related tab and unlink) and Browse-groups pages round out the surface. Settings -> Maintenance grows `Compute perceptual hashes` and `Rebuild pair queue` buttons; a daily scheduler phase runs find-pairs on its own.
- Detail-page Related entries panel above Similar entries (with collection siblings and a see-all) and a full `/images/{id}/relations` grid sectioned by relation type. The detail page grows a pHash row (copyable, with a `~4` link that runs the matching Hamming-distance search) and an Add-relation dialog with parent/child swap, autocomplete, an Overwrite button for conflicts, and an adaptive sentence.
- Search keywords. Twelve metadata filters: `name`, `size`, `mime`, `ratio`, `tagcount`, `duration`, `hash`, `prompt`, `model`, `sampler`, `seed`, `via`. `id:N` exact match. `phash:` bare, `phash:<hex>` exact, and `phash:<hex>~<d>` Hamming-distance lookup. `relation:<type>` closed vocabulary covering `duplicate`, `original`, `alternate`, `version`, `derivative`, `source`, `collection`, `any`, `none`.
- Search autocomplete: level-2 suggestions for `name`, `model`, `sampler`, `prompt`; values containing spaces are quoted automatically; the `system:` cheat-sheet color-codes category rows and caps the dropdown at 60vh so the 40+ rows no longer hide behind a tiny scrollbar.
- Sidebar Inbox, Favorites, and Sources panels matching the existing Tags / Folders / Collections layout; clicking a section name toggles it.
- Settings rework. Numeric inputs become sliders with bounds, ticks, and updated ranges; scheduler time-of-day moves to a 24h HH:MM text input; schedule checkboxes lay out in three columns; Stats is a 2-column block with stabilised size cells. New Relations subsection collects the find-pairs schedule and default session order. Default `max_file_size_mb` raised to 512; default auto-tagger `idle_release_after_minutes` raised to 15.
- Detail page (non-relations): copyable Image ID row above Path; click SHA-256 to copy; autocomplete in the source edit dialog; Source value rendered as a search link; flash confirmations after source / url / collection / order edits; editable metadata rows moved below a separator.
- API: `GET /api/v1/images/{id}/relations`, `POST /api/v1/relations`, `DELETE /api/v1/relations`. `GET /api/v1/images/{id}` grows a `phash` field.
- In-app help: search-syntax table subcategorised.

### Changed
- Footer shows the gallery's collection count instead of the saved-search count.

### Removed
- Settings -> Maintenance: `Merge general tags` (superseded by the v1.7 category dispatchers).

### Fixed
- Search: `date:>=YYYY-MM-DD` and `date:<=YYYY-MM-DD` extend to end-of-day; bare `<category>:` routes to `cat:<category>` instead of match-all; leading `OR` with no left operand is dropped; `name`, `folder`, `source`, `collection`, and `category:` filters are case-insensitive.
- API: aliases populated in the search response; batch tag add / remove route through the same single-image helpers as the UI (fan-out, alias resolution, implication propagation).
- Upload: View-inbox count is ceiling-aware on the OOB swap.
- Maintenance: `removeDuplicates` takes a job slot and batches its DELETE in chunks.
- Gallery: `DeleteImage` unlink gated on `PathInside` so an alias outside the gallery root can't be removed via a delete request; image decode bounded by a megapixel cap so a giant source can't OOM-kill the thumbnailer or tagger; `batchDelete` fetches its targets in a single `IN` query instead of N+1.
- Detail page: SHA-256 digest aligned with its label; external-edit dialogs close only on form responses.
- Source autocomplete quotes values with spaces.

### Internal
- Search latency: NOCASE-collated partial visible indexes for `source` / `folder` / `series`; AND-driver materialisation bounded to the bucket window; category-qualified tag pre-resolved to skip a per-row 2-table join; partial seed indexes on `sd_metadata` and `comfyui_metadata`; sort-index hint trimmed to filters without a covering column index; `basename_lower` indexed column added; sidebar fan-out reduced to `image_tags` and `saved_searches`; re-`ANALYZE` on boot after v1.7.2 schema additions.
- Schema additions auto-applied on upgrade: `images.phash`, `images.duration_seconds`, `images.basename_lower`, the relations tables (`dup_groups`, `dup_group_members`, `alt_groups`, `alt_group_members`, `version_edges`, `derivative_edges`, `not_related_pairs`, `potential_relation_pairs`, `relation_session`). Per-gallery BK-tree built at startup and kept in lockstep with row writes via the `gallery.PhashHooks.OnStored` hook. New SQL scalars `basename()` and `hammingdist()` registered at bootstrap.

## [v1.7.2] - 2026-05-14
### Added
- Auto-tagger runs in a `tagger-worker` subprocess by default. ORT runtime and CUDA libraries live in the child; releasing idle (or the new `Free memory` button) SIGTERMs the worker so its ~1 GB RSS returns to the kernel rather than staying mapped to the parent. Settings → Stats grows an Auto-tagger row with worker PID, mode, cache state, and per-job RSS; cache transitions log on load / reuse / release. CUDA JIT cache persists under `<data_path>/.nv-cache` to speed up next starts.
- Settings → Maintenance: `Free memory` button runs the same reclaim bundle the idle ticker uses (SQLite caches, Go heap, tagger worker SIGTERM) on demand and reports freed bytes.
- Settings → Auto-Tagger form exposes `idle_release_after_minutes` next to `use_cuda` and `parallel`
- Search autocomplete covers `source:` label values.
- Tags page: the Alias→ / Repoint→ dialog mints a canonical when the typed name doesn't yet exist, matching the Create-alias dialog.

### Changed
- Auto-tagger default `idle_release_after_minutes` drops from 30 to 10.
- Tags page: Alias/Repoint redirects to `/tags?origin=alias&q=<source>` so the user lands on the fresh alias row.
- Stats panel: Native row no longer counts the auto-tagger; the worker's ORT heap is on its own row.
- Detail page: tag-focus mode anchors the cursor to the previous tag after a delete instead of falling back to the first row.

### Fixed
- Detail page: `t` shortcut finds the Auto-tag button.
- Tags page: `delete-search` reloads the listing once the background job finishes instead of on a fixed 800 ms timer; create-tag redirect lands on `/tags?q=<name>` instead of being clobbered by a reload race; create-tag and create-alias names URL-escape in the redirect.
- Dialogs: autocomplete dropdowns render past the dialog edge instead of being trapped in an inner scrollbar.
- Gallery: inbox-filter tooltip count honours the rating ceiling.
- Search: `date:>=YYYY-MM-DD` and `date:<=YYYY-MM-DD` parse correctly (the two-char operators were never tried before the single-char ones).

### Internal
- Auto-tagger Backend interface lifts the cache, session pool, and per-image inference loop behind a single abstraction; the in-process implementation registers itself via `SetBackend`. The IPC backend pairs with a `tagger-worker` subcommand and a Unix-domain socket; `MONBOORU_TAGGER_BACKEND=inproc` remains as an escape hatch for a regression.
- Auto-tagger memory: glibc arena trim every 256 processed images bounds mid-run RSS; CUDA provider options retuned for batch=1 inference (HEURISTIC algo search, kSameAsRequested arena, in-default-stream copies); CPU initializer copies dropped when the CUDA EP is enabled; ORT intra/inter-op thread budget split across `parallel` workers.
- Auto-tagger hot path: float32 input buffer pooled by tile size; pad-white walk fused with the canvas zero; label-routing slice computed once per loaded tagger and reused.

## [v1.7.1] - 2026-05-14
### Added
- API: `GET /api/v1/images/{id}/tags` returns the per-image tag list with the same shape as the POST/DELETE counterparts.
- Search: `*xyz` parses as a suffix wildcard; `collection:` values autocomplete.
- Tags page: alias rows accept a category change; the create-alias dialog auto-creates a missing canonical target.
- Tag and alias mutations surface their result in the tag-flash slot.
- Detail page: topbar reshuffled; the inbox button labels by current state.
- Batch: selection bar and Actions chooser share one ordering; new `Toggle favorite` action; `Send to inbox` / `Remove from inbox` collapse into `Toggle inbox`.

### Changed
- Logger levels reorder `Debug < Info < Warn` to match slog; runtime semantics unchanged.

### Fixed
- Auto-tagger: pad-and-resize peak allocation capped to `size^2` so large sources can't OOM-kill the container with multiple taggers loaded.
- Search: `tagged:true` / `autotagged:true` partition counts cached and invalidated alongside image-tag membership writes; `inbox:` shapes pin `idx_images_ingested_visible` to stay inside the search budget.
- Search: bare `system:` returns no-match instead of matching every image; random sort with a small seed actually shuffles.
- Reader and gallery: out-of-range `?page=N`, `?page=0`, and `?page=-1` 303 to the clamped value so the URL agrees with what the pager renders.
- Detail page: clearing the collection nulls the stored order; pasting many tags commits in a single transaction.
- Tags page: duplicate category names surface as a friendly error; zero-usage rows skip the origin badge; alias canonical-suggest dropdown anchors to the input width.
- Categories: new-category color picker aligns with the text and Add controls.
- API: `tags` is required on POST/DELETE image-tag mutations; `page_size` aliases `limit`; `via` rejects selector-breaking characters; cbz tag mutation drops its leftover 415; path-mode ingest enforces gallery-root containment and rejects non-decodable files; delete cleans up an emptied source folder by default.
- Maintenance: prune-missing runs as a job, blocking concurrent submissions.
- Gallery: alias path collisions caught before the move transaction; watcher suppresses events during delete and tag jobs; thumb checkbox blurs on change so keyboard shortcuts fire after a click; bulk delete skips `RemoveMangaCache` for non-cbz rows.
- Tagger service: rating-category lookup miss is logged instead of silently producing a Service with `ratingCatID=0`.
- Router: `custom_css` containment check resolves symlinks first so an operator can't accidentally point it at a symlinked escape.
- Scheduler: cancel aborts the whole run instead of just the current phase.
- UI: toolbar tooltip keeps the inbox count visible when a rating ceiling is applied.
- Batch tag: chunk size drops to 100 rows when at least one resolved tag has implications so foreground requests slip between commits.

### Internal
- DB bootstrap: ANALYZE gated on `PRAGMA user_version`; partial-index ANALYZE bounded with `analysis_limit = 400`. Steady-state cold starts skip the pass.
- `internal/web/handlers_batch.go` extracts `resolveBatchScope`; `gallery.Sync` splits into per-case helpers; `galleryCtx` cached-count accessors share one helper; favorite/inbox toggle handlers share `toggleBoolColumn`.

## [v1.7.0] - 2026-05-11
### Added
- Comics & manga. `.cbz` / `.zip` archives ingest as single entries with `page_count`, optional `Collection` and order, and a parsed `ComicInfo.xml` panel. In app reader (`/images/{id}/read?page=N`) with bottom-bar controls and side click-to-prev/next chevrons; pages grid (`/images/{id}/pages`) with keyboard nav. Auto-tagger iterates every archive page. Search keywords `type:image` / `type:archive` / `pages:<op><n>`. Detail-page `Open in reader (R)` and `See all pages (P)` buttons. Manga `N pages` pill on gallery thumbnails.
- Collections. Any image can carry a `Collection` label and an optional order integer (rendered `#N`). Surfaces: detail-page edit dialog with autocomplete, search keyword `collection:<value>`, sidebar Collections section above Folders, batch `Set collection` action, `Order` sort (collection then order), thumbnail pill bottom-left.
- Search keyword `type:animated` replaces `animated:true` / `animated:false`. The `type:` vocabulary unions `image`, `archive`, and `animated`; static-only is reachable as `-type:animated`.
- Detail-page e-leader chord shortcuts: `e o` order, `e s` source, `e c` collection, `e u` url.
- Auto-tagger: frequency-gated merge with a per-category top-K cap (new `tagger.aggregation.min_hit_fraction` knob).  Manually re-adding an auto-tagged tag promotes the row to user-owned. Cancellation now responds inside the manga page loop, and status emission is serialised across parallel workers so progress reads as a stable side-by-side list.
- Upload: live progress feedback (`Uploading… <loaded> / <total> (NN%)`) and a `Saving on server…` label once bytes are fully sent.
- Detail page: orange warn-flash on mixed-outcome tag adds (some accepted, some rejected) bundling every applicable part; inline flash when a manual rating overwrites another.
- Settings: confirm dialog before regenerating the API token.
- Gallery grid: `ArrowDown` on the last visible row scrolls the layout to its bottom (and `ArrowUp` on the first to its top) so the search bar and pagination stay keyboard-reachable.

### Changed
- Detail-page metadata rows reorder to `Added via` / `Source` / `URL` / `Collection` / `Order` / `Pages`. `[edit]` markers render as inline text links. Duplicate-paths section relabelled `Duplicates` and restructured as a vertical list. `Similar images` renamed to `Similar entries` and partitioned by file type.
- Sidebar heading `AI source` shortened to `AI`; Collections section moves above Folders.
- Maintenance: `VACUUM` runs in a goroutine instead of on the request thread.
- Gallery export JSON bumped to v2 (adds `manga_metadata` array); v1 imports stay supported. Light-manifest version split from the full-export version so it stays at `1`.
- `tagged:true` / `autotagged:true` exact partition counts compute `visible_total - untagged_visible` so the two halves sum to the visible total.

### Fixed
- Sync: in-place edits (file rewritten at same byte length) detected via a new `image_paths.mtime` column; sha, dimensions, side-table metadata, and thumbnail refresh in place so user-curated tags survive.
- Ingest / sync: when a known SHA reappears at a new path, the previous canonical row is demoted to alias instead of being silently rewritten in place.
- Watcher: re-debounces on `Write` events so slow `.cbz` copies aren't ingested mid-flush.
- Search: saved searches persist `sort` / `order` / `seed`; `View inbox` link on the upload page shows a ceiling-aware count; bare-category-prefix filter (`?q=character:`) routes to the category-only filter; pages clamped past the end issue `303` (or `HX-Push-Url`) so the URL matches what the user sees.
- Gallery grid: overlay badges (favourite, inbox, missing) swap unicode for inline SVG so glyphs align across fonts; manga `N pages` pill anchors bottom-right of `.thumb-card`.
- Settings: active-gallery row counts pinned to the `baseData` snapshot so footer and table cells agree; password verification gates on the stored hash, not the `EnablePassword` flag; password-set form gets a hidden `username` field for password managers and accessibility tools.
- Maintenance: alias / duplicate-path deletes route through `gallery.PathInside` so they can't unlink files outside the gallery root; direct hits on `/settings/maintenance/duplicates-list` 303 to `/settings#maintenance`.
- Server: `custom_css` path validated against a trusted-dir allowlist at config load; `/favicon.ico` aliases to `/static/favicon.png`.
- Layout: footer wraps on viewports under 600 px; switch-gallery dialog closes on the success branch.
- Jobs: `Manager.Cancel` releases `scheduleHeld` so an early-return doesn't leak the reservation; `prune-thumbs` joins the cancel-button gate.
- Scheduler: cache invalidation moved into a `galleryCtx.Sync` wrapper so manual and scheduled syncs both refresh per-cx tag / folder / source caches.

### Internal
- `internal/web/handlers.go` split into 15 per-surface files. No behaviour change.
- Schema bootstrap (additive): `images.page_count`, `images.series`, `images.series_order`, `image_paths.mtime`, `saved_searches.sort` / `order` / `seed`. New `manga_metadata` table; new `idx_images_series` partial index.
- Per-page thumbnails pre-generated at ingest so the first `/pages` render is a static-file serve. 
- Batch tag operations chunk the per-tag transaction (one tx per 500 rows), mirroring `runBulkDelete`.
- Tests added: auto-tagger `storeResults` scope + rating prune, archive path-traversal, implication cycle / self-edge / depth-bound, user / auto tag-removal scope, bulk-delete cascade, vacuum handler, image-serve path-traversal, EXIF UserComment fixtures.
- Docker / Podman image refresh.

## [v1.6.0] - 2026-05-09
### Added
- Inbox/archive triage. Newly ingested or uploaded images land in the inbox (`is_inbox=1`); the user flips them to archived once curated. Pre-existing libraries upgrade as fully archived (the migration backfills `is_inbox=0` so nothing dumps into the inbox view on first boot). Surfaces: `inbox:true` / `inbox:false` search keywords, a toolbar `Inbox` toggle with a live count, a thumbnail grid badge, a detail-page button (`i` shortcut), and a single Send-to-inbox / Archive batch toggle on the selection bar and the Actions chooser.
- Upload page: inbox backlog count rendered next to a `View inbox` link.
- Favourite styling switched from the yellow ★ glyph to a red heart (♥/♡) across the gallery grid overlay, the toolbar `Favourites` filter button, and the detail-page action row. 
- Per-image external metadata: new `images.source` (free-form provenance label, e.g. site name) and `images.url` (canonical web URL) columns, edited from the detail page via small `[edit]` pop-ins. The URL field is rendered as an `http(s)`-only target=_blank link.
- Search keyword `source:my_label` for exact-match against the new `images.source` field, riding `idx_images_source`.
- Keyboard shortcut overhault:
  - Keyboard shortcut overlay: press `?` anywhere to display the cheat-sheet.
  - Chord-based navigation: `g g`, `g u`, `g c`, `g t`, `g s`, `g h` jump to gallery / upload / categories / tags / settings / help.
  - Vim aliases: `h j k l` for the gallery grid cursor, `[` and `]` for prev/next page, `g g` / `G` for first/last page, `p` for the page-jump dialog. Detail-page `h l j k` alias the prev/next-image arrows.
  - Gallery view-level shortcuts: `F` / `I` toggle the favorites / inbox filters, `R` random-sorts, `O` cycles sort, `D` flips direction, `S` opens save-search directly, `Home` / `End` jump to the first / last visible thumbnail.
  - Selection-bar shortcuts: `m` moves, `i` toggles inbox/archive, `Delete` deletes (with confirm), in addition to the existing `a` / `r` / `t`.
  - Detail page shortcuts: `m` opens the move dialog, `o` opens the original in a new tab, `Backspace` returns to the gallery.
  - Tags page shortcuts: `n` opens the create-tag dialog, `N` opens the create-alias dialog, page navigation parity with the gallery.
  - Categories page: `n` focuses the Add category form. Settings page: `1`-`6` jump to the section headings.
  - Anywhere: `Y` clicks the topbar Sync button, `,` / `.` walk the SFW ceiling, `\` opens the gallery-switch dialog when more than one gallery is configured.
- Settings → Stats section: per-gallery DB size and a memory containment tree.
- Footer: server-side render time displayed.

### Changed
- Detail-page metadata row labelled `Source` (showing the ingest origin) renamed to `Added via`, freeing the `Source` row for the new operator-edited label.
- Sidebar heading `Source` renamed to `AI source` to disambiguate from the new free-form source field.
- Search keyword `source:` renamed to `ai:` for the AI-generation-tool filter family. The omnibus value `source:ai` becomes `ai:any`. The other values (`a1111`, `comfyui`, `none`, `sd`) are unchanged. Saved searches and bookmarks must be updated by hand; there is no auto-rewrite.
- REST API field `source` on `POST /api/v1/images` and `POST /api/v1/images/{id}/tags` renamed to `via` to match the per-image `Added via` semantics.
- Tags-page row button `Merge→` relabelled `Alias→` (and its dialog prompt to `Alias <name> → to:` with submit text `Alias`).
- Detail page: implied tags and the alias section under the image fold into one collapsed `<details>` (closed by default). Summary text is `Implied tags`, `Aliases`, or `Implied tags and aliases` depending on what is present. Alias chips drop the merge-arrow + canonical span.
- Manual rating-tag adds now overwrite any existing rating on the image; the auto-tagger continues to keep the highest-rank rating it predicted.
- `GET /api/v1/tags` defaults to `show_zero=1` to match the /tags page; pass `show_zero=0` to hide declared-but-unused tags.
- /tags page row controls: Rename / Alias / Delete are hidden on the four canonical rating rows (they're reserved); the Category cell on rating rows and on alias rows is rendered read-only. Delete on a rating row still works as a usage-strip.

### Fixed
- /tags page: filter and pagination state preserved across rename, alias create, and alias/repoint merge so the operator stays on the same view after a write.
- /tags page: past-the-end page numbers clamp to the last populated page instead of returning empty.
- /tags page: tokens that match a category prefix only (`character:`) surface the category-only filter instead of being silently dropped.
- Detail page: prev/next layout no longer jumps when one neighbour is missing.
- Detail page: tag-focus mode survives the post-delete htmx swap; the cursor re-anchors to the next tag in the row.
- Detail page: partial-duplicate tag adds surface the duplicate token instead of silently dropping it.
- Gallery / detail: download, favourite, inbox, and external buttons share the same height across the action row; the favourite (♥/♡) glyph swap no longer reflows the row.
- Gallery: thumbnail badges share identical pixel dimensions (square, centred) so a card lines up with its peers.
- Search: gallery adjacency cache is invalidated on every membership write (tag add/remove, batch ops, alias merges) and on every `InvalidateCaches` call, so navigation no longer surfaces stale neighbours.
- Search: rating-ceiling chain is dropped from the loose-bound when the user expression has no upper bound, so cookie-applied SFW chains stop adding redundant work; ceiling+search COUNT defers to the slow exact COUNT instead of an inaccurate fast estimate.
- Search: `RankInQuery` fires on random sort under the existing gates (was over-conservative) and the cold-path worst case is bounded so a popular-tag detail page no longer walks an unbounded carrier set.
- Detail back link: page number aligns with the current image when the adjacency cache is warm.
- Scheduler: orphan-thumbnail sweep takes a job slot and observes context cancellation.
- API: `addImageTags` / `removeImageTags` pre-check image existence and return 404 instead of a write error; `POST /tagger/{name}` validates the tagger name from the URL against an allowlist; OpenAPI spec updated for the `source` / `via` / `url` rename.

### Removed
- BREAKING: search keyword `source:` (the AI filter form) is gone; use `ai:` instead. `source:` now means exact-match against the new `images.source` field.
- BREAKING: REST API field `source` is removed entirely from the two endpoints above. Use `via`. No deprecation period.
- BREAKING: gallery `h` / `l` page-navigation shortcuts; use `[` / `]` instead. The vim letters now alias the grid cursor.
- Keyboard shortcuts `PgUp` / `PgDn` (use `[` / `]`), `c` (use `g c`), `g r` (use `R`), and the detail-page `d` shortcut are dropped to match the new chord scheme.

### Internal
- New `idx_images_source(source)` and renamed `idx_images_source_type(source_type)` (the previous `idx_images_source` index was on `source_type`).
- `fastCountSource` helper renamed to `fastCountAI`; the slow-path `buildFilterExpr` flips `case "source":` to `case "ai":` and adds a separate `source:` exact-match branch.
- Search adjacency cache: gallery match-id list cached for adjacency reuse and reused by the gallery handler for multi-page searches; cap raised to 20 000 IDs; populated asynchronously through a singleflight gate.
- Search: covering index on `image_tags(tag_id, image_id)`; recent-id bound on multi-leg INTERSECT for newest sort (skipped when `order=asc`); multi-leg AND-driver INTERSECT capped at the two least-popular legs; AND-driver skipped in adjacency when the bucket gate fires; adjacency bucket gate skipped when the candidate set is small.
- Search: `suggestContextCap` tightened to 1 000 to bound the with-context autocomplete query under contention.
- DB: SQLite page cache capped at 2 MiB per connection; per-conn cache trimmed and idle write-pool conns closed after a quiet period.
- Tags: `ChunkedDeleteWithTagRecalc` extracted and folded into three callers; `suggestUsageRanked` extracted, replacing `search.suggestByUsage`; `mergeTagsPost` routes the canonical input through `resolveCanonicalTag` so alias / category resolution lines up across the tag-mutation surface.
- Maintenance: `prune-orphaned-thumbnails` runs as a background job through the scheduler instead of inline.

## [v1.5.1] - 2026-05-05
### Added
- Camie v2 auto-tagger as a third catalog entry (`camie-v2`). 789 MB ONNX, 70k+ tags across 7 categories. It's currently the only supported model that natively predicts artist (~7k tags) and copyright (~5k tags). 
- Per-gallery enabling for auto-taggers. Each tagger gains an optional `galleries = [...]` TOML key; empty / missing means every gallery (the legacy default), a non-empty list restricts the tagger to the named subset. Managed via Settings → Auto-Tagger → Galleries; Per-job entry points (gallery, detail page, upload, scheduler, REST API) all honour the per-gallery filter. 
- Newly discovered tagger model folders are persisted as enabled instead of staying implicit; the operator can disable from the table afterwards.
- Gallery batch-mode click toggle: while the batch bar is up, a plain left-click on a thumbnail toggles its checkbox.
- Manual and auto-tagger rating-tag adds prune lower-rank rating rows from the same image inside the write transaction. Pre-existing multi-rating rows from older data are left alone; only writes that fire the rule clean up.

### Changed
- Auto-tagger thresholds are now per-category: each tagger has a global threshold plus an optional `category_thresholds` map. Catalog rows ship with sensible defaults (wd-swinv2 carries `character = 0.85`; joytag stays single-knob since its profile only emits `general`). Edit per-tagger via Settings → Auto-Tagger → Configure;
- Settings → Auto-Tagger table redesigned: Downloaded and Enabled fold into a single Status column; the Confidence number-input is replaced by a Configure button + inline summary; a new Galleries column follows the same pattern; column widths pinned so rows align across taggers.
- Catalog default thresholds rebased according to models suggestions.

### Removed
- Detail-page X/Y position counter. The counter sat next to the back link and required a SUM/COUNT walk over the visible set on every detail-page load whenever the back-search carried no tag-shaped predicate to short-circuit on, affecting performance on large galleries. Adjacent prev/next navigation is unaffected.

### Internal
- Auto-tagger inference is profile-driven. Lifted the WD14-vs-JoyTag bool to a Profile struct capturing input size, layout, channels, normalize, pad, activation, label format, category scheme, and output index. Profiles resolve through embedded defaults at `internal/tagger/profile_default/<name>.json`, an optional `<modelPath>/<name>/tagger.json` sidecar, then a heuristic from the label-file extension. Profile fingerprint joins the cache reuse signature so a sidecar edit invalidates the session set.
- Auto-tagger label loaders gained a third format: `camie_json` parses Camie's `dataset_info.tag_mapping.idx_to_tag` + `tag_to_category` shape, populating `tagLabel.categoryName` for the `name_string` category-resolution path.
- Search: AND-driver materialises wildcard canonicals, so `blue*` at root substitutes a literal IN(...) instead of a per-row LIST SUBQUERY scan; popular AND-of-tags chains (every leaf above the rare-tag threshold) intersect off `idx_image_tags_tag` rather than nesting EXISTS over a full visible scan; a single-leaf AND-driver is now allowed under random sort.
- Search: rating-ceiling chain peels off `AndExpr{userExpr, chain}` so the count fast path applies to rating-filtered user searches, with a sum-of-usage upper bound on the chain leg; fast counts added for `rating:`, `folder:` recursive, `source:` csv, `tagged:` / `autotagged:`, and `generated:` filters; `idx_images_ingested_visible` pinned for the `tagged:` / `autotagged:` data path.
- Search: id-bucket gate for newest-sort sparse multi-AND prev/next adjacency.

## [v1.5.0] - 2026-05-02
### Added
- Tag implications: declare `parent → child` so adding the parent stamps the child on every image carrying the parent, and removing the parent walks the transitive closure to strip children that were only justified by it. Declaring or deleting an implication runs a chunked background propagation job. 
- /tags page rework: create-tag button and dialog; ascending/descending order filter; direct alias CRUD with category change; zero-usage filter (`Show` / `Only` / `Hide`) for declared-but-unused triage; danger button to delete every tag in the current search.
- Built-in `rating` tag category with canonical tags `general`, `sensitive`, `questionable`, `explicit`. The category is reserved, locked (no rename / merge / delete / move-category), and accepts only the four canonical names. Search supports `rating:explicit` with highest-wins semantics: an image carrying multiple ratings matches only the highest. The detail-page sidebar lifts the rating group to the top.
- Footer rating-ceiling selector. The footer cluster `rating: sfw . sensitive . questionable . explicit` posts to `/internal/rating-ceiling` and writes the `monbooru_rating_ceiling` cookie; gallery, sidebar, related-images, and detail-page adjacency then hide images carrying any rating above the ceiling. The first label `sfw` is a display alias for `general`. Folder tree, source counts, and the cached visible-count deliberately ignore the ceiling.
- Built-in tag categories `medium`, `person`, and `year`, so Hydrus-style `medium:digital`, `person:alice`, `year:2026` tokens round-trip into typed categories instead of landing as literal `general` tokens. Pre-existing user-created custom categories with these names are promoted to built-in on the next bootstrap. Hydrus `creator:` is rewritten to `artist:`, and `series:` / `studio:` to `copyright:`, on sidecar import.
- Operator-supplied `custom.css` override loaded after the built-in stylesheet, so a self-hoster can tweak the look without forking the binary.
- Gallery Actions chooser collapses Tag all / Remove tags / Auto-tag / Move / Delete behind one button, with arrow-key focus cycling, sub-dialogs (Remove tags strip, Auto-tag), and Escape / Cancel popping back to the chooser.
- Batch operations on the current search: auto-tag all, and move all to a folder. 
- Gallery batch-selection keyboard shortcuts: Space toggles, `a` adds tags, `r` removes tags, `t` opens auto-tag, `Ctrl+A` selects all, `Esc` clears.
- Detail page: tag-focus mode (`r` enters, arrows cycle on-image tags, Enter removes); a dim Aliases group lists aliases pointing at on-image tags; `t` opens the per-image Auto-tag dialog.
- Search: `system:` cheat-sheet autocomplete covers filter keywords and category drill-ins, with a description column on cheat-sheet rows.

### Changed
- Auto-tagger: WD14 rating labels (`general`, `sensitive`, `questionable`, `explicit`, plus the `rating:*` variants) now route to the `rating` category instead of `meta`. Pre-existing libraries with `meta:general`/`meta:sensitive`/etc. tags are not migrated automatically; re-run the tagger or move tags manually to switch them over.
- Zero-usage tags are no longer auto-pruned. Use the /tags Zero-usage filter for manual cleanup.
- Delete on a rating tag strips its usage from every image but keeps the row, since the four canonical ratings are reserved.

### Fixed
- Tag implications integrity: `MergeTags` repoints `tag_implications` to the canonical and promotes the canonical row when the alias was user-owned but the canonical was implied; `DeleteTag` sweeps the implied closure on every carrier image before dropping; `CreateAlias` repoints `tag_implications` when upgrading or repointing an existing row.
- Watcher: marking a file missing decrements `usage_count` of every tag the image carried and prunes any that hit zero, in the same write transaction as the is_missing flip. Tag counts no longer drift between watcher events and the next manual recompute. Alias paths are ignored in the mark-missing fallback, and the alias is promoted to canonical when the canonical's old path is gone.
- Search: `date:` filter accepts `YYYY` and `YYYY-MM` and is now validated; bare wildcard tokens become no-match instead of a full scan; random-sort seed is clamped to 31 bits.
- Saved searches: duplicate names are rejected.
- Import: type-to-confirm is validated before the upload is buffered; imported DB is bootstrapped before sanitisers run; per-entry decompressed size on zip archives is capped; light merge skips the wipe when the archive carries no files.
- Config: non-bcrypt `password_hash` is rejected on startup; non-positive `SessionLifetimeDays` is clamped to the default; `MaxFileSizeMB <= 0` is treated as no per-file cap.
- Auth: same-origin Referer check rejects non-http(s) schemes.
- API: trailing slash on `base_url` is trimmed before the CORS check.
- Footer: cached counts are invalidated on tag and saved-search mutations.

### Removed
- Daily schedule: the Recompute tag counts and Vacuum database toggles. Vacuum stays available as a manual button under Settings → Maintenance. Existing `recompute_tags` / `vacuum_db` keys in `monbooru.toml` are ignored on load and dropped on the next save.
- Settings → Tag actions: Run untagged / Run all (rolled into the gallery Actions chooser).
- `MONBOORU_UI_PAGE_SIZE` env override.

### Internal
- Search: multi-AND drives off the smallest tag's `image_tags` rows; `rating:` filter short-circuits when no image carries the level; suggest candidate cap halved to lower with-context contention; partial index for alias rows on `canonical_tag_id`.
- Tags: `MergeTags` folded into three set-based statements; `DeleteTag` bulk-deletes instead of per-image looping.
- Auto-tagger: per-model label dispatch tables embedded in the binary.
- Single source of truth for filter-keyword vocabulary across search and autocomplete.

## [v1.4.3] - 2026-04-29
### Fixed
- Auto-tagger: ORT environment and per-tagger sessions are cached across runs so back-to-back jobs no longer leak ~440 MB of glibc-arena memory each. The cache is torn down after `tagger.idle_release_after_minutes` (default 30) of inactivity, on Settings save, and on `use_cuda` flips.
- Auto-tagger: completion summary now reports `auto-tagged X of N image(s), K skipped` when images are skipped (missing rows, no extractable frames, store failures), instead of always reporting the submit count.

### Internal
- Search: prev/next adjacency on the detail page uses a row-value cursor and pins the partial sort index, dropping cursor lookups from seconds to milliseconds on large libraries; random-sort + tag-predicate adjacency bounds the outer scan to a 2000-id bucket around the current image; random-sort wildcard searches materialize the EXISTS body as an IN-list instead of re-evaluating per row.
- Search: detail-page X/Y counter is skipped for tag-predicate and folder-filtered searches; cursor-based prev/next still works and the template hides the counter when the total is zero.
- Memory: read connections run `PRAGMA shrink_memory` and `runtime/debug.FreeOSMemory()` every 5 minutes when the job manager is idle, so RSS plateaus instead of holding the post-burst peak.

## [v1.4.2] - 2026-04-28

### Internal
- Search: fast-path counts for pure-tag, wildcard, NOT, AND, OR, and category-qualified queries; pinned partial sort index for the unfiltered-newest path; fast-path approximations gated on a per-query usage threshold.
- Search: autocomplete candidate and context caps tightened; context CTE capped at 50k rows.
- Tags: `RelatedImages` drops popular tags from the seed set and uses a smaller candidate buffer; tag-count recalc chunks by `tag_id` range to release the SQLite writer.

## [v1.4.1] - 2026-04-27

### Added
- Tag names accept `?`, `>`, `<`, `=`, `^`.
- Enabled auto-taggers whose model files have gone missing are auto-disabled at startup instead of failing silently at inference time.
- Settings → Auto-Tagger: per-tagger download instructions. 
- Detail page: "Remove all tags" confirms before running.
- Auto-tag picker on the detail and upload pages is a radio list rather than a dropdown.

### Changed
- Schedule defaults: `run_auto_taggers` is `false` so freshly-installed instances don't auto-run taggers on every cycle; 
- Gallery import dialog defaults to Merge.
- Settings → General reshuffled into side-by-side rows: Files / UI, GPU / Parallel workers, Password / API token; Schedule checks render as a 2x3 grid; tags filters are clickable buttons rather than dropdowns; the tags-page filter submit is inline with a shorter label; auto-tagger actions are grouped under "Tag actions"; copy in Tag actions and Auto-Tagger sections is tightened.
- Galleries table: Info column renamed to Content and the saved-count dropped; per-action columns; actions right-aligned; Add gallery moved into a dialog opened by a button below the table.
- Auto-Tagger table: Disable column renamed to Enable; Downloaded moved left of Enabled; Status column width reserved so active and default badges stay side by side; tagger name column widened, status columns shrunk; bulk auto-tagging columns right-aligned and compacted.
- Detail page: columns stack on narrow viewports; "Tag all" is hidden while thumbnails are selected; a separator sits between Tag selected and Move selected.
- Upload UI: drop zone enlarged with bottom fields split into two columns; drop-zone prompt centered; reset button subdued.
- Built-in category Action cells expose a labelled Delete button instead of a generic icon.
- Dialog placeholders and microcopy refreshed across the UI; the batch-tag dialog placeholder spells out Add/remove.

### Fixed
- Auto-Tagger: emoticon-only labels (`:3`, `:o`-style) survive `sanitizeLabel` instead of being dropped; `_unsupported_<idx>` placeholder labels are filtered at inference time.
- Recalc and merge-general-tags invalidate the affected caches so stale counts don't survive a tag rewrite.
- Search-suggest hover is suppressed until the mouse moves, so the dropdown no longer lights up under a stationary cursor.
- Upload action buttons stay fixed when the auto-tag picker reveals, instead of shifting.
- Add-gallery dialog fields stack on their own lines so long paths don't overflow.
- Settings action headers align with the buttons in their column; settings tables wrap in a scroll container so they stay readable on narrow viewports.

### Internal
- Performance: per-gallery distinct-tagger and tag/saved-count caches; `SuggestTagsWithFilter` materialises its filter context once and caps candidate tags before combo counting; partial index pinned for the unfiltered-newest sort; partial index on `is_missing=0`; index on `image_tags(tagger_name)` for `is_auto=1` distinct scans; lazy sidebar URLs carry page IDs; the `DiscoverTaggers` walk is shared between settings and bulk-tagger rows.

## [v1.4.0] - 2026-04-26

### Added
- Import hydrus network and bloombooru exports directly from the gallery import dialog. (images and tags)
- Add gallery form accepts an optional `.db`/`.json`/`.zip` upload; the new gallery is created and the file imported into it in the same step. Add and merge import now switch the active gallery automatically.
- Settings → Auto-Tagger lists a catalog of suggested taggers (WD14 SwinV2, JoyTag) with copy-paste install commands relative to the host working directory; `<model_path>/models.json` overrides extend the catalog. Disabled tagger rows expose a Delete button.
- Batch tag add/remove from search and from selection: a Tag all button next to Delete all in the gallery header and a Tag selected button in the batch bar share a dialog with autocomplete and an Add/Remove radio.
- Bulk tag removal moved to its own Settings → Tags section so it stays reachable when no on-disk tagger is configured.
- Settings → General is one form with a single Save.
- Styled 404 page with a back link instead of bare text.
- Built-in category Action cells are labelled `(built-in)`.

### Changed
- Detail image floored at 200 px so tiny files no longer render as a dot.
- Upload list shows `<1 KiB` for sub-1 KB files instead of `0 KiB`.
- The `?` thumbnail fallback explains via a hover title why the thumbnail is unavailable.
- Gallery status reads "N matches" instead of "match N".
- Download button uses `⤓` to disambiguate from the sort-direction `↓`.
- Gallery batch action bar reordered: Tag selected sits left of Delete selected; Move/Delete are right-aligned and separated from Tag selected.
- Add gallery form is single-row with equal-height controls, drops the verbose hint, shows a busy state on submit, and renders a success flash before the page reload. The gallery import dialog drops its redundant hint too.

### Fixed
- Search/UI: search input blurs on submit and the suggest dropdown is dismissed; stale suggest swaps no longer race the new query; long tag chip names ellipsize on the detail page; the detail breadcrumb back-link stays on one line when the filename is long; the gallery toolbar (Save / Tag all / Delete all) tracks the current search via OOB swap.
- Search: LIKE metacharacters in tag prefix/substring and folder filters are escaped; malformed queries return 400 instead of being silently parsed away.
- API: `tag_warnings` are returned from `addImageTags` and surfaced in the upload flash; compat-imported tags are credited to the originating provider instead of always `import`.
- Auth/audit: settings audit logs record the `clientIP` so reverse-proxy IPs are captured; login backoff is capped at 30 s as the spec documents.
- Web: `syncTrigger` redirects only to a same-origin Referer; export warns when a gallery file is skipped because its path is unreadable.
- Tags/gallery: errors propagate from `DeleteCategoryMoveOrDelete`'s delete-all path, `Sync`'s scan loop, and adjacent-image lookups instead of being dropped; orphaned-thumbnail prune bails on a partial id scan rather than deleting valid thumbnails.
- Config: `ui.page_size` is clamped to its default when a non-positive value is configured.

### Removed
- `tools/blombooru-export.py` : superseded by the in-app blombooru import.

### Internal
- Compat layer split into `internal/web/compatibility/` with per-app translators (blombooru, hydrus) self-registering against a package-level provider table. Adding a third format is a new file with one Register call.
- Performance: cache the general-category id on the gallery context so `parseTagInput` skips a SELECT; narrow `RelatedImages` to the only column the template uses; scope `RecalcAndPrune` to touched tags in bulk removals; add partial indexes on `generation_hash` for both metadata tables.
- Docs: new migration guide (`docs/MIGRATING.md`)

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
- Gallery configuration no longer stores `db_path` or `thumbnails_path`. Each gallery lives under `<paths.data_path>/<name>/monbooru.db` + `/thumbnails/`, created on demand. `active_gallery` is renamed to `default_gallery` and only controls the startup pick; the topbar switcher changes the runtime active gallery without persisting. Legacy `[paths]` migration is removed - existing `monbooru.toml` files must be rewritten as `[[galleries]]` entries on a fresh config.
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
