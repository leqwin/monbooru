# Migrating to Monbooru

Monbooru's import dialog (Settings → Galleries → Import) accepts two
foreign-format archives in addition to its own .db / .json / .zip
exports:

- A **Hydrus Network** export zipped on disk.
- A **Blombooru** full-backup zip downloaded from the Blombooru admin
  panel.

Both are translated server-side into a "light" import (images + tags
only) during import in monbooru. The compatibility layer is
tag and image-only.

This document covers how to produce the two zips. Once you have one,
drop it into the import dialog like any other archive: pick **Replace** or **Merge** on a gallery to import the images.

---

## Hydrus Network

1. In Hydrus, select the files you want to export. Right-click →
   **share** → **export** → **files…**.
2. In the export dialog, add a sidecar **.txt** with the **all known
   tags / display tags** preset. Each exported image now gets an
   adjacent `<filename>.txt` listing one tag per line. Tags already
   namespaced in Hydrus (e.g. `character:illya`) keep the
   `category:tag` shape, which Monbooru honours when ingesting.
3. Run the export. Hydrus drops everything into a chosen folder; the
   filenames are typically the file's SHA-256 (`<sha>.<ext>`).
4. Zip the export folder. Either layout works: the zip can have all
   files at the root, or wrapped in a single top-level subfolder
   (e.g. `hydrus_export/...`); Monbooru strips a shared parent prefix
   before reading.
5. Drop the zip into the import dialog.

What lands in Monbooru:

- Each `<sha>.<ext>` keeps its filename (and folder structure) under
  the new gallery root.
- When the basename is a 64-hex-char string, Monbooru reuses that as
  the recorded SHA-256; otherwise the actual hash is computed at
  ingest.
- Each sidecar's tags are attached to the image. Tokens with a
  `category:` prefix that matches a known Monbooru category land in
  that category; everything else lands in `general`. Lines starting
  with `#` and blank lines are skipped.

What is not preserved: Hydrus URLs, ratings, notes, file relationships,
and any per-tag namespace that Monbooru doesn't have a category for.

---

## Blombooru

1. Open the Blombooru admin panel → **Backup** → **Download full
   backup**. Blombooru ships you a `.zip` carrying:
   - `backup.json` - the manifest with one entry per media item
   - `tags.csv` - `name, category_id, …` rows
   - `media/<file>` - every image referenced by the manifest
2. Drop that zip into Monbooru's import dialog.

What lands in Monbooru:

- Each `media/<file>` is extracted into the new gallery root under the
  same filename.
- Per-image tags from `backup.json` are resolved via `tags.csv`.
  Monbooru maps Blombooru's category ids to its own:

  | Blombooru `category_id` | Monbooru category |
  |---|---|
  | 0 | general |
  | 1 | artist |
  | 3 | copyright |
  | 4 | character |
  | 5 | meta |
  | (anything else / not in tags.csv) | general |

- Blombooru's `hash` field is reused as the SHA-256 only when it is a
  64-hex-char string. Older entries that ship an MD5 are silently
  re-hashed on ingest.

What is not preserved: albums, ratings, parent/child relations, mime
types, durations, or any custom metadata. 