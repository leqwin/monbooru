# Monbooru

Your own private, lightweight and fast booru.  
Designed for organizing your local media collection, including AI-generated images (Stable Diffusion, ComfyUI, A1111/Forge). Supports ONNX models for local auto-tagging of your collection (WD14, JoyTag...). The app itself never touches the internet; online features (downloading from boorus or reverse image lookup) are handled by monloader, an optional companion.

<table>
  <tr>
    <td><img src="/.github/assets/gallery.webp" width="400"/></td>
    <td><img src="/.github/assets/image.webp" width="400"/></td>
  </tr>
  <tr>
    <td><img src="/.github/assets/sd.webp" width="400"/></td>
    <td><img src="/.github/assets/tags.webp" width="400"/></td>
  </tr>
  <tr>
    <td><img src="/.github/assets/inbox.webp" width="400"/></td>
    <td><img src="/.github/assets/compare.webp" width="400"/></td>
  </tr>
</table>

---

## Features

- **Tag-based gallery** with folder tree, favorites, saved searches, collections, related-image suggestions, and rating tags (general / sensitive / questionable / explicit) with a SFW ceiling
- **Stable Diffusion metadata** from A1111/Forge and ComfyUI: prompts, models, seeds, full workflows
- **Local ONNX auto-tagging** (WD14 SwinV2, animetimm EVA02, JoyTag, Camie v2, or any compatible model) on CPU or GPU
- Images, video, animated GIFs, plus CBZ/ZIP archives browsed as a single object with a built-in manga reader; animated hover previews on the grid
- **Search** with wildcards, OR, exclusions, plus filters on folder, date, size, dimensions, category, generation recipe, and more
- **Keyboard-driven UI**: contextual shortcuts on every page, from grid navigation to batch actions; press `?` in-app for the full map
- **Tag management**: merge duplicates, create aliases, implications, custom categories
- **Image relations**: declared duplicates, alternates, version chains, derivatives; near-duplicate detection with a side-by-side review session and duplicate cleanup tools
- **Sources and provenance**: multiple sources per image, artist commentary and note boxes, plus your own per-image notes and annotations
- **Batch operations** on a selection or a whole search: delete, move, add/remove tags, auto-tag, set collection or source, toggle inbox/favorite, and more
- **File watcher** that catches new, moved, and deleted files within seconds; multi-file browser upload; SHA-256 dedup so the same file under two paths is one image
- **Inbox workflow** : new images land in the inbox for review
- **Multiple galleries** in one instance, each with its own filesystem and database; per-gallery export/import; import data from supported booru applications
- **REST API** for third-party integrations, with scoped bearer tokens, an OpenAPI spec, and built-in docs
- **Plugins and themes support**: drop it in the plugins (or themes) folder and monbooru launches and supervises it for you
- **Monloader integration (optional)** ([monloader](https://github.com/monbooru/monloader)) pulls images and tags from supported boorus and galleries, and reverse-looks up your own images against boorus, similarity services (IQDB, SauceNAO), and the Hydrus Public Tag Repository to backfill tags and sources; when a matched post serves a better file than your local copy, one click replaces it in place; the PTR connection can also pull tag aliases and implications into your catalog
- **Optional password login**

---

## Technical overview

Monbooru compiles to a single self-contained binary (~20 MB, web assets embedded) with a handful of Go dependencies. Storage is one SQLite file per gallery (no database server, no cache layer, nothing else to run). Your collection stays ordinary files and folders on disk, the database and thumbnails live in a separate data directory, so removing monbooru leaves your images exactly where they were. The UI is server-rendered with no JS framework, no frontend build step, ~300 KB of static assets total. Memory scales with the library: a fresh instance starts around 25 MB of RAM, a small library idles around 50 MB, and a million-image library settles around 500 MB, flat over time, with typical pages still rendering in a few milliseconds at that scale. The heavy features stay out of the core process: ONNX auto-tagging is disabled by default and runs in a separate worker that unloads itself when idle, and everything online (downloading, reverse lookup) lives in the optional monloader companion, so monbooru itself never touches the internet. The only external tool is ffmpeg, optional, for video and animated previews.

---

## Related applications

Monbooru is a self-hosted offline image organizer. Optional companions applications can be used for online features :

```mermaid
flowchart LR
    web["`- Any booru or gallery supported by gallery-dl
- Direct image URL`"]
    ptr["`Hydrus
Public Tag Repository`"]
    sender["`**monsender**
browser extension`"]
    loader["`**monloader**
downloader`"]
    booru(["`**monbooru**
Your self-hosted booru`"])

    web -->|browse| sender
    sender -->|send URL| loader
    web -->|paste URL| loader
    loader -->|push images, tags, source| booru

    booru -.->|lookup by hash| loader
    loader -.->|reverse lookup: md5 + similarity| web
    ptr <-.->|"`sync: sha256 tags + aliases + implications
contribute back`"| loader

    classDef hub  fill:#9d2235,stroke:#ef8f99,stroke-width:3px,color:#ffffff;
    classDef tool fill:#1a1818,stroke:#9d2235,stroke-width:1.5px,color:#e8e4e2;
    classDef src  fill:#1a1818,stroke:#a09898,stroke-width:1px,color:#a09898;

    class booru hub;
    class sender,loader tool;
    class web,ptr src;
```
- **monbooru** : this application; organizes, tags, and serves your collection. 
- **[monloader](https://github.com/monbooru/monloader)** : downloader; fetches files and per-post metadata (via gallery-dl) and pushes them into a monbooru gallery over the REST API; also answers monbooru's reverse lookups (boorus, IQDB/SauceNAO, Hydrus PTR) and source refetches.
- **[monsender](https://github.com/monbooru/monsender)** : browser extension; sends the URL of the page you're currently browsing to monloader.
- **[monbooru-plugins](https://github.com/monbooru/monbooru-plugins)** : community registry for third-party plugins and themes.


---

## Quick start (Docker)

Edit the volume paths in [`docker/docker-compose.yml`](docker/docker-compose.yml), then `docker compose up -d`. The app is available at `http://localhost:8080`.

See the monbooru documentation for help. In-app, type `system:` in the search bar for the syntax cheat-sheet and press `?` for the keyboard shortcuts; the footer's `help` link opens the documentation.

---

## Warning

> **Intended for local network use.** This project is not designed for direct exposure to the public internet.

---

## Acknowledgements

This section covers monbooru and its companion repos ([monloader](https://github.com/monbooru/monloader), [monsender](https://github.com/monbooru/monsender), [mondocs](https://github.com/monbooru/mondocs), [monbooru-plugins](https://github.com/monbooru/monbooru-plugins)).

Thanks to [@gary-host-laptop](https://github.com/gary-host-laptop) and [@CeareDelafont](https://github.com/CeareDelafont) for sustained contributions over time. Every shipped contribution is credited in the release that ships it; see [CONTRIBUTING](CONTRIBUTING.md).

This project is built on the work of others:

- [htmx](https://htmx.org/) powers the server-rendered UI.
- [SQLite](https://sqlite.org/), through the pure-Go [modernc.org/sqlite](https://gitlab.com/cznic/sqlite) driver, stores each gallery.
- [ONNX Runtime](https://onnxruntime.ai/), through [onnxruntime_go](https://github.com/yalue/onnxruntime_go), runs the auto-tagger. The models in the bundled catalog are the work of [SmilingWolf](https://huggingface.co/SmilingWolf) (WD SwinV2 v3), [animetimm](https://huggingface.co/animetimm) (EVA02), [fancyfeast](https://huggingface.co/fancyfeast) (JoyTag), and [Camais03](https://huggingface.co/Camais03) (Camie Tagger v2).
- [ffmpeg](https://ffmpeg.org/) decodes video for thumbnails and hover previews.
- monloader is mostly a wrapper for [gallery-dl](https://github.com/mikf/gallery-dl), which does the actual scraping. Reverse lookups by image similarity are answered by [IQDB](https://iqdb.org/) and [SauceNAO](https://saucenao.com/); md5 lookups by the boorus themselves. The PTR tag lookup syncs against the [Hydrus Public Tag Repository](https://hydrusnetwork.github.io/hydrus/PTR.html) (PTR) via [Hydrus Network](https://github.com/hydrusnetwork/hydrus)'s repository protocol; the tags, aliases, and implications it serves are the work of the hydrus community.
- monsender's in-page image detection uses code from [ushiro](https://github.com/gary-host-laptop/ushiro) by gary-host-laptop and [behind!](https://github.com/kubuzetto/behind) by kubuzetto, originally under MPL-2.0.
- The mondocs site is built with [Hugo](https://gohugo.io/).

The full Go dependency lists are in each repo's `go.mod`.

The sample images in the screenshots, here, in the companion repos, and in the documentation, were downloaded from Danbooru and are the work of their respective artists, who retain all rights. They appear only to demonstrate the app; the repositories' license doesn't apply to them.
