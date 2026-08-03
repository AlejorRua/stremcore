/**
 * StremCore API Client
 * Backend: GitHub + jsDelivr CDN
 * Base: https://cdn.jsdelivr.net/gh/jexiptv07/stremcore@main
 */

const BASE = "https://cdn.jsdelivr.net/gh/jexiptv07/stremcore@main";

// Cache en memoria para evitar requests repetidos
const _cache = new Map();

async function fetchJSON(url) {
  if (_cache.has(url)) return _cache.get(url);
  const res = await fetch(url);
  if (!res.ok) throw new Error(`HTTP ${res.status}: ${url}`);
  const data = await res.json();
  _cache.set(url, data);
  return data;
}

// ─────────────────────────────────────────────────────────────────────────────
// META  — géneros disponibles, totales
// ─────────────────────────────────────────────────────────────────────────────

/**
 * Devuelve estadísticas generales y lista de géneros disponibles.
 *
 * Respuesta:
 * {
 *   total_movies: 21968,
 *   total_series: 11397,
 *   total_seasons: 19657,
 *   per_page: 40,
 *   movie_genres: [{ slug, label, total }, ...],
 *   series_genres: [{ slug, label, total }, ...]
 * }
 */
export async function getMeta() {
  return fetchJSON(`${BASE}/meta.json`);
}

// ─────────────────────────────────────────────────────────────────────────────
// LISTA GENERAL  — películas y series paginadas
// ─────────────────────────────────────────────────────────────────────────────

/**
 * Lista TODAS las películas (ordenadas por fecha desc), 40 por página.
 *
 * @param {number} page  - número de página (empieza en 1)
 *
 * Respuesta:
 * {
 *   page: 1,
 *   total_pages: 550,
 *   total: 21968,
 *   data: [{ id, titulo, titulo_orig, poster, release_date, genres, rating }, ...]
 * }
 */
export async function getMovies(page = 1) {
  return fetchJSON(`${BASE}/pages/movies/all/${page}.json`);
}

/**
 * Lista TODAS las series (ordenadas por fecha desc), 40 por página.
 *
 * @param {number} page
 */
export async function getSeries(page = 1) {
  return fetchJSON(`${BASE}/pages/series/all/${page}.json`);
}

// ─────────────────────────────────────────────────────────────────────────────
// FILTRO POR CATEGORÍA
// ─────────────────────────────────────────────────────────────────────────────

/**
 * Géneros disponibles para películas:
 * accion | animacion | aventura | belica | ciencia-ficcion | comedia |
 * crimen | documental | drama | familia | fantasia | historia | misterio |
 * musica | pelicula-de-tv | romance | suspense | terror | western
 *
 * Géneros disponibles para series:
 * animacion | drama | romance | western
 */

/**
 * Películas filtradas por género.
 *
 * @param {string} genre  - slug del género (ej: "accion", "drama")
 * @param {number} page
 */
export async function getMoviesByGenre(genre, page = 1) {
  return fetchJSON(`${BASE}/pages/movies/${genre}/${page}.json`);
}

/**
 * Series filtradas por género.
 *
 * @param {string} genre  - slug del género (ej: "drama", "animacion")
 * @param {number} page
 */
export async function getSeriesByGenre(genre, page = 1) {
  return fetchJSON(`${BASE}/pages/series/${genre}/${page}.json`);
}

// ─────────────────────────────────────────────────────────────────────────────
// DETALLE DE PELÍCULA
// ─────────────────────────────────────────────────────────────────────────────

/**
 * Detalle completo de una película incluyendo servidores de video.
 *
 * @param {number|string} id  - ID de la película
 *
 * Respuesta:
 * {
 *   id, titulo, titulo_orig, poster, backdrop,
 *   release_date, genres, rating, runtime, overview, trailer, slug,
 *   servidores: [{ id, url, idioma, nombre, calidad, embed_id, player_url }, ...]
 * }
 */
export async function getMovie(id) {
  return fetchJSON(`${BASE}/movies/${id}.json`);
}

// ─────────────────────────────────────────────────────────────────────────────
// DETALLE DE SERIE + EPISODIOS
// ─────────────────────────────────────────────────────────────────────────────

/**
 * Detalle de una serie (sin episodios).
 *
 * @param {number|string} id  - ID de la serie
 *
 * Respuesta:
 * {
 *   id, titulo, titulo_orig, poster, backdrop,
 *   release_date, genres, rating, overview, trailer, slug
 * }
 */
export async function getSerie(id) {
  return fetchJSON(`${BASE}/series/${id}.json`);
}

/**
 * Episodios de una temporada específica.
 *
 * @param {number|string} serieId     - ID de la serie
 * @param {number}        seasonNum   - número de temporada (1, 2, 3...)
 *
 * Respuesta:
 * {
 *   number: 1,
 *   serie_id: 509870405,
 *   episodios: [
 *     {
 *       id, number, titulo, imagen,
 *       servidores: [{ id, url, idioma, nombre, calidad, embed_id, player_url }, ...]
 *     },
 *     ...
 *   ]
 * }
 */
export async function getSeason(serieId, seasonNum) {
  return fetchJSON(`${BASE}/series/${serieId}/t${seasonNum}.json`);
}

/**
 * Carga todas las temporadas disponibles de una serie.
 * Prueba t1, t2, t3... hasta que alguna falle (404).
 *
 * @param {number|string} serieId
 * @returns {Promise<Array>}  - array de temporadas ordenadas
 */
export async function getAllSeasons(serieId) {
  const seasons = [];
  let n = 1;
  while (true) {
    try {
      const season = await getSeason(serieId, n);
      seasons.push(season);
      n++;
    } catch {
      break; // no hay más temporadas
    }
  }
  return seasons;
}

// ─────────────────────────────────────────────────────────────────────────────
// HELPERS
// ─────────────────────────────────────────────────────────────────────────────

/** Limpia el cache en memoria */
export function clearCache() {
  _cache.clear();
}
