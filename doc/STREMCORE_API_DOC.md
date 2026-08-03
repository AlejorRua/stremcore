# StremCore API — Documentación Completa

**Backend:** GitHub + jsDelivr CDN  
**Repo:** `https://github.com/jexiptv07/stremcore`  
**Base URL:** `https://cdn.jsdelivr.net/gh/jexiptv07/stremcore@main`

---

## Archivos del proyecto

| Archivo | Descripción |
|---|---|
| `/home/localhost/Documents/export_to_github.py` | Exportación inicial completa desde PostgreSQL |
| `/home/localhost/Documents/generate_search_index.py` | Genera/actualiza índice de búsqueda |
| `/home/localhost/Documents/stremcore-api.js` | Cliente JS listo para usar en tu app |
| `/home/localhost/Documents/STREMCORE_API_DOC.md` | Esta documentación |

---

## Actualizar el catálogo

Cuando agregues nuevas películas o series a la base de datos:

```bash
# 1. Re-exportar todo
python3 /home/localhost/Documents/export_to_github.py

# 2. Regenerar índice de búsqueda
python3 /home/localhost/Documents/generate_search_index.py

# 3. Pushear a GitHub
cd /home/localhost/Documents/github_export
git add .
git commit -m "update catalog $(date +%Y-%m-%d)"
git push
```

---

## Estructura de archivos en GitHub

```
stremcore/
├── meta.json                        ← estadísticas + géneros
├── movies/
│   └── {id}.json                    ← detalle película + servidores
├── series/
│   ├── {id}.json                    ← detalle serie
│   └── {id}/
│       ├── t1.json                  ← temporada 1 (episodios + servidores)
│       ├── t2.json                  ← temporada 2
│       └── t{n}.json
├── pages/
│   ├── movies/
│   │   ├── all/
│   │   │   ├── 1.json               ← página 1 (40 películas)
│   │   │   └── {n}.json
│   │   ├── accion/
│   │   │   └── {n}.json
│   │   ├── drama/
│   │   │   └── {n}.json
│   │   └── {genero}/
│   │       └── {n}.json
│   └── series/
│       ├── all/
│       │   └── {n}.json
│       └── {genero}/
│           └── {n}.json
└── search/
    ├── movies/
    │   ├── 1.json                   ← chunk 1 de 11 (2000 items)
    │   └── {n}.json
    └── series/
        ├── 1.json                   ← chunk 1 de 6 (2000 items)
        └── {n}.json
```

---

## Endpoints

### BASE URL
```
https://cdn.jsdelivr.net/gh/jexiptv07/stremcore@main
```

---

### 1. META

#### `GET /meta.json`
Totales y lista de géneros disponibles.

**URL:**
```
https://cdn.jsdelivr.net/gh/jexiptv07/stremcore@main/meta.json
```

**Respuesta:**
```json
{
  "total_movies": 21968,
  "total_series": 11397,
  "total_seasons": 19657,
  "per_page": 40,
  "movie_genres": [
    { "slug": "accion",          "label": "Acción",          "total": 1453 },
    { "slug": "animacion",       "label": "Animación",        "total": 992  },
    { "slug": "aventura",        "label": "Aventura",         "total": 994  },
    { "slug": "belica",          "label": "Bélica",           "total": 130  },
    { "slug": "ciencia-ficcion", "label": "Ciencia ficción",  "total": 693  },
    { "slug": "comedia",         "label": "Comedia",          "total": 1525 },
    { "slug": "crimen",          "label": "Crimen",           "total": 676  },
    { "slug": "documental",      "label": "Documental",       "total": 55   },
    { "slug": "drama",           "label": "Drama",            "total": 8275 },
    { "slug": "familia",         "label": "Familia",          "total": 704  },
    { "slug": "fantasia",        "label": "Fantasía",         "total": 679  },
    { "slug": "historia",        "label": "Historia",         "total": 116  },
    { "slug": "misterio",        "label": "Misterio",         "total": 528  },
    { "slug": "musica",          "label": "Música",           "total": 89   },
    { "slug": "pelicula-de-tv",  "label": "Película de TV",   "total": 211  },
    { "slug": "romance",         "label": "Romance",          "total": 1056 },
    { "slug": "suspense",        "label": "Suspense",         "total": 734  },
    { "slug": "terror",          "label": "Terror",           "total": 1046 },
    { "slug": "western",         "label": "Western",          "total": 43   }
  ],
  "series_genres": [
    { "slug": "animacion", "label": "Animación", "total": 45  },
    { "slug": "drama",     "label": "Drama",     "total": 312 },
    { "slug": "romance",   "label": "Romance",   "total": 28  },
    { "slug": "western",   "label": "Western",   "total": 6   }
  ]
}
```

---

### 2. LISTADO DE PELÍCULAS

#### `GET /pages/movies/all/{page}.json`
Todas las películas ordenadas por fecha de estreno descendente, 40 por página.

**URL:**
```
https://cdn.jsdelivr.net/gh/jexiptv07/stremcore@main/pages/movies/all/1.json
```

**Respuesta:**
```json
{
  "page": 1,
  "total_pages": 550,
  "total": 21968,
  "data": [
    {
      "id": 1146239,
      "titulo": "Don't Mess with Grandma",
      "titulo_orig": "",
      "poster": "https://image.tmdb.org/t/p/original/5BVvVuuoXFuge8VlAf0T8PcztjM.jpg",
      "release_date": "2024-09-20",
      "genres": ["accion", "comedia"],
      "rating": "6.0"
    }
  ]
}
```

---

### 3. LISTADO DE SERIES

#### `GET /pages/series/all/{page}.json`
Todas las series ordenadas por fecha descendente, 40 por página.

**URL:**
```
https://cdn.jsdelivr.net/gh/jexiptv07/stremcore@main/pages/series/all/1.json
```

**Respuesta:** misma estructura que películas.

---

### 4. FILTRAR POR CATEGORÍA

#### `GET /pages/movies/{genero}/{page}.json`

**Géneros disponibles para películas:**

| Slug | Label |
|---|---|
| `accion` | Acción |
| `animacion` | Animación |
| `aventura` | Aventura |
| `belica` | Bélica |
| `ciencia-ficcion` | Ciencia ficción |
| `comedia` | Comedia |
| `crimen` | Crimen |
| `documental` | Documental |
| `drama` | Drama |
| `familia` | Familia |
| `fantasia` | Fantasía |
| `historia` | Historia |
| `misterio` | Misterio |
| `musica` | Música |
| `pelicula-de-tv` | Película de TV |
| `romance` | Romance |
| `suspense` | Suspense |
| `terror` | Terror |
| `western` | Western |

**URL ejemplo:**
```
https://cdn.jsdelivr.net/gh/jexiptv07/stremcore@main/pages/movies/accion/1.json
https://cdn.jsdelivr.net/gh/jexiptv07/stremcore@main/pages/movies/terror/2.json
```

#### `GET /pages/series/{genero}/{page}.json`

**Géneros disponibles para series:** `animacion` · `drama` · `romance` · `western`

**URL ejemplo:**
```
https://cdn.jsdelivr.net/gh/jexiptv07/stremcore@main/pages/series/drama/1.json
```

**Respuesta:** misma estructura que el listado general.

---

### 5. DETALLE DE PELÍCULA

#### `GET /movies/{id}.json`

**URL ejemplo:**
```
https://cdn.jsdelivr.net/gh/jexiptv07/stremcore@main/movies/1146239.json
```

**Respuesta:**
```json
{
  "id": 1146239,
  "titulo": "Don't Mess with Grandma",
  "titulo_orig": "",
  "poster": "https://image.tmdb.org/t/p/original/5BVvVuuoXFuge8VlAf0T8PcztjM.jpg",
  "backdrop": "https://image.tmdb.org/t/p/original/yl3wAoE0HhTQIOk1r9D1xbC8m5p.jpg",
  "release_date": "2024-09-20",
  "genres": ["accion", "comedia"],
  "rating": "6.0",
  "runtime": "81",
  "overview": "JT must fix his grandmother's leaky sink...",
  "trailer": "https://youtube.com/...",
  "slug": "dont-mess-with-grandma",
  "servidores": [
    {
      "id": 1,
      "nombre": "Streamwish",
      "url": "https://streamwish.to/e/abc123",
      "player_url": "https://streamwish.to/e/abc123",
      "embed_id": "abc123",
      "idioma": "latino",
      "calidad": "HD"
    },
    {
      "id": 2,
      "nombre": "Filemoon",
      "url": "https://filemoon.sx/e/xyz789",
      "player_url": "https://filemoon.sx/e/xyz789",
      "embed_id": "xyz789",
      "idioma": "english",
      "calidad": "HD"
    }
  ]
}
```

---

### 6. DETALLE DE SERIE

#### `GET /series/{id}.json`

**URL ejemplo:**
```
https://cdn.jsdelivr.net/gh/jexiptv07/stremcore@main/series/245318.json
```

**Respuesta:**
```json
{
  "id": 245318,
  "titulo": "Margo tiene problemas de dinero",
  "titulo_orig": "",
  "poster": "https://image.tmdb.org/t/p/original/zqUvfOQtaffnkQL0ZVKPD7Z1KIR.jpg",
  "backdrop": "https://image.tmdb.org/t/p/original/5O9CNnYuH1U0L3ym7q7q93eH99u.jpg",
  "release_date": "2026-04-14",
  "genres": ["drama"],
  "rating": "8.1",
  "overview": "Margo Millet, aspirante a escritora...",
  "trailer": "",
  "slug": "margo-tiene-problemas-de-dinero"
}
```

> Los servidores de las series están dentro de cada episodio en los archivos de temporada.

---

### 7. TEMPORADA (episodios + servidores)

#### `GET /series/{id}/t{n}.json`

**URL ejemplo:**
```
https://cdn.jsdelivr.net/gh/jexiptv07/stremcore@main/series/245318/t1.json
https://cdn.jsdelivr.net/gh/jexiptv07/stremcore@main/series/245318/t2.json
```

**Respuesta:**
```json
{
  "number": 1,
  "serie_id": 245318,
  "episodios": [
    {
      "id": 24531801001,
      "number": 1,
      "titulo": "Margo tiene problemas de dinero 1x1",
      "imagen": "https://image.tmdb.org/t/p/original/xc7rZSzaokaB7kmtYTDrtACoEdq.jpg",
      "servidores": [
        {
          "id": 1,
          "nombre": "Streamwish",
          "url": "https://streamwish.to/e/0u7k5cv96i6d",
          "player_url": "https://streamwish.to/e/0u7k5cv96i6d",
          "embed_id": "0u7k5cv96i6d",
          "idioma": "latino",
          "calidad": "HD"
        },
        {
          "id": 2,
          "nombre": "Filemoon",
          "url": "https://bysejikuar.com/e/mmg7gpv2up28",
          "player_url": "https://bysejikuar.com/e/mmg7gpv2up28",
          "embed_id": "mmg7gpv2up28",
          "idioma": "latino",
          "calidad": "HD"
        },
        {
          "id": 6,
          "nombre": "Streamwish",
          "url": "https://streamwish.to/e/ui7mc0co676c",
          "player_url": "https://streamwish.to/e/ui7mc0co676c",
          "embed_id": "ui7mc0co676c",
          "idioma": "english",
          "calidad": "HD"
        }
      ]
    },
    {
      "id": 24531801002,
      "number": 2,
      "titulo": "Margo tiene problemas de dinero 1x2",
      "imagen": "https://image.tmdb.org/t/p/original/xUwVCH0Q6jUcfTLJrav9Vnl7sYn.jpg",
      "servidores": [...]
    }
  ]
}
```

---

### 8. BÚSQUEDA

La búsqueda se hace **en el cliente** cargando los chunks del índice.

#### Índice de películas (11 chunks de ~2000 items)
```
https://cdn.jsdelivr.net/gh/jexiptv07/stremcore@main/search/movies/1.json
https://cdn.jsdelivr.net/gh/jexiptv07/stremcore@main/search/movies/2.json
...
https://cdn.jsdelivr.net/gh/jexiptv07/stremcore@main/search/movies/11.json
```

#### Índice de series (6 chunks de ~2000 items)
```
https://cdn.jsdelivr.net/gh/jexiptv07/stremcore@main/search/series/1.json
...
https://cdn.jsdelivr.net/gh/jexiptv07/stremcore@main/search/series/6.json
```

#### Estructura de cada item en el índice:
```json
{
  "chunk": 1,
  "total_chunks": 11,
  "total": 21968,
  "data": [
    {
      "id": 1146239,
      "t":  "Don't Mess with Grandma",
      "to": "",
      "p":  "https://image.tmdb.org/t/p/original/5BVvVuuoXFuge8VlAf0T8PcztjM.jpg",
      "d":  "2024-09-20",
      "g":  ["accion", "comedia"],
      "r":  "6.0"
    }
  ]
}
```
> Campos: `t`=titulo · `to`=titulo_orig · `p`=poster · `d`=release_date · `g`=genres · `r`=rating

#### Implementación de búsqueda en JS:

```js
const BASE = "https://cdn.jsdelivr.net/gh/jexiptv07/stremcore@main";

// Cargar todos los chunks y buscar
async function search(query, tipo = "movies") {
  const q = query.toLowerCase().trim();
  if (q.length < 2) return [];

  // Obtener total de chunks del primer archivo
  const first = await fetch(`${BASE}/search/${tipo}/1.json`).then(r => r.json());
  const totalChunks = first.total_chunks;

  // Cargar todos los chunks en paralelo
  const chunks = await Promise.all(
    Array.from({ length: totalChunks }, (_, i) =>
      fetch(`${BASE}/search/${tipo}/${i + 1}.json`).then(r => r.json())
    )
  );

  // Buscar en todos los items
  const results = [];
  for (const chunk of chunks) {
    for (const item of chunk.data) {
      if (
        item.t.toLowerCase().includes(q) ||
        item.to.toLowerCase().includes(q)
      ) {
        results.push({
          id:           item.id,
          titulo:       item.t,
          titulo_orig:  item.to,
          poster:       item.p,
          release_date: item.d,
          genres:       item.g,
          rating:       item.r,
        });
      }
    }
  }

  return results.slice(0, 50); // máximo 50 resultados
}

// Búsqueda solo en películas
const peliculas = await search("batman", "movies");

// Búsqueda solo en series
const series = await search("breaking", "series");

// Búsqueda en todo el catálogo
const [peliculas, series] = await Promise.all([
  search("avatar", "movies"),
  search("avatar", "series"),
]);
const todo = [...peliculas, ...series];
```

> **Tip:** Cachea los chunks en localStorage para no recargarlos en cada búsqueda.

---

## Cliente JS completo

Archivo: `/home/localhost/Documents/stremcore-api.js`

```js
const BASE = "https://cdn.jsdelivr.net/gh/jexiptv07/stremcore@main";
const _cache = new Map();

async function fetchJSON(url) {
  if (_cache.has(url)) return _cache.get(url);
  const res = await fetch(url);
  if (!res.ok) throw new Error(`HTTP ${res.status}`);
  const data = await res.json();
  _cache.set(url, data);
  return data;
}

export const getMeta              = ()              => fetchJSON(`${BASE}/meta.json`);
export const getMovies            = (page = 1)      => fetchJSON(`${BASE}/pages/movies/all/${page}.json`);
export const getSeries            = (page = 1)      => fetchJSON(`${BASE}/pages/series/all/${page}.json`);
export const getMoviesByGenre     = (genre, page=1) => fetchJSON(`${BASE}/pages/movies/${genre}/${page}.json`);
export const getSeriesByGenre     = (genre, page=1) => fetchJSON(`${BASE}/pages/series/${genre}/${page}.json`);
export const getMovie             = (id)            => fetchJSON(`${BASE}/movies/${id}.json`);
export const getSerie             = (id)            => fetchJSON(`${BASE}/series/${id}.json`);
export const getSeason            = (id, n)         => fetchJSON(`${BASE}/series/${id}/t${n}.json`);

export async function getAllSeasons(serieId) {
  const seasons = [];
  let n = 1;
  while (true) {
    try { seasons.push(await getSeason(serieId, n++)); }
    catch { break; }
  }
  return seasons;
}

export async function search(query, tipo = "movies") {
  const q = query.toLowerCase().trim();
  if (q.length < 2) return [];
  const first = await fetchJSON(`${BASE}/search/${tipo}/1.json`);
  const chunks = await Promise.all(
    Array.from({ length: first.total_chunks }, (_, i) =>
      fetchJSON(`${BASE}/search/${tipo}/${i + 1}.json`)
    )
  );
  const results = [];
  for (const chunk of chunks)
    for (const item of chunk.data)
      if (item.t.toLowerCase().includes(q) || item.to.toLowerCase().includes(q))
        results.push({ id:item.id, titulo:item.t, titulo_orig:item.to,
                       poster:item.p, release_date:item.d, genres:item.g, rating:item.r });
  return results.slice(0, 50);
}
```

---

## Tabla resumen de rutas

| Función | Método | URL |
|---|---|---|
| Meta / géneros | GET | `/meta.json` |
| Lista películas | GET | `/pages/movies/all/{page}.json` |
| Lista series | GET | `/pages/series/all/{page}.json` |
| Películas por género | GET | `/pages/movies/{genero}/{page}.json` |
| Series por género | GET | `/pages/series/{genero}/{page}.json` |
| Detalle película | GET | `/movies/{id}.json` |
| Detalle serie | GET | `/series/{id}.json` |
| Temporada + episodios | GET | `/series/{id}/t{n}.json` |
| Búsqueda películas | GET | `/search/movies/{1..11}.json` |
| Búsqueda series | GET | `/search/series/{1..6}.json` |
