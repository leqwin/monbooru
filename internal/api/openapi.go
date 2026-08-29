package api

import (
	"cmp"
	"encoding/json"
	"html/template"
	"maps"
	"net/http"
	"slices"
	"strings"
)

func buildSpec(baseURL string) map[string]any {
	return map[string]any{
		"openapi": "3.0.3",
		"info": map[string]any{
			"title":       "Monbooru API",
			"description": "REST API for monbooru image library",
			"version":     "1.1.0",
		},
		"servers": []map[string]any{
			{"url": baseURL + "/api/v1", "description": "This server"},
		},
		"components": map[string]any{
			"securitySchemes": map[string]any{
				"bearerAuth": map[string]any{
					"type":         "http",
					"scheme":       "bearer",
					"bearerFormat": "token",
					"description":  "Named, scoped token from Settings -> Authentication. Scopes: read (GET), write (POST/PATCH), delete (DELETE); a token missing the scope gets 403 insufficient_scope.",
				},
			},
			"schemas": map[string]any{
				"Error": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"error": map[string]any{"type": "string"},
						"code":  map[string]any{"type": "string"},
					},
				},
				"Tag": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"name":        map[string]any{"type": "string"},
						"category":    map[string]any{"type": "string"},
						"is_auto":     map[string]any{"type": "boolean"},
						"confidence":  map[string]any{"type": "number", "nullable": true},
						"tagger_name": map[string]any{"type": "string", "nullable": true, "description": "Provenance identifier: auto-tagger subfolder name when is_auto, caller-supplied 'via' (e.g. app name) when manual, null for UI-driven user adds"},
					},
				},
				"TagRow": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"id":           map[string]any{"type": "integer"},
						"name":         map[string]any{"type": "string"},
						"category":     map[string]any{"type": "string"},
						"color":        map[string]any{"type": "string"},
						"usage_count":  map[string]any{"type": "integer"},
						"is_alias":     map[string]any{"type": "boolean"},
						"origin":       map[string]any{"type": "string", "description": "Creation provenance label: 'user', a booru site, 'ptr', an auto-tagger name, an import label. Empty on rows predating the column."},
						"last_used_at": map[string]any{"type": "string", "description": "ISO 8601; most recent application to an image. Absent when never applied."},
					},
				},
				"Implication": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"parent_id":        map[string]any{"type": "integer"},
						"implied_id":       map[string]any{"type": "integer"},
						"implied_name":     map[string]any{"type": "string"},
						"implied_category": map[string]any{"type": "string"},
					},
				},
				"Category": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"id":         map[string]any{"type": "integer"},
						"name":       map[string]any{"type": "string"},
						"color":      map[string]any{"type": "string"},
						"is_builtin": map[string]any{"type": "boolean"},
					},
				},
				"Gallery": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"name":   map[string]any{"type": "string"},
						"images": map[string]any{"type": "integer", "description": "Visible (non-missing) image count"},
						"tags":   map[string]any{"type": "integer", "description": "Non-alias tag count"},
						"active": map[string]any{"type": "boolean", "description": "True for the gallery used when no ?gallery= selector is given"},
					},
				},
				"PaginatedTags":   paginatedSchema("#/components/schemas/TagRow"),
				"PaginatedImages": paginatedSchema("#/components/schemas/Image"),
				"APIInfo": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"api":     map[string]any{"type": "string"},
						"docs":    map[string]any{"type": "string"},
						"openapi": map[string]any{"type": "string"},
					},
				},
				"CreateImageResponse": map[string]any{
					"type":        "object",
					"description": "Bare Image on success; wrapped in {image, tag_warnings?, autotag?} when any tag failed or an auto-tag job was started.",
					"properties": map[string]any{
						"image":        map[string]any{"$ref": "#/components/schemas/Image"},
						"tag_warnings": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "nullable": true},
						"autotag":      map[string]any{"type": "string", "nullable": true, "description": "Human-readable status about the auto-tag job"},
					},
				},
				"DuplicateImageResponse": map[string]any{
					"type":        "object",
					"description": "Returned when the posted file's SHA-256 is already known. The existing image is returned with the pushed tags and provenance merged in. A multipart upload's redundant copy is discarded; a JSON path-reference at a new path is recorded as an alias.",
					"properties": map[string]any{
						"image":        map[string]any{"$ref": "#/components/schemas/Image"},
						"alias_added":  map[string]any{"type": "boolean", "description": "True when the posted path was recorded as an alias of the existing image (JSON path-reference); false when a redundant multipart upload was discarded"},
						"merge":        map[string]any{"$ref": "#/components/schemas/MergeSummary"},
						"tag_warnings": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "nullable": true},
					},
				},
				"MergeSummary": map[string]any{
					"type":        "object",
					"description": "What a duplicate push or enrich folded into the existing image. The merge reconciles the source's own tag slice to the pushed set; tags owned by the operator, the auto-tagger, or another source are never touched.",
					"properties": map[string]any{
						"tags_added":    map[string]any{"type": "integer"},
						"tags_retired":  map[string]any{"type": "integer", "description": "Tags this source contributed earlier and no longer lists, kept but flagged stale"},
						"rating_filled": map[string]any{"type": "boolean", "description": "True when the push supplied a rating and the image had none; an existing rating is never displaced"},
						"source_added":  map[string]any{"type": "boolean"},
					},
				},
				"ReplaceFileResponse": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"replaced":     map[string]any{"type": "boolean", "description": "False when the uploaded bytes already matched the stored file; the metadata merge still applied"},
						"merge":        map[string]any{"$ref": "#/components/schemas/MergeSummary"},
						"tag_warnings": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "nullable": true},
					},
				},
				"EnrichResponse": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"merge":        map[string]any{"$ref": "#/components/schemas/MergeSummary"},
						"verified":     map[string]any{"type": "boolean", "description": "False when verify was requested but the source reported no md5"},
						"tag_warnings": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "nullable": true},
					},
				},
				"TagArray": map[string]any{
					"type":  "array",
					"items": map[string]any{"$ref": "#/components/schemas/Tag"},
				},
				"Image": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"id":                   map[string]any{"type": "integer"},
						"sha256":               map[string]any{"type": "string"},
						"md5":                  map[string]any{"type": "string", "description": "md5 of the stored file, \"\" until computed. The local digest, not the one an origin claims (sources[].md5)"},
						"canonical_path":       map[string]any{"type": "string"},
						"aliases":              map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
						"file_type":            map[string]any{"type": "string"},
						"width":                map[string]any{"type": "integer", "nullable": true},
						"height":               map[string]any{"type": "integer", "nullable": true},
						"file_size":            map[string]any{"type": "integer"},
						"is_favorited":         map[string]any{"type": "boolean"},
						"is_inbox":             map[string]any{"type": "boolean", "description": "true = needs triage (sits in the inbox); false = curated. New ingests default to true."},
						"is_missing":           map[string]any{"type": "boolean"},
						"scheduled_lookup":     map[string]any{"type": "boolean", "description": "Whether the nightly online-booru lookup considers this image. Single-image GET only. Settable via PATCH /images/{id}; setting it true also resets an exhausted retry ladder."},
						"scheduled_lookup_ptr": map[string]any{"type": "boolean", "description": "The same opt-in for the nightly PTR lookup, which is free per image and has its own ladder. Single-image GET only, settable the same way."},
						"lookup": map[string]any{
							"type":        "object",
							"description": "Recorded lookup history per backend ('ptr' / 'booru'), absent when nothing has ever been tried. Single-image GET only.",
							"additionalProperties": map[string]any{
								"type": "object",
								"properties": map[string]any{
									"last_at":     map[string]any{"type": "string", "format": "date-time", "description": "When the last attempt concluded"},
									"last_result": map[string]any{"type": "string", "description": "'hit' or 'miss'; absent while nothing has concluded"},
									"attempts":    map[string]any{"type": "integer", "description": "Consecutive concluded misses"},
									"next_due_at": map[string]any{"type": "string", "format": "date-time", "description": "When the backend may try again; absent when nothing is scheduled"},
								},
							},
						},
						"auto_tagged_at":   map[string]any{"type": "string", "format": "date-time", "nullable": true},
						"source_type":      map[string]any{"type": "string", "description": "Generation-tool source: 'a1111', 'comfyui', 'a1111,comfyui', or 'none'. Filtered by the search keyword 'ai:'."},
						"origin":           map[string]any{"type": "string", "description": "How the image got into the gallery: 'ingest' for watcher/sync, 'upload' for the web UI, or any caller-supplied string (app name, URL...) set via POST /images with 'via'"},
						"source":           map[string]any{"type": "string", "description": "Site label of the primary (first) entry of 'sources'. Empty string when the image has no origin. Filtered by the search keyword 'source:' which matches any of the image's sources. Settable on create and via PATCH /images/{id}."},
						"url":              map[string]any{"type": "string", "description": "URL of the primary (first) entry of 'sources'. Empty string when the image has no origin. Must start with http:// or https://. Settable on create and via PATCH /images/{id}."},
						"page_count":       map[string]any{"type": "integer", "nullable": true, "description": "Number of pages for cbz / manga rows; null on every other file type."},
						"collection":       map[string]any{"type": "string", "description": "Home collection label - mirrors the first entry of 'collections'. Empty string when unset. Filtered by the search keyword 'collection:' (exact match). Settable on create and via PATCH /images/{id}."},
						"collection_order": map[string]any{"type": "integer", "nullable": true, "description": "1-based position within the home collection. Null when unset. Settable on create and via PATCH /images/{id}."},
						"collections": map[string]any{
							"type":        "array",
							"description": "Every collection the image belongs to. The scalar collection / collection_order fields mirror the home membership. Managed through the web UI; the API sets only the home via collection / collection_order.",
							"items": map[string]any{
								"type": "object",
								"properties": map[string]any{
									"name":  map[string]any{"type": "string"},
									"order": map[string]any{"type": "integer", "nullable": true, "description": "1-based position within this collection. Null when unset."},
								},
							},
						},
						"sources": map[string]any{
							"type":        "array",
							"description": "Every origin the image came from. The scalar source / url fields mirror the primary (first) origin. Managed through the web UI; the API sets only the primary via source / url.",
							"items": map[string]any{
								"type": "object",
								"properties": map[string]any{
									"site":       map[string]any{"type": "string", "description": "Site label, e.g. \"danbooru\"; empty for a url-only origin."},
									"post_id":    map[string]any{"type": "string", "description": "Upstream post id; omitted for a manually-added origin."},
									"url":        map[string]any{"type": "string"},
									"commentary": map[string]any{"type": "string", "description": "Artist commentary pulled from (or hand-entered for) this origin; omitted when empty."},
									"original":   map[string]any{"type": "string", "description": "Upstream artist source the post declared (usually a URL, newline-joined when several); omitted when empty."},
									"similarity": map[string]any{"type": "number", "description": "Best similarity-service score (0-100) a lookup matched this origin with; omitted for an exact or manual origin. A matched origin's refetches skip the md5 verify."},
								},
							},
						},
						"note":            map[string]any{"type": "string", "description": "Operator's freeform note. Never written by a push, enrich, or import."},
						"original_source": map[string]any{"type": "string", "description": "Legacy image-level original source URL. Read-only: only a gallery transfer still carries it."},
						"annotations": map[string]any{
							"type":        "array",
							"description": "Positional note boxes pulled per source, in original-image pixel coordinates. Omitted when empty.",
							"items": map[string]any{
								"type": "object",
								"properties": map[string]any{
									"x":         map[string]any{"type": "integer"},
									"y":         map[string]any{"type": "integer"},
									"w":         map[string]any{"type": "integer"},
									"h":         map[string]any{"type": "integer"},
									"body":      map[string]any{"type": "string", "description": "The stored body, in monbooru's annotation markup."},
									"body_text": map[string]any{"type": "string", "description": "The body flattened to plain text. Present only when the body carries markup."},
								},
							},
						},
						"ingested_at":   map[string]any{"type": "string", "format": "date-time"},
						"thumbnail_url": map[string]any{"type": "string"},
						"phash":         map[string]any{"type": "string", "nullable": true, "description": "16-char hex perceptual hash; null until the phash backfill or ingest has populated it."},
						"tags":          map[string]any{"type": "array", "items": map[string]any{"$ref": "#/components/schemas/Tag"}},
						"tag_sources": map[string]any{
							"type":                 "object",
							"description":          "Per-tag source ledger keyed by the tag's category:name form (bare for general): every source that applied or re-confirmed the tag. Present on the single-image GET only.",
							"additionalProperties": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
						},
					},
				},
				"ImageRelations": map[string]any{
					"type":        "object",
					"description": "Declared relations centred on one image. duplicate_group / alternate_group are null when the image is not part of a group; the four edge slots are null when no edge exists.",
					"properties": map[string]any{
						"duplicate_group": map[string]any{
							"type":     "object",
							"nullable": true,
							"properties": map[string]any{
								"id":                map[string]any{"type": "integer"},
								"original_image_id": map[string]any{"type": "integer"},
								"member_ids":        map[string]any{"type": "array", "items": map[string]any{"type": "integer"}},
							},
						},
						"alternate_group": map[string]any{
							"type":     "object",
							"nullable": true,
							"properties": map[string]any{
								"id":         map[string]any{"type": "integer"},
								"member_ids": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}},
							},
						},
						"version_parent":    map[string]any{"type": "integer", "nullable": true, "description": "id of the older revision; null when this image is the chain root."},
						"version_child":     map[string]any{"type": "integer", "nullable": true, "description": "id of the newer revision; null when this image is the chain leaf."},
						"derivative_source": map[string]any{"type": "integer", "nullable": true, "description": "id of the source image when this image is a derivative; null otherwise."},
						"derivatives":       map[string]any{"type": "array", "items": map[string]any{"type": "integer"}},
					},
				},
			},
		},
		"security": []map[string]any{
			{"bearerAuth": []string{}},
		},
		"paths": map[string]any{
			"/": map[string]any{
				"get": op("API info", "apiInfo",
					nil,
					map[string]any{
						"200": resp("API metadata", "#/components/schemas/APIInfo"),
						"503": resp("API disabled (no token configured)", "#/components/schemas/Error"),
					}),
			},
			"/images": map[string]any{
				"post": map[string]any{
					"summary":     "Add an image",
					"operationId": "createImage",
					"parameters":  []map[string]any{galleryParam()},
					"requestBody": map[string]any{
						"required": true,
						"content": map[string]any{
							"multipart/form-data": map[string]any{
								"schema": map[string]any{
									"type":     "object",
									"required": []string{"file"},
									"properties": map[string]any{
										"file":             map[string]any{"type": "string", "format": "binary", "description": "Image or video file"},
										"tags":             map[string]any{"type": "string", "description": "JSON-encoded array of tag names"},
										"folder":           map[string]any{"type": "string", "description": "Destination subfolder under the gallery root; missing directories are created. Leave blank for the gallery root."},
										"autotag":          map[string]any{"type": "string", "description": "Set to \"true\" to kick off an auto-tag job on the new image"},
										"tagger_name":      map[string]any{"type": "string", "description": "Optional auto-tagger name; when set with autotag, restricts the job to that tagger"},
										"via":              map[string]any{"type": "string", "description": "Optional caller-supplied identifier (app name, URL...). Stored as images.origin and attached to each initial tag via image_tags.tagger_name. Blank defaults images.origin to 'upload' for multipart mode."},
										"source":           map[string]any{"type": "string", "description": "Optional site label for the image's origin (site name, scraper...). Recorded as a source; on a duplicate-SHA push its tags and provenance merge into the existing image instead of being discarded."},
										"url":              map[string]any{"type": "string", "description": "Optional canonical web URL the image came from. Must start with http:// or https://. Recorded on the same origin as 'source'."},
										"md5":              map[string]any{"type": "string", "description": "Optional md5 the source claims for the file (<=64 chars). Recorded on the origin row as an audit trail; never a dedup key."},
										"parent_url":       map[string]any{"type": "string", "description": "Optional canonical URL of the post this post declares as its parent. Must start with http:// or https://. Recorded on the same origin as 'source'; when the parent's post lands in the gallery too, the pair is linked as a derivative relation."},
										"commentary":       map[string]any{"type": "string", "description": "Optional artist commentary for the pushed source (<=10000 chars)."},
										"original":         map[string]any{"type": "string", "description": "Optional upstream artist source the post declared (<=2048 chars, newline-joined when several). Recorded on the same origin as 'source'."},
										"notes":            map[string]any{"type": "string", "description": "Optional JSON-encoded array of positional note boxes ({x, y, w, h, body}; a box may carry body_html instead, the source's own HTML, converted to monbooru's markup on the way in) for the pushed source."},
										"collection":       map[string]any{"type": "string", "description": "Optional collection label (images.series). Written on the new row; on a duplicate-SHA push added as a membership that never displaces the existing home."},
										"collection_order": map[string]any{"type": "string", "description": "Optional 1-based position within the collection. Requires a non-empty collection in the same request."},
									},
								},
							},
							"application/json": map[string]any{
								"schema": map[string]any{
									"type":     "object",
									"required": []string{"path"},
									"properties": map[string]any{
										"path":             map[string]any{"type": "string", "description": "Path to a file already on disk. Absolute paths are used verbatim; relative paths are resolved under gallery/<folder> when folder is set, otherwise under the gallery root. WARNING: absolute paths give a token holder read access to anything the monbooru process can stat."},
										"tags":             map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
										"folder":           map[string]any{"type": "string", "description": "Destination subfolder for relative paths"},
										"autotag":          map[string]any{"type": "boolean", "description": "Kick off an auto-tag job on the new image"},
										"tagger_name":      map[string]any{"type": "string", "description": "Optional auto-tagger name"},
										"via":              map[string]any{"type": "string", "description": "Optional caller-supplied identifier. Stored as images.origin and attached to each initial tag. Blank defaults images.origin to 'ingest' for JSON path-reference mode."},
										"source":           map[string]any{"type": "string", "description": "Optional site label for the image's origin (site name, scraper...). Recorded as a source; on a duplicate-SHA push its tags and provenance merge into the existing image instead of being discarded."},
										"url":              map[string]any{"type": "string", "description": "Optional canonical web URL the image came from. Must start with http:// or https://. Recorded on the same origin as 'source'."},
										"md5":              map[string]any{"type": "string", "description": "Optional md5 the source claims for the file (<=64 chars). Recorded on the origin row as an audit trail; never a dedup key."},
										"parent_url":       map[string]any{"type": "string", "description": "Optional canonical URL of the post this post declares as its parent. Must start with http:// or https://. Recorded on the same origin as 'source'; when the parent's post lands in the gallery too, the pair is linked as a derivative relation."},
										"commentary":       map[string]any{"type": "string", "description": "Optional artist commentary for the pushed source (<=10000 chars)."},
										"original":         map[string]any{"type": "string", "description": "Optional upstream artist source the post declared (<=2048 chars, newline-joined when several). Recorded on the same origin as 'source'."},
										"notes":            map[string]any{"type": "array", "items": map[string]any{"type": "object"}, "description": "Optional positional note boxes ({x, y, w, h, body}; a box may carry body_html instead, the source's own HTML, converted to monbooru's markup on the way in) for the pushed source."},
										"collection":       map[string]any{"type": "string", "description": "Optional collection label (images.series). Written on the new row; on a duplicate-SHA push added as a membership that never displaces the existing home."},
										"collection_order": map[string]any{"type": "integer", "description": "Optional 1-based position within the collection. Requires a non-empty collection in the same request."},
									},
								},
							},
						},
					},
					"responses": map[string]any{
						"200": resp("Duplicate SHA-256: existing image returned with the pushed tags and provenance merged; alias_added is true only when a JSON path-reference recorded a new alias", "#/components/schemas/DuplicateImageResponse"),
						"201": resp("Image created", "#/components/schemas/CreateImageResponse"),
						"400": resp("Invalid request or unsupported file type", "#/components/schemas/Error"),
						"413": resp("File exceeds max size", "#/components/schemas/Error"),
						"500": resp("Ingest failure", "#/components/schemas/Error"),
					},
				},
			},
			"/images/search": map[string]any{
				"get": map[string]any{
					"summary":     "Search images",
					"operationId": "searchImages",
					"parameters": []map[string]any{
						galleryParam(),
						queryParam("q", "Search query (tag list, filters, wildcards)"),
						queryParam("sort", "Sort field: newest, filesize, order, random"),
						queryParam("order", "Sort order: asc, desc"),
						queryParam("seed", "Random-sort seed for stable pagination when sort=random"),
						queryParam("page", "Page number (1-based)"),
						queryParam("limit", "Results per page (max 200)"),
					},
					"responses": map[string]any{
						"200": resp("Paginated image list", "#/components/schemas/PaginatedImages"),
					},
				},
			},
			"/images/{id}": map[string]any{
				"get": op("Get image metadata", "getImage",
					[]map[string]any{pathParam("id", "Image ID"), galleryParam()},
					map[string]any{
						"200": resp("Image metadata", "#/components/schemas/Image"),
						"404": resp("Not found", "#/components/schemas/Error"),
					}),
				"patch": map[string]any{
					"summary":     "Edit image fields",
					"operationId": "patchImage",
					"parameters":  []map[string]any{pathParam("id", "Image ID"), galleryParam()},
					"requestBody": jsonBodySchema(true, map[string]any{
						"type":        "object",
						"description": "Any subset of the editable fields. An absent or null field is left unchanged; an empty string clears a text field. Clearing collection also clears collection_order unless one is supplied in the same request; to clear collection_order on its own, clear the collection.",
						"properties": map[string]any{
							"source":               map[string]any{"type": "string", "description": "Operator-edited provenance label (<=200 chars). Empty clears."},
							"url":                  map[string]any{"type": "string", "description": "Canonical web URL (<=2048 chars, http:// or https://). Empty clears."},
							"collection":           map[string]any{"type": "string", "description": "Collection label (images.series). Empty clears."},
							"collection_order":     map[string]any{"type": "integer", "description": "1-based position within the collection. Requires a non-empty collection (incoming or already stored)."},
							"is_favorited":         map[string]any{"type": "boolean"},
							"is_inbox":             map[string]any{"type": "boolean", "description": "true = sits in the inbox (needs triage); false = curated."},
							"scheduled_lookup":     map[string]any{"type": "boolean", "description": "Whether the nightly online-booru lookup considers this image. Setting it true also resets an exhausted retry ladder, exactly like the detail page's [look again]."},
							"scheduled_lookup_ptr": map[string]any{"type": "boolean", "description": "The same opt-in for the nightly PTR lookup. Setting it true resets that backend's ladder."},
						},
					}),
					"responses": map[string]any{
						"200": resp("Updated image", "#/components/schemas/Image"),
						"400": resp("Invalid request (bad url, order without a collection, or no editable fields supplied)", "#/components/schemas/Error"),
						"404": resp("Not found", "#/components/schemas/Error"),
						"409": resp("Relabel collides with another origin the image already carries", "#/components/schemas/Error"),
					},
				},
				"delete": map[string]any{
					"summary":     "Delete image from library",
					"operationId": "deleteImage",
					"parameters": []map[string]any{
						pathParam("id", "Image ID"),
						galleryParam(),
						queryParam("delete_empty_folder", "Remove containing folder if empty after deletion"),
					},
					"responses": map[string]any{
						"200": map[string]any{"description": "Deleted (folder also removed)"},
						"204": map[string]any{"description": "Deleted"},
						"404": resp("Not found", "#/components/schemas/Error"),
						"500": resp("Delete failed server-side", "#/components/schemas/Error"),
					},
				},
			},
			"/images/{id}/enrich": map[string]any{
				"post": map[string]any{
					"summary":     "Apply fetched metadata to an existing image",
					"operationId": "enrichImage",
					"description": "Metadata-only counterpart of a duplicate push, used by monloader's source refetch. Shares the duplicate-push merge semantics: the origin upserts, the source's own tag slice reconciles to the pushed set, and empty url / md5 / commentary / original / notes leave the stored values be.",
					"parameters":  []map[string]any{pathParam("id", "Image ID"), galleryParam()},
					"requestBody": jsonBodySchema(true, map[string]any{
						"type": "object",
						"properties": map[string]any{
							"tags":        map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
							"source":      map[string]any{"type": "string", "description": "Site label of the origin being refetched"},
							"url":         map[string]any{"type": "string"},
							"source_md5":  map[string]any{"type": "string", "description": "md5 the source claims (<=64 chars). Compared against the stored file when verify is set; recorded on the origin row."},
							"parent_url":  map[string]any{"type": "string", "description": "Canonical URL of the post this post declares as its parent. Recorded on the origin row; when both sides are in the gallery the pair is linked as a derivative relation. Empty keeps the stored value."},
							"post_width":  map[string]any{"type": "integer", "description": "What the post says the file it serves is. Recorded on the origin row and rendered beside the origin; drives upgrade:bigger. 0 or absent keeps the stored value."},
							"post_height": map[string]any{"type": "integer"},
							"post_size":   map[string]any{"type": "integer", "description": "Byte size the post declares. Used for the gain test when the post publishes no dimensions."},
							"post_ext":    map[string]any{"type": "string", "description": "File extension the post serves, no dot."},
							"verify":      map[string]any{"type": "boolean", "description": "When true and source_md5 is non-empty, the stored file is hashed and compared first; a mismatch applies nothing and returns 409 - unless the origin is similarity-matched, whose file differs by design."},
							"similarity":  map[string]any{"type": "number", "description": "Similarity-service score (0-100) when the post was found by image similarity rather than an exact hash. Recorded on the origin row; a matched origin's refetches skip the md5 verify. 0 or absent keeps the stored score."},
							"commentary":  map[string]any{"type": "string"},
							"original":    map[string]any{"type": "string", "description": "Upstream artist source the post declared (<=2048 chars, newline-joined when several). A non-empty value overwrites the stored one."},
							"notes":       map[string]any{"type": "array", "items": map[string]any{"type": "object"}, "description": "Positional note boxes ({x, y, w, h, body}; a box may carry body_html instead, the source's own HTML, converted to monbooru's markup on the way in); a non-empty array replaces the set this origin contributed."},
						},
					}),
					"responses": map[string]any{
						"200": resp("Metadata applied", "#/components/schemas/EnrichResponse"),
						"404": resp("Not found", "#/components/schemas/Error"),
						"409": resp("Hash mismatch: the source returned a different file; nothing was changed. Not raised for a similarity-matched origin", "#/components/schemas/Error"),
						"500": resp("A merge write failed (recorded as an error fetch status)", "#/components/schemas/Error"),
					},
				},
			},
			"/images/{id}/fetch-status": map[string]any{
				"post": map[string]any{
					"summary":     "Report a source-fetch outcome",
					"operationId": "reportFetchStatus",
					"description": "Lets monloader report a fetch that failed before it could enrich (unsupported URL, timeout, blocked) so the detail page's pending pill resolves instead of polling to its cap.",
					"parameters":  []map[string]any{pathParam("id", "Image ID"), galleryParam()},
					"requestBody": jsonBodySchema(true, map[string]any{
						"type":     "object",
						"required": []string{"state"},
						"properties": map[string]any{
							"state":   map[string]any{"type": "string", "description": "Outcome state, e.g. \"error\""},
							"message": map[string]any{"type": "string", "description": "Operator-facing detail shown on the image's fetch pill"},
						},
					}),
					"responses": map[string]any{
						"200": map[string]any{"description": "Recorded"},
						"400": resp("Missing state", "#/components/schemas/Error"),
					},
				},
			},
			"/images/{id}/tags": map[string]any{
				"get": op("List image tags", "listImageTags",
					[]map[string]any{pathParam("id", "Image ID"), galleryParam()},
					map[string]any{
						"200": resp("Image tag list", "#/components/schemas/TagArray"),
						"404": resp("Not found", "#/components/schemas/Error"),
					}),
				"post": map[string]any{
					"summary":     "Add tags to image",
					"operationId": "addImageTags",
					"parameters":  []map[string]any{pathParam("id", "Image ID"), galleryParam()},
					"requestBody": jsonBodySchema(true, map[string]any{
						"type":     "object",
						"required": []string{"tags"},
						"properties": map[string]any{
							"tags": map[string]any{
								"type":        "array",
								"items":       map[string]any{"type": "string"},
								"description": "Tag names to add",
							},
							"via": map[string]any{
								"type":        "string",
								"description": "Optional caller-supplied identifier attached to each added tag (app name, URL...); stored in image_tags.tagger_name so the detail page can surface which third party contributed them",
							},
						},
					}),
					"responses": map[string]any{
						"200": map[string]any{
							"description": "Bare TagArray on success; wrapped in {tags, tag_warnings} when any tag failed validation.",
							"content": map[string]any{
								"application/json": map[string]any{
									"schema": map[string]any{
										"oneOf": []map[string]any{
											{"$ref": "#/components/schemas/TagArray"},
											{
												"type": "object",
												"properties": map[string]any{
													"tags":         map[string]any{"$ref": "#/components/schemas/TagArray"},
													"tag_warnings": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
												},
											},
										},
									},
								},
							},
						},
					},
				},
				"delete": map[string]any{
					"summary":     "Remove tags from image",
					"operationId": "removeImageTags",
					"parameters":  []map[string]any{pathParam("id", "Image ID"), galleryParam()},
					"requestBody": jsonBodySchema(true, map[string]any{
						"type":     "object",
						"required": []string{"tags"},
						"properties": map[string]any{
							"tags": map[string]any{
								"type":        "array",
								"items":       map[string]any{"type": "string"},
								"description": "Tag names to remove",
							},
						},
					}),
					"responses": map[string]any{
						"200": resp("Updated tag list", "#/components/schemas/TagArray"),
						"400": resp("Empty `tags`", "#/components/schemas/Error"),
						"404": resp("Not found", "#/components/schemas/Error"),
						"409": resp("conflict for a name matching more than one tag on the image; tag_implied when another tag on the image implies the one being removed", "#/components/schemas/Error"),
					},
				},
			},
			"/images/{id}/file": map[string]any{
				"get": op("Download original image/video bytes", "getImageFile",
					[]map[string]any{pathParam("id", "Image ID"), galleryParam()},
					map[string]any{
						"200": map[string]any{"description": "Original file bytes", "content": binaryContent("application/octet-stream")},
						"404": resp("Not found", "#/components/schemas/Error"),
					}),
				"post": map[string]any{
					"summary":     "Replace the image's file in place",
					"description": "The file-carrying sibling of enrich: the uploaded bytes replace the existing row's file while the row and everything attached to it survive. Content-derived state (sha256, dimensions, size, type, embedded metadata, thumbnail, phash) is re-derived, annotation boxes scale to the new dimensions, and the accompanying metadata fields land through the same merge as a push. The driving origin is marked exact (similarity reset, md5 refreshed). An upload whose sha256 matches the stored file applies the metadata only and answers replaced=false. Image and animated rows only.",
					"operationId": "replaceImageFile",
					"parameters":  []map[string]any{pathParam("id", "Image ID"), galleryParam()},
					"requestBody": map[string]any{
						"required": true,
						"content": map[string]any{
							"multipart/form-data": map[string]any{
								"schema": map[string]any{
									"type":     "object",
									"required": []string{"file"},
									"properties": map[string]any{
										"file":       map[string]any{"type": "string", "format": "binary"},
										"tags":       map[string]any{"type": "string", "description": "JSON array of tag names, merged like an enrich"},
										"source":     map[string]any{"type": "string"},
										"post_id":    map[string]any{"type": "string"},
										"url":        map[string]any{"type": "string"},
										"md5":        map[string]any{"type": "string", "description": "md5 the source claims; recorded on the origin row"},
										"parent_url": map[string]any{"type": "string"},
										"commentary": map[string]any{"type": "string"},
										"original":   map[string]any{"type": "string"},
										"notes":      map[string]any{"type": "string", "description": "JSON array of positional note boxes"},
									},
								},
							},
						},
					},
					"responses": map[string]any{
						"200": resp("Replace outcome", "#/components/schemas/ReplaceFileResponse"),
						"404": resp("Not found", "#/components/schemas/Error"),
						"409": resp("wrong_type for an archive/video row or a non-image upload; already_exists when the uploaded bytes are another image (the pair is recorded as potential duplicates)", "#/components/schemas/Error"),
					},
				},
			},
			"/images/{id}/thumbnail": map[string]any{
				"get": op("Download the static thumbnail", "getImageThumbnail",
					[]map[string]any{pathParam("id", "Image ID"), galleryParam()},
					map[string]any{
						"200": map[string]any{"description": "JPEG thumbnail", "content": binaryContent("image/jpeg")},
						"404": resp("Image or thumbnail not found", "#/components/schemas/Error"),
					}),
			},
			"/images/{id}/page/{n}": map[string]any{
				"get": op("Download a manga page (cbz rows)", "getMangaPage",
					[]map[string]any{pathParam("id", "Image ID"), pathParam("n", "1-based page number"), galleryParam()},
					map[string]any{
						"200": map[string]any{"description": "Page bytes (lazily extracted from the archive)", "content": binaryContent("application/octet-stream")},
						"400": resp("Invalid page number", "#/components/schemas/Error"),
						"404": resp("Not a manga row, or page out of range", "#/components/schemas/Error"),
					}),
			},
			"/images/{id}/page/{n}/thumb": map[string]any{
				"get": op("Download a manga page thumbnail (cbz rows)", "getMangaPageThumb",
					[]map[string]any{pathParam("id", "Image ID"), pathParam("n", "1-based page number"), galleryParam()},
					map[string]any{
						"200": map[string]any{"description": "Page thumbnail (JPEG)", "content": binaryContent("image/jpeg")},
						"400": resp("Invalid page number", "#/components/schemas/Error"),
						"404": resp("Not a manga row, or page out of range", "#/components/schemas/Error"),
					}),
			},
			"/images/{id}/relations": map[string]any{
				"get": op("Get declared relations for an image", "getImageRelations",
					[]map[string]any{pathParam("id", "Image ID"), galleryParam()},
					map[string]any{
						"200": resp("Declared relations", "#/components/schemas/ImageRelations"),
						"404": resp("Image not found", "#/components/schemas/Error"),
					}),
			},
			"/relations": map[string]any{
				"post": map[string]any{
					"summary":     "Declare a relation",
					"operationId": "addRelation",
					"parameters":  []map[string]any{galleryParam()},
					"requestBody": jsonBodySchema(true, map[string]any{
						"type":     "object",
						"required": []string{"type", "a", "b"},
						"properties": map[string]any{
							"type":      map[string]any{"type": "string", "description": "One of: duplicate, alternate, version, derivative, not_related"},
							"a":         map[string]any{"type": "integer", "description": "Left image id. For directed types (version, derivative), `a` is the parent / source (older revision, derived-from)."},
							"b":         map[string]any{"type": "integer", "description": "Right image id. For directed types, the child / derivative."},
							"direction": map[string]any{"type": "string", "description": "Optional `ba` swaps left/right; default `ab` treats `a` as the parent/source side."},
						},
					}),
					"responses": map[string]any{
						"201": map[string]any{"description": "Relation created"},
						"400": resp("Invalid request (self-relation, unknown type, missing ids)", "#/components/schemas/Error"),
						"409": resp("Pair carries a conflicting relation already; remove it first.", "#/components/schemas/Error"),
					},
				},
				"delete": map[string]any{
					"summary":     "Remove a declared relation",
					"operationId": "removeRelation",
					"parameters":  []map[string]any{galleryParam()},
					"requestBody": jsonBodySchema(true, map[string]any{
						"type":     "object",
						"required": []string{"type"},
						"properties": map[string]any{
							"type":     map[string]any{"type": "string", "description": "One of: duplicate, alternate, version, derivative, not_related, dissolve_dup, dissolve_alt, dissolve_version, dissolve_derivative, promote_original"},
							"image_id": map[string]any{"type": "integer", "description": "Required for duplicate / alternate (the member to unlink); the new original for promote_original."},
							"a":        map[string]any{"type": "integer", "description": "Required for version / derivative / not_related; either order is accepted."},
							"b":        map[string]any{"type": "integer", "description": "Pair partner for a."},
							"group_id": map[string]any{"type": "integer", "description": "Required for dissolve_dup / dissolve_alt / promote_original."},
							"root_id":  map[string]any{"type": "integer", "description": "Required for dissolve_version / dissolve_derivative; any chain or tree member id (the walker locates the root)."},
						},
					}),
					"responses": map[string]any{
						"204": map[string]any{"description": "Removed (idempotent)"},
						"400": resp("Invalid request", "#/components/schemas/Error"),
					},
				},
			},
			"/tags": map[string]any{
				"get": map[string]any{
					"summary":     "List tags",
					"operationId": "listTags",
					"parameters": []map[string]any{
						galleryParam(),
						queryParam("q", "Name filter; prefix match, or * as a wildcard anywhere"),
						queryParam("category", "Filter by category name"),
						queryParam("sort", "Sort field (usage, alpha)"),
						queryParam("page", "Page number"),
						queryParam("limit", "Results per page (default 100, max 500)"),
						queryParam("show_zero", "Hide zero-usage non-alias tags ('0' to opt out). Default surfaces them so the API total matches the /tags page total."),
						queryParam("origin", "Filter by stored creation origin ('user', a booru site, 'ptr', an auto-tagger name, ...). The legacy 'alias' value narrows to alias rows. Empty (default) returns every tag."),
						queryParam("type", "Structural filter: 'tag' (non-alias rows), 'alias' (alias rows). Empty (default) returns both."),
					},
					"responses": map[string]any{
						"200": resp("Paginated tag list", "#/components/schemas/PaginatedTags"),
					},
				},
				"post": map[string]any{
					"summary":     "Create a tag (get-or-create)",
					"operationId": "createTag",
					"parameters":  []map[string]any{galleryParam()},
					"requestBody": jsonBodySchema(true, map[string]any{
						"type":     "object",
						"required": []string{"name"},
						"properties": map[string]any{
							"name":     map[string]any{"type": "string"},
							"category": map[string]any{"type": "string", "description": "Category name; defaults to general."},
						},
					}),
					"responses": map[string]any{
						"201": resp("The tag (created, or the existing row when the name already exists in the category)", "#/components/schemas/TagRow"),
						"400": resp("Missing name, unknown category, or invalid tag name", "#/components/schemas/Error"),
					},
				},
			},
			"/tags/{id}": map[string]any{
				"patch": map[string]any{
					"summary":     "Rename a tag and/or move it to another category",
					"operationId": "patchTag",
					"parameters":  []map[string]any{pathParam("id", "Tag ID"), galleryParam()},
					"requestBody": jsonBodySchema(true, map[string]any{
						"type": "object",
						"properties": map[string]any{
							"name":     map[string]any{"type": "string", "description": "New name."},
							"category": map[string]any{"type": "string", "description": "Move to this category by name."},
						},
					}),
					"responses": map[string]any{
						"200": resp("Updated tag", "#/components/schemas/TagRow"),
						"400": resp("No fields supplied, or a rating-tag edit", "#/components/schemas/Error"),
						"404": resp("Tag not found", "#/components/schemas/Error"),
						"409": resp("A tag with the new name already exists in the target category", "#/components/schemas/Error"),
					},
				},
				"delete": op("Delete a tag", "deleteTag",
					[]map[string]any{pathParam("id", "Tag ID"), galleryParam()},
					map[string]any{
						"204": map[string]any{"description": "Deleted (rating-category rows are usage-stripped; the catalog row stays)"},
						"404": resp("Tag not found", "#/components/schemas/Error"),
					}),
			},
			"/tags/aliases": map[string]any{
				"post": map[string]any{
					"summary":     "Create an alias",
					"operationId": "createAlias",
					"parameters":  []map[string]any{galleryParam()},
					"requestBody": jsonBodySchema(true, map[string]any{
						"type":     "object",
						"required": []string{"name", "canonical_id"},
						"properties": map[string]any{
							"name":         map[string]any{"type": "string", "description": "Alias name."},
							"category":     map[string]any{"type": "string", "description": "Category for the alias; defaults to general."},
							"canonical_id": map[string]any{"type": "integer", "description": "The tag this alias resolves to."},
						},
					}),
					"responses": map[string]any{
						"201": resp("The alias row", "#/components/schemas/TagRow"),
						"400": resp("Missing fields or an invalid alias target", "#/components/schemas/Error"),
						"404": resp("Canonical tag not found", "#/components/schemas/Error"),
						"409": resp("The name already names a tag with image_tags rows; merge it instead", "#/components/schemas/Error"),
					},
				},
			},
			"/tags/merge": map[string]any{
				"post": map[string]any{
					"summary":     "Merge one tag into another",
					"operationId": "mergeTags",
					"parameters":  []map[string]any{galleryParam()},
					"requestBody": jsonBodySchema(true, map[string]any{
						"type":     "object",
						"required": []string{"alias_id", "canonical_id"},
						"properties": map[string]any{
							"alias_id":     map[string]any{"type": "integer", "description": "Tag to retire; becomes an alias of canonical_id and its image_tags move onto it."},
							"canonical_id": map[string]any{"type": "integer", "description": "Surviving tag."},
						},
					}),
					"responses": map[string]any{
						"200": resp("The canonical tag", "#/components/schemas/TagRow"),
						"400": resp("Self-merge, missing ids, or a merge into an alias", "#/components/schemas/Error"),
					},
				},
			},
			"/tags/{id}/implications": map[string]any{
				"get": op("List a tag's implications", "listImplications",
					[]map[string]any{pathParam("id", "Parent tag ID"), galleryParam()},
					map[string]any{
						"200": map[string]any{"description": "Direct implications", "content": map[string]any{"application/json": map[string]any{"schema": map[string]any{"type": "array", "items": map[string]any{"$ref": "#/components/schemas/Implication"}}}}},
						"404": resp("Parent tag not found", "#/components/schemas/Error"),
					}),
				"post": map[string]any{
					"summary":     "Declare an implication",
					"operationId": "addImplication",
					"description": "Declares parent -> implied. The edge is effective immediately for future tag adds; the historical fan-out across images already carrying the parent (the web background job) is not run from the API.",
					"parameters":  []map[string]any{pathParam("id", "Parent tag ID"), galleryParam()},
					"requestBody": jsonBodySchema(true, map[string]any{
						"type":     "object",
						"required": []string{"implied_id"},
						"properties": map[string]any{
							"implied_id": map[string]any{"type": "integer", "description": "Existing tag id the parent implies. Both sides must be canonical (non-alias)."},
						},
					}),
					"responses": map[string]any{
						"200": map[string]any{"description": "Edge already declared (no-op)"},
						"201": map[string]any{"description": "Edge created"},
						"400": resp("Missing implied_id, self-implication, or an alias on either side", "#/components/schemas/Error"),
						"404": resp("Parent or implied tag not found", "#/components/schemas/Error"),
						"409": resp("Edge would close a cycle", "#/components/schemas/Error"),
					},
				},
			},
			"/tags/{id}/implications/{impliedID}": map[string]any{
				"delete": map[string]any{
					"summary":     "Remove an implication",
					"operationId": "removeImplication",
					"description": "Drops the edge only; the image-side sweep of rows implied solely by this edge is the web background job and is not run from the API.",
					"parameters":  []map[string]any{pathParam("id", "Parent tag ID"), pathParam("impliedID", "Implied tag ID"), galleryParam()},
					"responses": map[string]any{
						"204": map[string]any{"description": "Removed"},
						"404": resp("Edge not found", "#/components/schemas/Error"),
					},
				},
			},
			"/categories": map[string]any{
				"get": op("List tag categories", "listCategories",
					[]map[string]any{galleryParam()},
					map[string]any{
						"200": map[string]any{"description": "Categories", "content": map[string]any{"application/json": map[string]any{"schema": map[string]any{"type": "array", "items": map[string]any{"$ref": "#/components/schemas/Category"}}}}},
					}),
				"post": map[string]any{
					"summary":     "Create a category",
					"operationId": "createCategory",
					"parameters":  []map[string]any{galleryParam()},
					"requestBody": jsonBodySchema(true, map[string]any{
						"type":     "object",
						"required": []string{"name"},
						"properties": map[string]any{
							"name":  map[string]any{"type": "string", "description": "Lowercase letters, digits, underscore, or hyphen. Search-filter keywords are reserved."},
							"color": map[string]any{"type": "string", "description": "#rgb or #rrggbb; defaults to #888888 when blank."},
						},
					}),
					"responses": map[string]any{
						"201": resp("Created category", "#/components/schemas/Category"),
						"400": resp("Invalid or reserved name, or invalid colour", "#/components/schemas/Error"),
						"409": resp("A category with this name already exists", "#/components/schemas/Error"),
					},
				},
			},
			"/categories/{id}": map[string]any{
				"patch": map[string]any{
					"summary":     "Rename and/or recolor a category",
					"operationId": "patchCategory",
					"parameters":  []map[string]any{pathParam("id", "Category ID"), galleryParam()},
					"requestBody": jsonBodySchema(true, map[string]any{
						"type": "object",
						"properties": map[string]any{
							"name":  map[string]any{"type": "string", "description": "New name. Built-in categories refuse a rename."},
							"color": map[string]any{"type": "string", "description": "#rgb or #rrggbb. Allowed on built-in categories."},
						},
					}),
					"responses": map[string]any{
						"200": resp("Updated category", "#/components/schemas/Category"),
						"400": resp("No fields, invalid colour, reserved/invalid name, or a built-in rename", "#/components/schemas/Error"),
						"404": resp("Category not found", "#/components/schemas/Error"),
						"409": resp("A category with the new name already exists", "#/components/schemas/Error"),
					},
				},
				"delete": map[string]any{
					"summary":     "Delete a category",
					"operationId": "deleteCategory",
					"parameters":  []map[string]any{pathParam("id", "Category ID"), galleryParam()},
					"requestBody": jsonBodySchema(false, map[string]any{
						"type": "object",
						"properties": map[string]any{
							"action":    map[string]any{"type": "string", "description": "'move' (default; reparent the category's tags) or 'delete_all' (drop the tags too)."},
							"target_id": map[string]any{"type": "integer", "description": "Move target category id; defaults to general when omitted."},
						},
					}),
					"responses": map[string]any{
						"204": map[string]any{"description": "Deleted"},
						"400": resp("Built-in category, an unknown action, or an unusable move target", "#/components/schemas/Error"),
						"404": resp("Category not found", "#/components/schemas/Error"),
					},
				},
			},
			"/galleries": map[string]any{
				"get": op("List configured galleries", "listGalleries",
					nil,
					map[string]any{
						"200": map[string]any{"description": "Galleries with counts and the active flag", "content": map[string]any{"application/json": map[string]any{"schema": map[string]any{"type": "array", "items": map[string]any{"$ref": "#/components/schemas/Gallery"}}}}},
					}),
			},
		},
	}
}

// op builds one path operation. Every entry in the paths map above is the
// same four fields in the same order; spelling them out 33 times is how a
// mis-keyed one hides. Operations carrying a requestBody keep their
// literal, since the body is the bulk of those.
func op(summary, id string, params []map[string]any, responses map[string]any) map[string]any {
	m := map[string]any{"summary": summary, "operationId": id, "responses": responses}
	if len(params) > 0 {
		m["parameters"] = params
	}
	return m
}

func resp(desc, ref string) map[string]any {
	return map[string]any{"description": desc, "content": jsonContent(ref)}
}

func jsonBodySchema(required bool, schema map[string]any) map[string]any {
	return map[string]any{
		"required": required,
		"content":  map[string]any{"application/json": map[string]any{"schema": schema}},
	}
}

func paginatedSchema(itemRef string) map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"page":    map[string]any{"type": "integer"},
			"limit":   map[string]any{"type": "integer"},
			"total":   map[string]any{"type": "integer"},
			"results": map[string]any{"type": "array", "items": map[string]any{"$ref": itemRef}},
		},
	}
}

func jsonContent(ref string) map[string]any {
	return map[string]any{
		"application/json": map[string]any{
			"schema": map[string]any{"$ref": ref},
		},
	}
}

func binaryContent(mime string) map[string]any {
	return map[string]any{
		mime: map[string]any{"schema": map[string]any{"type": "string", "format": "binary"}},
	}
}

func pathParam(name, desc string) map[string]any {
	return map[string]any{
		"name": name, "in": "path", "required": true,
		"description": desc,
		"schema":      map[string]any{"type": "string"},
	}
}

func queryParam(name, desc string) map[string]any {
	return map[string]any{
		"name": name, "in": "query", "required": false,
		"description": desc,
		"schema":      map[string]any{"type": "string"},
	}
}

// baseURL reads the configured base under the web layer's config lock,
// the way every other reader of that field does.
func (h *Handler) baseURL() string {
	h.cfgMu.RLock()
	defer h.cfgMu.RUnlock()
	return h.cfg.Server.BaseURL
}

// galleryParam is the shared ?gallery=<name> selector. Omitted means
// the active gallery.
func galleryParam() map[string]any {
	return queryParam("gallery", "Target gallery name; omit for the active gallery (also accepted as X-Monbooru-Gallery header)")
}

// openAPIJSON serves the raw OpenAPI JSON spec.
func (h *Handler) openAPIJSON(w http.ResponseWriter, r *http.Request) {
	spec := buildSpec(h.baseURL())
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(spec)
}

// openAPIDocs serves a self-contained HTML page rendered from the
// OpenAPI spec served at /api/v1/openapi.json. No external assets are
// loaded at runtime, so the page works offline.
func (h *Handler) openAPIDocs(w http.ResponseWriter, r *http.Request) {
	view := extractDocsView(buildSpec(h.baseURL()))
	h.cfgMu.RLock()
	view.APIEnabled = len(h.cfg.Auth.Tokens) > 0
	h.cfgMu.RUnlock()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := docsTemplate.Execute(w, view); err != nil {
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

type docsView struct {
	Title      string
	Version    string
	BaseURL    string
	APIEnabled bool
	Endpoints  []endpointView
	Schemas    []schemaView
}

type endpointView struct {
	Method      string
	MethodLower string
	Path        string
	Summary     string
	Anchor      string
	Params      []paramView
	Request     *requestView
	Responses   []responseView
}

type paramView struct {
	Name, In, Description string
	Required              bool
}

type requestView struct {
	MediaTypes []mediaTypeView
}

type mediaTypeView struct {
	ContentType string
	Required    []string
	Properties  []propertyView
	Ref         string
	RefAnchor   string
}

type propertyView struct {
	Name, Type, Description string
	Nullable                bool
}

type responseView struct {
	Status, Description, Ref, RefAnchor string
}

type schemaView struct {
	Name       string
	Anchor     string
	Properties []propertyView
}

// methodOrder controls how HTTP methods are ordered for each path.
var methodOrder = []string{"get", "post", "put", "patch", "delete"}

// extractDocsView flattens the OpenAPI spec into the template view.
// It assumes the shape buildSpec produces; unknown keys are ignored.
func extractDocsView(spec map[string]any) docsView {
	view := docsView{}
	if info, ok := spec["info"].(map[string]any); ok {
		view.Title, _ = info["title"].(string)
		view.Version, _ = info["version"].(string)
	}
	if servers, ok := spec["servers"].([]map[string]any); ok && len(servers) > 0 {
		view.BaseURL, _ = servers[0]["url"].(string)
	}

	paths, _ := spec["paths"].(map[string]any)
	sortedPaths := slices.Sorted(maps.Keys(paths))
	for _, p := range sortedPaths {
		methods, _ := paths[p].(map[string]any)
		for _, m := range methodOrder {
			op, ok := methods[m].(map[string]any)
			if !ok {
				continue
			}
			e := endpointView{
				Method:      strings.ToUpper(m),
				MethodLower: m,
				Path:        p,
				Anchor:      m + "-" + anchorize(p),
			}
			e.Summary, _ = op["summary"].(string)
			if params, ok := op["parameters"].([]map[string]any); ok {
				for _, pp := range params {
					name, _ := pp["name"].(string)
					in, _ := pp["in"].(string)
					desc, _ := pp["description"].(string)
					req, _ := pp["required"].(bool)
					e.Params = append(e.Params, paramView{Name: name, In: in, Description: desc, Required: req})
				}
			}
			if body, ok := op["requestBody"].(map[string]any); ok {
				e.Request = extractRequest(body)
			}
			if resps, ok := op["responses"].(map[string]any); ok {
				e.Responses = extractResponses(resps)
			}
			view.Endpoints = append(view.Endpoints, e)
		}
	}

	if comps, ok := spec["components"].(map[string]any); ok {
		if schemas, ok := comps["schemas"].(map[string]any); ok {
			names := slices.Sorted(maps.Keys(schemas))
			for _, n := range names {
				s, _ := schemas[n].(map[string]any)
				sv := schemaView{Name: n, Anchor: anchorize(n)}
				if props, ok := s["properties"].(map[string]any); ok {
					sv.Properties = extractProps(props)
				}
				view.Schemas = append(view.Schemas, sv)
			}
		}
	}

	return view
}

func extractRequest(body map[string]any) *requestView {
	rv := &requestView{}
	content, ok := body["content"].(map[string]any)
	if !ok {
		return rv
	}
	cts := slices.Sorted(maps.Keys(content))
	for _, ct := range cts {
		mt, _ := content[ct].(map[string]any)
		schema, _ := mt["schema"].(map[string]any)
		mtv := mediaTypeView{ContentType: ct}
		if ref, ok := schema["$ref"].(string); ok {
			mtv.Ref = strings.TrimPrefix(ref, "#/components/schemas/")
			mtv.RefAnchor = anchorize(mtv.Ref)
		} else {
			if req, ok := schema["required"].([]string); ok {
				mtv.Required = req
			}
			if props, ok := schema["properties"].(map[string]any); ok {
				mtv.Properties = extractProps(props)
			}
		}
		rv.MediaTypes = append(rv.MediaTypes, mtv)
	}
	return rv
}

func extractResponses(resps map[string]any) []responseView {
	codes := slices.Sorted(maps.Keys(resps))
	out := make([]responseView, 0, len(codes))
	for _, c := range codes {
		r, _ := resps[c].(map[string]any)
		rv := responseView{Status: c}
		rv.Description, _ = r["description"].(string)
		if content, ok := r["content"].(map[string]any); ok {
			if app, ok := content["application/json"].(map[string]any); ok {
				if schema, ok := app["schema"].(map[string]any); ok {
					if ref, ok := schema["$ref"].(string); ok {
						rv.Ref = strings.TrimPrefix(ref, "#/components/schemas/")
						rv.RefAnchor = anchorize(rv.Ref)
					}
				}
			}
		}
		out = append(out, rv)
	}
	return out
}

func extractProps(props map[string]any) []propertyView {
	names := slices.Sorted(maps.Keys(props))
	out := make([]propertyView, 0, len(names))
	for _, n := range names {
		p, _ := props[n].(map[string]any)
		t, _ := p["type"].(string)
		d, _ := p["description"].(string)
		nullable, _ := p["nullable"].(bool)
		out = append(out, propertyView{Name: n, Type: t, Description: d, Nullable: nullable})
	}
	return out
}

func anchorize(s string) string {
	r := strings.ToLower(s)
	r = strings.ReplaceAll(r, "/", "-")
	r = strings.ReplaceAll(r, "{", "")
	r = strings.ReplaceAll(r, "}", "")
	r = strings.Trim(r, "-")
	r = cmp.Or(r, "root")
	return r
}

// docsTemplate renders the API documentation with inline CSS matching
// the rest of the UI.
var docsTemplate = template.Must(template.New("api-docs").Parse(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>{{.Title}} - Docs</title>
<style>
 body { background:#0d0d0d; color:#c8c8c8; font-family:"JetBrains Mono","Fira Mono","Courier New",monospace; font-size:14px; line-height:1.5; padding:24px; max-width:1000px; margin:0 auto; }
 h1 { font-size:20px; font-weight:bold; margin-bottom:4px; }
 h2 { font-size:16px; color:#c8c8c8; border-bottom:1px solid #2a2a2a; padding-bottom:4px; margin:24px 0 8px; }
 h3 { font-size:13px; color:#666; margin:12px 0 4px; font-weight:normal; text-transform:uppercase; letter-spacing:0.5px; }
 a { color:#9d2235; text-decoration:none; }
 a:hover { text-decoration:underline; }
 code { font-family:inherit; }
 table { border-collapse:collapse; width:100%; margin:6px 0 10px; font-size:13px; }
 th, td { border:1px solid #2a2a2a; padding:4px 8px; text-align:left; vertical-align:top; }
 th { color:#666; font-weight:normal; background:#111; }
 .muted { color:#666; font-size:12px; }
 .method { display:inline-block; padding:1px 6px; border:1px solid; font-weight:bold; margin-right:8px; font-size:12px; min-width:52px; text-align:center; }
 .method-get    { color:#22aa44; border-color:#22aa44; }
 .method-post   { color:#3d90e3; border-color:#3d90e3; }
 .method-put    { color:#ffaa00; border-color:#ffaa00; }
 .method-patch  { color:#ffaa00; border-color:#ffaa00; }
 .method-delete { color:#cc3333; border-color:#cc3333; }
 .path { color:#c8c8c8; }
 ul.toc { list-style:none; padding:0; margin:8px 0 20px; }
 ul.toc li { padding:2px 0; }
 .tag { color:#ffaa00; }
 .endpoint { padding-bottom:6px; }
 .hdr { position:sticky; top:0; background:#0d0d0d; padding:4px 0; }
</style>
</head>
{{define "proptable"}}<table>
 <thead><tr><th>Field</th><th>Type</th><th>Description</th></tr></thead>
 <tbody>
 {{range .}}
  <tr><td><code>{{.Name}}</code></td><td>{{.Type}}{{if .Nullable}} · nullable{{end}}</td><td>{{.Description}}</td></tr>
 {{end}}
 </tbody>
</table>{{end}}
<body>
 <p class="muted"><a href="/">← Back</a></p>
 <h1>{{.Title}}</h1>
 <p class="muted">Version {{.Version}} · base URL <code>{{.BaseURL}}</code></p>
 {{if .APIEnabled}}
 <p style="color:#22aa44;border:1px solid #22aa44;padding:4px 8px;">API is active - authenticate with your bearer token from Settings → Authentication.</p>
 {{else}}
 <p style="color:#ffaa00;border:1px solid #ffaa00;padding:4px 8px;">API is disabled - generate a token in Settings → Authentication to enable it. All endpoints currently return <code>503 api_disabled</code>.</p>
 {{end}}
 <p>Every endpoint except <code>/docs</code> and <code>/openapi.json</code> requires <code>Authorization: Bearer &lt;token&gt;</code>. Create a named token in Settings → Authentication; while none exists every authenticated endpoint returns <code>503 api_disabled</code>. Tokens are scoped (read/write/delete); a request whose token lacks the scope gets <code>403 insufficient_scope</code>.</p>
 <p>Endpoints take an optional <code>?gallery=&lt;name&gt;</code> (or <code>X-Monbooru-Gallery</code> header) to target a specific gallery; omit both for the active one.</p>
 <p class="muted">Raw spec: <a href="/api/v1/openapi.json">openapi.json</a></p>

 <h2>Endpoints</h2>
 <ul class="toc">
 {{range .Endpoints}}
  <li><a href="#{{.Anchor}}"><span class="method method-{{.MethodLower}}">{{.Method}}</span><span class="path">{{.Path}}</span></a>{{if .Summary}} <span class="muted">- {{.Summary}}</span>{{end}}</li>
 {{end}}
 </ul>

 {{range .Endpoints}}
 <div class="endpoint">
  <h2 id="{{.Anchor}}"><span class="method method-{{.MethodLower}}">{{.Method}}</span><span class="path">{{.Path}}</span></h2>
  {{if .Summary}}<p>{{.Summary}}</p>{{end}}

  {{if .Params}}
  <h3>Parameters</h3>
  <table>
   <thead><tr><th>Name</th><th>In</th><th>Required</th><th>Description</th></tr></thead>
   <tbody>
   {{range .Params}}
    <tr><td><code>{{.Name}}</code></td><td>{{.In}}</td><td>{{if .Required}}yes{{else}}no{{end}}</td><td>{{.Description}}</td></tr>
   {{end}}
   </tbody>
  </table>
  {{end}}

  {{if .Request}}
  <h3>Request body</h3>
  {{range .Request.MediaTypes}}
   <p class="muted">Content-Type: <code>{{.ContentType}}</code>{{if .Ref}} - schema <a href="#schema-{{.RefAnchor}}"><code>{{.Ref}}</code></a>{{end}}</p>
   {{if .Required}}<p class="muted">Required: {{range .Required}}<code>{{.}}</code> {{end}}</p>{{end}}
   {{if .Properties}}
   {{template "proptable" .Properties}}
   {{end}}
  {{end}}
  {{end}}

  {{if .Responses}}
  <h3>Responses</h3>
  <table>
   <thead><tr><th>Status</th><th>Description</th><th>Schema</th></tr></thead>
   <tbody>
   {{range .Responses}}
    <tr><td><code>{{.Status}}</code></td><td>{{.Description}}</td><td>{{if .Ref}}<a href="#schema-{{.RefAnchor}}"><code>{{.Ref}}</code></a>{{end}}</td></tr>
   {{end}}
   </tbody>
  </table>
  {{end}}
 </div>
 {{end}}

 <h2>Schemas</h2>
 {{range .Schemas}}
  <h3 id="schema-{{.Anchor}}" style="color:#c8c8c8; font-size:14px; text-transform:none; letter-spacing:0; margin-top:14px">{{.Name}}</h3>
  {{if .Properties}}
  {{template "proptable" .Properties}}
  {{else}}
  <p class="muted">(no fields)</p>
  {{end}}
 {{end}}
</body>
</html>`))
