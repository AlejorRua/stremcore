#!/usr/bin/env python3
"""Genera el indice de busqueda desde el catalogo JSON ya fusionado."""

import json
import os
import shutil


OUT = os.path.abspath(
    os.environ.get(
        "FLIX_MONITOR_EXPORT_ROOT",
        os.path.join(os.path.dirname(__file__), os.pardir),
    )
)
CHUNK = 2000

if OUT == os.path.abspath(os.sep):
    raise SystemExit("FLIX_MONITOR_EXPORT_ROOT no puede ser el directorio raiz")


def load_details(name):
    directory = os.path.join(OUT, name)
    details = []
    if not os.path.isdir(directory):
        return details
    for entry in os.scandir(directory):
        if not entry.is_file() or not entry.name.endswith(".json"):
            continue
        try:
            with open(entry.path, encoding="utf-8") as source:
                detail = json.load(source)
        except (OSError, json.JSONDecodeError):
            continue
        if isinstance(detail, dict) and detail.get("id") is not None:
            details.append(detail)
    return details


for name in ("movies", "series"):
    print(f"► Índice de búsqueda: {name}...")
    index = []
    for detail in load_details(name):
        index.append({
            "id": detail["id"],
            "t": detail.get("titulo", ""),
            "to": detail.get("titulo_orig", ""),
            "p": detail.get("poster", ""),
            "d": detail.get("release_date", ""),
            "g": detail.get("genres") or [],
            "r": detail.get("rating", ""),
        })

    index.sort(key=lambda item: (item["t"] or "").casefold())
    total_chunks = max(1, -(-len(index) // CHUNK))
    search_dir = os.path.join(OUT, "search", name)
    if os.path.isdir(search_dir):
        shutil.rmtree(search_dir)
    os.makedirs(search_dir, exist_ok=True)

    for i in range(total_chunks):
        chunk = index[i * CHUNK:(i + 1) * CHUNK]
        path = os.path.join(search_dir, f"{i + 1}.json")
        with open(path, "w", encoding="utf-8") as target:
            json.dump(
                {
                    "chunk": i + 1,
                    "total_chunks": total_chunks,
                    "total": len(index),
                    "data": chunk,
                },
                target,
                ensure_ascii=False,
                separators=(",", ":"),
            )

    print(f"  ✓ {len(index)} items en {total_chunks} chunks")

print(f"\n✅ Índices generados en {OUT}/search/")
