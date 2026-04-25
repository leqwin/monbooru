#!/usr/bin/env python3
"""
Export a Blombooru instance as a Monbooru "light" ZIP (tags.json + gallery/).

Mount this file into the blombooru web container and run it there (for instance in /scripts/):

    docker compose exec web python /scripts/blombooru-export.py \
        /scripts/blombooru-export.zip

Then in Monbooru: Settings -> Galleries -> Import -> upload the .zip.
"""
import hashlib
import json
import os
import sys
import zipfile
from pathlib import Path

import psycopg2

MEDIA_ROOT = Path(os.environ.get("BLOMBOORU_MEDIA", "/app/media"))


def db_conn():
    return psycopg2.connect(
        host=os.environ.get("POSTGRES_HOST", "db"),
        port=int(os.environ.get("POSTGRES_PORT_INTERNAL", 5432)),
        dbname=os.environ["POSTGRES_DB"],
        user=os.environ["POSTGRES_USER"],
        password=os.environ["POSTGRES_PASSWORD"],
    )


def sha256_of(path: Path) -> str:
    h = hashlib.sha256()
    with path.open("rb") as f:
        for chunk in iter(lambda: f.read(1 << 20), b""):
            h.update(chunk)
    return h.hexdigest()


def main(out_path: str) -> int:
    cur = db_conn().cursor()
    # t.category is a Postgres enum; psycopg2 only auto-parses arrays of
    # known base types, so an enum[] comes back as the raw literal string
    # "{general,artist,...}" and zip(names, cats) iterates it char by
    # char. Cast each element to text so the array is returned as a proper
    # Python list of category names.
    cur.execute("""
        SELECT m.id, m.path, m.filename,
               COALESCE(array_agg(t.name        ORDER BY t.id) FILTER (WHERE t.id IS NOT NULL), '{}'),
               COALESCE(array_agg(t.category::text ORDER BY t.id) FILTER (WHERE t.id IS NOT NULL), '{}')
        FROM blombooru_media m
        LEFT JOIN blombooru_media_tags mt ON mt.media_id = m.id
        LEFT JOIN blombooru_tags t        ON t.id        = mt.tag_id
        GROUP BY m.id
        ORDER BY m.id
    """)

    images = []
    with zipfile.ZipFile(out_path, "w", zipfile.ZIP_DEFLATED) as zf:
        for media_id, rel_path, filename, names, cats in cur:
            src = (MEDIA_ROOT.parent / rel_path).resolve()
            if not src.is_file():
                print(f"skip media id={media_id}: {src} missing", file=sys.stderr)
                continue
            tags = sorted(
                n if c == "general" else f"{c}:{n}"
                for n, c in zip(names, cats)
            )
            zf.write(src, f"gallery/{filename}")
            images.append({
                "sha256": sha256_of(src),
                "path": filename,
                "tags": tags,
            })

        zf.writestr("tags.json", json.dumps(
            {"version": 1, "images": images}, ensure_ascii=False
        ))

    print(f"wrote {out_path}: {len(images)} images", file=sys.stderr)
    return 0


if __name__ == "__main__":
    if len(sys.argv) != 2:
        print("usage: blombooru-export.py <out.zip>", file=sys.stderr)
        sys.exit(2)
    sys.exit(main(sys.argv[1]))
