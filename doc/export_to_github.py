#!/usr/bin/env python3
"""
Exporta poseidon_catalog + poseidon_store → estructura JSON para GitHub + jsDelivr
"""

import os, json, re, shutil, unicodedata, psycopg2, psycopg2.extras

# ── Config ────────────────────────────────────────────────────────────────────
DB = os.environ.get("DATABASE_URL", "").strip()
OUT = os.path.abspath(
    os.environ.get(
        "FLIX_MONITOR_EXPORT_ROOT",
        os.path.join(os.path.dirname(__file__), os.pardir),
    )
)
PER_PAGE = 40

if not DB:
    raise SystemExit("Falta DATABASE_URL; configura la conexion PostgreSQL antes de exportar")

if OUT == os.path.abspath(os.sep):
    raise SystemExit("FLIX_MONITOR_EXPORT_ROOT no puede ser el directorio raiz")

GENRE_NAMES = {
    26:    "accion",
    9306:  "action-adventure",
    51:    "animacion",
    25:    "aventura",
    158:   "belica",
    27:    "ciencia-ficcion",
    192:   "comedia",
    136:   "crimen",
    23209: "documental",
    157:   "drama",
    52:    "familia",
    86:    "fantasia",
    404:   "historia",
    9307:  "kids",
    249:   "misterio",
    307:   "musica",
    9496:  "pelicula-de-tv",
    15487: "reality",
    215:   "romance",
    9334:  "sci-fi-fantasy",
    23714: "soap",
    87:    "suspense",
    48809: "talk",
    422:   "terror",
    9582:  "war-politics",
    1594:  "western",
}

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

# ── Helpers ───────────────────────────────────────────────────────────────────
def write_json(path, data):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w", encoding="utf-8") as f:
        json.dump(data, f, ensure_ascii=False, separators=(",", ":"))

def clean_generated_output():
    """Regenera indices, pero conserva detalles y proveedores ya publicados."""
    generated_dirs = ("pages", "search")
    for name in generated_dirs:
        path = os.path.join(OUT, name)
        if os.path.isdir(path):
            shutil.rmtree(path)

    meta_path = os.path.join(OUT, "meta.json")
    if os.path.exists(meta_path):
        os.remove(meta_path)

def read_json(path):
    try:
        with open(path, encoding="utf-8") as f:
            data = json.load(f)
        return data if isinstance(data, dict) else None
    except (OSError, json.JSONDecodeError):
        return None

def load_top_level_details(name):
    directory = os.path.join(OUT, name)
    details = {}
    if not os.path.isdir(directory):
        return details
    for entry in os.scandir(directory):
        if not entry.is_file() or not entry.name.endswith(".json"):
            continue
        detail = read_json(entry.path)
        if detail and detail.get("id") is not None:
            details[str(detail["id"])] = detail
    return details

def server_key(server):
    if not isinstance(server, dict):
        return ""
    return str(server.get("url") or server.get("player_url") or "").strip()

def merge_servers(existing, fresh):
    merged = []
    seen = set()
    for server in list(existing or []) + list(fresh or []):
        if not isinstance(server, dict):
            continue
        key = server_key(server)
        if not key or key in seen:
            continue
        seen.add(key)
        item = dict(server)
        item["id"] = len(merged) + 1
        merged.append(item)
    return merged

def has_value(value):
    return value not in (None, "", [], {})

def merge_detail(existing, fresh):
    """Prioriza datos nuevos sin perder campos ni servidores ya publicados."""
    if not existing:
        return fresh
    merged = dict(existing)
    for key, value in fresh.items():
        if key == "servidores":
            continue
        if has_value(value) or key not in merged:
            merged[key] = value
    if "servidores" in existing or "servidores" in fresh:
        merged["servidores"] = merge_servers(
            existing.get("servidores"), fresh.get("servidores")
        )
    return merged

def merge_season(existing, fresh):
    if not existing:
        return fresh
    merged = merge_detail(existing, fresh)
    episodes = {}
    for episode in existing.get("episodios") or []:
        if isinstance(episode, dict) and episode.get("numero", episode.get("number")) is not None:
            number = episode.get("numero", episode.get("number"))
            episodes[str(number)] = dict(episode)
    for episode in fresh.get("episodios") or []:
        if not isinstance(episode, dict):
            continue
        number = episode.get("numero", episode.get("number"))
        key = str(number)
        episodes[key] = merge_detail(episodes.get(key), episode)
    merged["episodios"] = sorted(
        episodes.values(),
        key=lambda episode: int(episode.get("numero", episode.get("number", 0)) or 0),
    )
    return merged

def card_from_detail(detail):
    return {
        "id": detail.get("id"),
        "titulo": detail.get("titulo", ""),
        "titulo_orig": detail.get("titulo_orig", ""),
        "poster": detail.get("poster", ""),
        "release_date": detail.get("release_date", ""),
        "genres": detail.get("genres") or [],
        "rating": detail.get("rating", ""),
    }

def add_card(card, index, buckets):
    if card.get("id") is None or not card.get("titulo"):
        return
    index.append(card)
    for genre in card.get("genres") or []:
        buckets.setdefault(genre, []).append(card)

def genre_ids_to_slugs(genre_ids):
    if not genre_ids:
        return []
    return [GENRE_NAMES[g] for g in genre_ids if g in GENRE_NAMES]

def has_servers_list(item):
    servers = item.get("servidores") if isinstance(item, dict) else None
    return isinstance(servers, list) and len(servers) > 0

def season_with_playable_episodes(data):
    if not isinstance(data, dict):
        return None

    episodes = data.get("episodios") or []
    if not isinstance(episodes, list):
        return None

    playable = [ep for ep in episodes if has_servers_list(ep)]
    if not playable:
        return None

    clean_data = dict(data)
    clean_data["episodios"] = playable
    return clean_data

def save_pages(base_dir, items, total_label=None):
    total = len(items)
    total_pages = max(1, -(-total // PER_PAGE))  # ceil division
    for i in range(total_pages):
        chunk = items[i * PER_PAGE : (i + 1) * PER_PAGE]
        write_json(
            f"{base_dir}/{i + 1}.json",
            {"page": i + 1, "total_pages": total_pages, "total": total, "data": chunk},
        )
    return total_pages

def progress(label, done, total):
    pct = done * 100 // total
    bar = "█" * (pct // 5) + "░" * (20 - pct // 5)
    print(f"\r  {label}: [{bar}] {done}/{total}", end="", flush=True)

def env_true(name):
    return os.environ.get(name, "").strip().lower() in {"1", "true", "yes", "si", "sí", "on"}

def validate_catalog_size(conn):
    """Evita reemplazar el catalogo por una exportacion accidentalmente vacia."""
    meta_path = os.path.join(OUT, "meta.json")
    if env_true("FLIX_EXPORT_ALLOW_SHRINK") or not os.path.isfile(meta_path):
        return

    try:
        with open(meta_path, encoding="utf-8") as f:
            previous = json.load(f)
        min_ratio = float(os.environ.get("FLIX_EXPORT_MIN_RATIO", "0.80"))
    except (OSError, ValueError, TypeError, json.JSONDecodeError) as exc:
        raise SystemExit(f"No se pudo validar el catalogo actual: {exc}") from exc

    min_ratio = max(0.0, min(1.0, min_ratio))
    check = conn.cursor()
    check.execute("""
        SELECT COUNT(*)
        FROM poseidon_catalog c
        JOIN poseidon_store s ON s.key = 'poseidon:movie:' || c.id::text
        WHERE c.tipo = 'm'
          AND jsonb_typeof((s.value::jsonb)->'servidores') = 'array'
          AND jsonb_array_length((s.value::jsonb)->'servidores') > 0
    """)
    current_movies = check.fetchone()[0]
    check.execute("""
        SELECT COUNT(*)
        FROM poseidon_catalog c
        JOIN (
            SELECT DISTINCT split_part(key, ':', 3) AS serie_id
            FROM poseidon_store
            WHERE key ~ '^poseidon:season:[0-9]+:[0-9]+$'
              AND EXISTS (
                  SELECT 1
                  FROM jsonb_array_elements(
                      CASE
                        WHEN jsonb_typeof((value::jsonb)->'episodios') = 'array'
                        THEN (value::jsonb)->'episodios'
                        ELSE '[]'::jsonb
                      END
                  ) ep
                  WHERE jsonb_typeof(ep->'servidores') = 'array'
                    AND jsonb_array_length(ep->'servidores') > 0
              )
        ) playable ON playable.serie_id = c.id::text
        WHERE c.tipo = 's'
    """)
    current_series = check.fetchone()[0]
    check.close()

    counts = (
        ("peliculas", int(previous.get("total_movies", 0)), current_movies),
        ("series", int(previous.get("total_series", 0)), current_series),
    )
    failures = [
        f"{name}: antes={old}, ahora={new}"
        for name, old, new in counts
        if old > 0 and new < old * min_ratio
    ]
    if failures:
        detail = "; ".join(failures)
        raise SystemExit(
            f"Exportacion cancelada para proteger el catalogo ({detail}). "
            "Revisa DATABASE_URL o usa FLIX_EXPORT_ALLOW_SHRINK=true si la reduccion es intencional."
        )

# ── DB connection ─────────────────────────────────────────────────────────────
conn = psycopg2.connect(DB)
conn.autocommit = True

print("Conectado a PostgreSQL ✓\n")
validate_catalog_size(conn)
existing_movies = load_top_level_details("movies")
existing_series = load_top_level_details("series")
print("Regenerando indices y conservando detalles publicados...")
clean_generated_output()
print("  ✓ Catalogo base cargado desde GitHub\n")

# ══════════════════════════════════════════════════════════════════════════════
# 1. PELÍCULAS
# ══════════════════════════════════════════════════════════════════════════════
print("► Exportando películas...")

cur = conn.cursor(cursor_factory=psycopg2.extras.RealDictCursor)
cur.execute("""
    SELECT
        c.id, c.titulo, c.titulo_orig, c.poster_url, c.release_date, c.genres,
        c.data,
        s.value as store
    FROM poseidon_catalog c
    JOIN poseidon_store s ON s.key = 'poseidon:movie:' || c.id::text
    WHERE c.tipo = 'm'
      AND CASE
            WHEN jsonb_typeof((s.value::jsonb)->'servidores') = 'array'
            THEN jsonb_array_length((s.value::jsonb)->'servidores')
            ELSE 0
          END > 0
    ORDER BY c.release_date DESC
""")
rows = cur.fetchall()
db_movies = len(rows)

# Index card (lightweight, for list pages)
movies_index = []
# Category buckets
movies_by_genre = {}

for i, row in enumerate(rows):
    progress("películas", i + 1, db_movies)

    d = row["data"] or {}
    store = row["store"] or {}
    genres = genre_ids_to_slugs(row["genres"] or [])

    # ── Card (used in list pages) ──────────────────────────────────────────
    card = {
        "id":           row["id"],
        "titulo":       row["titulo"],
        "titulo_orig":  row["titulo_orig"] or "",
        "poster":       row["poster_url"] or d.get("images", {}).get("poster", ""),
        "release_date": row["release_date"] or "",
        "genres":       genres,
        "rating":       d.get("rating", ""),
    }
    # ── Detail file ────────────────────────────────────────────────────────
    detail = {
        **card,
        "backdrop":  d.get("images", {}).get("backdrop", ""),
        "overview":  d.get("overview", ""),
        "trailer":   d.get("trailer", ""),
        "runtime":   d.get("runtime", ""),
        "slug":      d.get("slug", ""),
        "servidores": store.get("servidores", []),
    }
    detail = merge_detail(existing_movies.pop(str(row["id"]), None), detail)
    card = card_from_detail(detail)
    add_card(card, movies_index, movies_by_genre)
    write_json(f"{OUT}/movies/{row['id']}.json", detail)

for detail in existing_movies.values():
    add_card(card_from_detail(detail), movies_index, movies_by_genre)

movies_index.sort(key=lambda item: item.get("release_date", ""), reverse=True)
for items in movies_by_genre.values():
    items.sort(key=lambda item: item.get("release_date", ""), reverse=True)
total_movies = len(movies_index)

print()

# ── Páginas generales ──────────────────────────────────────────────────────
print("  Generando páginas (all)...")
save_pages(f"{OUT}/pages/movies/all", movies_index)

# ── Páginas por género ─────────────────────────────────────────────────────
print("  Generando páginas por género...")
for genre, items in movies_by_genre.items():
    save_pages(f"{OUT}/pages/movies/{genre}", items)

print(f"  ✓ {total_movies} películas exportadas\n")

# ══════════════════════════════════════════════════════════════════════════════
# 2. SERIES
# ══════════════════════════════════════════════════════════════════════════════
print("► Exportando series...")

cur.execute("""
    SELECT id, titulo, titulo_orig, poster_url, release_date, genres, data
    FROM poseidon_catalog c
    JOIN (
        SELECT DISTINCT split_part(key, ':', 3) AS serie_id
        FROM poseidon_store
        WHERE key ~ '^poseidon:season:[0-9]+:[0-9]+$'
          AND EXISTS (
              SELECT 1
              FROM jsonb_array_elements(
                  CASE
                    WHEN jsonb_typeof((value::jsonb)->'episodios') = 'array'
                    THEN (value::jsonb)->'episodios'
                    ELSE '[]'::jsonb
                  END
              ) ep
              WHERE CASE
                    WHEN jsonb_typeof(ep->'servidores') = 'array'
                    THEN jsonb_array_length(ep->'servidores')
                    ELSE 0
                  END > 0
          )
    ) playable ON playable.serie_id = c.id::text
    WHERE c.tipo = 's'
    ORDER BY c.release_date DESC
""")
series_rows = cur.fetchall()
db_series = len(series_rows)
valid_series_ids = {str(row["id"]) for row in series_rows}

series_index = []
series_by_genre = {}

for i, row in enumerate(series_rows):
    progress("series", i + 1, db_series)

    d = row["data"] or {}
    genres = genre_ids_to_slugs(row["genres"] or [])

    card = {
        "id":           row["id"],
        "titulo":       row["titulo"],
        "titulo_orig":  row["titulo_orig"] or "",
        "poster":       row["poster_url"] or d.get("images", {}).get("poster", ""),
        "release_date": row["release_date"] or "",
        "genres":       genres,
        "rating":       d.get("rating", ""),
    }
    detail = {
        **card,
        "backdrop":  d.get("images", {}).get("backdrop", ""),
        "overview":  d.get("overview", ""),
        "trailer":   d.get("trailer", ""),
        "slug":      d.get("slug", ""),
    }
    detail = merge_detail(existing_series.pop(str(row["id"]), None), detail)
    card = card_from_detail(detail)
    add_card(card, series_index, series_by_genre)
    write_json(f"{OUT}/series/{row['id']}.json", detail)

for detail in existing_series.values():
    valid_series_ids.add(str(detail.get("id")))
    add_card(card_from_detail(detail), series_index, series_by_genre)

series_index.sort(key=lambda item: item.get("release_date", ""), reverse=True)
for items in series_by_genre.values():
    items.sort(key=lambda item: item.get("release_date", ""), reverse=True)
total_series = len(series_index)

print()

print("  Generando páginas (all)...")
save_pages(f"{OUT}/pages/series/all", series_index)

print("  Generando páginas por género...")
for genre, items in series_by_genre.items():
    save_pages(f"{OUT}/pages/series/{genre}", items)

print(f"  ✓ {total_series} series exportadas\n")

# ══════════════════════════════════════════════════════════════════════════════
# 3. TEMPORADAS (episodios + servidores)
# ══════════════════════════════════════════════════════════════════════════════
print("► Exportando temporadas...")

cur.execute("""
    SELECT key, value
    FROM poseidon_store
    WHERE key LIKE 'poseidon:season:%'
""")
season_rows = cur.fetchall()
total_seasons_seen = len(season_rows)
total_seasons = 0

for i, row in enumerate(season_rows):
    progress("temporadas", i + 1, total_seasons_seen)
    # key = "poseidon:season:{serie_id}:{n}"
    parts = row["key"].split(":")
    if len(parts) < 4:
        continue
    serie_id = parts[2]
    if serie_id not in valid_series_ids:
        continue
    season_num = parts[3]
    data = season_with_playable_episodes(row["value"])
    if not data:
        continue
    season_path = f"{OUT}/series/{serie_id}/t{season_num}.json"
    data = merge_season(read_json(season_path), data)
    write_json(season_path, data)

total_seasons = sum(
    1
    for root, _, files in os.walk(os.path.join(OUT, "series"))
    for name in files
    if root != os.path.join(OUT, "series") and re.fullmatch(r"t\d+\.json", name)
)

print(f"\n  ✓ {total_seasons} temporadas exportadas\n")

# ══════════════════════════════════════════════════════════════════════════════
# 4. META
# ══════════════════════════════════════════════════════════════════════════════
print("► Escribiendo meta.json...")

movie_genres_list  = [{"slug": k, "label": GENRE_LABELS.get(k, k), "total": len(v)} for k, v in sorted(movies_by_genre.items())]
series_genres_list = [{"slug": k, "label": GENRE_LABELS.get(k, k), "total": len(v)} for k, v in sorted(series_by_genre.items())]

write_json(f"{OUT}/meta.json", {
    "total_movies":  total_movies,
    "total_series":  total_series,
    "total_seasons": total_seasons,
    "per_page":      PER_PAGE,
    "movie_genres":  movie_genres_list,
    "series_genres": series_genres_list,
})

# ══════════════════════════════════════════════════════════════════════════════
print("\n✅ Exportación completa")
print(f"   Directorio: {OUT}")
print(f"   Películas:  {total_movies}")
print(f"   Series:     {total_series}")
print(f"   Temporadas: {total_seasons}")

cur.close()
conn.close()
