# Monbooru

Your own self-hosted and lightweight booru.  
Designed for organizing your local media collection, including AI-generated images (Stable Diffusion, ComfyUI, A1111/Forge). Works fully offline and with no telemetry. Supports ONNX models for local auto-tagging of your collection (WD14, JoyTag...).  

<table>
  <tr>
    <td><img src="/.github/assets/gallery.webp" width="400"/></td>
    <td><img src="/.github/assets/image.webp" width="400"/></td>
  </tr>
  <tr>
    <td><img src="/.github/assets/sd.webp" width="400"/></td>
    <td><img src="/.github/assets/tags.webp" width="400"/></td>
  </tr>
</table>

---

## Features

- Tag-based gallery with folder tree, favorites, saved searches, related-image suggestions
- A watcher that picks up new, moved, and deleted files in the gallery within a few seconds; and support for multi-file browser upload
- Stable Diffusion metadata extraction from A1111/Forge and ComfyUI (prompts, models, seeds, full workflow)
- Optional auto-tagging with local ONNX models (WD14, JoyTag, or any compatible model), CPU or GPU (CUDA)
- Search with wildcards, OR, exclusions, plus filters on folder, date, size, dimensions, category, generation recipe...
- Batch operations: bulk delete, bulk move, bulk auto-tag, delete-all-search-results, tag-all-search-results / tag-selected (add or remove tags in one go)
- Create multiple galleries with their own filesystem and switch between them
- Per-gallery export and import: full backup (.db, .json, .zip with images), or a light format (.json or .zip with images) with only images<->tags map. See [docs/MIGRATING.md](docs/MIGRATING.md) for migrating from another supported booru-style app.
- REST API for third-party integrations (e.g. adding images to the gallery from an external app)
- Daily maintenance schedule: scheduled sync, auto-tag, recompute counts, merge general tags, and vacuum databases...
- Fully offline, no inbound or outbound internet connection.

---

## Quick start (Docker)

Edit the volume paths in [`docker/docker-compose.yml`](docker/docker-compose.yml), then `docker compose up -d`. The app is available at `http://localhost:8080`.

---

## Search syntax

Tags separated by spaces means AND. Everything else stacks on top:

| Syntax | Effect |
|---|---|
| `cat dog` | has both tags |
| `cat OR dog` | either one |
| `-blonde_hair` | exclude |
| `blue*` / `*hair*` | wildcards |
| `fav:true` | favorites only |
| `source:a1111` / `source:comfyui` / `source:none` | by metadata source |
| `folder:2024/january` | images in this folder or any subfolder |
| `folder:"my set 1"` | quote paths that contain spaces |
| `folderonly:2024/january` | only images directly in this folder, no subfolders |
| `width:>=1920` `height:<768` | dimensions |
| `date:2024-03-15` `date:>2024-01-01` `date:2024-01..2024-06` | dates |
| `cat:character` | any tag in that category |
| `character:cat` | tag "cat" in the character category |
| `missing:true` | files gone from disk |
| `animated:true` / `animated:false` | gif/mp4/webm |
| `tagged:true` / `tagged:false` | images with or without tags |
| `autotagged:true` / `autotagged:false` | images with or without auto-tags |
| `generated:abcd1234abcd` | same generation recipe (hash shown on the image page) |

Autocomplete is combination-aware: the count next to each suggestion is for the full query, and suggestions that would return zero results are hidden.

Sort options: newest, file size, random shuffle. Any query can be saved from the sidebar for one-click access later.


---

## Auto-tagger

You can use autotaggers to tag your images automatically.  
Instructions are available in the settings page to download support autotaggers.
You can also place other ONNX models in the `models/` volume. Each tagger lives in its own subfolder with two files:

- `model.onnx`, the weights
- `tags.csv` in WD14 format (`tag_id,name,category_id`), or `tags.txt` with one label per line (all labels are assigned to `general`)

Reload the settings page after adding a model, then enable it in **Settings → Auto-Tagger**. Multiple taggers can run at the same time, and each tag records which tagger produced it. Auto-tags can be removed per tagger or all at once without touching manual tags.

If no tagger is enabled, auto-tagging is simply disabled. The rest of the app works normally.
The model stays loaded in memory across back-to-back jobs and is unloaded automatically after 30 minutes without a tagger run. Tune the window via `tagger.idle_release_after_minutes` in the TOML; set it to `0` to release on every run.

### GPU (CUDA)

The default image is CPU-only (~210 MB). For GPU inference, switch to the `-cuda` image (~2.3 GB), pass the GPU into the container the usual way, then enable **Settings → Auto-Tagger → Use GPU (CUDA)** (or set `MONBOORU_TAGGER_USE_CUDA=true`).

The current mode (GPU or CPU) is shown as a badge in the Auto-Tagger section. The `-cuda` image also runs on CPU when the toggle is off, so switching between the two does not require a rebuild. Worker count is configurable in Settings (default 4); increase it on GPU if preprocessing becomes the bottleneck.

---

## Environment variables

All of these override the TOML config. Pattern: `MONBOORU_{SECTION}_{KEY}`.

| Variable | Overrides | Type |
|---|---|---|
| `MONBOORU_SERVER_BIND_ADDRESS` | `server.bind_address` | string |
| `MONBOORU_SERVER_BASE_URL` | `server.base_url` | string |
| `MONBOORU_PATHS_DATA_PATH` | `paths.data_path` | string |
| `MONBOORU_PATHS_MODEL_PATH` | `paths.model_path` | string |
| `MONBOORU_GALLERY_WATCH_ENABLED` | `gallery.watch_enabled` | bool |
| `MONBOORU_GALLERY_MAX_FILE_SIZE_MB` | `gallery.max_file_size_mb` | int |
| `MONBOORU_TAGGER_USE_CUDA` | `tagger.use_cuda` | bool |
| `MONBOORU_AUTH_ENABLE_PASSWORD` | `auth.enable_password` | bool |
| `MONBOORU_AUTH_PASSWORD_HASH` | `auth.password_hash` | string |
| `MONBOORU_AUTH_SESSION_LIFETIME_DAYS` | `auth.session_lifetime_days` | int |
| `MONBOORU_AUTH_API_TOKEN` | `auth.api_token` | string |
| `MONBOORU_UI_PAGE_SIZE` | `ui.page_size` | int |
| `MONBOORU_LOG_LEVEL` | `log.level` | `warn` / `info` / `debug` |

Per-tagger settings (enable flags, confidence thresholds, worker count) live in the Settings UI, not in env vars.

---

## Keyboard shortcuts

| Key | Where | Action |
|---|---|---|
| `s` | Anywhere | Focus search input |
| `t` | Image detail | Focus tag input |
| `f` | Image detail | Toggle favorite |
| `Delete` | Image detail | Delete image (advances to the next in the current search) |
| `Space` | Image detail | Play / pause the video |
| `h` / `l` | Gallery | Previous / next page |
| Arrows | Gallery | Navigate the grid |
| `Enter` | Gallery | Open focused image |
| `←` / `→` | Image detail | Previous / next image |
| `Escape` | Anywhere | Close dialog, blur input, or go back |

---

## REST API

Disabled by default. Generate a token in **Settings → Authentication** to enable it.

- HTML reference: `/api/v1/docs` (also linked in the footer)
- OpenAPI spec: `/api/v1/openapi.json`

Covers search, tag add/remove, upload, delete.

---

## Building without Docker

```bash
# CPU only, no auto-tagger
go build -o monbooru ./cmd/monbooru

# With auto-tagger (requires the ONNX Runtime shared library on the system)
CGO_ENABLED=1 go build -tags tagger -o monbooru ./cmd/monbooru

./monbooru -config /path/to/monbooru.toml
```

---

## Inotify limit (Docker)

If the watcher reports an inotify limit error (logged as `no space left on device` from `inotify_add_watch`), raise `fs.inotify.max_user_watches` on the host (not inside the container) and restart. If you see `too many open files`, raise `fs.inotify.max_user_instances`. Alternatively, disable the watcher in Settings and use the manual Sync button when adding new files.

---

## Warning

> **Intended for local network use.** This project is not designed for direct exposure to the public internet.
