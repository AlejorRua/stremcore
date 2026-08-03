#!/usr/bin/env python3
"""Regenera pages/ y meta.json usando movies/ y series/ como fuente."""

import json
import os
import re
import shutil


OUT = os.path.abspath(
    os.environ.get(
        "FLIX_MONITOR_EXPORT_ROOT",
        os.path.join(os.path.dirname(__file__), os.pardir),
    )
)
PER_PAGE = 40

GENRE_LABELS = {
    "accion": "Acción",
    "action-adventure": "Action & Adventure",
    "animacion": "Animación",
    "aventura": "Aventura",
    "belica": "Bélica",
    "ciencia-ficcion": "Ciencia ficción",
    "comedia": "Comedia",
    "crimen": "Crimen",
    "documental": "Documental",
    "drama": "Drama",
    "familia": "Familia",
    "fantasia": "Fantasía",
    "historia": "Historia",
    "kids": "Kids",
    "misterio": "Misterio",
    "musica": "Música",
    "pelicula-de-tv": "Película de TV",
    "reality": "Reality",
    "romance": "Romance",
    "sci-fi-fantasy": "Sci-Fi & Fantasy",
    "soap": "Soap",
    "suspense": "Suspense",
    "talk": "Talk",
    "terror": "Terror",
    "war-politics": "War & Politics",
    "western": "Western",
}

if OUT == os.path.abspath(os.sep):
    raise SystemExit("FLIX_MONITOR_EXPORT_ROOT no puede ser el directorio raiz")


def write_json(path, data):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    temporary = path + ".tmp"
    with open(temporary, "w", encoding="utf-8") as target:
        json.dump(data, target, ensure_ascii=False, separators=(",", ":"))
        target.write("\n")
    os.replace(temporary, path)


def load_cards(name):
    directory = os.path.join(OUT, name)
    cards = []
    if not os.path.isdir(directory):
        return cards
    for entry in os.scandir(directory):
        if not entry.is_file() or not entry.name.endswith(".json"):
            continue
        try:
            with open(entry.path, encoding="utf-8") as source:
                detail = json.load(source)
        except (OSError, json.JSONDecodeError):
            continue
        if not isinstance(detail, dict) or detail.get("id") is None:
            continue
        card = {
            "id": detail["id"],
            "titulo": detail.get("titulo", ""),
            "titulo_orig": detail.get("titulo_orig", ""),
            "poster": detail.get("poster", ""),
            "release_date": detail.get("release_date", ""),
            "genres": detail.get("genres") or [],
            "rating": detail.get("rating", ""),
        }
        if detail.get("added_at"):
            card["added_at"] = detail["added_at"]
        cards.append(card)
    cards.sort(
        key=lambda item: (
            item.get("added_at", ""),
            item["release_date"],
            item["id"],
        ),
        reverse=True,
    )
    return cards


def save_pages(base, cards):
    total = len(cards)
    total_pages = max(1, -(-total // PER_PAGE))
    for page in range(1, total_pages + 1):
        start = (page - 1) * PER_PAGE
        write_json(
            os.path.join(base, f"{page}.json"),
            {
                "page": page,
                "total_pages": total_pages,
                "total": total,
                "data": cards[start:start + PER_PAGE],
            },
        )


def genre_buckets(cards):
    buckets = {}
    for card in cards:
        for genre in card["genres"]:
            buckets.setdefault(genre, []).append(card)
    return buckets


pages = os.path.join(OUT, "pages")
if os.path.isdir(pages):
    shutil.rmtree(pages)

movies = load_cards("movies")
series = load_cards("series")
movie_genres = genre_buckets(movies)
series_genres = genre_buckets(series)

save_pages(os.path.join(pages, "movies", "all"), movies)
for genre, cards in movie_genres.items():
    save_pages(os.path.join(pages, "movies", genre), cards)

save_pages(os.path.join(pages, "series", "all"), series)
for genre, cards in series_genres.items():
    save_pages(os.path.join(pages, "series", genre), cards)

total_seasons = sum(
    1
    for root, _, files in os.walk(os.path.join(OUT, "series"))
    for name in files
    if root != os.path.join(OUT, "series") and re.fullmatch(r"t\d+\.json", name)
)

write_json(os.path.join(OUT, "meta.json"), {
    "total_movies": len(movies),
    "total_series": len(series),
    "total_seasons": total_seasons,
    "per_page": PER_PAGE,
    "movie_genres": [
        {"slug": slug, "label": GENRE_LABELS.get(slug, slug), "total": len(cards)}
        for slug, cards in sorted(movie_genres.items())
    ],
    "series_genres": [
        {"slug": slug, "label": GENRE_LABELS.get(slug, slug), "total": len(cards)}
        for slug, cards in sorted(series_genres.items())
    ],
})

print(f"✅ Índices regenerados: {len(movies)} películas, {len(series)} series, {total_seasons} temporadas")
