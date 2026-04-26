package api

import (
	"encoding/json"
	"html/template"
	"net/http"
	"sort"
	"strings"
)

func buildSpec(baseURL string) map[string]any {
	return map[string]any{
		"openapi": "3.0.3",
		"info": map[string]any{
			"title":       "Monbooru API",
			"description": "REST API for monbooru image library",
			"version":     "1.0.0",
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
						"tagger_name": map[string]any{"type": "string", "nullable": true, "description": "Source identifier: auto-tagger subfolder name when is_auto, caller-supplied source (e.g. app name) when manual, null for UI-driven user adds"},
					},
				},
				"TagRow": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"id":          map[string]any{"type": "integer"},
						"name":        map[string]any{"type": "string"},
						"category":    map[string]any{"type": "string"},
						"color":       map[string]any{"type": "string"},
						"usage_count": map[string]any{"type": "integer"},
						"is_alias":    map[string]any{"type": "boolean"},
					},
				},
				"PaginatedTags": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"page":    map[string]any{"type": "integer"},
						"limit":   map[string]any{"type": "integer"},
						"total":   map[string]any{"type": "integer"},
						"results": map[string]any{"type": "array", "items": map[string]any{"$ref": "#/components/schemas/TagRow"}},
					},
				},
				"PaginatedImages": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"page":    map[string]any{"type": "integer"},
						"limit":   map[string]any{"type": "integer"},
						"total":   map[string]any{"type": "integer"},
						"results": map[string]any{"type": "array", "items": map[string]any{"$ref": "#/components/schemas/Image"}},
					},
				},
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
				"TagArray": map[string]any{
					"type":  "array",
					"items": map[string]any{"$ref": "#/components/schemas/Tag"},
				},
				"Image": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"id":             map[string]any{"type": "integer"},
						"sha256":         map[string]any{"type": "string"},
						"canonical_path": map[string]any{"type": "string"},
						"aliases":        map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
						"file_type":      map[string]any{"type": "string"},
						"width":          map[string]any{"type": "integer", "nullable": true},
						"height":         map[string]any{"type": "integer", "nullable": true},
						"file_size":      map[string]any{"type": "integer"},
						"is_favorited":   map[string]any{"type": "boolean"},
						"is_missing":     map[string]any{"type": "boolean"},
						"auto_tagged_at": map[string]any{"type": "string", "format": "date-time", "nullable": true},
						"source_type":    map[string]any{"type": "string"},
						"origin":         map[string]any{"type": "string", "description": "How the image got into the gallery: 'ingest' for watcher/sync, 'upload' for the web UI, or any caller-supplied string (app name, URL…) set via POST /images with 'source'"},
						"ingested_at":    map[string]any{"type": "string", "format": "date-time"},
						"thumbnail_url":  map[string]any{"type": "string"},
						"tags":           map[string]any{"type": "array", "items": map[string]any{"$ref": "#/components/schemas/Tag"}},
					},
				},
			},
		},
		"security": []map[string]any{
			{"bearerAuth": []string{}},
		},
		"paths": map[string]any{
			"/": map[string]any{
				"get": map[string]any{
					"summary":     "API info",
					"operationId": "apiInfo",
					"responses": map[string]any{
						"200": map[string]any{"description": "API metadata", "content": jsonContent("#/components/schemas/APIInfo")},
						"503": map[string]any{"description": "API disabled (no token configured)", "content": jsonContent("#/components/schemas/Error")},
					},
				},
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
										"file":        map[string]any{"type": "string", "format": "binary", "description": "Image or video file"},
										"tags":        map[string]any{"type": "string", "description": "JSON-encoded array of tag names"},
										"folder":      map[string]any{"type": "string", "description": "Destination subfolder under the gallery root; missing directories are created. Leave blank for the gallery root."},
										"autotag":     map[string]any{"type": "string", "description": "Set to \"true\" to kick off an auto-tag job on the new image"},
										"tagger_name": map[string]any{"type": "string", "description": "Optional auto-tagger name; when set with autotag, restricts the job to that tagger"},
										"source":      map[string]any{"type": "string", "description": "Optional source identifier (app name, URL…). Stored as images.origin and attached to each initial tag via image_tags.tagger_name. Blank defaults images.origin to 'upload' for multipart mode."},
									},
								},
							},
							"application/json": map[string]any{
								"schema": map[string]any{
									"type":     "object",
									"required": []string{"path"},
									"properties": map[string]any{
										"path":        map[string]any{"type": "string", "description": "Path to a file already on disk. Absolute paths are used verbatim; relative paths are resolved under gallery/<folder> when folder is set, otherwise under the gallery root. WARNING: absolute paths give a token holder read access to anything the monbooru process can stat."},
										"tags":        map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
										"folder":      map[string]any{"type": "string", "description": "Destination subfolder for relative paths"},
										"autotag":     map[string]any{"type": "boolean", "description": "Kick off an auto-tag job on the new image"},
										"tagger_name": map[string]any{"type": "string", "description": "Optional auto-tagger name"},
										"source":      map[string]any{"type": "string", "description": "Optional source identifier. Stored as images.origin and attached to each initial tag. Blank defaults images.origin to 'ingest' for JSON path-reference mode."},
									},
								},
							},
						},
					},
					"responses": map[string]any{
						"201": map[string]any{"description": "Image created", "content": jsonContent("#/components/schemas/CreateImageResponse")},
						"400": map[string]any{"description": "Invalid request or unsupported file type", "content": jsonContent("#/components/schemas/Error")},
						"409": map[string]any{"description": "Duplicate SHA-256", "content": jsonContent("#/components/schemas/Error")},
						"413": map[string]any{"description": "File exceeds max size", "content": jsonContent("#/components/schemas/Error")},
						"500": map[string]any{"description": "Ingest failure", "content": jsonContent("#/components/schemas/Error")},
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
						queryParam("sort", "Sort field: newest, filesize, random"),
						queryParam("order", "Sort order: asc, desc"),
						queryParam("page", "Page number (1-based)"),
						queryParam("limit", "Results per page (max 200)"),
					},
					"responses": map[string]any{
						"200": map[string]any{"description": "Paginated image list", "content": jsonContent("#/components/schemas/PaginatedImages")},
					},
				},
			},
			"/images/{id}": map[string]any{
				"get": map[string]any{
					"summary":     "Get image metadata",
					"operationId": "getImage",
					"parameters":  []map[string]any{pathParam("id", "Image ID"), galleryParam()},
					"responses": map[string]any{
						"200": map[string]any{"description": "Image metadata", "content": jsonContent("#/components/schemas/Image")},
						"404": map[string]any{"description": "Not found", "content": jsonContent("#/components/schemas/Error")},
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
						"404": map[string]any{"description": "Not found", "content": jsonContent("#/components/schemas/Error")},
					},
				},
			},
			"/images/{id}/tags": map[string]any{
				"post": map[string]any{
					"summary":     "Add tags to image",
					"operationId": "addImageTags",
					"parameters":  []map[string]any{pathParam("id", "Image ID"), galleryParam()},
					"requestBody": map[string]any{
						"required": true,
						"content": map[string]any{
							"application/json": map[string]any{
								"schema": map[string]any{
									"type":     "object",
									"required": []string{"tags"},
									"properties": map[string]any{
										"tags": map[string]any{
											"type":        "array",
											"items":       map[string]any{"type": "string"},
											"description": "Tag names to add",
										},
										"source": map[string]any{
											"type":        "string",
											"description": "Optional source identifier attached to each added tag (app name, URL…); stored in image_tags.tagger_name so the detail page can surface which third party contributed them",
										},
									},
								},
							},
						},
					},
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
					"requestBody": map[string]any{
						"required": true,
						"content": map[string]any{
							"application/json": map[string]any{
								"schema": map[string]any{
									"type":     "object",
									"required": []string{"tags"},
									"properties": map[string]any{
										"tags": map[string]any{
											"type":        "array",
											"items":       map[string]any{"type": "string"},
											"description": "Tag names to remove",
										},
									},
								},
							},
						},
					},
					"responses": map[string]any{
						"200": map[string]any{"description": "Updated tag list", "content": jsonContent("#/components/schemas/TagArray")},
					},
				},
			},
			"/tags": map[string]any{
				"get": map[string]any{
					"summary":     "List tags",
					"operationId": "listTags",
					"parameters": []map[string]any{
						galleryParam(),
						queryParam("q", "Prefix filter"),
						queryParam("category", "Filter by category name"),
						queryParam("sort", "Sort field (usage, alpha)"),
						queryParam("page", "Page number"),
						queryParam("limit", "Results per page (default 100, max 500)"),
					},
					"responses": map[string]any{
						"200": map[string]any{"description": "Paginated tag list", "content": jsonContent("#/components/schemas/PaginatedTags")},
					},
				},
			},
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

// galleryParam is the shared ?gallery=<name> selector. Omitted means
// the active gallery.
func galleryParam() map[string]any {
	return queryParam("gallery", "Target gallery name; omit for the active gallery (also accepted as X-Monbooru-Gallery header)")
}

// openAPIJSON serves the raw OpenAPI JSON spec.
func (h *Handler) openAPIJSON(w http.ResponseWriter, r *http.Request) {
	spec := buildSpec(h.cfg.Server.BaseURL)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(spec)
}

// openAPIDocs serves a self-contained HTML page rendered from the
// OpenAPI spec served at /api/v1/openapi.json. No external assets are
// loaded at runtime, so the page works offline.
func (h *Handler) openAPIDocs(w http.ResponseWriter, r *http.Request) {
	view := extractDocsView(buildSpec(h.cfg.Server.BaseURL))
	view.APIEnabled = h.cfg.Auth.APIToken != ""
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
	sortedPaths := make([]string, 0, len(paths))
	for p := range paths {
		sortedPaths = append(sortedPaths, p)
	}
	sort.Strings(sortedPaths)
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
			names := make([]string, 0, len(schemas))
			for n := range schemas {
				names = append(names, n)
			}
			sort.Strings(names)
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
	cts := make([]string, 0, len(content))
	for ct := range content {
		cts = append(cts, ct)
	}
	sort.Strings(cts)
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
	codes := make([]string, 0, len(resps))
	for c := range resps {
		codes = append(codes, c)
	}
	sort.Strings(codes)
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
	names := make([]string, 0, len(props))
	for n := range props {
		names = append(names, n)
	}
	sort.Strings(names)
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
	if r == "" {
		r = "root"
	}
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
 .method-post   { color:#9d2235; border-color:#9d2235; }
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
<body>
 <p class="muted"><a href="/">← Back</a></p>
 <h1>{{.Title}}</h1>
 <p class="muted">Version {{.Version}} · base URL <code>{{.BaseURL}}</code></p>
 {{if .APIEnabled}}
 <p style="color:#22aa44;border:1px solid #22aa44;padding:4px 8px;">API is active - authenticate with your bearer token from Settings → Authentication.</p>
 {{else}}
 <p style="color:#ffaa00;border:1px solid #ffaa00;padding:4px 8px;">API is disabled - generate a token in Settings → Authentication to enable it. All endpoints currently return <code>503 api_disabled</code>.</p>
 {{end}}
 <p>Every endpoint except <code>/docs</code> and <code>/openapi.json</code> requires <code>Authorization: Bearer &lt;token&gt;</code>. Generate a token from Settings → Authentication; while no token is set every authenticated endpoint returns <code>503 api_disabled</code>.</p>
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
   <table>
    <thead><tr><th>Field</th><th>Type</th><th>Description</th></tr></thead>
    <tbody>
    {{range .Properties}}
     <tr><td><code>{{.Name}}</code></td><td>{{.Type}}{{if .Nullable}} · nullable{{end}}</td><td>{{.Description}}</td></tr>
    {{end}}
    </tbody>
   </table>
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
  <table>
   <thead><tr><th>Field</th><th>Type</th><th>Description</th></tr></thead>
   <tbody>
   {{range .Properties}}
    <tr><td><code>{{.Name}}</code></td><td>{{.Type}}{{if .Nullable}} · nullable{{end}}</td><td>{{.Description}}</td></tr>
   {{end}}
   </tbody>
  </table>
  {{else}}
  <p class="muted">(no fields)</p>
  {{end}}
 {{end}}
</body>
</html>`))
