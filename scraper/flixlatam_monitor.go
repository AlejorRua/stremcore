package main

import (
	"context"
	"encoding/json"
	"fmt"
	htmlpkg "html"
	"log"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type flixLatestEpisode struct {
	Title   string `json:"title"`
	Slug    string `json:"slug"`
	URL     string `json:"url"`
	Poster  string `json:"poster,omitempty"`
	Season  int    `json:"season"`
	Episode int    `json:"episode"`
}

type flixLatestItem struct {
	Title       string `json:"title"`
	Slug        string `json:"slug"`
	URL         string `json:"url"`
	Poster      string `json:"poster,omitempty"`
	ReleaseDate string `json:"release_date,omitempty"`
	Rating      string `json:"rating,omitempty"`
}

type flixLatestSnapshot struct {
	Episodes []flixLatestEpisode `json:"episodes"`
	Movies   []flixLatestItem    `json:"movies"`
	Series   []flixLatestItem    `json:"series"`
}

type flixMonitorState struct {
	ProcessedEpisodes map[string]bool `json:"processed_episodes"`
	ProcessedMovies   map[string]bool `json:"processed_movies"`
	ProcessedSeries   map[string]bool `json:"processed_series"`
	UpdatedAt         string          `json:"updated_at"`
}

func runFlixLatamMonitor() {
	interval := envDuration("FLIX_MONITOR_INTERVAL", time.Minute)
	statePath := envString("FLIX_MONITOR_STATE", filepath.Join(exportRootDir(), "state", "flixlatam_latest.json"))
	once := hasArg("--once") || envBool("FLIX_MONITOR_ONCE", false)
	log.Printf("[flix-monitor] iniciado interval=%s state=%s", interval, statePath)

	run := func() error {
		changed, err := runFlixLatamMonitorCycle(statePath)
		if changed && envBool("FLIX_MONITOR_EXPORT", true) {
			if exportErr := runCatalogExportAndPush(); exportErr != nil {
				if err != nil {
					return fmt.Errorf("%v; export/push: %w", err, exportErr)
				}
				return fmt.Errorf("export/push: %w", exportErr)
			}
		}
		return err
	}

	if err := run(); err != nil {
		log.Printf("[flix-monitor] error: %v", err)
		if once {
			os.Exit(1)
		}
	}
	if once {
		return
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for range ticker.C {
		if err := run(); err != nil {
			log.Printf("[flix-monitor] error: %v", err)
		}
	}
}

func runFlixLatamMonitorCycle(statePath string) (bool, error) {
	if changeFile := strings.TrimSpace(os.Getenv("FLIX_CHANGE_FILE")); changeFile != "" {
		return runFlixLatamChangeFile(statePath, changeFile)
	}

	home, err := scrapeFlixLatam(flixlatamBase + "/")
	if err != nil {
		return false, err
	}
	snapshot := parseFlixLatamHomeLatest(home)
	state := loadFlixMonitorState(statePath)
	changed := false

	for _, ep := range snapshot.Episodes {
		key := flixEpisodeKey(ep)
		if state.ProcessedEpisodes[key] {
			continue
		}
		log.Printf("[flix-monitor] episodio nuevo: %s T%dE%d (%s)", ep.Title, ep.Season, ep.Episode, ep.Slug)
		if refreshFlixLatamEpisode(ep) {
			state.ProcessedEpisodes[key] = true
			changed = true
		} else {
			log.Printf("[flix-monitor] episodio pendiente, se reintentara: %s", key)
		}
	}

	for _, movie := range snapshot.Movies {
		if state.ProcessedMovies[movie.Slug] {
			continue
		}
		log.Printf("[flix-monitor] pelicula nueva: %s (%s)", movie.Title, movie.Slug)
		if refreshFlixLatamMovie(movie) {
			state.ProcessedMovies[movie.Slug] = true
			changed = true
		} else {
			log.Printf("[flix-monitor] pelicula pendiente, se reintentara: %s", movie.Slug)
		}
	}

	for _, serie := range snapshot.Series {
		if state.ProcessedSeries[serie.Slug] {
			continue
		}
		log.Printf("[flix-monitor] serie nueva: %s (%s)", serie.Title, serie.Slug)
		if refreshFlixLatamSeries(serie) {
			state.ProcessedSeries[serie.Slug] = true
			changed = true
		} else {
			log.Printf("[flix-monitor] serie pendiente, se reintentara: %s", serie.Slug)
		}
	}

	if changed {
		state.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		if err := saveFlixMonitorState(statePath, state); err != nil {
			return changed, err
		}
		log.Printf("[flix-monitor] cambios procesados y state actualizado")
	} else {
		log.Printf("[flix-monitor] sin novedades")
	}
	return changed, nil
}

type flixWatcherChangeSet struct {
	Episodes []flixWatcherItem `json:"episodes"`
	Movies   []flixWatcherItem `json:"movies"`
	Series   []flixWatcherItem `json:"series"`
}

type flixWatcherItem struct {
	Kind     string `json:"kind"`
	Key      string `json:"key"`
	Slug     string `json:"slug"`
	Title    string `json:"title"`
	URL      string `json:"url"`
	Image    string `json:"image"`
	Season   int    `json:"season"`
	Episode  int    `json:"episode"`
	Year     string `json:"year"`
	Metadata string `json:"metadata"`
}

func runFlixLatamChangeFile(statePath, changeFile string) (bool, error) {
	data, err := os.ReadFile(changeFile)
	if err != nil {
		return false, err
	}
	var changes flixWatcherChangeSet
	if err := json.Unmarshal(data, &changes); err != nil {
		return false, fmt.Errorf("leyendo change file %s: %w", changeFile, err)
	}
	state := loadFlixMonitorState(statePath)
	changed := false
	pending := 0
	total := 0

	for _, item := range changes.Episodes {
		ep, ok := watcherEpisode(item)
		if !ok {
			total++
			log.Printf("[flix-monitor] episodio invalido en change file: %+v", item)
			pending++
			continue
		}
		key := flixEpisodeKey(ep)
		if state.ProcessedEpisodes[key] {
			continue
		}
		total++
		log.Printf("[flix-monitor] episodio por change file: %s T%dE%d (%s)", ep.Title, ep.Season, ep.Episode, ep.Slug)
		if refreshFlixLatamEpisode(ep) {
			state.ProcessedEpisodes[key] = true
			changed = true
		} else {
			log.Printf("[flix-monitor] episodio pendiente, se reintentara: %s", key)
			pending++
		}
	}

	for _, item := range changes.Movies {
		movie, ok := watcherItem(item, true)
		if !ok {
			total++
			log.Printf("[flix-monitor] pelicula invalida en change file: %+v", item)
			pending++
			continue
		}
		if state.ProcessedMovies[movie.Slug] {
			continue
		}
		total++
		log.Printf("[flix-monitor] pelicula por change file: %s (%s)", movie.Title, movie.Slug)
		if refreshFlixLatamMovie(movie) {
			state.ProcessedMovies[movie.Slug] = true
			changed = true
		} else {
			log.Printf("[flix-monitor] pelicula pendiente, se reintentara: %s", movie.Slug)
			pending++
		}
	}

	for _, item := range changes.Series {
		serie, ok := watcherItem(item, false)
		if !ok {
			total++
			log.Printf("[flix-monitor] serie invalida en change file: %+v", item)
			pending++
			continue
		}
		if state.ProcessedSeries[serie.Slug] {
			continue
		}
		total++
		log.Printf("[flix-monitor] serie por change file: %s (%s)", serie.Title, serie.Slug)
		if refreshFlixLatamSeries(serie) {
			state.ProcessedSeries[serie.Slug] = true
			changed = true
		} else {
			log.Printf("[flix-monitor] serie pendiente, se reintentara: %s", serie.Slug)
			pending++
		}
	}

	if changed {
		state.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		if err := saveFlixMonitorState(statePath, state); err != nil {
			return changed, err
		}
		log.Printf("[flix-monitor] change file procesado y state actualizado: %s", changeFile)
	} else {
		log.Printf("[flix-monitor] change file sin cambios aplicados: %s", changeFile)
	}
	if pending > 0 {
		return changed, fmt.Errorf("change file con %d/%d item(s) pendiente(s): %s", pending, total, changeFile)
	}
	return changed, nil
}

func watcherEpisode(item flixWatcherItem) (flixLatestEpisode, bool) {
	slug := item.Slug
	season := item.Season
	episode := item.Episode
	itemURL := item.URL
	if itemURL != "" {
		if m := epURLRe.FindStringSubmatch(itemURL); len(m) == 4 {
			slug = htmlpkg.UnescapeString(m[1])
			season, _ = strconv.Atoi(m[2])
			episode, _ = strconv.Atoi(m[3])
		}
	}
	if slug == "" || season <= 0 || episode <= 0 {
		return flixLatestEpisode{}, false
	}
	if itemURL == "" {
		itemURL = fmt.Sprintf("%s/serie/%s/temporada/%d/capitulo/%d", flixlatamBase, slug, season, episode)
	}
	title := strings.TrimSpace(item.Title)
	if title == "" {
		title = flixTitleFromSlug(slug)
	}
	return flixLatestEpisode{Title: title, Slug: slug, URL: itemURL, Poster: item.Image, Season: season, Episode: episode}, true
}

func watcherItem(item flixWatcherItem, movie bool) (flixLatestItem, bool) {
	slug := item.Slug
	itemURL := item.URL
	if itemURL != "" {
		if movie {
			if m := movieURLRe.FindStringSubmatch(itemURL); len(m) == 2 {
				slug = htmlpkg.UnescapeString(m[1])
			}
		} else if m := serieURLRe.FindStringSubmatch(itemURL); len(m) == 2 && !strings.Contains(m[1], "/temporada/") {
			slug = htmlpkg.UnescapeString(m[1])
		}
	}
	if slug == "" {
		return flixLatestItem{}, false
	}
	if itemURL == "" {
		kind := "serie"
		if movie {
			kind = "pelicula"
		}
		itemURL = flixlatamBase + "/" + kind + "/" + slug
	}
	title := strings.TrimSpace(item.Title)
	if title == "" {
		title = flixTitleFromSlug(slug)
	}
	releaseDate := ""
	year := strings.TrimSpace(item.Year)
	if len(year) == 4 {
		releaseDate = year + "-01-01"
	}
	return flixLatestItem{Title: title, Slug: slug, URL: itemURL, Poster: item.Image, ReleaseDate: releaseDate}, true
}

func flixTitleFromSlug(slug string) string {
	parts := strings.Fields(strings.ReplaceAll(strings.Trim(slug, "-"), "-", " "))
	if len(parts) == 0 {
		return slug
	}
	return strings.Join(parts, " ")
}

func parseFlixLatamHomeLatest(home string) flixLatestSnapshot {
	epsHTML := sectionBetween(home, "<!-- Últimos Episodios -->", "<!-- Películas -->")
	moviesHTML := sectionBetween(home, "<!-- Películas -->", "<!-- Series -->")
	seriesHTML := sectionBetween(home, "<!-- Series -->", "</footer>")
	return flixLatestSnapshot{
		Episodes: parseFlixLatestEpisodes(epsHTML),
		Movies:   parseFlixLatestMovies(moviesHTML),
		Series:   parseFlixLatestSeries(seriesHTML),
	}
}

func sectionBetween(s, start, end string) string {
	si := strings.Index(s, start)
	if si < 0 {
		return ""
	}
	s = s[si+len(start):]
	ei := strings.Index(s, end)
	if ei < 0 {
		return s
	}
	return s[:ei]
}

var (
	articleBlockRe = regexp.MustCompile(`(?is)<article\b[^>]*>.*?</article>`)
	epURLRe        = regexp.MustCompile(`(?i)https?://flixlatam\.com/serie/([^"']+)/temporada/(\d+)/capitulo/(\d+)`)
	movieURLRe     = regexp.MustCompile(`(?i)https?://flixlatam\.com/pelicula/([^"']+)`)
	serieURLRe     = regexp.MustCompile(`(?i)https?://flixlatam\.com/serie/([^"']+)`)
	h3Re           = regexp.MustCompile(`(?is)<h3\b[^>]*>(.*?)</h3>`)
	imgSrcRe       = regexp.MustCompile(`(?is)<img\b[^>]*\bsrc=["']([^"']+)["']`)
	yearSpanRe     = regexp.MustCompile(`(?is)<span\b[^>]*>\s*((?:19|20)\d{2})\s*</span>`)
)

func parseFlixLatestEpisodes(section string) []flixLatestEpisode {
	seen := map[string]bool{}
	var out []flixLatestEpisode
	for _, art := range articleBlockRe.FindAllString(section, -1) {
		m := epURLRe.FindStringSubmatch(art)
		if len(m) != 4 {
			continue
		}
		season, _ := strconv.Atoi(m[2])
		episode, _ := strconv.Atoi(m[3])
		ep := flixLatestEpisode{
			Slug:    htmlpkg.UnescapeString(m[1]),
			URL:     m[0],
			Season:  season,
			Episode: episode,
			Title:   articleTitle(art),
			Poster:  articlePoster(art),
		}
		key := flixEpisodeKey(ep)
		if ep.Slug == "" || season <= 0 || episode <= 0 || seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, ep)
	}
	return out
}

func parseFlixLatestMovies(section string) []flixLatestItem {
	seen := map[string]bool{}
	var out []flixLatestItem
	for _, art := range articleBlockRe.FindAllString(section, -1) {
		m := movieURLRe.FindStringSubmatch(art)
		if len(m) != 2 {
			continue
		}
		item := articleItem(art, m[1], m[0])
		if item.Slug == "" || item.Title == "" || seen[item.Slug] {
			continue
		}
		seen[item.Slug] = true
		out = append(out, item)
	}
	return out
}

func parseFlixLatestSeries(section string) []flixLatestItem {
	seen := map[string]bool{}
	var out []flixLatestItem
	for _, art := range articleBlockRe.FindAllString(section, -1) {
		m := serieURLRe.FindStringSubmatch(art)
		if len(m) != 2 || strings.Contains(m[1], "/temporada/") {
			continue
		}
		item := articleItem(art, m[1], m[0])
		if item.Slug == "" || item.Title == "" || seen[item.Slug] {
			continue
		}
		seen[item.Slug] = true
		out = append(out, item)
	}
	return out
}

func articleItem(art, slug, url string) flixLatestItem {
	year := firstRegexpGroup(yearSpanRe, art)
	releaseDate := ""
	if year != "" {
		releaseDate = year + "-01-01"
	}
	return flixLatestItem{
		Title:       articleTitle(art),
		Slug:        htmlpkg.UnescapeString(slug),
		URL:         url,
		Poster:      articlePoster(art),
		ReleaseDate: releaseDate,
		Rating:      strings.TrimSpace(stripTags(extractBetween(art, `<div class="rating">`, `</div>`))),
	}
}

func articleTitle(art string) string {
	return htmlpkg.UnescapeString(strings.TrimSpace(stripTags(firstRegexpGroup(h3Re, art))))
}

func articlePoster(art string) string {
	return htmlpkg.UnescapeString(firstRegexpGroup(imgSrcRe, art))
}

func firstRegexpGroup(re *regexp.Regexp, s string) string {
	m := re.FindStringSubmatch(s)
	if len(m) < 2 {
		return ""
	}
	return strings.TrimSpace(m[1])
}

func flixEpisodeKey(ep flixLatestEpisode) string {
	return fmt.Sprintf("%s:t%d:e%d", ep.Slug, ep.Season, ep.Episode)
}

func refreshFlixLatamEpisode(ep flixLatestEpisode) bool {
	post, ok := pgFindCatalogPostByFlixLatamSlug(ep.Slug, "s")
	if !ok {
		post = resolveFlixLatamCatalogPost(flixPostFromEpisode(ep), "s")
		pgUpsertCatalog([]CvtPost{post}, "s")
	} else if post.FlixLatamSlug == "" {
		post.FlixLatamSlug = ep.Slug
		pgUpsertCatalog([]CvtPost{post}, "s")
	}
	deleteFlixLatamSerieCaches(post, ep.Season)
	warmSerieDetail(post)
	key := fmt.Sprintf("poseidon:season:%d:%d", post.ID, ep.Season)
	if _, err := fetchAndCacheSeasonEpisodes(post.ID, ep.Season, key); err != nil {
		log.Printf("[flix-monitor] no se pudo refrescar %s: %v", key, err)
		return false
	}
	if b, ok := pgGet(key); ok && seasonDetailHasServers(b) {
		pgSetCatalogHasServers("s", post.ID, true)
		return true
	}
	return false
}

func refreshFlixLatamSeries(item flixLatestItem) bool {
	post, ok := pgFindCatalogPostByFlixLatamSlug(item.Slug, "s")
	if !ok {
		post = resolveFlixLatamCatalogPost(flixPostFromItem(item, false), "s")
		pgUpsertCatalog([]CvtPost{post}, "s")
	}
	deleteFlixLatamSerieCaches(post, 0)
	warmSerieDetail(post)
	warmSerieSeasons(post)
	return anyStoredSeasonHasServers(post.ID)
}

func refreshFlixLatamMovie(item flixLatestItem) bool {
	post, ok := pgFindCatalogPostByFlixLatamSlug(item.Slug, "m")
	if !ok {
		post = resolveFlixLatamCatalogPost(flixPostFromItem(item, true), "m")
		pgUpsertCatalog([]CvtPost{post}, "m")
	}
	deleteStoreKey(fmt.Sprintf("poseidon:movie:%d", post.ID))
	warmMovieDetail(post)
	key := fmt.Sprintf("poseidon:movie:%d", post.ID)
	if b, ok := pgGet(key); ok && movieDetailHasServers(b) {
		pgSetCatalogHasServers("m", post.ID, true)
		return true
	}
	return false
}

func resolveFlixLatamCatalogPost(candidate CvtPost, tipo string) CvtPost {
	if candidate.TMDbID > 0 {
		if existing, ok := pgFindCatalogPost(candidate.TMDbID, tipo); ok {
			merged := mergeDuplicatePost(existing, candidate)
			merged.ID = existing.ID
			if merged.FlixLatamSlug == "" {
				merged.FlixLatamSlug = candidate.FlixLatamSlug
			}
			merged.FlixLatamOnly = false
			return merged
		}
	}
	return candidate
}

func flixPostFromEpisode(ep flixLatestEpisode) CvtPost {
	item := flixLatestItem{Title: ep.Title, Slug: ep.Slug, URL: flixlatamBase + "/serie/" + ep.Slug, Poster: ep.Poster}
	return flixPostFromItem(item, false)
}

func flixPostFromItem(item flixLatestItem, movie bool) CvtPost {
	post := CvtPost{
		ID:            slugToID(item.Slug),
		Title:         item.Title,
		Slug:          item.Slug,
		Images:        CvtImages{Poster: item.Poster},
		Rating:        item.Rating,
		ReleaseDate:   item.ReleaseDate,
		FlixLatamSlug: item.Slug,
		FlixLatamOnly: true,
	}
	if movie {
		post.Type = "movies"
	} else {
		post.Type = "tvshows"
	}
	return enrichFlixPostFromDetail(post, movie)
}

func enrichFlixPostFromDetail(post CvtPost, movie bool) CvtPost {
	kind := "serie"
	if movie {
		kind = "pelicula"
	}
	html, err := scrapeFlixLatam(flixlatamBase + "/" + kind + "/" + post.FlixLatamSlug)
	if err != nil {
		return post
	}
	if title := stripTags(extractBetween(html, `<h1>`, `</h1>`)); title != "" {
		post.Title = htmlpkg.UnescapeString(title)
	}
	if overview := stripTags(extractBetween(html, `<div class="wp-content"><p>`, `</p>`)); overview != "" {
		post.Overview = htmlpkg.UnescapeString(overview)
	}
	if rating := stripTags(extractBetween(html, `<div class="rating-value">`, `</div>`)); rating != "" {
		post.Rating = strings.TrimSpace(strings.TrimPrefix(rating, "★"))
	}
	sheader := extractBetween(html, `<div class="sheader">`, `<div class="sbox">`)
	if poster := extractBetween(sheader, `src="`, `"`); poster != "" {
		post.Images.Poster = htmlpkg.UnescapeString(poster)
	}
	if rd := firstMatch(html, `(?is)<span\s+class=["']date["']>\s*((?:19|20)\d{2})(?:-\d{2}-\d{2})?\s*</span>`); rd != "" {
		if len(rd) == 4 {
			post.ReleaseDate = rd + "-01-01"
		} else {
			post.ReleaseDate = rd
		}
	}
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
	if len(genreIDs) > 0 {
		post.Genres = genreIDs
	}
	return post
}

func pgFindCatalogPostByFlixLatamSlug(slug, tipo string) (CvtPost, bool) {
	if pgPool == nil || slug == "" {
		return CvtPost{}, false
	}
	var data []byte
	err := pgPool.QueryRow(context.Background(), `
		SELECT data
		FROM poseidon_catalog
		WHERE tipo=$1 AND data <> '{}'::jsonb AND data->>'flixlatam_slug'=$2
		ORDER BY has_servers DESC, updated_at DESC, id DESC
		LIMIT 1
	`, tipo, slug).Scan(&data)
	if err != nil {
		return CvtPost{}, false
	}
	return pgPostFromData(data)
}

func deleteFlixLatamSerieCaches(post CvtPost, season int) {
	deleteStoreKey("poseidon:serie:" + strconv.Itoa(post.ID))
	if post.FlixLatamSlug != "" {
		deleteStoreKey("flixlatam:seasons:" + post.FlixLatamSlug)
	}
	if season > 0 {
		deleteStoreKey(fmt.Sprintf("poseidon:season:%d:%d", post.ID, season))
	}
}

func deleteStoreKey(key string) {
	if pgPool != nil {
		_, _ = pgPool.Exec(context.Background(), `DELETE FROM poseidon_store WHERE key=$1`, key)
	}
	if redisAvailable.Load() {
		rdb.Del(rctx, key)
	}
}

func anyStoredSeasonHasServers(serieID int) bool {
	if pgPool == nil {
		return false
	}
	rows, err := pgPool.Query(context.Background(), `
		SELECT value
		FROM poseidon_store
		WHERE key LIKE $1
	`, fmt.Sprintf("poseidon:season:%d:%%", serieID))
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var value []byte
		if rows.Scan(&value) == nil && seasonDetailHasServers(value) {
			pgSetCatalogHasServers("s", serieID, true)
			return true
		}
	}
	return false
}

func loadFlixMonitorState(path string) flixMonitorState {
	state := flixMonitorState{
		ProcessedEpisodes: map[string]bool{},
		ProcessedMovies:   map[string]bool{},
		ProcessedSeries:   map[string]bool{},
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return state
	}
	if json.Unmarshal(b, &state) != nil {
		return state
	}
	if state.ProcessedEpisodes == nil {
		state.ProcessedEpisodes = map[string]bool{}
	}
	if state.ProcessedMovies == nil {
		state.ProcessedMovies = map[string]bool{}
	}
	if state.ProcessedSeries == nil {
		state.ProcessedSeries = map[string]bool{}
	}
	return state
}

func saveFlixMonitorState(path string, state flixMonitorState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

func runCatalogExportAndPush() error {
	exportDoc := envString("FLIX_MONITOR_EXPORT_DOC", filepath.Join(exportRootDir(), "doc"))
	exportRoot := envString("FLIX_MONITOR_EXPORT_ROOT", exportRootDir())
	if err := runCommand(exportDoc, "python3", "export_to_github.py"); err != nil {
		return err
	}
	if err := runCommand(exportDoc, "python3", "generate_search_index.py"); err != nil {
		return err
	}
	paths := []string{"movies", "series", "pages", "search", "meta.json", "state"}
	args := append([]string{"-C", exportRoot, "status", "--porcelain", "--"}, paths...)
	out, err := exec.Command("git", args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("git status: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if strings.TrimSpace(string(out)) == "" {
		log.Printf("[flix-monitor] export sin cambios")
		return nil
	}
	args = append([]string{"-C", exportRoot, "add", "-A", "--"}, paths...)
	if out, err = exec.Command("git", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("git add: %w: %s", err, strings.TrimSpace(string(out)))
	}
	msg := "update latest flixlatam content"
	if out, err = exec.Command("git", "-C", exportRoot, "commit", "-m", msg).CombinedOutput(); err != nil {
		return fmt.Errorf("git commit: %w: %s", err, strings.TrimSpace(string(out)))
	}
	remote := envString("FLIX_MONITOR_GIT_REMOTE", "origin")
	branch := envString("FLIX_MONITOR_GIT_BRANCH", "main")
	if err := gitPushWithRebase(exportRoot, remote, branch); err != nil {
		return err
	}
	if extra := strings.TrimSpace(os.Getenv("FLIX_MONITOR_EXTRA_REMOTE")); extra != "" {
		if err := gitPushWithRebase(exportRoot, extra, branch); err != nil {
			log.Printf("[flix-monitor] push remoto extra %s fallo: %v", extra, err)
		}
	}
	return nil
}

func gitPushWithRebase(repo, remote, branch string) error {
	out, err := exec.Command("git", "-C", repo, "push", remote, branch).CombinedOutput()
	if err == nil {
		log.Printf("[flix-monitor] push %s/%s ok", remote, branch)
		return nil
	}
	log.Printf("[flix-monitor] push rechazado, intentando rebase: %s", strings.TrimSpace(string(out)))
	if out, err = exec.Command("git", "-C", repo, "pull", "--rebase", remote, branch).CombinedOutput(); err != nil {
		return fmt.Errorf("git pull --rebase: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if out, err = exec.Command("git", "-C", repo, "push", remote, branch).CombinedOutput(); err != nil {
		return fmt.Errorf("git push: %w: %s", err, strings.TrimSpace(string(out)))
	}
	log.Printf("[flix-monitor] push %s/%s ok despues de rebase", remote, branch)
	return nil
}

func runCommand(dir, name string, args ...string) error {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if len(out) > 0 {
		log.Printf("[flix-monitor] %s output:\n%s", name, strings.TrimSpace(string(out)))
	}
	if err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

func exportRootDir() string {
	if v := strings.TrimSpace(os.Getenv("FLIX_MONITOR_EXPORT_ROOT")); v != "" {
		return v
	}
	wd, err := os.Getwd()
	if err == nil {
		if filepath.Base(wd) == "scraper" {
			return filepath.Dir(wd)
		}
		if filepath.Base(wd) == "doc" {
			return filepath.Dir(wd)
		}
	}
	return "/home/localhost/Documents/github_export"
}

func envString(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	v := strings.TrimSpace(strings.ToLower(os.Getenv(key)))
	if v == "" {
		return fallback
	}
	return v == "1" || v == "true" || v == "yes" || v == "si" || v == "sí" || v == "on"
}

func envDuration(key string, fallback time.Duration) time.Duration {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	if d, err := time.ParseDuration(v); err == nil {
		return d
	}
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return time.Duration(n) * time.Second
	}
	return fallback
}

func hasArg(arg string) bool {
	for _, a := range os.Args[1:] {
		if a == arg {
			return true
		}
	}
	return false
}

func cleanFlixURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}
