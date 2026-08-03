package main

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type repoCatalogEntry struct {
	ID          int      `json:"id"`
	Titulo      string   `json:"titulo"`
	TituloOrig  string   `json:"titulo_orig"`
	Poster      string   `json:"poster"`
	ReleaseDate string   `json:"release_date"`
	Genres      []string `json:"genres"`
	Rating      string   `json:"rating"`
	Backdrop    string   `json:"backdrop"`
	Overview    string   `json:"overview"`
	Trailer     string   `json:"trailer"`
	Slug        string   `json:"slug"`
}

type repoMovieDetail struct {
	repoCatalogEntry
	Runtime    string   `json:"runtime"`
	Servidores []Server `json:"servidores"`
}

type jsonCatalogIndex struct {
	root   string
	movies []repoCatalogEntry
	series []repoCatalogEntry
}

type jsonUpdateConfig struct {
	Root      string
	StatePath string
	HomeURL   string
	YearURL   string
	DryRun    bool
	MaxItems  int
	Workers   int
	HTTPOnly  bool
}

type jsonPendingChanges struct {
	Episodes []flixLatestEpisode
	Movies   []flixLatestItem
	Series   []flixLatestItem
}

func runJSONCatalogUpdater(args []string) int {
	flags := flag.NewFlagSet("update-json", flag.ContinueOnError)
	root := exportRootDir()
	cfg := jsonUpdateConfig{}
	flags.StringVar(&cfg.Root, "root", envString("FLIX_JSON_ROOT", root), "raiz del catalogo JSON")
	flags.StringVar(&cfg.StatePath, "state", "", "archivo de estado")
	flags.StringVar(&cfg.HomeURL, "url", envString("FLIX_HOME_URL", flixlatamBase+"/"), "home de FlixLatam")
	flags.StringVar(
		&cfg.YearURL,
		"year-url",
		envString("FLIX_YEAR_URL", fmt.Sprintf("%s/year/%d", flixlatamBase, time.Now().UTC().Year())),
		"listado anual de FlixLatam; vacio lo desactiva",
	)
	flags.BoolVar(&cfg.DryRun, "dry-run", false, "solo detectar; no escribir archivos")
	flags.IntVar(&cfg.MaxItems, "max-items", 0, "maximo de items por tipo; 0 procesa todos")
	flags.IntVar(&cfg.Workers, "workers", 4, "descargas simultaneas de episodios")
	flags.BoolVar(&cfg.HTTPOnly, "detect-only", false, "alias de --dry-run")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	cfg.Root = filepath.Clean(cfg.Root)
	if cfg.StatePath == "" {
		cfg.StatePath = filepath.Join(cfg.Root, "state", "flixlatam_latest.json")
	}
	if cfg.Workers < 1 {
		cfg.Workers = 1
	}
	if cfg.HTTPOnly {
		cfg.DryRun = true
	}

	changes, catalog, state, err := detectJSONCatalogChanges(cfg)
	if err != nil {
		log.Printf("[json-update] deteccion fallo: %v", err)
		return 1
	}
	logJSONPending(changes)
	if cfg.DryRun {
		log.Printf("[json-update] dry-run finalizado; no se modificaron archivos")
		return 0
	}

	var failures []error
	successful := 0
	for _, item := range limitedItems(changes.Movies, cfg.MaxItems) {
		if err := updateJSONMovie(cfg, catalog, item); err != nil {
			failures = append(failures, fmt.Errorf("pelicula %s: %w", item.Slug, err))
			continue
		}
		state.ProcessedMovies[item.Slug] = true
		successful++
	}
	for _, item := range limitedItems(changes.Series, cfg.MaxItems) {
		if err := updateJSONSeries(cfg, catalog, item); err != nil {
			failures = append(failures, fmt.Errorf("serie %s: %w", item.Slug, err))
			continue
		}
		state.ProcessedSeries[item.Slug] = true
		successful++
	}
	// Recarga el indice porque una serie nueva puede recibir tambien un episodio nuevo.
	if len(changes.Series) > 0 {
		catalog, err = loadJSONCatalogIndex(cfg.Root)
		if err != nil {
			failures = append(failures, err)
		}
	}
	for _, item := range limitedItems(changes.Episodes, cfg.MaxItems) {
		if err := updateJSONEpisode(cfg, catalog, item); err != nil {
			failures = append(failures, fmt.Errorf("episodio %s: %w", flixEpisodeKey(item), err))
			continue
		}
		state.ProcessedEpisodes[flixEpisodeKey(item)] = true
		successful++
	}

	if successful > 0 {
		state.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
		if err := writeJSONAtomic(cfg.StatePath, state); err != nil {
			failures = append(failures, fmt.Errorf("guardando estado: %w", err))
		}
	}
	if len(failures) > 0 {
		for _, failure := range failures {
			log.Printf("[json-update] pendiente: %v", failure)
		}
		if successful == 0 {
			return 1
		}
		log.Printf("[json-update] actualizacion parcial: exitosos=%d pendientes=%d", successful, len(failures))
		return 0
	}
	log.Printf("[json-update] catalogo JSON actualizado correctamente")
	return 0
}

func detectJSONCatalogChanges(cfg jsonUpdateConfig) (jsonPendingChanges, *jsonCatalogIndex, flixMonitorState, error) {
	home, err := scrapeFlixLatam(cfg.HomeURL)
	if err != nil {
		return jsonPendingChanges{}, nil, flixMonitorState{}, err
	}
	snapshot := parseFlixLatamHomeLatest(home)
	if strings.TrimSpace(cfg.YearURL) != "" {
		yearSnapshot, yearErr := fetchFlixLatamYearSnapshot(cfg.YearURL)
		if yearErr != nil {
			return jsonPendingChanges{}, nil, flixMonitorState{}, yearErr
		}
		snapshot.Movies = mergeFlixLatestItems(snapshot.Movies, yearSnapshot.Movies)
		snapshot.Series = mergeFlixLatestItems(snapshot.Series, yearSnapshot.Series)
	}
	if len(snapshot.Episodes)+len(snapshot.Movies)+len(snapshot.Series) == 0 {
		return jsonPendingChanges{}, nil, flixMonitorState{}, errors.New("las fuentes no contienen episodios, peliculas ni series reconocibles")
	}
	catalog, err := loadJSONCatalogIndex(cfg.Root)
	if err != nil {
		return jsonPendingChanges{}, nil, flixMonitorState{}, err
	}
	state := loadFlixMonitorState(cfg.StatePath)
	changes := jsonPendingChanges{}
	for _, movie := range snapshot.Movies {
		if catalog.findMovie(movie) == nil {
			changes.Movies = appendUniqueJSONItem(changes.Movies, movie)
		} else if !cfg.DryRun {
			state.ProcessedMovies[movie.Slug] = true
		}
	}
	for _, serie := range snapshot.Series {
		if catalog.findSeries(serie.Slug, serie.Title, serie.ReleaseDate) == nil {
			changes.Series = appendUniqueJSONItem(changes.Series, serie)
		} else if !cfg.DryRun {
			state.ProcessedSeries[serie.Slug] = true
		}
	}
	for _, episode := range snapshot.Episodes {
		if !catalog.episodeExists(episode) {
			changes.Episodes = append(changes.Episodes, episode)
		} else if !cfg.DryRun {
			state.ProcessedEpisodes[flixEpisodeKey(episode)] = true
		}
	}
	return changes, catalog, state, nil
}

func fetchFlixLatamYearSnapshot(yearURL string) (flixLatestSnapshot, error) {
	firstPage, err := scrapeFlixLatam(yearURL)
	if err != nil {
		return flixLatestSnapshot{}, fmt.Errorf("descargando listado anual %s: %w", yearURL, err)
	}
	totalPages := parseFlixLatamTotalPages(firstPage)
	if totalPages < 1 {
		totalPages = 1
	}

	snapshot := flixLatestSnapshot{}
	addPage := func(pageHTML string) {
		snapshot.Movies = mergeFlixLatestItems(snapshot.Movies, parseFlixLatestMovies(pageHTML))
		snapshot.Series = mergeFlixLatestItems(snapshot.Series, parseFlixLatestSeries(pageHTML))
	}
	addPage(firstPage)

	separator := "?"
	if strings.Contains(yearURL, "?") {
		separator = "&"
	}
	for page := 2; page <= totalPages; page++ {
		pageURL := fmt.Sprintf("%s%spage=%d", yearURL, separator, page)
		pageHTML, pageErr := scrapeFlixLatam(pageURL)
		if pageErr != nil {
			return flixLatestSnapshot{}, fmt.Errorf("descargando pagina anual %d/%d: %w", page, totalPages, pageErr)
		}
		addPage(pageHTML)
	}

	log.Printf(
		"[json-update] listado anual: paginas=%d peliculas=%d series=%d",
		totalPages,
		len(snapshot.Movies),
		len(snapshot.Series),
	)
	return snapshot, nil
}

func mergeFlixLatestItems(current, incoming []flixLatestItem) []flixLatestItem {
	seen := make(map[string]bool, len(current)+len(incoming))
	for _, item := range current {
		seen[strings.ToLower(strings.TrimSpace(item.Slug))] = true
	}
	for _, item := range incoming {
		key := strings.ToLower(strings.TrimSpace(item.Slug))
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		current = append(current, item)
	}
	return current
}

func appendUniqueJSONItem(items []flixLatestItem, candidate flixLatestItem) []flixLatestItem {
	key := normalizeTitle(candidate.Title) + "|" + yearOf(candidate.ReleaseDate)
	for _, item := range items {
		if normalizeTitle(item.Title)+"|"+yearOf(item.ReleaseDate) == key {
			return items
		}
	}
	return append(items, candidate)
}

func logJSONPending(changes jsonPendingChanges) {
	log.Printf("[json-update] detectados episodios=%d peliculas=%d series=%d", len(changes.Episodes), len(changes.Movies), len(changes.Series))
	for _, item := range changes.Movies {
		log.Printf("[json-update] pelicula nueva: %s (%s)", item.Title, item.Slug)
	}
	for _, item := range changes.Series {
		log.Printf("[json-update] serie nueva: %s (%s)", item.Title, item.Slug)
	}
	for _, item := range changes.Episodes {
		log.Printf("[json-update] episodio nuevo: %s T%dE%d (%s)", item.Title, item.Season, item.Episode, item.Slug)
	}
}

func limitedItems[T any](items []T, max int) []T {
	if max > 0 && len(items) > max {
		return items[:max]
	}
	return items
}

func loadJSONCatalogIndex(root string) (*jsonCatalogIndex, error) {
	index := &jsonCatalogIndex{root: root}
	var err error
	index.movies, err = loadJSONEntries(filepath.Join(root, "movies"))
	if err != nil {
		return nil, err
	}
	index.series, err = loadJSONEntries(filepath.Join(root, "series"))
	if err != nil {
		return nil, err
	}
	return index, nil
}

func loadJSONEntries(dir string) ([]repoCatalogEntry, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("leyendo %s: %w", dir, err)
	}
	out := make([]repoCatalogEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		var detail repoCatalogEntry
		if readJSONFile(filepath.Join(dir, entry.Name()), &detail) == nil && detail.ID > 0 {
			out = append(out, detail)
		}
	}
	return out, nil
}

func (c *jsonCatalogIndex) findMovie(item flixLatestItem) *repoCatalogEntry {
	return findJSONEntry(c.movies, item.Slug, item.Title, item.ReleaseDate)
}

func (c *jsonCatalogIndex) findSeries(slug, title, releaseDate string) *repoCatalogEntry {
	return findJSONEntry(c.series, slug, title, releaseDate)
}

func findJSONEntry(entries []repoCatalogEntry, slug, title, releaseDate string) *repoCatalogEntry {
	slug = strings.ToLower(strings.TrimSpace(slug))
	for i := range entries {
		if slug != "" && strings.ToLower(strings.TrimSpace(entries[i].Slug)) == slug {
			return &entries[i]
		}
	}
	titleKey := normalizeTitle(title)
	year := yearOf(releaseDate)
	var titleMatch *repoCatalogEntry
	for i := range entries {
		if normalizeTitle(entries[i].Titulo) != titleKey || titleKey == "" {
			continue
		}
		if year != "" && yearOf(entries[i].ReleaseDate) == year {
			return &entries[i]
		}
		if titleMatch == nil {
			titleMatch = &entries[i]
		} else {
			titleMatch = nil
			break
		}
	}
	return titleMatch
}

func (c *jsonCatalogIndex) episodeExists(item flixLatestEpisode) bool {
	serie := c.findSeries(item.Slug, item.Title, "")
	if serie == nil {
		return false
	}
	var season SeasonDetail
	path := filepath.Join(c.root, "series", strconv.Itoa(serie.ID), fmt.Sprintf("t%d.json", item.Season))
	if readJSONFile(path, &season) != nil {
		return false
	}
	for _, episode := range season.Episodios {
		if episode.Number == item.Episode && len(episode.Servidores) > 0 {
			return true
		}
	}
	return false
}

func updateJSONMovie(cfg jsonUpdateConfig, catalog *jsonCatalogIndex, item flixLatestItem) error {
	post := flixPostFromItem(item, true)
	if existing := catalog.findMovie(item); existing != nil {
		post.ID = existing.ID
	}
	servers := fetchFlixLatamMovieServers(item.Slug)
	if len(servers) == 0 {
		return errors.New("no se encontraron servidores reproducibles")
	}
	path := filepath.Join(cfg.Root, "movies", strconv.Itoa(post.ID)+".json")
	var previous repoMovieDetail
	_ = readJSONFile(path, &previous)
	detail := repoMovieDetail{
		repoCatalogEntry: repoCatalogEntry{
			ID: post.ID, Titulo: post.Title, TituloOrig: post.OriginalTitle,
			Poster: post.Images.Poster, ReleaseDate: releaseDate(post.ReleaseDate),
			Genres: jsonGenreSlugs(post.Genres), Rating: post.Rating,
			Backdrop: post.Images.Backdrop, Overview: post.Overview,
			Trailer: post.Trailer, Slug: item.Slug,
		},
		Runtime: post.Runtime, Servidores: mergeJSONMovieServers(previous.Servidores, servers),
	}
	mergeRepoMovieFields(&detail, previous)
	if err := writeJSONAtomic(path, detail); err != nil {
		return err
	}
	log.Printf("[json-update] pelicula escrita: %s servidores=%d", path, len(detail.Servidores))
	return nil
}

func updateJSONSeries(cfg jsonUpdateConfig, catalog *jsonCatalogIndex, item flixLatestItem) error {
	post := flixPostFromItem(item, false)
	if existing := catalog.findSeries(item.Slug, item.Title, item.ReleaseDate); existing != nil {
		post.ID = existing.ID
	}
	seasons, err := fetchFlixLatamSeasons(item.Slug)
	if err != nil {
		return err
	}
	playable := 0
	for _, season := range seasons {
		detail := fetchJSONSeason(post.ID, item.Slug, season, cfg.Workers)
		if len(detail.Episodios) == 0 {
			continue
		}
		path := filepath.Join(cfg.Root, "series", strconv.Itoa(post.ID), fmt.Sprintf("t%d.json", season.Number))
		var previous SeasonDetail
		_ = readJSONFile(path, &previous)
		detail = mergeJSONSeason(previous, detail)
		if err := writeJSONAtomic(path, detail); err != nil {
			return err
		}
		playable += len(detail.Episodios)
	}
	path := filepath.Join(cfg.Root, "series", strconv.Itoa(post.ID)+".json")
	var previous repoCatalogEntry
	_ = readJSONFile(path, &previous)
	detail := repoCatalogEntry{
		ID: post.ID, Titulo: post.Title, TituloOrig: post.OriginalTitle,
		Poster: post.Images.Poster, ReleaseDate: releaseDate(post.ReleaseDate),
		Genres: jsonGenreSlugs(post.Genres), Rating: post.Rating,
		Backdrop: post.Images.Backdrop, Overview: post.Overview,
		Trailer: post.Trailer, Slug: item.Slug,
	}
	mergeRepoEntryFields(&detail, previous)
	if err := writeJSONAtomic(path, detail); err != nil {
		return err
	}
	if playable == 0 {
		log.Printf("[json-update] serie escrita sin episodios disponibles todavia: %s", path)
	} else {
		log.Printf("[json-update] serie escrita: %s episodios=%d", path, playable)
	}
	return nil
}

func updateJSONEpisode(cfg jsonUpdateConfig, catalog *jsonCatalogIndex, item flixLatestEpisode) error {
	if catalog.episodeExists(item) {
		return nil
	}
	serie := catalog.findSeries(item.Slug, item.Title, "")
	if serie == nil {
		seriesItem := flixLatestItem{Title: item.Title, Slug: item.Slug, URL: flixlatamBase + "/serie/" + item.Slug, Poster: item.Poster}
		if err := updateJSONSeries(cfg, catalog, seriesItem); err != nil {
			return fmt.Errorf("creando serie del episodio: %w", err)
		}
		refreshed, err := loadJSONCatalogIndex(cfg.Root)
		if err != nil {
			return err
		}
		serie = refreshed.findSeries(item.Slug, item.Title, "")
		if serie == nil {
			return errors.New("no se pudo identificar la serie")
		}
	}
	servers := fetchFlixLatamEpisodeServers(item.Slug, item.Season, item.Episode)
	if len(servers) == 0 {
		return errors.New("no se encontraron servidores reproducibles")
	}
	path := filepath.Join(cfg.Root, "series", strconv.Itoa(serie.ID), fmt.Sprintf("t%d.json", item.Season))
	var season SeasonDetail
	_ = readJSONFile(path, &season)
	if season.SerieID == 0 {
		season.SerieID = serie.ID
		season.Number = item.Season
	}
	fresh := EpisodeDetail{
		ID:     serie.ID*100000 + item.Season*1000 + item.Episode,
		Number: item.Episode, Titulo: fmt.Sprintf("Episodio %d", item.Episode),
		Servidores: servers,
	}
	season.Episodios = mergeJSONEpisode(season.Episodios, fresh)
	if err := writeJSONAtomic(path, season); err != nil {
		return err
	}
	log.Printf("[json-update] episodio escrito: %s T%dE%d servidores=%d", item.Slug, item.Season, item.Episode, len(servers))
	return nil
}

func fetchJSONSeason(serieID int, slug string, season flixlatamSeasonInfo, workers int) SeasonDetail {
	type result struct {
		index int
		ep    EpisodeDetail
	}
	results := make(chan result, len(season.Episodes))
	sem := make(chan struct{}, workers)
	var wg sync.WaitGroup
	for i, episode := range season.Episodes {
		wg.Add(1)
		go func(index int, episode flixlatamEpisodeInfo) {
			defer wg.Done()
			sem <- struct{}{}
			servers := fetchFlixLatamEpisodeServers(slug, season.Number, episode.Number)
			<-sem
			results <- result{index: index, ep: EpisodeDetail{
				ID:     serieID*100000 + season.Number*1000 + episode.Number,
				Number: episode.Number, Titulo: episode.Title, Servidores: servers,
			}}
		}(i, episode)
	}
	wg.Wait()
	close(results)
	episodes := make([]EpisodeDetail, len(season.Episodes))
	for result := range results {
		episodes[result.index] = result.ep
	}
	playable := episodes[:0]
	for _, episode := range episodes {
		if len(episode.Servidores) > 0 {
			playable = append(playable, episode)
		}
	}
	return SeasonDetail{SerieID: serieID, Number: season.Number, Episodios: playable}
}

func mergeJSONMovieServers(existing, fresh []Server) []Server {
	seen := map[string]bool{}
	out := make([]Server, 0, len(existing)+len(fresh))
	for _, server := range append(append([]Server{}, existing...), fresh...) {
		key := strings.TrimSpace(server.URL)
		if key == "" {
			key = strings.TrimSpace(server.PlayerURL)
		}
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		server.ID = len(out) + 1
		out = append(out, server)
	}
	return out
}

func mergeJSONEpisodeServers(existing, fresh []EpisodeServer) []EpisodeServer {
	seen := map[string]bool{}
	out := make([]EpisodeServer, 0, len(existing)+len(fresh))
	for _, server := range append(append([]EpisodeServer{}, existing...), fresh...) {
		key := strings.TrimSpace(server.URL)
		if key == "" {
			key = strings.TrimSpace(server.PlayerURL)
		}
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		server.ID = len(out) + 1
		out = append(out, server)
	}
	return out
}

func mergeJSONEpisode(existing []EpisodeDetail, fresh EpisodeDetail) []EpisodeDetail {
	found := false
	for i := range existing {
		if existing[i].Number != fresh.Number {
			continue
		}
		existing[i].Servidores = mergeJSONEpisodeServers(existing[i].Servidores, fresh.Servidores)
		if existing[i].Titulo == "" {
			existing[i].Titulo = fresh.Titulo
		}
		found = true
		break
	}
	if !found {
		existing = append(existing, fresh)
	}
	sort.Slice(existing, func(i, j int) bool { return existing[i].Number < existing[j].Number })
	return existing
}

func mergeJSONSeason(existing, fresh SeasonDetail) SeasonDetail {
	if existing.SerieID == 0 {
		return fresh
	}
	for _, episode := range fresh.Episodios {
		existing.Episodios = mergeJSONEpisode(existing.Episodios, episode)
	}
	return existing
}

func mergeRepoMovieFields(detail *repoMovieDetail, previous repoMovieDetail) {
	mergeRepoEntryFields(&detail.repoCatalogEntry, previous.repoCatalogEntry)
	if detail.Runtime == "" {
		detail.Runtime = previous.Runtime
	}
}

func mergeRepoEntryFields(detail *repoCatalogEntry, previous repoCatalogEntry) {
	if previous.ID == 0 {
		return
	}
	if detail.Titulo == "" {
		detail.Titulo = previous.Titulo
	}
	if detail.TituloOrig == "" {
		detail.TituloOrig = previous.TituloOrig
	}
	if detail.Poster == "" {
		detail.Poster = previous.Poster
	}
	if detail.ReleaseDate == "" {
		detail.ReleaseDate = previous.ReleaseDate
	}
	if len(detail.Genres) == 0 {
		detail.Genres = previous.Genres
	}
	if detail.Rating == "" {
		detail.Rating = previous.Rating
	}
	if detail.Backdrop == "" {
		detail.Backdrop = previous.Backdrop
	}
	if detail.Overview == "" {
		detail.Overview = previous.Overview
	}
	if detail.Trailer == "" {
		detail.Trailer = previous.Trailer
	}
	if detail.Slug == "" {
		detail.Slug = previous.Slug
	}
}

func jsonGenreSlugs(ids []int) []string {
	slugs := make([]string, 0, len(ids))
	seen := map[string]bool{}
	for _, id := range ids {
		slug := jsonGenreSlug(id)
		if slug != "" && !seen[slug] {
			seen[slug] = true
			slugs = append(slugs, slug)
		}
	}
	return slugs
}

func jsonGenreSlug(id int) string {
	return map[int]string{
		26: "accion", 9306: "action-adventure", 51: "animacion", 25: "aventura",
		158: "belica", 27: "ciencia-ficcion", 192: "comedia", 136: "crimen",
		23209: "documental", 157: "drama", 52: "familia", 86: "fantasia",
		404: "historia", 9307: "kids", 249: "misterio", 307: "musica",
		9496: "pelicula-de-tv", 15487: "reality", 215: "romance",
		9334: "sci-fi-fantasy", 23714: "soap", 87: "suspense",
		48809: "talk", 422: "terror", 9582: "war-politics", 1594: "western",
	}[id]
}

func readJSONFile(path string, target any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, target)
}

func writeJSONAtomic(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
