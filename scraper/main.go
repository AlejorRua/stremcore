package main

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"
)

// ── Constants ─────────────────────────────────────────────────────────────────

const (
	cvtBase         = "https://compucalitv.tv/api/rest"
	imageBase       = "https://compucalitv.tv/wp-content/uploads"
	phd2Base        = "https://www.poseidonhd2.co"
	flixlatamBase   = "https://flixlatam.com"
	perPage         = 18
	buildBatch      = 8
	phd2BatchSize   = 5
	buildDelay      = 150 * time.Millisecond
	cacheRebuildTTL = 2 * time.Hour
	redisKey        = "poseidon:cache"
	detailTTL       = 7 * 24 * time.Hour // 7 días — se renueva en cada acceso
	seasonTTL       = 24 * time.Hour
	hdRefreshTTL    = 24 * time.Hour
	warmBatchSize   = 5 // requests paralelos en el warmer (detalles)
	warmDelay       = 400 * time.Millisecond
	warmSeasonBatch = 10 // series en paralelo al calentar episodios
	warmSeasonDelay = 300 * time.Millisecond
	userAgent       = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"
)

// ── Genre maps ────────────────────────────────────────────────────────────────

var genreNames = map[int]string{
	26:    "Acción",
	9306:  "Action & Adventure",
	51:    "Animación",
	25:    "Aventura",
	158:   "Bélica",
	27:    "Ciencia ficción",
	192:   "Comedia",
	136:   "Crimen",
	23209: "Documental",
	157:   "Drama",
	52:    "Familia",
	86:    "Fantasía",
	404:   "Historia",
	9307:  "Kids",
	249:   "Misterio",
	307:   "Música",
	9496:  "Película de TV",
	15487: "Reality",
	215:   "Romance",
	9334:  "Sci-Fi & Fantasy",
	23714: "Soap",
	87:    "Suspense",
	48809: "Talk",
	422:   "Terror",
	9582:  "War & Politics",
	1594:  "Western",
}

var genreByName = map[string]int{}

var phd2GenreToID = map[string]int{
	"Action":             26,
	"Adventure":          25,
	"Animation":          51,
	"Comedy":             192,
	"Crime":              136,
	"Documentary":        23209,
	"Drama":              157,
	"Family":             52,
	"Fantasy":            86,
	"History":            404,
	"Horror":             422,
	"Music":              307,
	"Mystery":            249,
	"Romance":            215,
	"Science Fiction":    27,
	"Thriller":           87,
	"War":                158,
	"Western":            1594,
	"TV Movie":           9496,
	"Action & Adventure": 9306,
	"Sci-Fi & Fantasy":   9334,
	"War & Politics":     9582,
	"Kids":               9307,
	"Reality":            15487,
	"Talk":               48809,
	"Soap":               23714,
}

func init() {
	for id, name := range genreNames {
		genreByName[name] = id
	}
}

// ── Slug cache ────────────────────────────────────────────────────────────────

var slugCache sync.Map

func cachePost(id int, slug string) { slugCache.Store(id, slug) }

func resolveSlug(id int) (string, bool) {
	v, ok := slugCache.Load(id)
	if !ok {
		return "", false
	}
	return v.(string), true
}

func fetchSlugForID(id int, postType string) (string, bool) {
	for page := 1; page <= 15; page++ {
		data, err := cvtGet("/listing", map[string]string{
			"post_type": postType,
			"page":      strconv.Itoa(page),
		})
		if err != nil {
			break
		}
		var resp CvtListResp
		if json.Unmarshal(data, &resp) != nil || resp.Error {
			break
		}
		for _, p := range resp.Data.Posts {
			cachePost(p.ID, p.Slug)
			if p.ID == id {
				return p.Slug, true
			}
		}
		if page >= resp.Data.Pagination.LastPage {
			break
		}
	}
	return "", false
}

// ── Post indexes (ID → CvtPost) ───────────────────────────────────────────────

var movieIndex sync.Map
var serieIndex sync.Map

func findMovieByID(id int) (CvtPost, bool) {
	v, ok := movieIndex.Load(id)
	if !ok {
		return CvtPost{}, false
	}
	return v.(CvtPost), true
}

func findSerieByID(id int) (CvtPost, bool) {
	v, ok := serieIndex.Load(id)
	if !ok {
		return CvtPost{}, false
	}
	return v.(CvtPost), true
}

// ── Global sorted cache ───────────────────────────────────────────────────────

type sortedList struct {
	items     []CvtPost
	total     int
	lastPage  int
	ready     bool
	updatedAt time.Time
}

type globalCache struct {
	mu     sync.RWMutex
	movies sortedList
	series sortedList
}

var gc = &globalCache{}

func (c *globalCache) getMovies() (sortedList, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.movies, c.movies.ready
}

func (c *globalCache) getSeries() (sortedList, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.series, c.series.ready
}

func (c *globalCache) setMovies(sl sortedList) {
	c.mu.Lock()
	c.movies = sl
	c.mu.Unlock()
	movieIndex.Range(func(k, _ any) bool { movieIndex.Delete(k); return true })
	for _, p := range sl.items {
		movieIndex.Store(p.ID, p)
	}
}

func (c *globalCache) setSeries(sl sortedList) {
	c.mu.Lock()
	c.series = sl
	c.mu.Unlock()
	serieIndex.Range(func(k, _ any) bool { serieIndex.Delete(k); return true })
	for _, p := range sl.items {
		serieIndex.Store(p.ID, p)
	}
}

// ── Catalog snapshot cache ────────────────────────────────────────────────────

type CacheSnapshot struct {
	UpdatedAt time.Time `json:"updated_at"`
	Movies    []CvtPost `json:"movies"`
	Series    []CvtPost `json:"series"`
}

var rdb *redis.Client
var rctx = context.Background()
var redisAvailable atomic.Bool
var warmerRunning atomic.Bool

func initRedis() {
	addr := os.Getenv("REDIS_ADDR")
	if addr == "" {
		addr = "localhost:6379"
	}
	rdb = redis.NewClient(&redis.Options{Addr: addr})
	if err := rdb.Ping(rctx).Err(); err != nil {
		redisAvailable.Store(false)
		log.Printf("[redis] Advertencia: no se pudo conectar a %s: %v", addr, err)
	} else {
		redisAvailable.Store(true)
		log.Printf("[redis] Conectado a %s", addr)
	}
}

func saveCache(movies, series []CvtPost) {
	snap := CacheSnapshot{UpdatedAt: time.Now(), Movies: movies, Series: series}
	b, err := json.Marshal(snap)
	if err != nil {
		log.Printf("[cache] Error serializando: %v", err)
		return
	}
	setCacheBytes(redisKey, b, 0)
	log.Printf("[cache] Guardado (%d bytes)", len(b))
}

func loadCache() (movies, series []CvtPost, ok bool) {
	b, pgOk := pgGet(redisKey)
	source := "PostgreSQL"
	if !pgOk {
		if !redisAvailable.Load() {
			return nil, nil, false
		}
		var err error
		b, err = rdb.Get(rctx, redisKey).Bytes()
		if err != nil {
			return nil, nil, false
		}
		source = "Redis"
		go pgSet(redisKey, b)
	} else if redisAvailable.Load() {
		go rdb.Set(rctx, redisKey, b, 0)
	}
	var snap CacheSnapshot
	if err := json.Unmarshal(b, &snap); err != nil {
		log.Printf("[cache] Error parseando: %v", err)
		return nil, nil, false
	}
	log.Printf("[cache] Cargado desde %s (actualizado: %v)", source, snap.UpdatedAt.Format(time.RFC3339))
	return snap.Movies, snap.Series, true
}

// ── PostgreSQL store ──────────────────────────────────────────────────────────

var pgPool *pgxpool.Pool

func initPostgres() {
	dsn := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if dsn == "" {
		log.Printf("[pg] DATABASE_URL no configurada")
		return
	}

	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		log.Printf("[pg] DSN inválido: %v", err)
		return
	}
	targetDB := cfg.ConnConfig.Database

	// Conectar al sistema (postgres) para crear la DB si no existe
	sysCfg := cfg.Copy()
	sysCfg.ConnConfig.Database = "postgres"
	if sysPool, err := pgxpool.NewWithConfig(context.Background(), sysCfg); err == nil {
		var exists bool
		_ = sysPool.QueryRow(context.Background(),
			"SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname=$1)", targetDB).Scan(&exists)
		if !exists {
			if _, err := sysPool.Exec(context.Background(),
				fmt.Sprintf(`CREATE DATABASE "%s"`, targetDB)); err == nil {
				log.Printf("[pg] Base de datos '%s' creada", targetDB)
			}
		}
		sysPool.Close()
	}

	// Conectar a la base de datos objetivo
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		log.Printf("[pg] Error conectando a '%s': %v", targetDB, err)
		return
	}
	if err := pool.Ping(context.Background()); err != nil {
		log.Printf("[pg] Ping fallido en '%s': %v", targetDB, err)
		pool.Close()
		return
	}

	// Crear tablas e índices si no existen
	_, err = pool.Exec(context.Background(), `
		CREATE TABLE IF NOT EXISTS poseidon_store (
			key         TEXT        PRIMARY KEY,
			value       JSONB       NOT NULL,
			updated_at  TIMESTAMPTZ DEFAULT NOW()
		);
		CREATE INDEX IF NOT EXISTS poseidon_store_prefix
			ON poseidon_store (key text_pattern_ops);

		CREATE TABLE IF NOT EXISTS poseidon_catalog (
			id          BIGINT      NOT NULL,
			tipo        CHAR(1)     NOT NULL,
			titulo      TEXT        NOT NULL,
			titulo_orig TEXT        NOT NULL DEFAULT '',
			poster_url  TEXT        NOT NULL DEFAULT '',
			release_date TEXT       NOT NULL DEFAULT '',
			genres      JSONB       NOT NULL DEFAULT '[]'::jsonb,
			data        JSONB       NOT NULL DEFAULT '{}'::jsonb,
			has_servers BOOLEAN     NOT NULL DEFAULT FALSE,
			updated_at  TIMESTAMPTZ DEFAULT NOW(),
			PRIMARY KEY (id, tipo)
		);
		ALTER TABLE poseidon_catalog ADD COLUMN IF NOT EXISTS release_date TEXT NOT NULL DEFAULT '';
		ALTER TABLE poseidon_catalog ADD COLUMN IF NOT EXISTS genres JSONB NOT NULL DEFAULT '[]'::jsonb;
		ALTER TABLE poseidon_catalog ADD COLUMN IF NOT EXISTS data JSONB NOT NULL DEFAULT '{}'::jsonb;
		ALTER TABLE poseidon_catalog ADD COLUMN IF NOT EXISTS has_servers BOOLEAN NOT NULL DEFAULT FALSE;
		ALTER TABLE poseidon_catalog ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ DEFAULT NOW();
		CREATE INDEX IF NOT EXISTS poseidon_catalog_titulo
			ON poseidon_catalog (LOWER(titulo));
		CREATE INDEX IF NOT EXISTS poseidon_catalog_titulo_orig
			ON poseidon_catalog (LOWER(titulo_orig));
		CREATE INDEX IF NOT EXISTS poseidon_catalog_tipo
			ON poseidon_catalog (tipo);
		CREATE INDEX IF NOT EXISTS poseidon_catalog_tipo_visible_date
			ON poseidon_catalog (tipo, has_servers, release_date DESC);
		CREATE INDEX IF NOT EXISTS poseidon_catalog_genres
			ON poseidon_catalog USING GIN (genres);
	`)
	if err != nil {
		log.Printf("[pg] Error creando tablas: %v", err)
		pool.Close()
		return
	}

	pgPool = pool
	log.Printf("[pg] Conectado a PostgreSQL (base: '%s')", targetDB)
}

func pgGet(key string) ([]byte, bool) {
	if pgPool == nil {
		return nil, false
	}
	var value []byte
	err := pgPool.QueryRow(context.Background(),
		"SELECT value FROM poseidon_store WHERE key=$1", key).Scan(&value)
	if err != nil {
		return nil, false
	}
	return value, true
}

func pgStoreUpdatedAt(key string) (time.Time, bool) {
	if pgPool == nil {
		return time.Time{}, false
	}
	var updatedAt time.Time
	err := pgPool.QueryRow(context.Background(),
		"SELECT updated_at FROM poseidon_store WHERE key=$1", key).Scan(&updatedAt)
	if err != nil {
		return time.Time{}, false
	}
	return updatedAt, true
}

func pgSet(key string, value []byte) {
	if pgPool == nil {
		return
	}
	_, err := pgPool.Exec(context.Background(), `
		INSERT INTO poseidon_store (key, value, updated_at)
		VALUES ($1, $2::jsonb, NOW())
		ON CONFLICT (key) DO UPDATE
			SET value = EXCLUDED.value, updated_at = NOW()
	`, key, string(value))
	if err != nil {
		log.Printf("[pg] Error guardando %s: %v", key, err)
	}
}

func pgSetCatalogHasServers(tipo string, id int, hasServers bool) {
	if pgPool == nil || id <= 0 {
		return
	}
	_, err := pgPool.Exec(context.Background(), `
		UPDATE poseidon_catalog
		SET has_servers=$3, updated_at=NOW()
		WHERE id=$1 AND tipo=$2
	`, int64(id), tipo, hasServers)
	if err != nil {
		log.Printf("[pg] Error actualizando disponibilidad %s:%d: %v", tipo, id, err)
	}
}

// pgUpsertCatalog saves or updates a batch of items in poseidon_catalog.
func pgUpsertCatalog(posts []CvtPost, tipo string) {
	if pgPool == nil || len(posts) == 0 {
		return
	}
	for _, p := range posts {
		orig := p.OriginalTitle
		poster := fullImageURL(p.Images.Poster)
		data, err := json.Marshal(p)
		if err != nil {
			continue
		}
		genres, err := json.Marshal(p.Genres)
		if err != nil {
			genres = []byte("[]")
		}
		hasServers, _ := cachedServerAvailability(tipo, p.ID)
		if _, err := pgPool.Exec(context.Background(), `
			INSERT INTO poseidon_catalog
				(id, tipo, titulo, titulo_orig, poster_url, release_date, genres, data, has_servers, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, $8::jsonb, $9, NOW())
			ON CONFLICT (id, tipo) DO UPDATE
				SET titulo=EXCLUDED.titulo,
					titulo_orig=EXCLUDED.titulo_orig,
					poster_url=EXCLUDED.poster_url,
					release_date=EXCLUDED.release_date,
					genres=EXCLUDED.genres,
					data=EXCLUDED.data,
					has_servers=EXCLUDED.has_servers,
					updated_at=NOW()
		`, int64(p.ID), tipo, p.Title, orig, poster, releaseDate(p.ReleaseDate), string(genres), string(data), hasServers); err != nil {
			log.Printf("[pg] Error guardando catálogo %s:%d: %v", tipo, p.ID, err)
		}
	}
}

func pgPostFromData(data []byte) (CvtPost, bool) {
	var post CvtPost
	if len(data) == 0 || json.Unmarshal(data, &post) != nil || post.ID == 0 {
		return CvtPost{}, false
	}
	return post, true
}

func pgFindCatalogPost(id int, tipo string) (CvtPost, bool) {
	if pgPool == nil {
		return CvtPost{}, false
	}
	var data []byte
	err := pgPool.QueryRow(context.Background(), `
		SELECT data
		FROM poseidon_catalog
		WHERE id=$1 AND tipo=$2 AND data <> '{}'::jsonb
	`, int64(id), tipo).Scan(&data)
	if err != nil {
		return CvtPost{}, false
	}
	return pgPostFromData(data)
}

func pgLoadCatalogPosts(tipo string) ([]CvtPost, bool) {
	if pgPool == nil {
		return nil, false
	}
	rows, err := pgPool.Query(context.Background(), `
		SELECT data
		FROM poseidon_catalog
		WHERE tipo=$1 AND data <> '{}'::jsonb
		ORDER BY release_date DESC, id DESC
	`, tipo)
	if err != nil {
		return nil, false
	}
	defer rows.Close()

	var posts []CvtPost
	for rows.Next() {
		var data []byte
		if rows.Scan(&data) != nil {
			continue
		}
		if post, ok := pgPostFromData(data); ok {
			posts = append(posts, post)
		}
	}
	return posts, true
}

func pgLoadCatalogSnapshot() (movies, series []CvtPost, ok bool) {
	movies, moviesOK := pgLoadCatalogPosts("m")
	series, seriesOK := pgLoadCatalogPosts("s")
	if !moviesOK && !seriesOK {
		return nil, nil, false
	}
	return movies, series, len(movies) > 0 || len(series) > 0
}

func pgListCatalogPosts(tipo string, page int, genreID *int) ([]CvtPost, int, int, bool) {
	if pgPool == nil {
		return nil, 0, 0, false
	}
	args := []any{tipo}
	where := "tipo=$1 AND has_servers=TRUE AND data <> '{}'::jsonb"
	if genreID != nil {
		args = append(args, fmt.Sprintf("[%d]", *genreID))
		where += fmt.Sprintf(" AND genres @> $%d::jsonb", len(args))
	}

	query := fmt.Sprintf(`
		SELECT data
		FROM poseidon_catalog
		WHERE %s
		ORDER BY release_date DESC, id DESC
	`, where)
	rows, err := pgPool.Query(context.Background(), query, args...)
	if err != nil {
		return nil, 0, 0, false
	}
	defer rows.Close()

	all := make([]CvtPost, 0, perPage)
	for rows.Next() {
		var data []byte
		if rows.Scan(&data) != nil {
			continue
		}
		if post, ok := pgPostFromData(data); ok {
			all = append(all, post)
		}
	}
	deduped := dedupeCatalogPosts(all)
	posts, total := pageFromList(deduped, page)
	return posts, total, calcTotalPages(total), true
}

// pgSearchCatalog searches poseidon_catalog for items matching the query string.
func pgSearchCatalog(q, tipo string, limit int) []struct {
	ID        int64
	Titulo    string
	PosterURL string
} {
	if pgPool == nil {
		return nil
	}
	pattern := "%" + strings.ToLower(q) + "%"
	var rows []struct {
		ID        int64
		Titulo    string
		PosterURL string
	}
	var query string
	var args []any
	if tipo == "" {
		query = `SELECT data FROM poseidon_catalog
			WHERE has_servers=TRUE
			  AND data <> '{}'::jsonb
			  AND (LOWER(titulo) LIKE $1 OR LOWER(titulo_orig) LIKE $1)
			ORDER BY release_date DESC, id DESC
			LIMIT $2`
		args = []any{pattern, limit * 4}
	} else {
		query = `SELECT data FROM poseidon_catalog
			WHERE tipo=$1
			  AND has_servers=TRUE
			  AND data <> '{}'::jsonb
			  AND (LOWER(titulo) LIKE $2 OR LOWER(titulo_orig) LIKE $2)
			ORDER BY release_date DESC, id DESC
			LIMIT $3`
		args = []any{tipo, pattern, limit * 4}
	}
	dbRows, err := pgPool.Query(context.Background(), query, args...)
	if err != nil {
		return nil
	}
	defer dbRows.Close()
	var posts []CvtPost
	for dbRows.Next() {
		var data []byte
		if dbRows.Scan(&data) != nil {
			continue
		}
		if post, ok := pgPostFromData(data); ok {
			posts = append(posts, post)
		}
	}
	for _, post := range dedupeCatalogPosts(posts) {
		rows = append(rows, struct {
			ID        int64
			Titulo    string
			PosterURL string
		}{int64(post.ID), post.Title, fullImageURL(post.Images.Poster)})
		if len(rows) >= limit {
			break
		}
	}
	return rows
}

// pgCatalogCount returns total movies and series stored in poseidon_catalog.
func pgCatalogCount() (movies, series int) {
	if pgPool == nil {
		return
	}
	pgPool.QueryRow(context.Background(),
		`SELECT COUNT(*) FILTER (WHERE tipo='m'), COUNT(*) FILTER (WHERE tipo='s') FROM poseidon_catalog`,
	).Scan(&movies, &series)
	return
}

func pgVisibleCatalogCount() (movies, series int) {
	if pgPool == nil {
		return
	}
	if _, total, _, ok := pgListCatalogPosts("m", 1, nil); ok {
		movies = total
	}
	if _, total, _, ok := pgListCatalogPosts("s", 1, nil); ok {
		series = total
	}
	return
}

// ── Detail cache (PostgreSQL + Redis) ────────────────────────────────────────

func setCacheBytes(key string, b []byte, ttl time.Duration) {
	b = sanitizeCacheValue(key, b)
	pgSet(key, b)
	rememberAvailabilityFromCacheValue(key, b)
	if !redisAvailable.Load() {
		return
	}
	if err := rdb.Set(rctx, key, b, ttl).Err(); err != nil {
		redisAvailable.Store(false)
		log.Printf("[redis] Error guardando %s: %v", key, err)
	}
}

// getDetailCache returns cached JSON bytes. Checks PostgreSQL first, falls back to Redis.
func getDetailCache(key string) ([]byte, bool) {
	if b, ok := pgGet(key); ok {
		sanitized := sanitizeCacheValue(key, b)
		if string(sanitized) != string(b) {
			pgSet(key, sanitized)
			b = sanitized
		}
		rememberAvailabilityFromCacheValue(key, b)
		if redisAvailable.Load() {
			go rdb.Set(rctx, key, b, detailTTL)
		}
		return b, true
	}

	if !redisAvailable.Load() {
		return nil, false
	}
	b, err := rdb.Get(rctx, key).Bytes()
	if err == nil {
		sanitized := sanitizeCacheValue(key, b)
		if string(sanitized) != string(b) {
			b = sanitized
			go rdb.Set(rctx, key, b, detailTTL)
		}
		pgSet(key, b)
		rememberAvailabilityFromCacheValue(key, b)
		return b, true
	}
	return nil, false
}

// setDetailCache serializes v to JSON and writes to PostgreSQL + Redis.
func setDetailCache(key string, v any, ttl time.Duration) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	setCacheBytes(key, b, ttl)
}

// serveRawJSON writes pre-encoded JSON bytes directly (avoids double marshal).
func serveRawJSON(w http.ResponseWriter, b []byte) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Write(b)
}

// ── HTTP client ───────────────────────────────────────────────────────────────

var httpClient = &http.Client{Timeout: 15 * time.Second}

// sfGroup deduplicates concurrent identical season-episode fetches (thundering herd protection).
var sfGroup singleflight.Group

// noRedirectClient does not follow redirects — used to capture Location headers.
var noRedirectClient = &http.Client{
	Timeout: 10 * time.Second,
	CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

func cvtGet(path string, params map[string]string) ([]byte, error) {
	u, _ := url.Parse(cvtBase + path)
	q := u.Query()
	for k, v := range params {
		q.Set(k, v)
	}
	u.RawQuery = q.Encode()
	req, err := http.NewRequest("GET", u.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

// ── CVT raw structs ───────────────────────────────────────────────────────────

type CvtImages struct {
	Poster   string `json:"poster"`
	Backdrop string `json:"backdrop"`
	Logo     string `json:"logo"`
}

type CvtPost struct {
	ID            int               `json:"_id"`
	Title         string            `json:"title"`
	Slug          string            `json:"slug"`
	Overview      string            `json:"overview"`
	Images        CvtImages         `json:"images"`
	Trailer       string            `json:"trailer"`
	Rating        string            `json:"rating"`
	Genres        []int             `json:"genres"`
	Type          string            `json:"type"`
	ReleaseDate   string            `json:"release_date"`
	Runtime       string            `json:"runtime"`
	OriginalTitle string            `json:"original_title"`
	TMDbID        int               `json:"tmdb_id,omitempty"`
	PHD2Slug      string            `json:"phd2_slug,omitempty"`
	PHD2Only      bool              `json:"phd2_only,omitempty"`
	FlixLatamSlug string            `json:"flixlatam_slug,omitempty"`
	FlixLatamOnly bool              `json:"flixlatam_only,omitempty"`
	ProviderLinks map[string]string `json:"provider_links,omitempty"`
	ProviderOnly  bool              `json:"provider_only,omitempty"`
}

type CvtPagination struct {
	CurrentPage int `json:"current_page"`
	LastPage    int `json:"last_page"`
	PerPage     int `json:"per_page"`
	Total       int `json:"total"`
}

type CvtListData struct {
	Posts      []CvtPost     `json:"posts"`
	Pagination CvtPagination `json:"pagination"`
}

type CvtListResp struct {
	Error bool        `json:"error"`
	Data  CvtListData `json:"data"`
}

type CvtSingleResp struct {
	Error bool    `json:"error"`
	Data  CvtPost `json:"data"`
}

type CvtPlayerItem struct {
	Lang    string `json:"lang"`
	Quality string `json:"quality"`
	URL     string `json:"url"`
}

type CvtPlayerResp struct {
	Error bool            `json:"error"`
	Data  []CvtPlayerItem `json:"data"`
}

type CvtEpisode struct {
	ID            int    `json:"_id"`
	Title         string `json:"title"`
	Slug          string `json:"slug"`
	Overview      string `json:"overview"`
	StillPath     string `json:"still_path"`
	SeasonNumber  int    `json:"season_number"`
	EpisodeNumber int    `json:"episode_number"`
}

type CvtEpisodesResp struct {
	Error bool         `json:"error"`
	Data  []CvtEpisode `json:"data"`
}

// ── PHD2 scraping structs ─────────────────────────────────────────────────────

type Phd2NextData struct {
	Props struct {
		PageProps struct {
			Movies []Phd2ListItem `json:"movies"`
			Pages  int            `json:"pages"`
		} `json:"pageProps"`
	} `json:"props"`
}

type Phd2ListItem struct {
	Titles struct {
		Name string `json:"name"`
	} `json:"titles"`
	Images struct {
		Poster   string `json:"poster"`
		Backdrop string `json:"backdrop"`
	} `json:"images"`
	Rate struct {
		Average float64 `json:"average"`
	} `json:"rate"`
	Overview string `json:"overview"`
	TMDbId   string `json:"TMDbId"`
	Genres   []struct {
		Name string `json:"name"`
	} `json:"genres"`
	Runtime     int    `json:"runtime"`
	ReleaseDate string `json:"releaseDate"`
	URL         struct {
		Slug string `json:"slug"`
	} `json:"url"`
}

type Phd2DetailNextData struct {
	Props struct {
		PageProps struct {
			ThisMovie struct {
				Videos map[string][]Phd2Video `json:"videos"`
			} `json:"thisMovie"`
		} `json:"pageProps"`
	} `json:"props"`
}

type Phd2Video struct {
	Cyberlocker string `json:"cyberlocker"`
	Result      string `json:"result"`
	Quality     string `json:"quality"`
}

type Phd2SerieNextData struct {
	Props struct {
		PageProps struct {
			ThisSerie struct {
				Seasons []Phd2Season `json:"seasons"`
			} `json:"thisSerie"`
		} `json:"pageProps"`
	} `json:"props"`
}

type Phd2Season struct {
	Number   int           `json:"number"`
	Episodes []Phd2Episode `json:"episodes"`
}

type Phd2Episode struct {
	Title  string `json:"title"`
	Number int    `json:"number"`
	Image  string `json:"image"`
}

type Phd2EpisodeNextData struct {
	Props struct {
		PageProps struct {
			Episode struct {
				Videos map[string][]Phd2Video `json:"videos"`
			} `json:"episode"`
		} `json:"pageProps"`
	} `json:"props"`
}

// ── API response structs ──────────────────────────────────────────────────────

type MovieSummary struct {
	ID        int    `json:"id"`
	Titulo    string `json:"titulo"`
	PosterURL string `json:"poster_url"`
}
type MoviePage struct {
	Movies     []MovieSummary `json:"movies"`
	Page       int            `json:"page"`
	PerPage    int            `json:"per_page"`
	Total      int            `json:"total"`
	TotalPages int            `json:"total_pages"`
}
type Server struct {
	ID        int    `json:"id"`
	Idioma    string `json:"idioma"`
	Nombre    string `json:"nombre"`
	Calidad   string `json:"calidad"`
	PlayerURL string `json:"player_url"`
	URL       string `json:"url"`
	EmbedID   string `json:"embed_id"`
}
type MovieDetail struct {
	ID             int      `json:"id"`
	TmdbID         int      `json:"tmdb_id"`
	Titulo         string   `json:"titulo"`
	TituloOriginal string   `json:"titulo_original"`
	PosterURL      string   `json:"poster_url"`
	BannerURL      string   `json:"banner_url"`
	Descripcion    string   `json:"descripcion"`
	Rating         float64  `json:"rating"`
	RuntimeMin     int      `json:"runtime_min"`
	ReleaseDate    string   `json:"release_date"`
	URL            string   `json:"url"`
	Generos        []string `json:"generos"`
	Servidores     []Server `json:"servidores"`
}
type SerieSummary struct {
	ID        int    `json:"id"`
	Titulo    string `json:"titulo"`
	PosterURL string `json:"poster_url"`
}
type SeriePage struct {
	Series     []SerieSummary `json:"series"`
	Page       int            `json:"page"`
	PerPage    int            `json:"per_page"`
	Total      int            `json:"total"`
	TotalPages int            `json:"total_pages"`
}
type SearchResult struct {
	ID        int    `json:"id"`
	Titulo    string `json:"titulo"`
	PosterURL string `json:"poster_url"`
	EsSerie   bool   `json:"es_serie"`
}
type SeasonInfo struct {
	ID             int `json:"id"`
	Number         int `json:"number"`
	TotalEpisodios int `json:"total_episodios"`
}
type SerieDetail struct {
	ID             int          `json:"id"`
	TmdbID         int          `json:"tmdb_id"`
	Titulo         string       `json:"titulo"`
	TituloOriginal string       `json:"titulo_original"`
	PosterURL      string       `json:"poster_url"`
	BannerURL      string       `json:"banner_url"`
	Descripcion    string       `json:"descripcion"`
	Rating         float64      `json:"rating"`
	ReleaseDate    string       `json:"release_date"`
	Generos        []string     `json:"generos"`
	Temporadas     []SeasonInfo `json:"temporadas"`
}
type EpisodeServer struct {
	ID        int    `json:"id"`
	Idioma    string `json:"idioma"`
	Nombre    string `json:"nombre"`
	Calidad   string `json:"calidad"`
	PlayerURL string `json:"player_url"`
	URL       string `json:"url"`
	EmbedID   string `json:"embed_id"`
}
type EpisodeDetail struct {
	ID         int             `json:"id"`
	Number     int             `json:"number"`
	Titulo     string          `json:"titulo"`
	Imagen     string          `json:"imagen"`
	Servidores []EpisodeServer `json:"servidores"`
}
type SeasonDetail struct {
	SerieID   int             `json:"serie_id"`
	Number    int             `json:"number"`
	Episodios []EpisodeDetail `json:"episodios"`
}

func isCamQuality(q string) bool {
	q = strings.ToLower(strings.TrimSpace(q))
	return strings.Contains(q, "cam")
}

func isHDQuality(q string) bool {
	q = strings.ToLower(strings.TrimSpace(q))
	return strings.Contains(q, "hd") ||
		strings.Contains(q, "720") ||
		strings.Contains(q, "1080") ||
		strings.Contains(q, "2160") ||
		strings.Contains(q, "4k")
}

func normalizedServerText(parts ...string) string {
	text := strings.ToLower(strings.Join(parts, " "))
	text = strings.ReplaceAll(text, " ", "")
	text = strings.ReplaceAll(text, "-", "")
	text = strings.ReplaceAll(text, "_", "")
	text = strings.ReplaceAll(text, ".", "")
	return text
}

func isAllowedServerProvider(name, playerURL, finalURL string) bool {
	text := normalizedServerText(name, playerURL, finalURL)
	allowed := []string{
		"streamwish",
		"hlswish",
		"embedwish",
		"voe",
		"voexs",
		"doodstream",
		"goodstream",
		"filemon",
		"filemoon",
		"vimeo",
		"vidhide",
		"streamtape",
		"lulustream",
		"vtube",
		"uploadfox",
		"up2box",
		"mixdrop",
		"uqload",
		"okru",
	}
	for _, provider := range allowed {
		if strings.Contains(text, provider) {
			return true
		}
	}
	return false
}

func serverIdentity(name, playerURL, finalURL, embedID string) string {
	id := finalURL
	if id == "" {
		id = playerURL
	}
	if id == "" {
		id = name + ":" + embedID
	}
	return strings.ToLower(strings.TrimSpace(id))
}

func sanitizeMovieServers(servers []Server) []Server {
	hasHD := false
	for _, s := range servers {
		if isAllowedServerProvider(s.Nombre, s.PlayerURL, s.URL) && isHDQuality(s.Calidad) {
			hasHD = true
			break
		}
	}
	out := make([]Server, 0, len(servers))
	seen := map[string]bool{}
	for _, s := range servers {
		if !isAllowedServerProvider(s.Nombre, s.PlayerURL, s.URL) {
			continue
		}
		if isCamQuality(s.Calidad) {
			if hasHD {
				continue
			}
		}
		key := serverIdentity(s.Nombre, s.PlayerURL, s.URL, s.EmbedID)
		if seen[key] {
			continue
		}
		seen[key] = true
		s.ID = len(out) + 1
		out = append(out, s)
	}
	return out
}

func sanitizeEpisodeServers(servers []EpisodeServer) []EpisodeServer {
	hasHD := false
	for _, s := range servers {
		if isAllowedServerProvider(s.Nombre, s.PlayerURL, s.URL) && isHDQuality(s.Calidad) {
			hasHD = true
			break
		}
	}
	out := make([]EpisodeServer, 0, len(servers))
	seen := map[string]bool{}
	for _, s := range servers {
		if !isAllowedServerProvider(s.Nombre, s.PlayerURL, s.URL) {
			continue
		}
		if isCamQuality(s.Calidad) {
			if hasHD {
				continue
			}
		}
		key := serverIdentity(s.Nombre, s.PlayerURL, s.URL, s.EmbedID)
		if seen[key] {
			continue
		}
		seen[key] = true
		s.ID = len(out) + 1
		out = append(out, s)
	}
	return out
}

func sanitizeMovieDetail(detail MovieDetail) MovieDetail {
	detail.Servidores = sanitizeMovieServers(detail.Servidores)
	return detail
}

func sanitizeSeasonDetail(detail SeasonDetail) SeasonDetail {
	for i := range detail.Episodios {
		detail.Episodios[i].Servidores = sanitizeEpisodeServers(detail.Episodios[i].Servidores)
	}
	return detail
}

func seasonHasAllowedServers(detail SeasonDetail) bool {
	detail = sanitizeSeasonDetail(detail)
	for _, ep := range detail.Episodios {
		if len(ep.Servidores) > 0 {
			return true
		}
	}
	return false
}

func movieDetailHasHD(b []byte) bool {
	var detail MovieDetail
	if json.Unmarshal(b, &detail) != nil {
		return false
	}
	detail = sanitizeMovieDetail(detail)
	for _, s := range detail.Servidores {
		if isHDQuality(s.Calidad) {
			return true
		}
	}
	return false
}

func seasonDetailHasHD(b []byte) bool {
	var detail SeasonDetail
	if json.Unmarshal(b, &detail) != nil {
		return false
	}
	detail = sanitizeSeasonDetail(detail)
	for _, ep := range detail.Episodios {
		for _, s := range ep.Servidores {
			if isHDQuality(s.Calidad) {
				return true
			}
		}
	}
	return false
}

func seasonDetailNeedsRefresh(b []byte) bool {
	var detail SeasonDetail
	if json.Unmarshal(b, &detail) != nil {
		return true
	}
	detail = sanitizeSeasonDetail(detail)
	if len(detail.Episodios) == 0 {
		return true
	}
	for _, ep := range detail.Episodios {
		if len(ep.Servidores) == 0 {
			return true
		}
		hasHD := false
		for _, s := range ep.Servidores {
			if isHDQuality(s.Calidad) {
				hasHD = true
				break
			}
		}
		if !hasHD {
			return true
		}
	}
	return false
}

func storeValueStale(key string, maxAge time.Duration) bool {
	updatedAt, ok := pgStoreUpdatedAt(key)
	if !ok {
		return true
	}
	return time.Since(updatedAt) >= maxAge
}

func shouldRefreshMovieDetail(key string, b []byte) bool {
	if !movieDetailHasServers(b) {
		return true
	}
	if !movieDetailHasHD(b) {
		return true
	}
	return storeValueStale(key, hdRefreshTTL)
}

func shouldRefreshSeasonDetail(key string, b []byte) bool {
	if !seasonDetailHasServers(b) {
		return true
	}
	if !seasonDetailHasHD(b) || seasonDetailNeedsRefresh(b) {
		return true
	}
	return storeValueStale(key, hdRefreshTTL)
}

func sanitizeCacheValue(key string, b []byte) []byte {
	switch {
	case strings.HasPrefix(key, "poseidon:movie:"):
		var detail MovieDetail
		if json.Unmarshal(b, &detail) != nil {
			return b
		}
		out, err := json.Marshal(sanitizeMovieDetail(detail))
		if err != nil {
			return b
		}
		return out
	case strings.HasPrefix(key, "poseidon:season:"):
		var detail SeasonDetail
		if json.Unmarshal(b, &detail) != nil {
			return b
		}
		out, err := json.Marshal(sanitizeSeasonDetail(detail))
		if err != nil {
			return b
		}
		return out
	default:
		return b
	}
}

// ── Server availability index ────────────────────────────────────────────────

var serverAvailability sync.Map

// flixlatamMovieByTitle maps normalizedTitle+"|"+year → FlixLatam slug (populated during catalog build)
var flixlatamMovieByTitle sync.Map

func availabilityKey(tipo string, id int) string {
	return tipo + ":" + strconv.Itoa(id)
}

func rememberServerAvailability(tipo string, id int, hasServers bool) {
	if id <= 0 {
		return
	}
	serverAvailability.Store(availabilityKey(tipo, id), hasServers)
}

func cachedServerAvailability(tipo string, id int) (bool, bool) {
	v, ok := serverAvailability.Load(availabilityKey(tipo, id))
	if !ok {
		return false, false
	}
	hasServers, _ := v.(bool)
	return hasServers, true
}

func cacheKeyID(key, prefix string) (int, bool) {
	if !strings.HasPrefix(key, prefix) {
		return 0, false
	}
	rest := strings.TrimPrefix(key, prefix)
	if before, _, ok := strings.Cut(rest, ":"); ok {
		rest = before
	}
	id, err := strconv.Atoi(rest)
	return id, err == nil
}

func movieDetailHasServers(b []byte) bool {
	var detail MovieDetail
	if json.Unmarshal(b, &detail) != nil {
		return false
	}
	detail = sanitizeMovieDetail(detail)
	return len(detail.Servidores) > 0
}

func seasonDetailHasServers(b []byte) bool {
	var detail SeasonDetail
	if json.Unmarshal(b, &detail) != nil {
		return false
	}
	return seasonHasAllowedServers(detail)
}

func serieSeasonNumbers(b []byte) []int {
	var detail SerieDetail
	if json.Unmarshal(b, &detail) != nil {
		return nil
	}
	nums := make([]int, 0, len(detail.Temporadas))
	for _, t := range detail.Temporadas {
		if t.Number > 0 {
			nums = append(nums, t.Number)
		}
	}
	return nums
}

func rememberAvailabilityFromCacheValue(key string, b []byte) {
	if id, ok := cacheKeyID(key, "poseidon:movie:"); ok {
		hasServers := movieDetailHasServers(b)
		rememberServerAvailability("m", id, hasServers)
		pgSetCatalogHasServers("m", id, hasServers)
		return
	}
	if id, ok := cacheKeyID(key, "poseidon:season:"); ok {
		if hasServers, found := pgSerieHasServers(id); found {
			rememberServerAvailability("s", id, hasServers)
			pgSetCatalogHasServers("s", id, hasServers)
			return
		}
		hasServers := seasonDetailHasServers(b)
		rememberServerAvailability("s", id, hasServers)
		pgSetCatalogHasServers("s", id, hasServers)
	}
}

func loadAvailabilityFromStore() {
	if pgPool == nil {
		return
	}
	rows, err := pgPool.Query(context.Background(), `
		SELECT key, value
		FROM poseidon_store
		WHERE key LIKE 'poseidon:movie:%'
		   OR key LIKE 'poseidon:season:%'
	`)
	if err != nil {
		log.Printf("[catalog] No se pudo cargar disponibilidad de servidores: %v", err)
		return
	}
	defer rows.Close()

	moviesWithServers := 0
	moviesWithoutServers := 0
	seriesWithServers := map[int]bool{}
	for rows.Next() {
		var key string
		var value []byte
		if rows.Scan(&key, &value) != nil {
			continue
		}
		if id, ok := cacheKeyID(key, "poseidon:movie:"); ok {
			hasServers := movieDetailHasServers(value)
			rememberServerAvailability("m", id, hasServers)
			if hasServers {
				moviesWithServers++
			} else {
				moviesWithoutServers++
			}
			continue
		}
		if id, ok := cacheKeyID(key, "poseidon:season:"); ok && seasonDetailHasServers(value) {
			rememberServerAvailability("s", id, true)
			seriesWithServers[id] = true
		}
	}
	log.Printf("[catalog] Disponibilidad cargada: %d pelis con servidores, %d pelis sin servidores, %d series con servidores",
		moviesWithServers, moviesWithoutServers, len(seriesWithServers))
}

func cleanupStoredServerQualities() {
	if pgPool == nil {
		return
	}
	rows, err := pgPool.Query(context.Background(), `
		SELECT key, value
		FROM poseidon_store
		WHERE key LIKE 'poseidon:movie:%'
		   OR key LIKE 'poseidon:season:%'
	`)
	if err != nil {
		log.Printf("[catalog] No se pudo limpiar calidades guardadas: %v", err)
		return
	}
	defer rows.Close()

	type entry struct {
		key   string
		value []byte
	}
	var entries []entry
	for rows.Next() {
		var e entry
		if rows.Scan(&e.key, &e.value) != nil {
			continue
		}
		entries = append(entries, e)
	}

	updated := 0
	for _, e := range entries {
		sanitized := sanitizeCacheValue(e.key, e.value)
		if string(sanitized) == string(e.value) {
			continue
		}
		pgSet(e.key, sanitized)
		if redisAvailable.Load() {
			_ = rdb.Set(rctx, e.key, sanitized, cacheTTLForKey(e.key)).Err()
		}
		updated++
	}
	if updated > 0 {
		log.Printf("[catalog] Limpieza de calidades aplicada: %d registros actualizados", updated)
	}
}

func rebuildCatalogAvailabilityFromStore() {
	if pgPool == nil {
		return
	}
	if _, err := pgPool.Exec(context.Background(), `
		UPDATE poseidon_catalog
		SET has_servers=FALSE, updated_at=NOW()
	`); err != nil {
		log.Printf("[catalog] No se pudo reiniciar disponibilidad: %v", err)
		return
	}

	rows, err := pgPool.Query(context.Background(), `
		SELECT key, value
		FROM poseidon_store
		WHERE key LIKE 'poseidon:movie:%'
		   OR key LIKE 'poseidon:season:%'
	`)
	if err != nil {
		log.Printf("[catalog] No se pudo recalcular disponibilidad: %v", err)
		return
	}
	defer rows.Close()

	moviesWithServers := 0
	seriesWithServers := map[int]bool{}
	for rows.Next() {
		var key string
		var value []byte
		if rows.Scan(&key, &value) != nil {
			continue
		}
		if id, ok := cacheKeyID(key, "poseidon:movie:"); ok {
			hasServers := movieDetailHasServers(value)
			rememberServerAvailability("m", id, hasServers)
			if hasServers {
				pgSetCatalogHasServers("m", id, true)
				moviesWithServers++
			}
			continue
		}
		if id, ok := cacheKeyID(key, "poseidon:season:"); ok && seasonDetailHasServers(value) {
			seriesWithServers[id] = true
		}
	}
	for id := range seriesWithServers {
		rememberServerAvailability("s", id, true)
		pgSetCatalogHasServers("s", id, true)
	}
	log.Printf("[catalog] Disponibilidad recalculada: %d pelis con servidores, %d series con servidores",
		moviesWithServers, len(seriesWithServers))
}

func cacheTTLForKey(key string) time.Duration {
	if strings.HasPrefix(key, "poseidon:season:") ||
		strings.HasPrefix(key, "phd2:seasons:") ||
		strings.HasPrefix(key, "flixlatam:seasons:") {
		return seasonTTL
	}
	return detailTTL
}

func syncRedisStoreToPostgres() {
	if pgPool == nil || !redisAvailable.Load() {
		return
	}

	patterns := []string{
		"poseidon:*",
		"phd2:seasons:*",
		"flixlatam:seasons:*",
	}
	seen := map[string]bool{}
	scanned := 0
	synced := 0
	skipped := 0

	for _, pattern := range patterns {
		iter := rdb.Scan(rctx, 0, pattern, 1000).Iterator()
		for iter.Next(rctx) {
			key := iter.Val()
			if seen[key] {
				continue
			}
			seen[key] = true
			scanned++

			b, err := rdb.Get(rctx, key).Bytes()
			if err != nil || !json.Valid(b) {
				skipped++
				continue
			}
			if strings.HasPrefix(key, "poseidon:movie:") ||
				strings.HasPrefix(key, "poseidon:season:") {
				sanitized := sanitizeCacheValue(key, b)
				pgSet(key, sanitized)
				if string(sanitized) != string(b) {
					_ = rdb.Set(rctx, key, sanitized, cacheTTLForKey(key)).Err()
				}
			} else {
				pgSet(key, b)
			}
			synced++
		}
		if err := iter.Err(); err != nil {
			log.Printf("[redis] Error sincronizando %s hacia PostgreSQL: %v", pattern, err)
		}
	}
	log.Printf("[redis] Sincronizado hacia PostgreSQL: %d claves guardadas, %d omitidas, %d revisadas", synced, skipped, scanned)
}

func loadAvailabilityFromRedis() {
	if !redisAvailable.Load() {
		return
	}
	moviesWithServers := 0
	moviesWithoutServers := 0
	seriesWithServers := map[int]bool{}
	loadPattern := func(pattern string) {
		iter := rdb.Scan(rctx, 0, pattern, 1000).Iterator()
		for iter.Next(rctx) {
			key := iter.Val()
			b, err := rdb.Get(rctx, key).Bytes()
			if err != nil {
				continue
			}
			if id, ok := cacheKeyID(key, "poseidon:movie:"); ok {
				hasServers := movieDetailHasServers(b)
				rememberServerAvailability("m", id, hasServers)
				if hasServers {
					moviesWithServers++
				} else {
					moviesWithoutServers++
				}
				continue
			}
			if id, ok := cacheKeyID(key, "poseidon:season:"); ok && seasonDetailHasServers(b) {
				rememberServerAvailability("s", id, true)
				seriesWithServers[id] = true
			}
		}
		if err := iter.Err(); err != nil {
			log.Printf("[redis] Error cargando disponibilidad (%s): %v", pattern, err)
		}
	}
	loadPattern("poseidon:movie:*")
	loadPattern("poseidon:season:*")
	log.Printf("[redis] Disponibilidad cargada: %d pelis con servidores, %d pelis sin servidores, %d series con servidores",
		moviesWithServers, moviesWithoutServers, len(seriesWithServers))
}

func movieHasServers(id int) bool {
	if hasServers, ok := cachedServerAvailability("m", id); ok {
		return hasServers
	}
	if pgPool != nil {
		key := "poseidon:movie:" + strconv.Itoa(id)
		if b, ok := pgGet(key); ok {
			hasServers := movieDetailHasServers(b)
			rememberServerAvailability("m", id, hasServers)
			return hasServers
		}
	}
	rememberServerAvailability("m", id, false)
	return false
}

func pgSerieHasServers(id int) (bool, bool) {
	if pgPool == nil {
		return false, false
	}
	rows, err := pgPool.Query(context.Background(),
		"SELECT value FROM poseidon_store WHERE key LIKE $1",
		fmt.Sprintf("poseidon:season:%d:%%", id),
	)
	if err != nil {
		return false, false
	}
	defer rows.Close()

	found := false
	for rows.Next() {
		found = true
		var value []byte
		if rows.Scan(&value) != nil {
			continue
		}
		if seasonDetailHasServers(value) {
			return true, true
		}
	}
	return false, found
}

func serieHasServers(id int) bool {
	if hasServers, ok := cachedServerAvailability("s", id); ok {
		return hasServers
	}
	if hasServers, found := pgSerieHasServers(id); found {
		rememberServerAvailability("s", id, hasServers)
		return hasServers
	}
	if pgPool != nil {
		detailKey := "poseidon:serie:" + strconv.Itoa(id)
		b, ok := pgGet(detailKey)
		if !ok {
			rememberServerAvailability("s", id, false)
			return false
		}
		for _, seasonNum := range serieSeasonNumbers(b) {
			seasonKey := "poseidon:season:" + strconv.Itoa(id) + ":" + strconv.Itoa(seasonNum)
			if sb, ok := pgGet(seasonKey); ok && seasonDetailHasServers(sb) {
				rememberServerAvailability("s", id, true)
				return true
			}
		}
	}
	rememberServerAvailability("s", id, false)
	return false
}

func visibleMoviePosts(posts []CvtPost) []CvtPost {
	out := make([]CvtPost, 0, len(posts))
	for _, p := range posts {
		if movieHasServers(p.ID) {
			out = append(out, p)
		}
	}
	return out
}

func visibleSeriePosts(posts []CvtPost) []CvtPost {
	out := make([]CvtPost, 0, len(posts))
	for _, p := range posts {
		if serieHasServers(p.ID) {
			out = append(out, p)
		}
	}
	return out
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func json200(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	json.NewEncoder(w).Encode(v)
}

func jsonErr(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func pageParam(r *http.Request) int {
	p, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if p < 1 {
		return 1
	}
	return p
}

// fullImageURL handles both CVT paths (/uploads/...) and already-full PHD2 URLs.
func fullImageURL(path string) string {
	if path == "" {
		return ""
	}
	if strings.HasPrefix(path, "http") {
		return path
	}
	return imageBase + path
}

func parseRating(s string) float64 {
	f, _ := strconv.ParseFloat(s, 64)
	return f
}

func parseRuntime(s string) int {
	f, _ := strconv.ParseFloat(s, 64)
	return int(f)
}

func resolveGenres(ids []int) []string {
	names := make([]string, 0, len(ids))
	for _, id := range ids {
		if name, ok := genreNames[id]; ok {
			names = append(names, name)
		}
	}
	return names
}

func serverName(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil || u.Hostname() == "" {
		return "Server"
	}
	host := strings.TrimPrefix(u.Hostname(), "www.")
	parts := strings.Split(host, ".")
	name := parts[0]
	if len(name) == 0 {
		return "Server"
	}
	return strings.ToUpper(name[:1]) + name[1:]
}

func releaseDate(s string) string {
	if len(s) >= 10 {
		return s[:10]
	}
	return ""
}

func cvtToServers(items []CvtPlayerItem) []Server {
	out := make([]Server, len(items))
	for i, item := range items {
		out[i] = Server{
			ID: i + 1, Idioma: item.Lang, Nombre: serverName(item.URL),
			Calidad: item.Quality, PlayerURL: item.URL, URL: item.URL,
		}
	}
	return sanitizeMovieServers(out)
}

func cvtToEpServers(items []CvtPlayerItem) []EpisodeServer {
	out := make([]EpisodeServer, len(items))
	for i, item := range items {
		out[i] = EpisodeServer{
			ID: i + 1, Idioma: item.Lang, Nombre: serverName(item.URL),
			Calidad: item.Quality, PlayerURL: item.URL, URL: item.URL,
		}
	}
	return sanitizeEpisodeServers(out)
}

func fetchPlayer(postID int, postType string) []CvtPlayerItem {
	data, err := cvtGet("/player", map[string]string{
		"post_id":   strconv.Itoa(postID),
		"post_type": postType,
	})
	if err != nil {
		return nil
	}
	var resp CvtPlayerResp
	if json.Unmarshal(data, &resp) != nil || resp.Error {
		return nil
	}
	return resp.Data
}

func livePage(postType string, page int) ([]CvtPost, int, int, error) {
	data, err := cvtGet("/listing", map[string]string{
		"post_type": postType,
		"page":      strconv.Itoa(page),
	})
	if err != nil {
		return nil, 0, 0, err
	}
	var resp CvtListResp
	if json.Unmarshal(data, &resp) != nil || resp.Error {
		return nil, 0, 0, fmt.Errorf("upstream error")
	}
	posts := resp.Data.Posts
	sort.Slice(posts, func(i, j int) bool {
		return posts[i].ReleaseDate > posts[j].ReleaseDate
	})
	for _, p := range posts {
		cachePost(p.ID, p.Slug)
	}
	return posts, resp.Data.Pagination.Total, resp.Data.Pagination.LastPage, nil
}

func livePageWithParams(postType string, page int, extra map[string]string) ([]CvtPost, int, int, error) {
	params := map[string]string{"post_type": postType, "page": strconv.Itoa(page)}
	for k, v := range extra {
		params[k] = v
	}
	data, err := cvtGet("/listing", params)
	if err != nil {
		return nil, 0, 0, err
	}
	var resp CvtListResp
	if json.Unmarshal(data, &resp) != nil || resp.Error {
		return nil, 0, 0, fmt.Errorf("upstream error")
	}
	posts := resp.Data.Posts
	sort.Slice(posts, func(i, j int) bool { return posts[i].ReleaseDate > posts[j].ReleaseDate })
	for _, p := range posts {
		cachePost(p.ID, p.Slug)
	}
	return posts, resp.Data.Pagination.Total, resp.Data.Pagination.LastPage, nil
}

func stripTrailingSlash(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" && strings.HasSuffix(r.URL.Path, "/") {
			r.URL.Path = strings.TrimRight(r.URL.Path, "/")
			http.Redirect(w, r, r.URL.String(), http.StatusMovedPermanently)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func pageFromList(items []CvtPost, page int) ([]CvtPost, int) {
	start := (page - 1) * perPage
	if start >= len(items) {
		return nil, len(items)
	}
	end := start + perPage
	if end > len(items) {
		end = len(items)
	}
	return items[start:end], len(items)
}

func calcTotalPages(total int) int {
	if total == 0 {
		return 1
	}
	t := total / perPage
	if total%perPage != 0 {
		t++
	}
	return t
}

func resolveGenresContains(ids []int, name string) bool {
	for _, id := range ids {
		if genreNames[id] == name {
			return true
		}
	}
	return false
}

// ── PHD2 scraper ──────────────────────────────────────────────────────────────

func scrapeNextData(pageURL string) ([]byte, error) {
	req, err := http.NewRequest("GET", pageURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	html := string(body)
	marker := `id="__NEXT_DATA__"`
	idx := strings.Index(html, marker)
	if idx < 0 {
		return nil, fmt.Errorf("__NEXT_DATA__ no encontrado en %s", pageURL)
	}
	start := strings.Index(html[idx:], ">")
	if start < 0 {
		return nil, fmt.Errorf("tag __NEXT_DATA__ malformado")
	}
	start += idx + 1
	end := strings.Index(html[start:], "</script>")
	if end < 0 {
		return nil, fmt.Errorf("script __NEXT_DATA__ sin cerrar")
	}
	return []byte(html[start : start+end]), nil
}

func phd2ItemToPost(item Phd2ListItem, isSeries bool) CvtPost {
	tmdbID, _ := strconv.Atoi(item.TMDbId)

	var genres []int
	for _, g := range item.Genres {
		if id, ok := phd2GenreToID[g.Name]; ok {
			genres = append(genres, id)
		}
	}

	// PHD2 slugs are like "movies/936075/michael" or "series/225891/the-madison"
	// Extract just the last segment for use in detail URLs
	slug := item.URL.Slug
	if parts := strings.Split(slug, "/"); len(parts) > 0 {
		slug = parts[len(parts)-1]
	}

	rd := item.ReleaseDate
	if len(rd) > 10 {
		rd = rd[:10]
	}

	return CvtPost{
		ID:          tmdbID,
		Title:       item.Titles.Name,
		Slug:        slug,
		Overview:    item.Overview,
		Images:      CvtImages{Poster: item.Images.Poster, Backdrop: item.Images.Backdrop},
		Rating:      strconv.FormatFloat(item.Rate.Average, 'f', 1, 64),
		Genres:      genres,
		ReleaseDate: rd,
		Runtime:     strconv.Itoa(item.Runtime),
		TMDbID:      tmdbID,
		PHD2Slug:    slug,
		PHD2Only:    true,
	}
}

func fetchAllPHD2Pages(listingPath string, isSeries bool) []CvtPost {
	data, err := scrapeNextData(phd2Base + listingPath + "1")
	if err != nil {
		log.Printf("[phd2] Error página 1 (%s): %v", listingPath, err)
		return nil
	}
	var firstND Phd2NextData
	if err := json.Unmarshal(data, &firstND); err != nil {
		log.Printf("[phd2] Error parseando página 1: %v", err)
		return nil
	}
	totalPages := firstND.Props.PageProps.Pages
	if totalPages <= 0 {
		totalPages = 1
	}
	log.Printf("[phd2] %s: %d páginas", listingPath, totalPages)

	allItems := make([][]CvtPost, totalPages)
	for _, item := range firstND.Props.PageProps.Movies {
		allItems[0] = append(allItems[0], phd2ItemToPost(item, isSeries))
	}

	for batchStart := 2; batchStart <= totalPages; batchStart += phd2BatchSize {
		batchEnd := batchStart + phd2BatchSize - 1
		if batchEnd > totalPages {
			batchEnd = totalPages
		}
		var wg sync.WaitGroup
		for page := batchStart; page <= batchEnd; page++ {
			wg.Add(1)
			go func(p int) {
				defer wg.Done()
				d, err := scrapeNextData(phd2Base + listingPath + strconv.Itoa(p))
				if err != nil {
					return
				}
				var nd Phd2NextData
				if json.Unmarshal(d, &nd) != nil {
					return
				}
				var posts []CvtPost
				for _, item := range nd.Props.PageProps.Movies {
					posts = append(posts, phd2ItemToPost(item, isSeries))
				}
				allItems[p-1] = posts
			}(page)
		}
		wg.Wait()
		time.Sleep(buildDelay)
	}

	var flat []CvtPost
	for _, batch := range allItems {
		flat = append(flat, batch...)
	}
	log.Printf("[phd2] %s: %d items descargados", listingPath, len(flat))
	return flat
}

// ── Title normalization + merge ───────────────────────────────────────────────

func normalizeTitle(s string) string {
	s = strings.ToLower(s)
	s = strings.NewReplacer(
		"á", "a", "à", "a", "ä", "a", "â", "a",
		"é", "e", "è", "e", "ë", "e", "ê", "e",
		"í", "i", "ì", "i", "ï", "i", "î", "i",
		"ó", "o", "ò", "o", "ö", "o", "ô", "o",
		"ú", "u", "ù", "u", "ü", "u", "û", "u",
		"ñ", "n",
	).Replace(s)
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == ' ' {
			b.WriteRune(r)
		}
	}
	words := strings.Fields(b.String())
	if len(words) > 1 && isYearToken(words[len(words)-1]) {
		words = words[:len(words)-1]
	}
	return strings.Join(words, " ")
}

func yearOf(date string) string {
	if len(date) >= 4 {
		return date[:4]
	}
	return ""
}

func isYearToken(s string) bool {
	if len(s) != 4 {
		return false
	}
	year, err := strconv.Atoi(s)
	return err == nil && year >= 1900 && year <= 2100
}

func catalogDedupeKey(p CvtPost) string {
	title := normalizeTitle(p.Title)
	year := yearOf(p.ReleaseDate)
	if title == "" {
		return "id:" + strconv.Itoa(p.ID)
	}
	return title + "|" + year
}

func dedupeCatalogPosts(posts []CvtPost) []CvtPost {
	out := make([]CvtPost, 0, len(posts))
	seen := map[string]bool{}
	for _, p := range posts {
		key := catalogDedupeKey(p)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, p)
	}
	return out
}

func mergeDuplicatePost(base, extra CvtPost) CvtPost {
	if base.TMDbID == 0 && extra.TMDbID > 0 {
		base.TMDbID = extra.TMDbID
	}
	if base.PHD2Slug == "" && extra.PHD2Slug != "" {
		base.PHD2Slug = extra.PHD2Slug
	}
	if base.FlixLatamSlug == "" && extra.FlixLatamSlug != "" {
		base.FlixLatamSlug = extra.FlixLatamSlug
	}
	if base.Slug == "" && extra.Slug != "" {
		base.Slug = extra.Slug
	}
	if base.Images.Poster == "" && extra.Images.Poster != "" {
		base.Images.Poster = extra.Images.Poster
	}
	if base.Images.Backdrop == "" && extra.Images.Backdrop != "" {
		base.Images.Backdrop = extra.Images.Backdrop
	}
	base.ProviderLinks = mergeProviderLinks(base.ProviderLinks, extra.ProviderLinks)
	if base.ProviderOnly && !extra.ProviderOnly {
		base.ProviderOnly = false
	}
	return base
}

func dedupeMergedPosts(posts []CvtPost) []CvtPost {
	out := make([]CvtPost, 0, len(posts))
	seen := map[string]int{}
	for _, p := range posts {
		key := catalogDedupeKey(p)
		if idx, ok := seen[key]; ok {
			out[idx] = mergeDuplicatePost(out[idx], p)
			continue
		}
		seen[key] = len(out)
		out = append(out, p)
	}
	return out
}

// mergePostLists merges CVT + PHD2 by title+year (or TMDbID if already set).
// Matched CVT posts get PHD2Slug and TMDbID. Unmatched PHD2 posts appended as PHD2Only.
func mergePostLists(cvt, phd2 []CvtPost) []CvtPost {
	phd2ByTitle := make(map[string]int)
	phd2ByTitleOnly := make(map[string]int)
	titleOnlyCounts := make(map[string]int)
	phd2ByTMDb := make(map[int]int)
	for i, p := range phd2 {
		title := normalizeTitle(p.Title)
		year := yearOf(p.ReleaseDate)
		key := title + "|" + year
		if key != "|" {
			phd2ByTitle[key] = i
		}
		if title != "" {
			phd2ByTitleOnly[title] = i
			titleOnlyCounts[title]++
		}
		if p.TMDbID > 0 {
			phd2ByTMDb[p.TMDbID] = i
		}
	}

	matched := make([]bool, len(phd2))
	result := make([]CvtPost, 0, len(cvt)+len(phd2))

	for _, post := range cvt {
		enriched := post
		// Try TMDbID match first (works on second run when CVT posts already have TMDbID)
		if post.TMDbID > 0 {
			if idx, ok := phd2ByTMDb[post.TMDbID]; ok {
				enriched = mergeDuplicatePost(enriched, phd2[idx])
				matched[idx] = true
				result = append(result, enriched)
				continue
			}
		}
		// Title+year match
		title := normalizeTitle(post.Title)
		key := title + "|" + yearOf(post.ReleaseDate)
		if idx, ok := phd2ByTitle[key]; ok {
			enriched = mergeDuplicatePost(enriched, phd2[idx])
			matched[idx] = true
		} else if idx, ok := phd2ByTitleOnly[title]; ok && titleOnlyCounts[title] == 1 && (yearOf(post.ReleaseDate) == "" || yearOf(phd2[idx].ReleaseDate) == "") {
			enriched = mergeDuplicatePost(enriched, phd2[idx])
			matched[idx] = true
		}
		result = append(result, enriched)
	}

	for i, p := range phd2 {
		if !matched[i] {
			result = append(result, p)
		}
	}
	return dedupeMergedPosts(result)
}

// ── PHD2 server fetcher ───────────────────────────────────────────────────────

// resolvePlayerURL resolves a PHD2 proxy URL to the real embed URL.
// The proxy either sends a 3xx redirect (Location header) or HTML with: var url = '...';
func resolvePlayerURL(playerURL string) (embedURL, embedID string) {
	req, err := http.NewRequest("GET", playerURL, nil)
	if err != nil {
		return playerURL, ""
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := noRedirectClient.Do(req)
	if err != nil {
		return playerURL, ""
	}
	defer resp.Body.Close()

	// Redirect: grab Location header directly
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		loc := resp.Header.Get("Location")
		if loc == "" {
			return playerURL, ""
		}
		parts := strings.Split(strings.TrimRight(loc, "/"), "/")
		return loc, parts[len(parts)-1]
	}

	// 200: parse HTML for: var url = '...';
	body, _ := io.ReadAll(resp.Body)
	html := string(body)
	marker := "var url = '"
	idx := strings.Index(html, marker)
	if idx < 0 {
		return playerURL, ""
	}
	rest := html[idx+len(marker):]
	end := strings.Index(rest, "'")
	if end < 0 {
		return playerURL, ""
	}
	embedURL = rest[:end]
	parts := strings.Split(strings.TrimRight(embedURL, "/"), "/")
	embedID = parts[len(parts)-1]
	return embedURL, embedID
}

func fetchPHD2Servers(tmdbID int, slug string, isSeries bool) []Server {
	path := "/pelicula/" + strconv.Itoa(tmdbID) + "/" + slug
	if isSeries {
		path = "/serie/" + strconv.Itoa(tmdbID) + "/" + slug
	}
	data, err := scrapeNextData(phd2Base + path)
	if err != nil {
		return nil
	}
	var nd Phd2DetailNextData
	if json.Unmarshal(data, &nd) != nil {
		return nil
	}
	videos := nd.Props.PageProps.ThisMovie.Videos
	if len(videos) == 0 {
		return nil
	}

	// Preferred order then remaining
	langs := make([]string, 0, len(videos))
	preferred := []string{"latino", "subtitulado", "english"}
	seen := make(map[string]bool)
	for _, l := range preferred {
		if _, ok := videos[l]; ok {
			langs = append(langs, l)
			seen[l] = true
		}
	}
	for l := range videos {
		if !seen[l] {
			langs = append(langs, l)
		}
	}

	var servers []Server
	id := 1
	for _, lang := range langs {
		for _, v := range videos[lang] {
			nombre := v.Cyberlocker
			if len(nombre) > 0 {
				nombre = strings.ToUpper(nombre[:1]) + nombre[1:]
			}
			servers = append(servers, Server{
				ID:        id,
				Idioma:    lang,
				Nombre:    nombre,
				Calidad:   v.Quality,
				PlayerURL: v.Result,
			})
			id++
		}
	}

	// Resolve proxy URLs to real embed URLs in parallel
	var wg sync.WaitGroup
	wg.Add(len(servers))
	for i := range servers {
		go func(i int) {
			defer wg.Done()
			embedURL, embedID := resolvePlayerURL(servers[i].PlayerURL)
			servers[i].PlayerURL = embedURL
			servers[i].URL = embedURL
			servers[i].EmbedID = embedID
		}(i)
	}
	wg.Wait()

	return sanitizeMovieServers(servers)
}

func fetchPHD2SerieSeasons(tmdbID int, slug string) ([]Phd2Season, error) {
	key := fmt.Sprintf("phd2:seasons:%d", tmdbID)
	if b, ok := getDetailCache(key); ok {
		var seasons []Phd2Season
		if json.Unmarshal(b, &seasons) == nil {
			return seasons, nil
		}
	}
	path := fmt.Sprintf("/serie/%d/%s", tmdbID, slug)
	data, err := scrapeNextData(phd2Base + path)
	if err != nil {
		return nil, err
	}
	var nd Phd2SerieNextData
	if err := json.Unmarshal(data, &nd); err != nil {
		return nil, err
	}
	// Filter out season 0 and empty seasons
	var seasons []Phd2Season
	for _, s := range nd.Props.PageProps.ThisSerie.Seasons {
		if s.Number > 0 && len(s.Episodes) > 0 {
			seasons = append(seasons, s)
		}
	}
	if b, err := json.Marshal(seasons); err == nil {
		setCacheBytes(key, b, detailTTL)
	}
	return seasons, nil
}

func fetchPHD2EpisodeServers(tmdbID int, slug string, season, episode int) []EpisodeServer {
	path := fmt.Sprintf("/serie/%d/%s/temporada/%d/episodio/%d", tmdbID, slug, season, episode)
	data, err := scrapeNextData(phd2Base + path)
	if err != nil {
		return nil
	}
	var nd Phd2EpisodeNextData
	if json.Unmarshal(data, &nd) != nil {
		return nil
	}
	videos := nd.Props.PageProps.Episode.Videos
	if len(videos) == 0 {
		return nil
	}

	langs := make([]string, 0, len(videos))
	preferred := []string{"latino", "subtitulado", "english"}
	seen := make(map[string]bool)
	for _, l := range preferred {
		if _, ok := videos[l]; ok {
			langs = append(langs, l)
			seen[l] = true
		}
	}
	for l := range videos {
		if !seen[l] {
			langs = append(langs, l)
		}
	}

	var raw []EpisodeServer
	id := 1
	for _, lang := range langs {
		for _, v := range videos[lang] {
			nombre := v.Cyberlocker
			if len(nombre) > 0 {
				nombre = strings.ToUpper(nombre[:1]) + nombre[1:]
			}
			raw = append(raw, EpisodeServer{
				ID:        id,
				Idioma:    lang,
				Nombre:    nombre,
				Calidad:   v.Quality,
				PlayerURL: v.Result,
			})
			id++
		}
	}

	// Resolve proxy URLs in parallel
	var wg sync.WaitGroup
	wg.Add(len(raw))
	for i := range raw {
		go func(i int) {
			defer wg.Done()
			embedURL, embedID := resolvePlayerURL(raw[i].PlayerURL)
			raw[i].PlayerURL = embedURL
			raw[i].URL = embedURL
			raw[i].EmbedID = embedID
		}(i)
	}
	wg.Wait()
	return sanitizeEpisodeServers(raw)
}

// ── FlixLatam scraper ─────────────────────────────────────────────────────────

var flixlatamGenreSlugToID = map[string]int{
	"accion":          26,
	"animacion":       51,
	"aventura":        25,
	"belica":          158,
	"ciencia-ficcion": 27,
	"comedia":         192,
	"crimen":          136,
	"documental":      23209,
	"drama":           157,
	"fantasia":        86,
	"familia":         52,
	"guerra":          158,
	"historia":        404,
	"romance":         215,
	"suspense":        87,
	"terror":          422,
	"western":         1594,
	"misterio":        249,
	"sci-fi-fantasy":  9334,
	"dorama":          157,
}

// slugToID generates a stable numeric ID from a slug using FNV-32a.
func slugToID(s string) int {
	h := uint32(2166136261)
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return int(h>>1) + 1
}

// extractBetween returns the text between two string markers in s.
func extractBetween(s, start, end string) string {
	si := strings.Index(s, start)
	if si < 0 {
		return ""
	}
	s = s[si+len(start):]
	ei := strings.Index(s, end)
	if ei < 0 {
		return ""
	}
	return strings.TrimSpace(s[:ei])
}

// stripTags removes all HTML tags from s.
func stripTags(s string) string {
	var b strings.Builder
	inTag := false
	for _, c := range s {
		if c == '<' {
			inTag = true
		} else if c == '>' {
			inTag = false
		} else if !inTag {
			b.WriteRune(c)
		}
	}
	return strings.TrimSpace(b.String())
}

func scrapeFlixLatam(pageURL string) (string, error) {
	req, err := http.NewRequest("GET", pageURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func parseFlixLatamTotalPages(html string) int {
	max := 1
	rest := html
	for {
		idx := strings.Index(rest, "?page=")
		if idx < 0 {
			break
		}
		rest = rest[idx+6:]
		end := strings.IndexAny(rest, `"'& `)
		if end < 0 {
			break
		}
		n, err := strconv.Atoi(rest[:end])
		if err == nil && n > max {
			max = n
		}
	}
	return max
}

func parseFlixLatamListingItems(html string) []CvtPost {
	var posts []CvtPost
	rest := html
	for {
		si := strings.Index(rest, `<article class="item">`)
		if si < 0 {
			break
		}
		rest = rest[si:]
		ei := strings.Index(rest, "</article>")
		if ei < 0 {
			break
		}
		art := rest[:ei+10]
		rest = rest[ei+10:]

		// Slug from /serie/{slug}
		slug := ""
		if i := strings.Index(art, `/serie/`); i >= 0 {
			s := art[i+7:]
			if j := strings.IndexAny(s, `"' `); j >= 0 {
				slug = s[:j]
			}
		}
		if slug == "" {
			continue
		}

		// Poster: first img src
		poster := ""
		if i := strings.Index(art, `<img src="`); i >= 0 {
			s := art[i+10:]
			if j := strings.Index(s, `"`); j >= 0 {
				poster = s[:j]
			}
		}

		// Title from h3
		title := stripTags(extractBetween(art, `<h3>`, `</h3>`))

		// Year: first 4-digit number in a bare <span>
		year := ""
		tmp := art
		for {
			ssi := strings.Index(tmp, "<span>")
			if ssi < 0 {
				break
			}
			tmp = tmp[ssi+6:]
			eei := strings.Index(tmp, "</span>")
			if eei < 0 {
				break
			}
			candidate := tmp[:eei]
			tmp = tmp[eei+7:]
			if len(candidate) == 4 {
				if _, err := strconv.Atoi(candidate); err == nil {
					year = candidate
					break
				}
			}
		}

		// Rating
		rating := extractBetween(art, `<div class="rating">`, `</div>`)

		if title == "" || slug == "" {
			continue
		}
		rd := ""
		if year != "" {
			rd = year + "-01-01"
		}
		posts = append(posts, CvtPost{
			ID:            slugToID(slug),
			Title:         title,
			Slug:          slug,
			Images:        CvtImages{Poster: poster},
			Rating:        rating,
			ReleaseDate:   rd,
			FlixLatamSlug: slug,
			FlixLatamOnly: true,
		})
	}
	return posts
}

func parseFlixLatamMovieListingItems(html string) []CvtPost {
	var posts []CvtPost
	rest := html
	for {
		si := strings.Index(rest, `<article class="item">`)
		if si < 0 {
			break
		}
		rest = rest[si:]
		ei := strings.Index(rest, "</article>")
		if ei < 0 {
			break
		}
		art := rest[:ei+10]
		rest = rest[ei+10:]

		slug := ""
		if i := strings.Index(art, `/pelicula/`); i >= 0 {
			s := art[i+10:]
			if j := strings.IndexAny(s, `"' `); j >= 0 {
				slug = s[:j]
			}
		}
		if slug == "" {
			continue
		}

		poster := ""
		if i := strings.Index(art, `<img src="`); i >= 0 {
			s := art[i+10:]
			if j := strings.Index(s, `"`); j >= 0 {
				poster = s[:j]
			}
		}

		title := stripTags(extractBetween(art, `<h3>`, `</h3>`))

		year := ""
		tmp := art
		for {
			ssi := strings.Index(tmp, "<span>")
			if ssi < 0 {
				break
			}
			tmp = tmp[ssi+6:]
			eei := strings.Index(tmp, "</span>")
			if eei < 0 {
				break
			}
			candidate := tmp[:eei]
			tmp = tmp[eei+7:]
			if len(candidate) == 4 {
				if _, err := strconv.Atoi(candidate); err == nil {
					year = candidate
					break
				}
			}
		}

		rating := extractBetween(art, `<div class="rating">`, `</div>`)

		if title == "" {
			continue
		}
		rd := ""
		if year != "" {
			rd = year + "-01-01"
		}
		posts = append(posts, CvtPost{
			ID:            slugToID(slug),
			Title:         title,
			Slug:          slug,
			Images:        CvtImages{Poster: poster},
			Rating:        rating,
			ReleaseDate:   rd,
			FlixLatamSlug: slug,
			FlixLatamOnly: true,
		})
	}
	return posts
}

func fetchAllFlixLatamMoviesPages() []CvtPost {
	html, err := scrapeFlixLatam(flixlatamBase + "/peliculas/populares")
	if err != nil {
		log.Printf("[flixlatam] Error página 1 películas: %v", err)
		return nil
	}
	totalPages := parseFlixLatamTotalPages(html)
	if totalPages <= 0 {
		totalPages = 1
	}
	log.Printf("[flixlatam] /peliculas/populares: %d páginas", totalPages)

	allItems := make([][]CvtPost, totalPages)
	allItems[0] = parseFlixLatamMovieListingItems(html)

	for batchStart := 2; batchStart <= totalPages; batchStart += phd2BatchSize {
		batchEnd := batchStart + phd2BatchSize - 1
		if batchEnd > totalPages {
			batchEnd = totalPages
		}
		var wg sync.WaitGroup
		for page := batchStart; page <= batchEnd; page++ {
			wg.Add(1)
			go func(p int) {
				defer wg.Done()
				h, err := scrapeFlixLatam(fmt.Sprintf("%s/peliculas/populares?page=%d", flixlatamBase, p))
				if err != nil {
					return
				}
				allItems[p-1] = parseFlixLatamMovieListingItems(h)
			}(page)
		}
		wg.Wait()
		time.Sleep(buildDelay)
	}

	var flat []CvtPost
	for _, batch := range allItems {
		flat = append(flat, batch...)
	}
	// Populate title index for enrichment matching
	for _, p := range flat {
		k := normalizeTitle(p.Title) + "|" + yearOf(p.ReleaseDate)
		if k != "|" {
			flixlatamMovieByTitle.Store(k, p.FlixLatamSlug)
		}
	}
	log.Printf("[flixlatam] %d películas descargadas", len(flat))
	return flat
}

func fetchAllFlixLatamSeriesPages() []CvtPost {
	html, err := scrapeFlixLatam(flixlatamBase + "/series/populares")
	if err != nil {
		log.Printf("[flixlatam] Error página 1: %v", err)
		return nil
	}
	totalPages := parseFlixLatamTotalPages(html)
	if totalPages <= 0 {
		totalPages = 1
	}
	log.Printf("[flixlatam] /series/populares: %d páginas", totalPages)

	allItems := make([][]CvtPost, totalPages)
	allItems[0] = parseFlixLatamListingItems(html)

	for batchStart := 2; batchStart <= totalPages; batchStart += phd2BatchSize {
		batchEnd := batchStart + phd2BatchSize - 1
		if batchEnd > totalPages {
			batchEnd = totalPages
		}
		var wg sync.WaitGroup
		for page := batchStart; page <= batchEnd; page++ {
			wg.Add(1)
			go func(p int) {
				defer wg.Done()
				h, err := scrapeFlixLatam(fmt.Sprintf("%s/series/populares?page=%d", flixlatamBase, p))
				if err != nil {
					return
				}
				allItems[p-1] = parseFlixLatamListingItems(h)
			}(page)
		}
		wg.Wait()
		time.Sleep(buildDelay)
	}

	var flat []CvtPost
	for _, batch := range allItems {
		flat = append(flat, batch...)
	}
	log.Printf("[flixlatam] %d series descargadas", len(flat))
	return flat
}

// ── FlixLatam detail + episode scraping ──────────────────────────────────────

type flixlatamEpisodeInfo struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
}

type flixlatamSeasonInfo struct {
	Number   int                    `json:"number"`
	Episodes []flixlatamEpisodeInfo `json:"episodes"`
}

func parseFlixLatamSeasons(html string) []flixlatamSeasonInfo {
	var seasons []flixlatamSeasonInfo
	parts := strings.Split(html, `<div class="se-c">`)
	for _, part := range parts[1:] {
		snStr := extractBetween(part, `<span class="se-t">`, `</span>`)
		sn, err := strconv.Atoi(strings.TrimSpace(snStr))
		if err != nil {
			continue
		}
		epHTML := extractBetween(part, `<ul class="episodios">`, `</ul>`)
		var episodes []flixlatamEpisodeInfo
		for _, ep := range strings.Split(epHTML, "<li>")[1:] {
			numStr := ""
			if i := strings.Index(ep, `/capitulo/`); i >= 0 {
				s := ep[i+10:]
				if j := strings.IndexAny(s, `"'/`); j >= 0 {
					numStr = s[:j]
				}
			}
			epNum, _ := strconv.Atoi(numStr)
			if epNum <= 0 {
				continue
			}
			epTitle := stripTags(extractBetween(ep, `<div class="episodiotitle">`, `</div>`))
			episodes = append(episodes, flixlatamEpisodeInfo{Number: epNum, Title: epTitle})
		}
		seasons = append(seasons, flixlatamSeasonInfo{Number: sn, Episodes: episodes})
	}
	return seasons
}

func fetchFlixLatamSeasons(slug string) ([]flixlatamSeasonInfo, error) {
	key := "flixlatam:seasons:" + slug
	if b, ok := getDetailCache(key); ok {
		var seasons []flixlatamSeasonInfo
		if json.Unmarshal(b, &seasons) == nil {
			return seasons, nil
		}
	}
	html, err := scrapeFlixLatam(flixlatamBase + "/serie/" + slug)
	if err != nil {
		return nil, err
	}
	seasons := parseFlixLatamSeasons(html)
	if b, merr := json.Marshal(seasons); merr == nil {
		setCacheBytes(key, b, detailTTL)
	}
	return seasons, nil
}

// decodeJWTLink extracts the "link" field from a JWT payload without signature verification.
func decodeJWTLink(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	payload := parts[1]
	switch len(payload) % 4 {
	case 2:
		payload += "=="
	case 3:
		payload += "="
	}
	payload = strings.ReplaceAll(payload, "-", "+")
	payload = strings.ReplaceAll(payload, "_", "/")
	b, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return ""
	}
	var data struct {
		Link string `json:"link"`
	}
	if json.Unmarshal(b, &data) != nil {
		return ""
	}
	return data.Link
}

func embed69AESKey(html string) []byte {
	challenge := firstMatch(html, `(?m)const\s+POW_CHALLENGE\s*=\s*['"]([^'"]+)['"]`)
	difficultyStr := firstMatch(html, `(?m)const\s+POW_DIFFICULTY\s*=\s*(\d+)`)
	salt := firstMatch(html, `(?m)const\s+POW_SALT\s*=\s*['"]([^'"]+)['"]`)
	if challenge == "" || difficultyStr == "" || salt == "" {
		return nil
	}
	difficulty, err := strconv.Atoi(difficultyStr)
	if err != nil || difficulty < 0 || difficulty > 8 {
		return nil
	}
	prefix := strings.Repeat("0", difficulty)
	for nonce := 0; nonce < 50000000; nonce++ {
		sum := sha256.Sum256([]byte(challenge + strconv.Itoa(nonce)))
		if strings.HasPrefix(hex.EncodeToString(sum[:]), prefix) {
			keyHash := sha256.Sum256([]byte(challenge + strconv.Itoa(nonce) + salt))
			key := make([]byte, len(keyHash))
			copy(key, keyHash[:])
			return key
		}
	}
	return nil
}

func pkcs7Unpad(b []byte) []byte {
	if len(b) == 0 {
		return nil
	}
	pad := int(b[len(b)-1])
	if pad <= 0 || pad > aes.BlockSize || pad > len(b) {
		return b
	}
	for _, v := range b[len(b)-pad:] {
		if int(v) != pad {
			return b
		}
	}
	return b[:len(b)-pad]
}

func decryptEmbed69AESLink(encrypted string, key []byte) string {
	if len(key) < 32 {
		return ""
	}
	raw, err := base64.StdEncoding.DecodeString(encrypted)
	if err != nil || len(raw) <= aes.BlockSize || len(raw)%aes.BlockSize != 0 {
		return ""
	}
	block, err := aes.NewCipher(key[:32])
	if err != nil {
		return ""
	}
	iv := raw[:aes.BlockSize]
	ciphertext := raw[aes.BlockSize:]
	plain := make([]byte, len(ciphertext))
	cipher.NewCBCDecrypter(block, iv).CryptBlocks(plain, ciphertext)
	return strings.TrimSpace(strings.Trim(string(pkcs7Unpad(plain)), "`"))
}

func decodeEmbed69Link(link string, aesKey []byte) string {
	link = strings.TrimSpace(strings.Trim(link, "`"))
	if link == "" {
		return ""
	}
	if strings.Count(link, ".") == 2 {
		return decodeJWTLink(link)
	}
	if strings.HasPrefix(link, "http://") || strings.HasPrefix(link, "https://") {
		return link
	}
	if aesKey != nil {
		return decryptEmbed69AESLink(link, aesKey)
	}
	return ""
}

// fetchEmbed69Servers fetches actual video server URLs from an embed69 player page.
// Embed69 currently stores server links in dataLink and protects them with either legacy JWT payloads or AES-CBC + a short PoW.
func fetchEmbed69Servers(embedURL string) []EpisodeServer {
	req, err := http.NewRequest("GET", embedURL, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Referer", flixlatamBase+"/")
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	html := string(body)

	// Extract: let dataLink = [...];
	marker := "let dataLink = "
	idx := strings.Index(html, marker)
	if idx < 0 {
		return nil
	}
	rest := html[idx+len(marker):]
	end := strings.Index(rest, ";\n")
	if end < 0 {
		end = strings.Index(rest, ";")
	}
	if end < 0 {
		return nil
	}
	jsonStr := strings.TrimSpace(rest[:end])

	type embed69Entry struct {
		FileID        int    `json:"file_id"`
		VideoLanguage string `json:"video_language"`
		SortedEmbeds  []struct {
			Servername string `json:"servername"`
			Link       string `json:"link"`
			Type       string `json:"type"`
		} `json:"sortedEmbeds"`
	}
	var dataLink []embed69Entry
	if json.Unmarshal([]byte(jsonStr), &dataLink) != nil {
		return nil
	}

	serverNames := map[string]string{
		"vidhide": "Vidhide", "streamwish": "Streamwish", "filemoon": "Filemoon",
		"filemon": "Filemon", "hlswish": "Hlswish", "voexs": "Voexs",
		"voe": "Voe", "doodstream": "Doodstream", "streamtape": "Streamtape",
		"lulustream": "Lulustream", "vtube": "Vtube", "streamingembed": "VIP",
		"uploadfox": "Uploadfox", "up2box": "Up2Box",
	}
	langMap := map[string]string{
		"LAT": "Latino", "ESP": "Castellano", "ENG": "Inglés", "SUB": "Subtitulado",
	}

	aesKey := embed69AESKey(html)

	var servers []EpisodeServer
	id := 1
	for _, entry := range dataLink {
		lang := langMap[entry.VideoLanguage]
		if lang == "" {
			lang = entry.VideoLanguage
		}
		for _, embed := range entry.SortedEmbeds {
			if embed.Type == "download" {
				continue
			}
			actualURL := decodeEmbed69Link(embed.Link, aesKey)
			if actualURL == "" {
				continue
			}
			name := serverNames[embed.Servername]
			if name == "" {
				name = strings.Title(embed.Servername)
			}
			if !isAllowedServerProvider(name, actualURL, actualURL) {
				continue
			}
			parts := strings.Split(strings.TrimRight(actualURL, "/"), "/")
			embedID := parts[len(parts)-1]
			servers = append(servers, EpisodeServer{
				ID:        id,
				Idioma:    lang,
				Nombre:    name,
				Calidad:   "HD",
				PlayerURL: actualURL,
				URL:       actualURL,
				EmbedID:   embedID,
			})
			id++
		}
	}
	return sanitizeEpisodeServers(servers)
}

func fetchFlixLatamEpisodeServers(slug string, season, episode int) []EpisodeServer {
	u := fmt.Sprintf("%s/serie/%s/temporada/%d/capitulo/%d", flixlatamBase, slug, season, episode)
	html, err := scrapeFlixLatam(u)
	if err != nil {
		return nil
	}

	// Extract player names from the options list (e.g. "Embed69", "Moe", etc.)
	var playerNames []string
	optHTML := extractBetween(html, `<ul id="playerOptions">`, `</ul>`)
	for _, part := range strings.Split(optHTML, `<li`)[1:] {
		name := strings.TrimSpace(extractBetween(part, `<span class="title">`, `</span>`))
		if name != "" {
			playerNames = append(playerNames, name)
		}
	}

	// Collect iframe sources from source-box divs.
	// Active player uses src=; inactive players use data-src= (lazy load).
	var iframeSrcs []string
	for _, part := range strings.Split(html, `class="pframe source-box`)[1:] {
		src := extractBetween(part, `<iframe src="`, `"`)
		if src == "" {
			src = extractBetween(part, `<iframe data-src="`, `"`)
		}
		if src != "" {
			iframeSrcs = append(iframeSrcs, src)
		}
	}

	var servers []EpisodeServer
	id := 1
	for i, src := range iframeSrcs {
		src = absoluteFlixLatamPlayerURL(src)
		name := ""
		if i < len(playerNames) {
			name = playerNames[i]
		}

		if strings.Contains(strings.ToLower(name), "embed69") || strings.Contains(src, "embed69.org") || strings.Contains(src, "/vidurl/") {
			e69Servers := fetchEmbed69Servers(src)
			for _, s := range e69Servers {
				s.ID = id
				servers = append(servers, s)
				id++
			}
			continue
		}

		// Generic player: use player name when available, fallback to URL last segment
		parts := strings.Split(strings.TrimRight(src, "/"), "/")
		embedID := parts[len(parts)-1]
		if name == "" {
			name = embedID
		}
		if !isAllowedServerProvider(name, src, src) {
			continue
		}
		servers = append(servers, EpisodeServer{
			ID:        id,
			Idioma:    "Latino",
			Nombre:    name,
			Calidad:   "HD",
			PlayerURL: src,
			URL:       src,
			EmbedID:   embedID,
		})
		id++
	}
	return sanitizeEpisodeServers(servers)
}

func fetchFlixLatamMovieServers(slug string) []Server {
	html, err := scrapeFlixLatam(flixlatamBase + "/pelicula/" + slug)
	if err != nil {
		return nil
	}

	var playerNames []string
	optHTML := extractBetween(html, `<ul id="playerOptions">`, `</ul>`)
	for _, part := range strings.Split(optHTML, `<li`)[1:] {
		name := strings.TrimSpace(extractBetween(part, `<span class="title">`, `</span>`))
		if name != "" {
			playerNames = append(playerNames, name)
		}
	}

	var iframeSrcs []string
	for _, part := range strings.Split(html, `class="pframe source-box`)[1:] {
		src := extractBetween(part, `<iframe src="`, `"`)
		if src == "" {
			src = extractBetween(part, `<iframe data-src="`, `"`)
		}
		if src != "" {
			iframeSrcs = append(iframeSrcs, src)
		}
	}

	var servers []Server
	id := 1
	for i, src := range iframeSrcs {
		src = absoluteFlixLatamPlayerURL(src)
		name := ""
		if i < len(playerNames) {
			name = playerNames[i]
		}

		if strings.Contains(strings.ToLower(name), "embed69") || strings.Contains(src, "embed69.org") || strings.Contains(src, "/vidurl/") {
			for _, s := range fetchEmbed69Servers(src) {
				servers = append(servers, Server{
					ID: id, Idioma: s.Idioma, Nombre: s.Nombre,
					Calidad: s.Calidad, PlayerURL: s.PlayerURL, URL: s.URL, EmbedID: s.EmbedID,
				})
				id++
			}
			continue
		}

		parts := strings.Split(strings.TrimRight(src, "/"), "/")
		embedID := parts[len(parts)-1]
		if name == "" {
			name = embedID
		}
		if !isAllowedServerProvider(name, src, src) {
			continue
		}
		servers = append(servers, Server{
			ID: id, Idioma: "Latino", Nombre: name,
			Calidad: "HD", PlayerURL: src, URL: src, EmbedID: embedID,
		})
		id++
	}
	return sanitizeMovieServers(servers)
}

func absoluteFlixLatamPlayerURL(src string) string {
	src = strings.TrimSpace(src)
	ref, err := url.Parse(src)
	if err != nil {
		return src
	}
	base, err := url.Parse(flixlatamBase + "/")
	if err != nil {
		return src
	}
	return base.ResolveReference(ref).String()
}

// ── CVT full-fetch ────────────────────────────────────────────────────────────

func fetchAllCVTPages(postType string) ([]CvtPost, int, int) {
	first, err := cvtGet("/listing", map[string]string{"post_type": postType, "page": "1"})
	if err != nil {
		return nil, 0, 0
	}
	var firstResp CvtListResp
	if json.Unmarshal(first, &firstResp) != nil || firstResp.Error {
		return nil, 0, 0
	}
	totalPages := firstResp.Data.Pagination.LastPage
	total := firstResp.Data.Pagination.Total
	if totalPages == 0 {
		return firstResp.Data.Posts, total, 1
	}

	allPosts := make([][]CvtPost, totalPages)
	allPosts[0] = firstResp.Data.Posts
	for _, p := range firstResp.Data.Posts {
		cachePost(p.ID, p.Slug)
	}

	for batchStart := 2; batchStart <= totalPages; batchStart += buildBatch {
		batchEnd := batchStart + buildBatch - 1
		if batchEnd > totalPages {
			batchEnd = totalPages
		}
		var wg sync.WaitGroup
		for page := batchStart; page <= batchEnd; page++ {
			wg.Add(1)
			go func(p int) {
				defer wg.Done()
				data, err := cvtGet("/listing", map[string]string{
					"post_type": postType,
					"page":      strconv.Itoa(p),
				})
				if err != nil {
					return
				}
				var resp CvtListResp
				if json.Unmarshal(data, &resp) != nil || resp.Error {
					return
				}
				for _, post := range resp.Data.Posts {
					cachePost(post.ID, post.Slug)
				}
				allPosts[p-1] = resp.Data.Posts
			}(page)
		}
		wg.Wait()
		time.Sleep(buildDelay)
	}

	var flat []CvtPost
	for _, batch := range allPosts {
		flat = append(flat, batch...)
	}
	return flat, total, totalPages
}

// ── Detail pre-warming ────────────────────────────────────────────────────────

// warmMovieDetail builds and caches a movie detail if it is missing playable servers.
func warmMovieDetail(post CvtPost) {
	key := "poseidon:movie:" + strconv.Itoa(post.ID)
	if b, ok := getDetailCache(key); ok && !shouldRefreshMovieDetail(key, b) {
		rememberServerAvailability("m", post.ID, true)
		return
	}

	if post.PHD2Only {
		servers := fetchPHD2Servers(post.TMDbID, post.PHD2Slug, false)
		// Fallback: try FlixLatam if PHD2 returned nothing
		if len(servers) == 0 && post.FlixLatamSlug != "" {
			for i, s := range fetchFlixLatamMovieServers(post.FlixLatamSlug) {
				s.ID = i + 1
				servers = append(servers, s)
			}
		}
		offset := len(servers)
		for i, s := range fetchExternalMovieServers(post) {
			s.ID = offset + i + 1
			servers = append(servers, s)
		}
		detail := MovieDetail{
			ID:          post.ID,
			TmdbID:      post.TMDbID,
			Titulo:      post.Title,
			PosterURL:   fullImageURL(post.Images.Poster),
			BannerURL:   fullImageURL(post.Images.Backdrop),
			Descripcion: post.Overview,
			Rating:      parseRating(post.Rating),
			RuntimeMin:  parseRuntime(post.Runtime),
			ReleaseDate: releaseDate(post.ReleaseDate),
			URL:         phd2Base + "/pelicula/" + strconv.Itoa(post.TMDbID) + "/" + post.PHD2Slug,
			Generos:     resolveGenres(post.Genres),
			Servidores:  servers,
		}
		setDetailCache(key, detail, detailTTL)
		return
	}

	// FlixLatam-only movie (not in CVT or PHD2)
	if post.FlixLatamOnly && post.FlixLatamSlug != "" && post.PHD2Slug == "" {
		servers := fetchFlixLatamMovieServers(post.FlixLatamSlug)
		offset := len(servers)
		for i, s := range fetchExternalMovieServers(post) {
			s.ID = offset + i + 1
			servers = append(servers, s)
		}
		detail := MovieDetail{
			ID:          post.ID,
			TmdbID:      post.TMDbID,
			Titulo:      post.Title,
			PosterURL:   post.Images.Poster,
			BannerURL:   post.Images.Backdrop,
			Descripcion: post.Overview,
			Rating:      parseRating(post.Rating),
			ReleaseDate: releaseDate(post.ReleaseDate),
			URL:         flixlatamBase + "/pelicula/" + post.FlixLatamSlug,
			Generos:     resolveGenres(post.Genres),
			Servidores:  servers,
		}
		setDetailCache(key, detail, detailTTL)
		return
	}

	if post.ProviderOnly && post.Slug == "" && post.PHD2Slug == "" && post.FlixLatamSlug == "" {
		servers := fetchExternalMovieServers(post)
		detail := MovieDetail{
			ID:          post.ID,
			TmdbID:      post.TMDbID,
			Titulo:      post.Title,
			PosterURL:   fullImageURL(post.Images.Poster),
			BannerURL:   fullImageURL(post.Images.Backdrop),
			Descripcion: post.Overview,
			Rating:      parseRating(post.Rating),
			RuntimeMin:  parseRuntime(post.Runtime),
			ReleaseDate: releaseDate(post.ReleaseDate),
			URL:         firstProviderURL(post),
			Generos:     resolveGenres(post.Genres),
			Servidores:  servers,
		}
		setDetailCache(key, detail, detailTTL)
		return
	}

	slug := post.Slug
	if slug == "" {
		return
	}

	type singleRes struct {
		post CvtPost
		err  error
	}
	singleCh := make(chan singleRes, 1)
	playerCh := make(chan []CvtPlayerItem, 1)
	phd2Ch := make(chan []Server, 1)
	flixCh := make(chan []Server, 1)
	extCh := make(chan []Server, 1)

	go func() {
		data, err := cvtGet("/single", map[string]string{"post_type": "movies", "post_name": slug})
		if err != nil {
			singleCh <- singleRes{err: err}
			return
		}
		var resp CvtSingleResp
		if json.Unmarshal(data, &resp) != nil || resp.Error {
			singleCh <- singleRes{err: fmt.Errorf("upstream error")}
			return
		}
		singleCh <- singleRes{post: resp.Data}
	}()
	go func() { playerCh <- fetchPlayer(post.ID, "movies") }()
	go func() {
		if post.PHD2Slug != "" && post.TMDbID > 0 {
			phd2Ch <- fetchPHD2Servers(post.TMDbID, post.PHD2Slug, false)
		} else {
			phd2Ch <- nil
		}
	}()
	go func() {
		if post.FlixLatamSlug != "" {
			flixCh <- fetchFlixLatamMovieServers(post.FlixLatamSlug)
		} else {
			flixCh <- nil
		}
	}()
	go func() {
		if hasProviderLinks(post) {
			extCh <- fetchExternalMovieServers(post)
		} else {
			extCh <- nil
		}
	}()

	sr := <-singleCh
	if sr.err != nil {
		<-playerCh
		<-phd2Ch
		<-flixCh
		<-extCh
		return
	}
	p := sr.post
	servers := cvtToServers(<-playerCh)
	offset := len(servers)
	for i, s := range <-phd2Ch {
		s.ID = offset + i + 1
		servers = append(servers, s)
	}
	offset = len(servers)
	for i, s := range <-flixCh {
		s.ID = offset + i + 1
		servers = append(servers, s)
	}
	offset = len(servers)
	for i, s := range <-extCh {
		s.ID = offset + i + 1
		servers = append(servers, s)
	}
	detail := MovieDetail{
		ID:             p.ID,
		TmdbID:         post.TMDbID,
		Titulo:         p.Title,
		TituloOriginal: p.OriginalTitle,
		PosterURL:      fullImageURL(p.Images.Poster),
		BannerURL:      fullImageURL(p.Images.Backdrop),
		Descripcion:    p.Overview,
		Rating:         parseRating(p.Rating),
		RuntimeMin:     parseRuntime(p.Runtime),
		ReleaseDate:    releaseDate(p.ReleaseDate),
		URL:            "https://compucalitv.tv/peliculas/" + p.Slug,
		Generos:        resolveGenres(p.Genres),
		Servidores:     servers,
	}
	setDetailCache(key, detail, detailTTL)
}

// warmSerieDetail builds and caches a serie detail (seasons). Only skips if Redis has
// fresh data — a PostgreSQL-only hit means the data is stale and must be re-scraped.
func warmSerieDetail(post CvtPost) {
	key := "poseidon:serie:" + strconv.Itoa(post.ID)
	if redisAvailable.Load() && rdb.Exists(rctx, key).Val() > 0 {
		return
	}
	// Also evict stale season/episode caches so they get refreshed on next access.
	if redisAvailable.Load() && post.FlixLatamSlug != "" {
		rdb.Del(rctx, "flixlatam:seasons:"+post.FlixLatamSlug)
	}
	if redisAvailable.Load() && post.TMDbID > 0 {
		rdb.Del(rctx, fmt.Sprintf("phd2:seasons:%d", post.TMDbID))
	}

	if post.FlixLatamSlug != "" {
		html, err := scrapeFlixLatam(flixlatamBase + "/serie/" + post.FlixLatamSlug)
		if err != nil {
			return
		}
		title := stripTags(extractBetween(html, `<h1>`, `</h1>`))
		if title == "" {
			title = post.Title
		}
		overview := stripTags(extractBetween(html, `<div class="wp-content"><p>`, `</p>`))
		ratingStr := stripTags(extractBetween(html, `<div class="rating-value">`, `</div>`))
		var rating float64
		if i := strings.Index(ratingStr, "/"); i >= 0 {
			rating, _ = strconv.ParseFloat(strings.TrimSpace(ratingStr[:i]), 64)
		} else {
			rating, _ = strconv.ParseFloat(strings.TrimSpace(ratingStr), 64)
		}
		sheader := extractBetween(html, `<div class="sheader">`, `<div class="sbox">`)
		poster := extractBetween(sheader, `src="`, `"`)
		if poster == "" {
			poster = fullImageURL(post.Images.Poster)
		}
		rd := releaseDate(post.ReleaseDate)
		tmp := html
		for {
			idx := strings.Index(tmp, `<span class="date">`)
			if idx < 0 {
				break
			}
			tmp = tmp[idx+19:]
			ei := strings.Index(tmp, `</span>`)
			if ei < 0 {
				break
			}
			candidate := stripTags(tmp[:ei])
			if len(candidate) == 10 && candidate[4] == '-' && candidate[7] == '-' {
				rd = candidate
				break
			}
		}
		var genreIDs []int
		sgenHTML := extractBetween(html, `<div class="sgeneros">`, `</div>`)
		for _, gp := range strings.Split(sgenHTML, `/generos/`)[1:] {
			ei := strings.IndexAny(gp, `"'>`)
			if ei >= 0 {
				if gid, ok2 := flixlatamGenreSlugToID[gp[:ei]]; ok2 {
					genreIDs = append(genreIDs, gid)
				}
			}
		}
		seasons := parseFlixLatamSeasons(html)
		if b, err := json.Marshal(seasons); err == nil {
			setCacheBytes("flixlatam:seasons:"+post.FlixLatamSlug, b, seasonTTL)
		}
		temporadas := make([]SeasonInfo, 0, len(seasons))
		for _, s := range seasons {
			temporadas = append(temporadas, SeasonInfo{
				ID:             post.ID*1000 + s.Number,
				Number:         s.Number,
				TotalEpisodios: len(s.Episodes),
			})
		}
		detail := SerieDetail{
			ID:          post.ID,
			TmdbID:      post.TMDbID,
			Titulo:      title,
			PosterURL:   poster,
			BannerURL:   fullImageURL(post.Images.Backdrop),
			Descripcion: overview,
			Rating:      rating,
			ReleaseDate: rd,
			Generos:     resolveGenres(genreIDs),
			Temporadas:  temporadas,
		}
		setDetailCache(key, detail, detailTTL)
		return
	}

	if post.PHD2Only {
		seasons, err := fetchPHD2SerieSeasons(post.TMDbID, post.PHD2Slug)
		if err != nil {
			return
		}
		temporadas := make([]SeasonInfo, 0, len(seasons))
		for _, s := range seasons {
			temporadas = append(temporadas, SeasonInfo{
				ID:             post.ID*1000 + s.Number,
				Number:         s.Number,
				TotalEpisodios: len(s.Episodes),
			})
		}
		detail := SerieDetail{
			ID:          post.ID,
			TmdbID:      post.TMDbID,
			Titulo:      post.Title,
			PosterURL:   fullImageURL(post.Images.Poster),
			BannerURL:   fullImageURL(post.Images.Backdrop),
			Descripcion: post.Overview,
			Rating:      parseRating(post.Rating),
			ReleaseDate: releaseDate(post.ReleaseDate),
			Generos:     resolveGenres(post.Genres),
			Temporadas:  temporadas,
		}
		setDetailCache(key, detail, detailTTL)
	}

	if post.ProviderOnly && post.FlixLatamSlug == "" && post.PHD2Slug == "" && post.Slug == "" {
		if detail, ok := buildExternalSerieDetail(post); ok {
			setDetailCache(key, detail, detailTTL)
		}
	}
}

// warmSerieSeasons pre-warms episode servers for every season of a series.
// Reads season list from the already-cached SerieDetail, then calls fetchAndCacheSeasonEpisodes.
func warmSerieSeasons(post CvtPost) {
	detailKey := "poseidon:serie:" + strconv.Itoa(post.ID)
	b, ok := getDetailCache(detailKey)
	if !ok {
		return
	}
	var detail SerieDetail
	if json.Unmarshal(b, &detail) != nil {
		return
	}
	for _, t := range detail.Temporadas {
		cacheKey := "poseidon:season:" + strconv.Itoa(post.ID) + ":" + strconv.Itoa(t.Number)
		if b, ok := getDetailCache(cacheKey); ok && !shouldRefreshSeasonDetail(cacheKey, b) {
			continue
		}
		fetchAndCacheSeasonEpisodes(post.ID, t.Number, cacheKey) //nolint:errcheck
	}
}

func runWarmer(movies, series []CvtPost) {
	log.Printf("[warmer] Iniciando: %d películas, %d series", len(movies), len(series))
	start := time.Now()
	var warmedM, warmedS, warmedSeas atomic.Int64

	// Pass 1: series detail + episode servers, by batch, so visible series
	// are marked in PostgreSQL as soon as each batch finds servers.
	for i := 0; i < len(series); i += warmBatchSize {
		end := i + warmBatchSize
		if end > len(series) {
			end = len(series)
		}
		var wg sync.WaitGroup
		for _, post := range series[i:end] {
			wg.Add(1)
			go func(p CvtPost) {
				defer wg.Done()
				warmSerieDetail(p)
				warmedS.Add(1)
			}(post)
		}
		wg.Wait()

		var seasonWG sync.WaitGroup
		for _, post := range series[i:end] {
			seasonWG.Add(1)
			go func(p CvtPost) {
				defer seasonWG.Done()
				warmSerieSeasons(p)
				warmedSeas.Add(1)
			}(post)
		}
		seasonWG.Wait()
		time.Sleep(warmSeasonDelay)
	}
	log.Printf("[warmer] Series/episodios listo: %d detalles, %d series procesadas en %v", warmedS.Load(), warmedSeas.Load(), time.Since(start))

	// Pass 2: movie details
	start = time.Now()
	for i := 0; i < len(movies); i += warmBatchSize {
		end := i + warmBatchSize
		if end > len(movies) {
			end = len(movies)
		}
		var wg sync.WaitGroup
		for _, post := range movies[i:end] {
			wg.Add(1)
			go func(p CvtPost) {
				defer wg.Done()
				warmMovieDetail(p)
				warmedM.Add(1)
			}(post)
		}
		wg.Wait()
		time.Sleep(warmDelay)
	}
	log.Printf("[warmer] Películas listo: %d en %v", warmedM.Load(), time.Since(start))
	log.Printf("[warmer] Completo — toda la DB pre-calentada")
}

// enrichMoviesWithoutServers re-procesa todas las películas con servidores vacíos
// intentando obtenerlos de PHD2, CVT y FlixLatam.
func enrichMoviesWithoutServers() {
	if pgPool == nil {
		return
	}
	log.Printf("[enrich] Iniciando enriquecimiento de películas sin servidores...")

	rows, err := pgPool.Query(context.Background(), `
		SELECT s.key, c.data
		FROM poseidon_store s
		JOIN poseidon_catalog c
		  ON c.id = substring(s.key FROM 'poseidon:movie:([0-9]+)')::bigint
		 AND c.tipo = 'm'
		WHERE s.key LIKE 'poseidon:movie:%'
		  AND jsonb_array_length(COALESCE((s.value->'servidores'), '[]'::jsonb)) = 0
	`)
	if err != nil {
		log.Printf("[enrich] Error consultando películas sin servidores: %v", err)
		return
	}

	type entry struct {
		key  string
		data []byte
	}
	var entries []entry
	for rows.Next() {
		var e entry
		if rows.Scan(&e.key, &e.data) == nil {
			entries = append(entries, e)
		}
	}
	rows.Close()

	log.Printf("[enrich] %d películas sin servidores a procesar", len(entries))
	enriched := 0
	for _, e := range entries {
		var post CvtPost
		if json.Unmarshal(e.data, &post) != nil {
			continue
		}
		// Si no tiene FlixLatamSlug, intentar match por título en el índice
		if post.FlixLatamSlug == "" {
			titleKey := normalizeTitle(post.Title) + "|" + yearOf(post.ReleaseDate)
			if v, ok := flixlatamMovieByTitle.Load(titleKey); ok {
				post.FlixLatamSlug = v.(string)
			}
		}
		// Forzar re-fetch borrando la entrada cacheada
		_, _ = pgPool.Exec(context.Background(), "DELETE FROM poseidon_store WHERE key=$1", e.key)
		if redisAvailable.Load() {
			rdb.Del(rctx, e.key)
		}
		warmMovieDetail(post)
		// Verificar si ahora tiene servidores
		if b, ok := pgGet(e.key); ok && movieDetailHasServers(b) {
			enriched++
			pgSetCatalogHasServers("m", post.ID, true)
			rememberServerAvailability("m", post.ID, true)
		}
		time.Sleep(150 * time.Millisecond)
	}
	log.Printf("[enrich] Completo: %d/%d películas recuperadas", enriched, len(entries))
}

func startWarmer(movies, series []CvtPost) {
	if len(movies) == 0 && len(series) == 0 {
		return
	}
	if !warmerRunning.CompareAndSwap(false, true) {
		log.Printf("[warmer] Ya hay un warmup en curso; se omite este ciclo")
		return
	}
	go func() {
		defer warmerRunning.Store(false)
		runWarmer(movies, series)
	}()
}

// ── Cache builder ─────────────────────────────────────────────────────────────

func startCacheBuilder() {
	go func() {
		// Load PostgreSQL catalog first so API starts from the persistent source.
		if movies, series, ok := pgLoadCatalogSnapshot(); ok {
			gc.setMovies(sortedList{
				items: movies, total: len(movies),
				lastPage: calcTotalPages(len(movies)), ready: true, updatedAt: time.Now(),
			})
			gc.setSeries(sortedList{
				items: series, total: len(series),
				lastPage: calcTotalPages(len(series)), ready: true, updatedAt: time.Now(),
			})
			log.Printf("[pg] Catálogo cargado: %d pelís, %d series", len(movies), len(series))
		} else if movies, series, ok := loadCache(); ok {
			pgUpsertCatalog(movies, "m")
			pgUpsertCatalog(series, "s")
			gc.setMovies(sortedList{
				items: movies, total: len(movies),
				lastPage: calcTotalPages(len(movies)), ready: true, updatedAt: time.Now(),
			})
			gc.setSeries(sortedList{
				items: series, total: len(series),
				lastPage: calcTotalPages(len(series)), ready: true, updatedAt: time.Now(),
			})
			log.Printf("[cache] Snapshot cargado como fallback: %d pelís, %d series", len(movies), len(series))
		}

		for {
			log.Printf("[cache] Construyendo cache CVT + PHD2...")
			var wg sync.WaitGroup
			var newMovies, newSeries []CvtPost

			wg.Add(2)
			go func() {
				defer wg.Done()
				cvt, _, _ := fetchAllCVTPages("movies")
				log.Printf("[cache] CVT películas: %d. Descargando PHD2...", len(cvt))
				phd2 := fetchAllPHD2Pages("/peliculas/estrenos/page/", false)
				step1 := mergePostLists(cvt, phd2)
				log.Printf("[cache] Descargando FlixLatam películas...")
				flixlatamMovies := fetchAllFlixLatamMoviesPages()
				step2 := mergePostLists(step1, flixlatamMovies)
				log.Printf("[cache] Descargando proveedores externos películas...")
				externalMovies := fetchAllExternalMoviesPages()
				merged := mergePostLists(step2, externalMovies)
				sort.Slice(merged, func(i, j int) bool {
					return merged[i].ReleaseDate > merged[j].ReleaseDate
				})
				log.Printf("[cache] Películas: %d CVT + %d PHD2 + %d FlixLatam + %d externos → %d total", len(cvt), len(phd2), len(flixlatamMovies), len(externalMovies), len(merged))
				gc.setMovies(sortedList{
					items: merged, total: len(merged),
					lastPage: calcTotalPages(len(merged)), ready: true, updatedAt: time.Now(),
				})
				newMovies = merged
			}()
			go func() {
				defer wg.Done()
				log.Printf("[cache] Descargando series FlixLatam + PHD2...")
				flixlatam := fetchAllFlixLatamSeriesPages()
				log.Printf("[cache] FlixLatam: %d series. Descargando PHD2...", len(flixlatam))
				phd2 := fetchAllPHD2Pages("/series/estrenos/page/", true)
				step1 := mergePostLists(flixlatam, phd2)
				log.Printf("[cache] Descargando proveedores externos series...")
				externalSeries := fetchAllExternalSeriesPages()
				merged := mergePostLists(step1, externalSeries)
				sort.Slice(merged, func(i, j int) bool {
					return merged[i].ReleaseDate > merged[j].ReleaseDate
				})
				log.Printf("[cache] Series: %d FlixLatam + %d PHD2 + %d externos → %d total", len(flixlatam), len(phd2), len(externalSeries), len(merged))
				gc.setSeries(sortedList{
					items: merged, total: len(merged),
					lastPage: calcTotalPages(len(merged)), ready: true, updatedAt: time.Now(),
				})
				newSeries = merged
			}()
			wg.Wait()

			pgUpsertCatalog(newMovies, "m")
			pgUpsertCatalog(newSeries, "s")
			saveCache(newMovies, newSeries)
			log.Printf("[cache] Completo. Próxima actualización en %v", cacheRebuildTTL)
			startWarmer(newMovies, newSeries)
			go enrichMoviesWithoutServers()
			time.Sleep(cacheRebuildTTL)
		}
	}()
}

// ── New-content watcher ───────────────────────────────────────────────────────

const seriesWatcherInterval = 4 * time.Hour
const seriesWatcherDelay = 800 * time.Millisecond // between series

// startSeriesNewContentWatcher periodically checks every cached series for new
// seasons or episodes and evicts stale caches so the next request gets fresh data.
func startSeriesNewContentWatcher() {
	go func() {
		time.Sleep(45 * time.Minute) // stagger from the main cache rebuild
		for {
			checkSeriesForNewContent()
			time.Sleep(seriesWatcherInterval)
		}
	}()
}

func checkSeriesForNewContent() {
	sl, ready := gc.getSeries()
	if !ready || len(sl.items) == 0 {
		return
	}
	log.Printf("[watcher] Verificando nuevas temporadas/episodios en %d series...", len(sl.items))
	evicted := 0

	for _, post := range sl.items {
		cacheKey := "poseidon:serie:" + strconv.Itoa(post.ID)
		b, ok := getDetailCache(cacheKey)
		if !ok {
			// Not cached yet — nothing to compare against.
			time.Sleep(seriesWatcherDelay)
			continue
		}
		var cached SerieDetail
		if json.Unmarshal(b, &cached) != nil {
			time.Sleep(seriesWatcherDelay)
			continue
		}

		// Build fingerprint: map[seasonNumber]episodeCount
		current := make(map[int]int, len(cached.Temporadas))
		for _, t := range cached.Temporadas {
			current[t.Number] = t.TotalEpisodios
		}

		var fresh map[int]int

		switch {
		case post.FlixLatamSlug != "":
			html, err := scrapeFlixLatam(flixlatamBase + "/serie/" + post.FlixLatamSlug)
			if err != nil {
				time.Sleep(seriesWatcherDelay)
				continue
			}
			seasons := parseFlixLatamSeasons(html)
			fresh = make(map[int]int, len(seasons))
			for _, s := range seasons {
				fresh[s.Number] = len(s.Episodes)
			}

		case post.PHD2Only && post.TMDbID > 0 && post.PHD2Slug != "":
			path := fmt.Sprintf("/serie/%d/%s", post.TMDbID, post.PHD2Slug)
			data, err := scrapeNextData(phd2Base + path)
			if err != nil {
				time.Sleep(seriesWatcherDelay)
				continue
			}
			var nd Phd2SerieNextData
			if json.Unmarshal(data, &nd) != nil {
				time.Sleep(seriesWatcherDelay)
				continue
			}
			fresh = make(map[int]int)
			for _, s := range nd.Props.PageProps.ThisSerie.Seasons {
				if s.Number > 0 && len(s.Episodes) > 0 {
					fresh[s.Number] = len(s.Episodes)
				}
			}

		default:
			time.Sleep(seriesWatcherDelay)
			continue
		}

		changed := len(fresh) != len(current)
		if !changed {
			for sn, epCount := range fresh {
				if current[sn] != epCount {
					changed = true
					break
				}
			}
		}

		if changed {
			log.Printf("[watcher] %s (id=%d): nuevo contenido detectado — evictando caché", post.Title, post.ID)
			if pgPool != nil {
				pgPool.Exec(rctx, `DELETE FROM poseidon_store WHERE key = $1`, cacheKey)
				if post.FlixLatamSlug != "" {
					pgPool.Exec(rctx, `DELETE FROM poseidon_store WHERE key = $1`, "flixlatam:seasons:"+post.FlixLatamSlug)
				}
				if post.TMDbID > 0 {
					pgPool.Exec(rctx, `DELETE FROM poseidon_store WHERE key = $1`, fmt.Sprintf("phd2:seasons:%d", post.TMDbID))
				}
			}
			if redisAvailable.Load() {
				rdb.Del(rctx, cacheKey)
				if post.FlixLatamSlug != "" {
					rdb.Del(rctx, "flixlatam:seasons:"+post.FlixLatamSlug)
				}
				if post.TMDbID > 0 {
					rdb.Del(rctx, fmt.Sprintf("phd2:seasons:%d", post.TMDbID))
				}
			}
			evicted++
		}

		time.Sleep(seriesWatcherDelay)
	}
	log.Printf("[watcher] Completado: %d/%d series con nuevo contenido detectado y caché evictado", evicted, len(sl.items))
}

// ── Movie handlers ────────────────────────────────────────────────────────────

func listMovies(w http.ResponseWriter, r *http.Request) {
	page := pageParam(r)

	if posts, total, lastPage, ok := pgListCatalogPosts("m", page, nil); ok {
		movies := make([]MovieSummary, 0, len(posts))
		for _, p := range posts {
			movies = append(movies, MovieSummary{p.ID, p.Title, fullImageURL(p.Images.Poster)})
		}
		json200(w, MoviePage{Movies: movies, Page: page, PerPage: perPage, Total: total, TotalPages: lastPage})
		return
	}

	sl, ready := gc.getMovies()
	if ready {
		visible := visibleMoviePosts(sl.items)
		posts, total := pageFromList(visible, page)
		movies := make([]MovieSummary, 0, len(posts))
		for _, p := range posts {
			movies = append(movies, MovieSummary{p.ID, p.Title, fullImageURL(p.Images.Poster)})
		}
		json200(w, MoviePage{
			Movies: movies, Page: page, PerPage: perPage,
			Total: total, TotalPages: calcTotalPages(total),
		})
		return
	}

	posts, total, lastPage, err := livePage("movies", page)
	if err != nil {
		jsonErr(w, 502, err.Error())
		return
	}
	posts = visibleMoviePosts(posts)
	total = len(posts)
	lastPage = calcTotalPages(total)
	movies := make([]MovieSummary, 0, len(posts))
	for _, p := range posts {
		movies = append(movies, MovieSummary{p.ID, p.Title, fullImageURL(p.Images.Poster)})
	}
	json200(w, MoviePage{Movies: movies, Page: page, PerPage: perPage, Total: total, TotalPages: lastPage})
}

func getMovie(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		jsonErr(w, 400, "id invalido")
		return
	}

	// Serve cached detail only when it already has playable servers.
	cacheKey := "poseidon:movie:" + strconv.Itoa(id)
	if b, ok := getDetailCache(cacheKey); ok {
		if movieDetailHasServers(b) {
			serveRawJSON(w, b)
			return
		}
	}

	cached, hasCached := findMovieByID(id)
	if !hasCached {
		cached, hasCached = pgFindCatalogPost(id, "m")
	}

	// PHD2-only post: no CVT data available
	if hasCached && cached.PHD2Only {
		servers := fetchPHD2Servers(cached.TMDbID, cached.PHD2Slug, false)
		offset := len(servers)
		for i, s := range fetchExternalMovieServers(cached) {
			s.ID = offset + i + 1
			servers = append(servers, s)
		}
		detail := MovieDetail{
			ID:          cached.ID,
			TmdbID:      cached.TMDbID,
			Titulo:      cached.Title,
			PosterURL:   fullImageURL(cached.Images.Poster),
			BannerURL:   fullImageURL(cached.Images.Backdrop),
			Descripcion: cached.Overview,
			Rating:      parseRating(cached.Rating),
			RuntimeMin:  parseRuntime(cached.Runtime),
			ReleaseDate: releaseDate(cached.ReleaseDate),
			URL:         phd2Base + "/pelicula/" + strconv.Itoa(cached.TMDbID) + "/" + cached.PHD2Slug,
			Generos:     resolveGenres(cached.Genres),
			Servidores:  servers,
		}
		detail = sanitizeMovieDetail(detail)
		setDetailCache(cacheKey, detail, detailTTL)
		if len(detail.Servidores) == 0 {
			jsonErr(w, 404, "sin servidores compatibles")
			return
		}
		json200(w, detail)
		return
	}

	if hasCached && cached.FlixLatamOnly && cached.FlixLatamSlug != "" && cached.PHD2Slug == "" {
		servers := fetchFlixLatamMovieServers(cached.FlixLatamSlug)
		offset := len(servers)
		for i, s := range fetchExternalMovieServers(cached) {
			s.ID = offset + i + 1
			servers = append(servers, s)
		}
		detail := MovieDetail{
			ID:          cached.ID,
			TmdbID:      cached.TMDbID,
			Titulo:      cached.Title,
			PosterURL:   fullImageURL(cached.Images.Poster),
			BannerURL:   fullImageURL(cached.Images.Backdrop),
			Descripcion: cached.Overview,
			Rating:      parseRating(cached.Rating),
			RuntimeMin:  parseRuntime(cached.Runtime),
			ReleaseDate: releaseDate(cached.ReleaseDate),
			URL:         flixlatamBase + "/pelicula/" + cached.FlixLatamSlug,
			Generos:     resolveGenres(cached.Genres),
			Servidores:  servers,
		}
		detail = sanitizeMovieDetail(detail)
		setDetailCache(cacheKey, detail, detailTTL)
		if len(detail.Servidores) == 0 {
			jsonErr(w, 404, "sin servidores compatibles")
			return
		}
		json200(w, detail)
		return
	}

	if hasCached && cached.ProviderOnly && cached.Slug == "" && cached.PHD2Slug == "" && cached.FlixLatamSlug == "" {
		detail := MovieDetail{
			ID:          cached.ID,
			TmdbID:      cached.TMDbID,
			Titulo:      cached.Title,
			PosterURL:   fullImageURL(cached.Images.Poster),
			BannerURL:   fullImageURL(cached.Images.Backdrop),
			Descripcion: cached.Overview,
			Rating:      parseRating(cached.Rating),
			RuntimeMin:  parseRuntime(cached.Runtime),
			ReleaseDate: releaseDate(cached.ReleaseDate),
			URL:         firstProviderURL(cached),
			Generos:     resolveGenres(cached.Genres),
			Servidores:  fetchExternalMovieServers(cached),
		}
		detail = sanitizeMovieDetail(detail)
		setDetailCache(cacheKey, detail, detailTTL)
		if len(detail.Servidores) == 0 {
			jsonErr(w, 404, "sin servidores compatibles")
			return
		}
		json200(w, detail)
		return
	}

	// CVT post (may also have PHD2Slug for extra servers)
	slug, ok := resolveSlug(id)
	if !ok {
		if hasCached && cached.Slug != "" {
			slug = cached.Slug
			ok = true
		}
	}
	if !ok {
		slug, ok = fetchSlugForID(id, "movies")
		if !ok {
			jsonErr(w, 404, "no encontrado")
			return
		}
	}

	type singleRes struct {
		post CvtPost
		err  error
	}
	singleCh := make(chan singleRes, 1)
	playerCh := make(chan []CvtPlayerItem, 1)
	phd2Ch := make(chan []Server, 1)
	flixCh := make(chan []Server, 1)
	extCh := make(chan []Server, 1)

	go func() {
		data, err := cvtGet("/single", map[string]string{"post_type": "movies", "post_name": slug})
		if err != nil {
			singleCh <- singleRes{err: err}
			return
		}
		var resp CvtSingleResp
		if json.Unmarshal(data, &resp) != nil || resp.Error {
			singleCh <- singleRes{err: fmt.Errorf("upstream error")}
			return
		}
		singleCh <- singleRes{post: resp.Data}
	}()
	go func() { playerCh <- fetchPlayer(id, "movies") }()
	go func() {
		if hasCached && cached.PHD2Slug != "" && cached.TMDbID > 0 {
			phd2Ch <- fetchPHD2Servers(cached.TMDbID, cached.PHD2Slug, false)
		} else {
			phd2Ch <- nil
		}
	}()
	go func() {
		if hasCached && cached.FlixLatamSlug != "" {
			flixCh <- fetchFlixLatamMovieServers(cached.FlixLatamSlug)
		} else {
			flixCh <- nil
		}
	}()
	go func() {
		if hasCached && hasProviderLinks(cached) {
			extCh <- fetchExternalMovieServers(cached)
		} else {
			extCh <- nil
		}
	}()

	sr := <-singleCh
	if sr.err != nil {
		<-playerCh
		<-phd2Ch
		<-flixCh
		<-extCh
		jsonErr(w, 502, sr.err.Error())
		return
	}
	p := sr.post

	servers := cvtToServers(<-playerCh)
	offset := len(servers)
	for i, s := range <-phd2Ch {
		s.ID = offset + i + 1
		servers = append(servers, s)
	}
	offset = len(servers)
	for i, s := range <-flixCh {
		s.ID = offset + i + 1
		servers = append(servers, s)
	}
	offset = len(servers)
	for i, s := range <-extCh {
		s.ID = offset + i + 1
		servers = append(servers, s)
	}

	detail := MovieDetail{
		ID:             p.ID,
		TmdbID:         cached.TMDbID,
		Titulo:         p.Title,
		TituloOriginal: p.OriginalTitle,
		PosterURL:      fullImageURL(p.Images.Poster),
		BannerURL:      fullImageURL(p.Images.Backdrop),
		Descripcion:    p.Overview,
		Rating:         parseRating(p.Rating),
		RuntimeMin:     parseRuntime(p.Runtime),
		ReleaseDate:    releaseDate(p.ReleaseDate),
		URL:            "https://compucalitv.tv/peliculas/" + p.Slug,
		Generos:        resolveGenres(p.Genres),
		Servidores:     servers,
	}
	detail = sanitizeMovieDetail(detail)
	setDetailCache(cacheKey, detail, detailTTL)
	if len(detail.Servidores) == 0 {
		jsonErr(w, 404, "sin servidores compatibles")
		return
	}
	json200(w, detail)
}

// localSearchNorm normalizes a string for case-insensitive substring search.
func localSearchNorm(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

func searchMovies(w http.ResponseWriter, r *http.Request) {
	q := localSearchNorm(r.URL.Query().Get("q"))
	if q == "" {
		json200(w, []MovieSummary{})
		return
	}
	// Try DB first (persistent, survives restarts)
	if pgPool != nil {
		rows := pgSearchCatalog(q, "m", 50)
		movies := make([]MovieSummary, 0, len(rows))
		for _, row := range rows {
			movies = append(movies, MovieSummary{int(row.ID), row.Titulo, row.PosterURL})
		}
		json200(w, movies)
		return
	}
	// Fallback: in-memory catalog
	sl, ready := gc.getMovies()
	if !ready {
		json200(w, []MovieSummary{})
		return
	}
	movies := make([]MovieSummary, 0, 30)
	for _, p := range sl.items {
		if movieHasServers(p.ID) && (strings.Contains(localSearchNorm(p.Title), q) || strings.Contains(localSearchNorm(p.OriginalTitle), q)) {
			movies = append(movies, MovieSummary{p.ID, p.Title, fullImageURL(p.Images.Poster)})
			if len(movies) >= 50 {
				break
			}
		}
	}
	json200(w, movies)
}

func listByGenre(w http.ResponseWriter, r *http.Request) {
	genre := r.PathValue("genre")
	page := pageParam(r)
	gid, ok := genreByName[genre]

	if ok {
		if posts, total, lastPage, dbOK := pgListCatalogPosts("m", page, &gid); dbOK {
			movies := make([]MovieSummary, 0, len(posts))
			for _, p := range posts {
				movies = append(movies, MovieSummary{p.ID, p.Title, fullImageURL(p.Images.Poster)})
			}
			json200(w, MoviePage{Movies: movies, Page: page, PerPage: perPage, Total: total, TotalPages: lastPage})
			return
		}
	}

	if sl, ready := gc.getMovies(); ready {
		var filtered []CvtPost
		for _, p := range sl.items {
			if !movieHasServers(p.ID) {
				continue
			}
			if ok {
				for _, g := range p.Genres {
					if g == gid {
						filtered = append(filtered, p)
						break
					}
				}
			} else if resolveGenresContains(p.Genres, genre) {
				filtered = append(filtered, p)
			}
		}
		posts, total := pageFromList(filtered, page)
		movies := make([]MovieSummary, 0, len(posts))
		for _, p := range posts {
			movies = append(movies, MovieSummary{p.ID, p.Title, fullImageURL(p.Images.Poster)})
		}
		json200(w, MoviePage{Movies: movies, Page: page, PerPage: perPage, Total: total, TotalPages: calcTotalPages(total)})
		return
	}

	if !ok {
		jsonErr(w, 404, "género no encontrado")
		return
	}
	posts, total, lastPage, err := livePageWithParams("movies", page, map[string]string{"genres": strconv.Itoa(gid)})
	if err != nil {
		jsonErr(w, 502, err.Error())
		return
	}
	posts = visibleMoviePosts(posts)
	total = len(posts)
	lastPage = calcTotalPages(total)
	movies := make([]MovieSummary, 0, len(posts))
	for _, p := range posts {
		movies = append(movies, MovieSummary{p.ID, p.Title, fullImageURL(p.Images.Poster)})
	}
	json200(w, MoviePage{Movies: movies, Page: page, PerPage: perPage, Total: total, TotalPages: lastPage})
}

func listGenres(w http.ResponseWriter, r *http.Request) {
	names := make([]string, 0, len(genreNames))
	for _, name := range genreNames {
		names = append(names, name)
	}
	sort.Strings(names)
	json200(w, names)
}

// ── Serie handlers ────────────────────────────────────────────────────────────

func listSeries(w http.ResponseWriter, r *http.Request) {
	page := pageParam(r)

	if posts, total, lastPage, ok := pgListCatalogPosts("s", page, nil); ok {
		series := make([]SerieSummary, 0, len(posts))
		for _, p := range posts {
			series = append(series, SerieSummary{p.ID, p.Title, fullImageURL(p.Images.Poster)})
		}
		json200(w, SeriePage{Series: series, Page: page, PerPage: perPage, Total: total, TotalPages: lastPage})
		return
	}

	sl, ready := gc.getSeries()
	if ready {
		visible := visibleSeriePosts(sl.items)
		posts, total := pageFromList(visible, page)
		series := make([]SerieSummary, 0, len(posts))
		for _, p := range posts {
			series = append(series, SerieSummary{p.ID, p.Title, fullImageURL(p.Images.Poster)})
		}
		json200(w, SeriePage{
			Series: series, Page: page, PerPage: perPage,
			Total: total, TotalPages: calcTotalPages(total),
		})
		return
	}

	posts, total, lastPage, err := livePage("tvshows", page)
	if err != nil {
		jsonErr(w, 502, err.Error())
		return
	}
	posts = visibleSeriePosts(posts)
	total = len(posts)
	lastPage = calcTotalPages(total)
	series := make([]SerieSummary, 0, len(posts))
	for _, p := range posts {
		series = append(series, SerieSummary{p.ID, p.Title, fullImageURL(p.Images.Poster)})
	}
	json200(w, SeriePage{Series: series, Page: page, PerPage: perPage, Total: total, TotalPages: lastPage})
}

func getSerie(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		jsonErr(w, 400, "id invalido")
		return
	}

	cacheKey := "poseidon:serie:" + strconv.Itoa(id)
	if b, ok := getDetailCache(cacheKey); ok {
		serveRawJSON(w, b)
		return
	}

	cached, hasCached := findSerieByID(id)
	if !hasCached {
		cached, hasCached = pgFindCatalogPost(id, "s")
	}

	// PHD2-only series: fetch seasons from PHD2 detail page
	if hasCached && cached.PHD2Only && cached.FlixLatamSlug == "" {
		seasons, err := fetchPHD2SerieSeasons(cached.TMDbID, cached.PHD2Slug)
		temporadas := []SeasonInfo{}
		if err == nil {
			for _, s := range seasons {
				temporadas = append(temporadas, SeasonInfo{
					ID:             cached.ID*1000 + s.Number,
					Number:         s.Number,
					TotalEpisodios: len(s.Episodes),
				})
			}
		}
		detail := SerieDetail{
			ID:          cached.ID,
			TmdbID:      cached.TMDbID,
			Titulo:      cached.Title,
			PosterURL:   fullImageURL(cached.Images.Poster),
			BannerURL:   fullImageURL(cached.Images.Backdrop),
			Descripcion: cached.Overview,
			Rating:      parseRating(cached.Rating),
			ReleaseDate: releaseDate(cached.ReleaseDate),
			Generos:     resolveGenres(cached.Genres),
			Temporadas:  temporadas,
		}
		setDetailCache(cacheKey, detail, detailTTL)
		json200(w, detail)
		return
	}

	// FlixLatam series: scrape detail page for full metadata + seasons
	if hasCached && cached.FlixLatamSlug != "" {
		html, err := scrapeFlixLatam(flixlatamBase + "/serie/" + cached.FlixLatamSlug)
		if err != nil {
			jsonErr(w, 502, err.Error())
			return
		}

		title := stripTags(extractBetween(html, `<h1>`, `</h1>`))
		if title == "" {
			title = cached.Title
		}
		overview := stripTags(extractBetween(html, `<div class="wp-content"><p>`, `</p>`))
		ratingStr := stripTags(extractBetween(html, `<div class="rating-value">`, `</div>`))
		var rating float64
		if i := strings.Index(ratingStr, "/"); i >= 0 {
			rating, _ = strconv.ParseFloat(strings.TrimSpace(ratingStr[:i]), 64)
		} else {
			rating, _ = strconv.ParseFloat(strings.TrimSpace(ratingStr), 64)
		}

		// Poster from sheader section
		sheader := extractBetween(html, `<div class="sheader">`, `<div class="sbox">`)
		poster := extractBetween(sheader, `src="`, `"`)
		if poster == "" {
			poster = fullImageURL(cached.Images.Poster)
		}

		// Release date YYYY-MM-DD if present in a <span class="date"> element
		rd := releaseDate(cached.ReleaseDate)
		tmp := html
		for {
			idx := strings.Index(tmp, `<span class="date">`)
			if idx < 0 {
				break
			}
			tmp = tmp[idx+19:]
			ei := strings.Index(tmp, `</span>`)
			if ei < 0 {
				break
			}
			candidate := stripTags(tmp[:ei])
			if len(candidate) == 10 && candidate[4] == '-' && candidate[7] == '-' {
				rd = candidate
				break
			}
		}

		// Genres from URL slugs in .sgeneros
		var genreIDs []int
		sgenHTML := extractBetween(html, `<div class="sgeneros">`, `</div>`)
		for _, gp := range strings.Split(sgenHTML, `/generos/`)[1:] {
			ei := strings.IndexAny(gp, `"'>`)
			if ei >= 0 {
				if gid, ok := flixlatamGenreSlugToID[gp[:ei]]; ok {
					genreIDs = append(genreIDs, gid)
				}
			}
		}

		// Seasons + episode list
		seasons := parseFlixLatamSeasons(html)
		if b, merr := json.Marshal(seasons); merr == nil {
			setCacheBytes("flixlatam:seasons:"+cached.FlixLatamSlug, b, seasonTTL)
		}
		temporadas := make([]SeasonInfo, 0, len(seasons))
		for _, s := range seasons {
			temporadas = append(temporadas, SeasonInfo{
				ID:             cached.ID*1000 + s.Number,
				Number:         s.Number,
				TotalEpisodios: len(s.Episodes),
			})
		}

		detail := SerieDetail{
			ID:          cached.ID,
			TmdbID:      cached.TMDbID,
			Titulo:      title,
			PosterURL:   poster,
			BannerURL:   fullImageURL(cached.Images.Backdrop),
			Descripcion: overview,
			Rating:      rating,
			ReleaseDate: rd,
			Generos:     resolveGenres(genreIDs),
			Temporadas:  temporadas,
		}
		setDetailCache(cacheKey, detail, detailTTL)
		json200(w, detail)
		return
	}

	if hasCached && cached.ProviderOnly && cached.FlixLatamSlug == "" && cached.PHD2Slug == "" && cached.Slug == "" {
		if detail, ok := buildExternalSerieDetail(cached); ok {
			setDetailCache(cacheKey, detail, detailTTL)
			json200(w, detail)
			return
		}
		jsonErr(w, 404, "no encontrado")
		return
	}

	// Legacy CVT path
	slug, ok := resolveSlug(id)
	if !ok {
		if hasCached && cached.Slug != "" {
			slug = cached.Slug
			ok = true
		}
	}
	if !ok {
		slug, ok = fetchSlugForID(id, "tvshows")
		if !ok {
			jsonErr(w, 404, "no encontrado")
			return
		}
	}

	type singleRes struct {
		post CvtPost
		err  error
	}
	singleCh := make(chan singleRes, 1)
	epsCh := make(chan []CvtEpisode, 1)

	go func() {
		data, err := cvtGet("/single", map[string]string{"post_type": "tvshows", "post_name": slug})
		if err != nil {
			singleCh <- singleRes{err: err}
			return
		}
		var resp CvtSingleResp
		if json.Unmarshal(data, &resp) != nil || resp.Error {
			singleCh <- singleRes{err: fmt.Errorf("upstream error")}
			return
		}
		singleCh <- singleRes{post: resp.Data}
	}()
	go func() {
		data, err := cvtGet("/episodes", map[string]string{"post_id": strconv.Itoa(id)})
		if err != nil {
			epsCh <- nil
			return
		}
		var resp CvtEpisodesResp
		if json.Unmarshal(data, &resp) != nil || resp.Error {
			epsCh <- nil
			return
		}
		epsCh <- resp.Data
	}()

	sr := <-singleCh
	if sr.err != nil {
		jsonErr(w, 502, sr.err.Error())
		return
	}
	p := sr.post
	eps := <-epsCh

	seasonCount := map[int]int{}
	for _, ep := range eps {
		seasonCount[ep.SeasonNumber]++
	}
	// Also merge seasons from PHD2 that CVT may not have (e.g. newly released seasons)
	if hasCached && cached.PHD2Slug != "" && cached.TMDbID > 0 {
		if phd2Seasons, err := fetchPHD2SerieSeasons(cached.TMDbID, cached.PHD2Slug); err == nil {
			for _, s := range phd2Seasons {
				if _, exists := seasonCount[s.Number]; !exists {
					seasonCount[s.Number] = len(s.Episodes)
				}
			}
		}
	}
	seasonNums := make([]int, 0, len(seasonCount))
	for n := range seasonCount {
		seasonNums = append(seasonNums, n)
	}
	sort.Ints(seasonNums)

	temporadas := make([]SeasonInfo, 0, len(seasonNums))
	for _, n := range seasonNums {
		temporadas = append(temporadas, SeasonInfo{
			ID:             id*1000 + n,
			Number:         n,
			TotalEpisodios: seasonCount[n],
		})
	}

	tmdbID := 0
	if hasCached {
		tmdbID = cached.TMDbID
	}

	detail := SerieDetail{
		ID:             p.ID,
		TmdbID:         tmdbID,
		Titulo:         p.Title,
		TituloOriginal: p.OriginalTitle,
		PosterURL:      fullImageURL(p.Images.Poster),
		BannerURL:      fullImageURL(p.Images.Backdrop),
		Descripcion:    p.Overview,
		Rating:         parseRating(p.Rating),
		ReleaseDate:    releaseDate(p.ReleaseDate),
		Generos:        resolveGenres(p.Genres),
		Temporadas:     temporadas,
	}
	setDetailCache(cacheKey, detail, detailTTL)
	json200(w, detail)
}

func getSeasonEpisodes(w http.ResponseWriter, r *http.Request) {
	serieID, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		jsonErr(w, 400, "id invalido")
		return
	}
	seasonNum, err := strconv.Atoi(r.PathValue("n"))
	if err != nil {
		jsonErr(w, 400, "temporada invalida")
		return
	}

	cacheKey := "poseidon:season:" + strconv.Itoa(serieID) + ":" + strconv.Itoa(seasonNum)
	if b, ok := getDetailCache(cacheKey); ok {
		if shouldRefreshSeasonDetail(cacheKey, b) {
			if raw, err := fetchAndCacheSeasonEpisodes(serieID, seasonNum, cacheKey); err == nil {
				b = raw
			}
		}
		if !seasonDetailHasServers(b) {
			jsonErr(w, 404, "sin servidores compatibles")
			return
		}
		serveRawJSON(w, b)
		return
	}

	// Singleflight: if multiple users request the same uncached season concurrently,
	// only one scrape runs — all others wait and share the result.
	raw, err, _ := sfGroup.Do(cacheKey, func() (any, error) {
		return fetchAndCacheSeasonEpisodes(serieID, seasonNum, cacheKey)
	})
	if err != nil {
		jsonErr(w, 502, err.Error())
		return
	}
	if !seasonDetailHasServers(raw.([]byte)) {
		jsonErr(w, 404, "sin servidores compatibles")
		return
	}
	serveRawJSON(w, raw.([]byte))
}

// fetchAndCacheSeasonEpisodes scrapes episode servers for a season and caches the result.
// Called via singleflight so concurrent identical requests share one scrape.
func fetchAndCacheSeasonEpisodes(serieID, seasonNum int, cacheKey string) ([]byte, error) {
	cacheAndMarshal := func(season SeasonDetail, hasServers bool) ([]byte, error) {
		season = sanitizeSeasonDetail(season)
		hasServers = seasonHasAllowedServers(season)
		b, err := json.Marshal(season)
		if err != nil {
			return nil, err
		}
		setCacheBytes(cacheKey, b, seasonTTL)
		return b, nil
	}

	serieCached, found := findSerieByID(serieID)
	if !found {
		serieCached, found = pgFindCatalogPost(serieID, "s")
	}
	if !found {
		return nil, fmt.Errorf("serie no encontrada")
	}

	// FlixLatam path
	if serieCached.FlixLatamSlug != "" {
		seasons, err := fetchFlixLatamSeasons(serieCached.FlixLatamSlug)
		if err != nil {
			return nil, err
		}
		var targetSeason *flixlatamSeasonInfo
		for i := range seasons {
			if seasons[i].Number == seasonNum {
				targetSeason = &seasons[i]
				break
			}
		}
		if targetSeason == nil {
			return nil, fmt.Errorf("temporada no encontrada")
		}
		type flixResult struct {
			ep      flixlatamEpisodeInfo
			servers []EpisodeServer
		}
		results := make([]flixResult, len(targetSeason.Episodes))
		var wg sync.WaitGroup
		for i, ep := range targetSeason.Episodes {
			wg.Add(1)
			go func(idx int, ep flixlatamEpisodeInfo) {
				defer wg.Done()
				results[idx] = flixResult{ep: ep, servers: fetchFlixLatamEpisodeServers(serieCached.FlixLatamSlug, seasonNum, ep.Number)}
			}(i, ep)
		}
		wg.Wait()
		episodios := make([]EpisodeDetail, len(results))
		hasServers := false
		for i, res := range results {
			epID := serieID*100000 + seasonNum*1000 + res.ep.Number
			episodios[i] = EpisodeDetail{ID: epID, Number: res.ep.Number, Titulo: res.ep.Title, Servidores: res.servers}
			if len(res.servers) > 0 {
				hasServers = true
			}
		}
		season := SeasonDetail{SerieID: serieID, Number: seasonNum, Episodios: episodios}
		if serieCached.PHD2Slug != "" && serieCached.TMDbID > 0 {
			season = appendPHD2SeasonServers(serieCached, season, seasonNum)
		}
		if hasProviderLinks(serieCached) {
			season = appendExternalSeasonServers(serieCached, season)
		}
		return cacheAndMarshal(season, hasServers)
	}

	// PHD2-only path
	if serieCached.PHD2Only {
		seasons, err := fetchPHD2SerieSeasons(serieCached.TMDbID, serieCached.PHD2Slug)
		if err != nil {
			return nil, err
		}
		var targetSeason *Phd2Season
		for i := range seasons {
			if seasons[i].Number == seasonNum {
				targetSeason = &seasons[i]
				break
			}
		}
		if targetSeason == nil || len(targetSeason.Episodes) == 0 {
			return nil, fmt.Errorf("temporada no encontrada")
		}
		type phd2Result struct {
			ep      Phd2Episode
			servers []EpisodeServer
		}
		results := make([]phd2Result, len(targetSeason.Episodes))
		var wg sync.WaitGroup
		for i, ep := range targetSeason.Episodes {
			wg.Add(1)
			go func(idx int, ep Phd2Episode) {
				defer wg.Done()
				results[idx] = phd2Result{ep: ep, servers: fetchPHD2EpisodeServers(serieCached.TMDbID, serieCached.PHD2Slug, seasonNum, ep.Number)}
			}(i, ep)
		}
		wg.Wait()
		episodios := make([]EpisodeDetail, len(results))
		hasServers := false
		for i, res := range results {
			epID := serieID*100000 + seasonNum*1000 + res.ep.Number
			episodios[i] = EpisodeDetail{ID: epID, Number: res.ep.Number, Titulo: res.ep.Title, Imagen: res.ep.Image, Servidores: res.servers}
			if len(res.servers) > 0 {
				hasServers = true
			}
		}
		season := SeasonDetail{SerieID: serieID, Number: seasonNum, Episodios: episodios}
		if hasProviderLinks(serieCached) {
			season = appendExternalSeasonServers(serieCached, season)
		}
		return cacheAndMarshal(season, hasServers)
	}

	if serieCached.ProviderOnly && serieCached.FlixLatamSlug == "" && serieCached.PHD2Slug == "" && serieCached.Slug == "" {
		episodios := fetchExternalSeasonEpisodes(serieCached, seasonNum)
		if len(episodios) == 0 {
			return nil, fmt.Errorf("temporada no encontrada")
		}
		return cacheAndMarshal(SeasonDetail{SerieID: serieID, Number: seasonNum, Episodios: episodios}, len(episodios) > 0)
	}

	// CVT legacy path
	data, err := cvtGet("/episodes", map[string]string{"post_id": strconv.Itoa(serieID)})
	if err != nil {
		return nil, err
	}
	var resp CvtEpisodesResp
	if json.Unmarshal(data, &resp) != nil || resp.Error {
		return nil, fmt.Errorf("error de upstream")
	}
	var seasonEps []CvtEpisode
	for _, ep := range resp.Data {
		if ep.SeasonNumber == seasonNum {
			seasonEps = append(seasonEps, ep)
		}
	}
	// If CVT has no episodes for this season, fall back to PHD2 if available
	if len(seasonEps) == 0 && serieCached.PHD2Slug != "" && serieCached.TMDbID > 0 {
		phd2Seasons, err := fetchPHD2SerieSeasons(serieCached.TMDbID, serieCached.PHD2Slug)
		if err == nil {
			var targetSeason *Phd2Season
			for i := range phd2Seasons {
				if phd2Seasons[i].Number == seasonNum {
					targetSeason = &phd2Seasons[i]
					break
				}
			}
			if targetSeason != nil && len(targetSeason.Episodes) > 0 {
				type phd2Result struct {
					ep      Phd2Episode
					servers []EpisodeServer
				}
				results := make([]phd2Result, len(targetSeason.Episodes))
				var wg sync.WaitGroup
				for i, ep := range targetSeason.Episodes {
					wg.Add(1)
					go func(idx int, ep Phd2Episode) {
						defer wg.Done()
						results[idx] = phd2Result{ep: ep, servers: fetchPHD2EpisodeServers(serieCached.TMDbID, serieCached.PHD2Slug, seasonNum, ep.Number)}
					}(i, ep)
				}
				wg.Wait()
				episodios := make([]EpisodeDetail, len(results))
				hasServers := false
				for i, res := range results {
					epID := serieID*100000 + seasonNum*1000 + res.ep.Number
					episodios[i] = EpisodeDetail{ID: epID, Number: res.ep.Number, Titulo: res.ep.Title, Imagen: res.ep.Image, Servidores: res.servers}
					if len(res.servers) > 0 {
						hasServers = true
					}
				}
				season := SeasonDetail{SerieID: serieID, Number: seasonNum, Episodios: episodios}
				if hasProviderLinks(serieCached) {
					season = appendExternalSeasonServers(serieCached, season)
				}
				return cacheAndMarshal(season, hasServers)
			}
		}
		return nil, fmt.Errorf("temporada no encontrada")
	}
	if len(seasonEps) == 0 {
		return nil, fmt.Errorf("temporada no encontrada")
	}
	sort.Slice(seasonEps, func(i, j int) bool { return seasonEps[i].EpisodeNumber < seasonEps[j].EpisodeNumber })
	type result struct {
		ep      CvtEpisode
		servers []CvtPlayerItem
	}
	results := make([]result, len(seasonEps))
	var wg sync.WaitGroup
	for i, ep := range seasonEps {
		wg.Add(1)
		go func(idx int, ep CvtEpisode) {
			defer wg.Done()
			results[idx] = result{ep: ep, servers: fetchPlayer(ep.ID, "episodes")}
		}(i, ep)
	}
	wg.Wait()
	episodios := make([]EpisodeDetail, len(results))
	hasServers := false
	for i, res := range results {
		imagen := ""
		if res.ep.StillPath != "" {
			imagen = "https://image.tmdb.org/t/p/w500" + res.ep.StillPath
		}
		episodios[i] = EpisodeDetail{ID: res.ep.ID, Number: res.ep.EpisodeNumber, Titulo: res.ep.Title, Imagen: imagen, Servidores: cvtToEpServers(res.servers)}
		if len(episodios[i].Servidores) > 0 {
			hasServers = true
		}
	}
	season := SeasonDetail{SerieID: serieID, Number: seasonNum, Episodios: episodios}
	if hasProviderLinks(serieCached) {
		season = appendExternalSeasonServers(serieCached, season)
	}
	return cacheAndMarshal(season, hasServers)
}

func appendPHD2SeasonServers(post CvtPost, season SeasonDetail, seasonNum int) SeasonDetail {
	phd2Seasons, err := fetchPHD2SerieSeasons(post.TMDbID, post.PHD2Slug)
	if err != nil {
		return season
	}
	var targetSeason *Phd2Season
	for i := range phd2Seasons {
		if phd2Seasons[i].Number == seasonNum {
			targetSeason = &phd2Seasons[i]
			break
		}
	}
	if targetSeason == nil || len(targetSeason.Episodes) == 0 {
		return season
	}

	epIndex := make(map[int]int, len(season.Episodios))
	for i := range season.Episodios {
		epIndex[season.Episodios[i].Number] = i
	}
	for _, ep := range targetSeason.Episodes {
		servers := fetchPHD2EpisodeServers(post.TMDbID, post.PHD2Slug, seasonNum, ep.Number)
		if len(servers) == 0 {
			continue
		}
		if idx, ok := epIndex[ep.Number]; ok {
			season.Episodios[idx].Servidores = mergeEpisodeServers(season.Episodios[idx].Servidores, servers)
			if season.Episodios[idx].Imagen == "" {
				season.Episodios[idx].Imagen = ep.Image
			}
			continue
		}
		epID := post.ID*100000 + seasonNum*1000 + ep.Number
		season.Episodios = append(season.Episodios, EpisodeDetail{ID: epID, Number: ep.Number, Titulo: ep.Title, Imagen: ep.Image, Servidores: normalizeEpisodeServerIDs(servers)})
		epIndex[ep.Number] = len(season.Episodios) - 1
	}
	sort.Slice(season.Episodios, func(i, j int) bool { return season.Episodios[i].Number < season.Episodios[j].Number })
	return season
}

func mergeEpisodeServers(base, extra []EpisodeServer) []EpisodeServer {
	seen := make(map[string]bool, len(base)+len(extra))
	out := make([]EpisodeServer, 0, len(base)+len(extra))
	add := func(s EpisodeServer) {
		key := strings.TrimSpace(s.URL)
		if key == "" {
			key = strings.TrimSpace(s.PlayerURL)
		}
		if key == "" || seen[key] {
			return
		}
		seen[key] = true
		out = append(out, s)
	}
	for _, s := range base {
		add(s)
	}
	for _, s := range extra {
		add(s)
	}
	return normalizeEpisodeServerIDs(out)
}

func normalizeEpisodeServerIDs(servers []EpisodeServer) []EpisodeServer {
	for i := range servers {
		servers[i].ID = i + 1
	}
	return servers
}

func searchSeries(w http.ResponseWriter, r *http.Request) {
	q := localSearchNorm(r.URL.Query().Get("q"))
	if q == "" {
		json200(w, []SerieSummary{})
		return
	}
	if pgPool != nil {
		rows := pgSearchCatalog(q, "s", 50)
		series := make([]SerieSummary, 0, len(rows))
		for _, row := range rows {
			series = append(series, SerieSummary{int(row.ID), row.Titulo, row.PosterURL})
		}
		json200(w, series)
		return
	}
	sl, ready := gc.getSeries()
	if !ready {
		json200(w, []SerieSummary{})
		return
	}
	series := make([]SerieSummary, 0, 30)
	for _, p := range sl.items {
		if serieHasServers(p.ID) && (strings.Contains(localSearchNorm(p.Title), q) || strings.Contains(localSearchNorm(p.OriginalTitle), q)) {
			series = append(series, SerieSummary{p.ID, p.Title, fullImageURL(p.Images.Poster)})
			if len(series) >= 50 {
				break
			}
		}
	}
	json200(w, series)
}

func listSeriesByGenre(w http.ResponseWriter, r *http.Request) {
	genre := r.PathValue("genre")
	page := pageParam(r)
	gid, ok := genreByName[genre]
	if !ok {
		jsonErr(w, 404, "género no encontrado")
		return
	}

	if posts, total, lastPage, dbOK := pgListCatalogPosts("s", page, &gid); dbOK {
		series := make([]SerieSummary, 0, len(posts))
		for _, p := range posts {
			series = append(series, SerieSummary{p.ID, p.Title, fullImageURL(p.Images.Poster)})
		}
		json200(w, SeriePage{Series: series, Page: page, PerPage: perPage, Total: total, TotalPages: lastPage})
		return
	}

	if sl, ready := gc.getSeries(); ready {
		var filtered []CvtPost
		for _, p := range sl.items {
			if !serieHasServers(p.ID) {
				continue
			}
			for _, g := range p.Genres {
				if g == gid {
					filtered = append(filtered, p)
					break
				}
			}
		}
		posts, total := pageFromList(filtered, page)
		series := make([]SerieSummary, 0, len(posts))
		for _, p := range posts {
			series = append(series, SerieSummary{p.ID, p.Title, fullImageURL(p.Images.Poster)})
		}
		json200(w, SeriePage{Series: series, Page: page, PerPage: perPage, Total: total, TotalPages: calcTotalPages(total)})
		return
	}

	posts, total, lastPage, err := livePageWithParams("tvshows", page, map[string]string{"genres": strconv.Itoa(gid)})
	if err != nil {
		jsonErr(w, 502, err.Error())
		return
	}
	posts = visibleSeriePosts(posts)
	total = len(posts)
	lastPage = calcTotalPages(total)
	series := make([]SerieSummary, 0, len(posts))
	for _, p := range posts {
		series = append(series, SerieSummary{p.ID, p.Title, fullImageURL(p.Images.Poster)})
	}
	json200(w, SeriePage{Series: series, Page: page, PerPage: perPage, Total: total, TotalPages: lastPage})
}

// ── Global search ─────────────────────────────────────────────────────────────

func searchCombined(w http.ResponseWriter, r *http.Request) {
	q := localSearchNorm(r.URL.Query().Get("q"))
	if q == "" {
		json200(w, []SearchResult{})
		return
	}
	if pgPool != nil {
		movieRows := pgSearchCatalog(q, "m", 30)
		serieRows := pgSearchCatalog(q, "s", 30)
		results := make([]SearchResult, 0, len(movieRows)+len(serieRows))
		for _, row := range movieRows {
			results = append(results, SearchResult{int(row.ID), row.Titulo, row.PosterURL, false})
		}
		for _, row := range serieRows {
			results = append(results, SearchResult{int(row.ID), row.Titulo, row.PosterURL, true})
		}
		json200(w, results)
		return
	}
	// Fallback: in-memory
	results := []SearchResult{}
	if sl, ready := gc.getMovies(); ready {
		for _, p := range sl.items {
			if movieHasServers(p.ID) && (strings.Contains(localSearchNorm(p.Title), q) || strings.Contains(localSearchNorm(p.OriginalTitle), q)) {
				results = append(results, SearchResult{p.ID, p.Title, fullImageURL(p.Images.Poster), false})
				if len(results) >= 30 {
					break
				}
			}
		}
	}
	if sl, ready := gc.getSeries(); ready {
		for _, p := range sl.items {
			if serieHasServers(p.ID) && (strings.Contains(localSearchNorm(p.Title), q) || strings.Contains(localSearchNorm(p.OriginalTitle), q)) {
				results = append(results, SearchResult{p.ID, p.Title, fullImageURL(p.Images.Poster), true})
				if len(results) >= 60 {
					break
				}
			}
		}
	}
	json200(w, results)
}

func searchAll(w http.ResponseWriter, r *http.Request) {
	raw := strings.ReplaceAll(r.PathValue("query"), "+", " ")
	q := localSearchNorm(raw)
	movies := []MovieSummary{}
	series := []SerieSummary{}

	movieRows := pgSearchCatalog(q, "m", 30)
	serieRows := pgSearchCatalog(q, "s", 30)
	if pgPool != nil {
		for _, row := range movieRows {
			movies = append(movies, MovieSummary{int(row.ID), row.Titulo, row.PosterURL})
		}
		for _, row := range serieRows {
			series = append(series, SerieSummary{int(row.ID), row.Titulo, row.PosterURL})
		}
	} else {
		if sl, ready := gc.getMovies(); ready {
			for _, p := range sl.items {
				if movieHasServers(p.ID) && (strings.Contains(localSearchNorm(p.Title), q) || strings.Contains(localSearchNorm(p.OriginalTitle), q)) {
					movies = append(movies, MovieSummary{p.ID, p.Title, fullImageURL(p.Images.Poster)})
					if len(movies) >= 30 {
						break
					}
				}
			}
		}
		if sl, ready := gc.getSeries(); ready {
			for _, p := range sl.items {
				if serieHasServers(p.ID) && (strings.Contains(localSearchNorm(p.Title), q) || strings.Contains(localSearchNorm(p.OriginalTitle), q)) {
					series = append(series, SerieSummary{p.ID, p.Title, fullImageURL(p.Images.Poster)})
					if len(series) >= 30 {
						break
					}
				}
			}
		}
	}
	json200(w, map[string]any{
		"query":  raw,
		"movies": movies,
		"series": series,
		"total":  len(movies) + len(series),
	})
}

const catalogHTML = `<!DOCTYPE html>
<html lang="es">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Catálogo</title>
<style>
*{box-sizing:border-box;margin:0;padding:0}
body{background:#0f0f0f;color:#eee;font-family:'Segoe UI',sans-serif;min-height:100vh}
header{background:#1a1a2e;padding:20px;text-align:center;border-bottom:2px solid #e50914}
header h1{font-size:1.8rem;color:#e50914;letter-spacing:2px}
header p{color:#aaa;font-size:.85rem;margin-top:4px}
.search-bar{display:flex;justify-content:center;padding:24px 16px;gap:10px}
.search-bar input{width:100%;max-width:520px;padding:12px 18px;font-size:1rem;border-radius:8px;border:2px solid #333;background:#1e1e1e;color:#eee;outline:none;transition:border .2s}
.search-bar input:focus{border-color:#e50914}
.search-bar button{padding:12px 22px;background:#e50914;color:#fff;border:none;border-radius:8px;font-size:1rem;cursor:pointer;white-space:nowrap}
.search-bar button:hover{background:#c0000e}
.tabs{display:flex;justify-content:center;gap:12px;padding:0 16px 20px}
.tab{padding:8px 24px;border-radius:20px;cursor:pointer;font-size:.9rem;border:2px solid #333;background:transparent;color:#aaa;transition:.2s}
.tab.active{background:#e50914;border-color:#e50914;color:#fff}
.stats{text-align:center;color:#666;font-size:.82rem;padding-bottom:16px}
.grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(140px,1fr));gap:16px;padding:0 16px 40px;max-width:1400px;margin:0 auto}
.card{background:#1a1a1a;border-radius:10px;overflow:hidden;cursor:default;transition:transform .2s,box-shadow .2s}
.card:hover{transform:translateY(-4px);box-shadow:0 8px 24px rgba(229,9,20,.3)}
.card img{width:100%;aspect-ratio:2/3;object-fit:cover;display:block;background:#111}
.card-title{padding:8px;font-size:.78rem;line-height:1.3;color:#ddd;text-align:center;min-height:44px;display:flex;align-items:center;justify-content:center}
.badge{display:inline-block;font-size:.65rem;padding:2px 6px;border-radius:4px;margin-bottom:4px}
.badge-m{background:#1a4a8a;color:#7ab}
.badge-s{background:#2a1a4a;color:#a7b}
.empty{text-align:center;color:#555;padding:60px 16px;font-size:1rem}
#loading{text-align:center;padding:60px;color:#555}
.spinner{display:inline-block;width:36px;height:36px;border:3px solid #333;border-top-color:#e50914;border-radius:50%;animation:spin .7s linear infinite;margin-bottom:12px}
@keyframes spin{to{transform:rotate(360deg)}}
</style>
</head>
<body>
<header>
  <h1>CATÁLOGO</h1>
  <p id="headerSub">Cargando...</p>
</header>
<div class="search-bar">
  <input id="searchInput" type="text" placeholder="Buscar película o serie..." autocomplete="off">
  <button onclick="doSearch()">Buscar</button>
</div>
<div class="tabs">
  <button class="tab active" onclick="setTab('all')">Todo</button>
  <button class="tab" onclick="setTab('m')">Películas</button>
  <button class="tab" onclick="setTab('s')">Series</button>
</div>
<div class="stats" id="statsLine"></div>
<div id="grid" class="grid"></div>
<script>
let currentTab = 'all';
let lastMovies = [];
let lastSeries = [];

function setTab(t) {
  currentTab = t;
  document.querySelectorAll('.tab').forEach((el,i) => el.classList.toggle('active', ['all','m','s'][i]===t));
  renderCurrent();
}

function renderCurrent() {
  const movies = currentTab !== 's' ? lastMovies : [];
  const series = currentTab !== 'm' ? lastSeries : [];
  renderGrid(movies, series);
}

function renderGrid(movies, series) {
  const grid = document.getElementById('grid');
  if (!movies.length && !series.length) {
    grid.innerHTML = '<div class="empty" style="grid-column:1/-1">Sin resultados</div>';
    return;
  }
  let html = '';
  movies.forEach(m => {
    const img = m.poster_url || 'data:image/svg+xml,<svg xmlns="http://www.w3.org/2000/svg"/>';
    html += '<div class="card"><img src="'+escHtml(img)+'" loading="lazy" onerror="this.src=\'data:image/svg+xml,<svg xmlns=\\\'http://www.w3.org/2000/svg\\\'/>\'" alt=""><div class="card-title"><span class="badge badge-m">PEL</span><br>'+escHtml(m.titulo)+'</div></div>';
  });
  series.forEach(s => {
    const img = s.poster_url || 'data:image/svg+xml,<svg xmlns="http://www.w3.org/2000/svg"/>';
    html += '<div class="card"><img src="'+escHtml(img)+'" loading="lazy" onerror="this.src=\'data:image/svg+xml,<svg xmlns=\\\'http://www.w3.org/2000/svg\\\'/>\'" alt=""><div class="card-title"><span class="badge badge-s">SERIE</span><br>'+escHtml(s.titulo)+'</div></div>';
  });
  grid.innerHTML = html;
}

function escHtml(s) { return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;'); }

function doSearch() {
  const q = document.getElementById('searchInput').value.trim();
  if (q.length < 2) { loadDefault(); return; }
  document.getElementById('grid').innerHTML = '<div id="loading"><div class="spinner"></div><br>Buscando...</div>';
  Promise.all([
    fetch('/api/movies/search?q='+encodeURIComponent(q)).then(r=>r.json()),
    fetch('/api/series/search?q='+encodeURIComponent(q)).then(r=>r.json())
  ]).then(([movies, series]) => {
    lastMovies = movies || [];
    lastSeries = series || [];
    document.getElementById('statsLine').textContent = (lastMovies.length+lastSeries.length) + ' resultados para "'+q+'"';
    renderCurrent();
  });
}

function loadDefault() {
  document.getElementById('grid').innerHTML = '<div id="loading"><div class="spinner"></div><br>Cargando catálogo...</div>';
  fetch('/api/catalog/stats').then(r=>r.json()).then(stats => {
    const visible = stats.visibles || {};
    const pm = visible.peliculas || stats.memoria.peliculas || stats.db.peliculas;
    const ps = visible.series || stats.memoria.series || stats.db.series;
    document.getElementById('headerSub').textContent = pm.toLocaleString() + ' películas · ' + ps.toLocaleString() + ' series';
    document.getElementById('statsLine').textContent = 'Mostrando los más recientes';
  });
  Promise.all([
    fetch('/api/movies?page=1').then(r=>r.json()),
    fetch('/api/series?page=1').then(r=>r.json())
  ]).then(([mp, sp]) => {
    lastMovies = mp.movies || [];
    lastSeries = sp.series || [];
    renderCurrent();
  });
}

document.getElementById('searchInput').addEventListener('keydown', e => { if(e.key==='Enter') doSearch(); });
document.getElementById('searchInput').addEventListener('input', e => { if(e.target.value.trim()==='') loadDefault(); });

loadDefault();
</script>
</body>
</html>`

func catalogHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Write([]byte(catalogHTML))
}

func catalogStats(w http.ResponseWriter, r *http.Request) {
	movies, series := pgCatalogCount()
	sl1, _ := gc.getMovies()
	sl2, _ := gc.getSeries()
	visibleMovies := len(visibleMoviePosts(sl1.items))
	visibleSeries := len(visibleSeriePosts(sl2.items))
	if pgPool != nil {
		visibleMovies, visibleSeries = pgVisibleCatalogCount()
	}
	json200(w, map[string]any{
		"db": map[string]int{
			"peliculas": movies,
			"series":    series,
			"total":     movies + series,
		},
		"memoria": map[string]int{
			"peliculas": sl1.total,
			"series":    sl2.total,
			"total":     sl1.total + sl2.total,
		},
		"visibles": map[string]int{
			"peliculas": visibleMovies,
			"series":    visibleSeries,
			"total":     visibleMovies + visibleSeries,
		},
	})
}

// ── main ──────────────────────────────────────────────────────────────────────

func main() {
	if len(os.Args) > 1 && os.Args[1] == "update-json" {
		os.Exit(runJSONCatalogUpdater(os.Args[2:]))
	}

	if len(os.Args) > 1 && os.Args[1] == "monitor-flixlatam" {
		initPostgres()
		if pgPool == nil {
			log.Printf("[flix-monitor] no se puede continuar sin una conexion PostgreSQL valida")
			os.Exit(1)
		}
		loadAvailabilityFromStore()
		runFlixLatamMonitor()
		return
	}

	initRedis()
	initPostgres()
	loadAvailabilityFromStore()
	loadAvailabilityFromRedis()
	go func() {
		syncRedisStoreToPostgres()
		cleanupStoredServerQualities()
		rebuildCatalogAvailabilityFromStore()
		loadAvailabilityFromStore()
		loadAvailabilityFromRedis()
	}()
	startCacheBuilder()
	startSeriesNewContentWatcher()

	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", catalogHandler)
	mux.HandleFunc("GET /search/{query}", searchAll)
	mux.HandleFunc("GET /api/search", searchCombined)
	mux.HandleFunc("GET /api/genres", listGenres)

	mux.HandleFunc("GET /api/movies", listMovies)
	mux.HandleFunc("GET /api/movies/search", searchMovies)
	mux.HandleFunc("GET /api/movies/genre/{genre}", listByGenre)
	mux.HandleFunc("GET /api/movies/{id}", getMovie)

	mux.HandleFunc("GET /api/series", listSeries)
	mux.HandleFunc("GET /api/series/search", searchSeries)
	mux.HandleFunc("GET /api/series/genre/{genre}", listSeriesByGenre)
	mux.HandleFunc("GET /api/series/{id}", getSerie)
	mux.HandleFunc("GET /api/series/{id}/season/{n}", getSeasonEpisodes)

	mux.HandleFunc("GET /api/catalog/stats", catalogStats)
	mux.HandleFunc("GET /catalogo", catalogHandler)

	mux.HandleFunc("POST /admin/enrich-movies", func(w http.ResponseWriter, r *http.Request) {
		go enrichMoviesWithoutServers()
		json200(w, map[string]string{"status": "started"})
	})

	mux.HandleFunc("POST /admin/refresh-serie/{id}", func(w http.ResponseWriter, r *http.Request) {
		id, err := strconv.Atoi(r.PathValue("id"))
		if err != nil {
			jsonErr(w, 400, "id invalido")
			return
		}
		serieKey := "poseidon:serie:" + strconv.Itoa(id)
		if pgPool != nil {
			pgPool.Exec(rctx, `DELETE FROM poseidon_store WHERE key = $1`, serieKey)
		}
		if redisAvailable.Load() {
			rdb.Del(rctx, serieKey)
			// Also clear PHD2/FlixLatam season sub-caches so they get re-scraped
			post, ok := findSerieByID(id)
			if !ok {
				post, ok = pgFindCatalogPost(id, "s")
			}
			if ok {
				if post.PHD2Slug != "" && post.TMDbID > 0 {
					rdb.Del(rctx, fmt.Sprintf("phd2:seasons:%d", post.TMDbID))
				}
				if post.FlixLatamSlug != "" {
					rdb.Del(rctx, "flixlatam:seasons:"+post.FlixLatamSlug)
				}
			}
		}
		json200(w, map[string]string{"status": "ok", "cleared": serieKey})
	})

	log.Println("API escuchando en :8080")
	log.Fatal(http.ListenAndServe(":8080", stripTrailingSlash(mux)))
}
