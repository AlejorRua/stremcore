package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Interruptor de la fuente de contenido NUEVO (2026-11-21). FlixLatam/embed69 bloquea las
// IPs de GitHub Actions para titulos nuevos ("Making sure you're not a bot!"), asi que las
// peliculas/series nunca llegan a tener capitulos. Con FLIX_NEW_CONTENT_SOURCE=poseidon2 se
// PAUSA por completo el descubrimiento+lectura de contenido nuevo via FlixLatam (no se hace
// ninguna llamada a flixlatam.com) y se usa poseidonhd2.co en su lugar, con el mismo formato
// de catalogo de siempre. El contenido YA existente (de cualquier fuente) nunca se toca.
// Sacar la variable de entorno vuelve exactamente al comportamiento anterior - esto no
// reemplaza nada, solo pausa una rama y prende otra.
func newContentSourceIsPoseidon2() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv("FLIX_NEW_CONTENT_SOURCE")), "poseidon2")
}

func runJSONCatalogUpdaterPoseidon2(cfg jsonUpdateConfig) int {
	catalog, err := loadJSONCatalogIndex(cfg.Root)
	if err != nil {
		log.Printf("[json-update] poseidon2: no se pudo cargar el catalogo: %v", err)
		return 1
	}

	// El dueño pidio explicitamente no mezclar contenido viejo con estrenos nuevos (su catalogo
	// ordena por "ultimos agregados", y el panel de poseidonhd2 confirmó traer su CATALOGO
	// HISTORICO COMPLETO por /estrenos/page/N (19224 peliculas en 803 paginas la primera vez
	// que se probo) - solo esta ORDENADO del mas nuevo al mas viejo, no filtrado. Por eso no se
	// usa fetchAllPHD2Pages (trae todo) sino fetchRecentPHD2Items, que pagina y PARA apenas una
	// pagina entera no tiene ya mas nada del anio en curso - asi solo entran estrenos del anio
	// (igual al filtro "/year/<anio>" que ya usa FlixLatam) y de paso es mucho mas rapido.
	currentYear := time.Now().UTC().Year()
	movies := fetchRecentPHD2Items("/peliculas/estrenos/page/", false, currentYear, 40)
	series := fetchRecentPHD2Items("/series/estrenos/page/", true, currentYear, 40)

	// findMovie/findSeries recorren TODO el catalogo (recalculando normalizeTitle de cada
	// entrada) por cada llamada - bien para FlixLatam (unos pocos items nuevos por corrida),
	// pero poseidonhd2 puede traer miles de "estrenos" y multiplicado por un catalogo de
	// decenas de miles esto se vuelve lento de verdad. Este indice rapido (titulo+anio) saca
	// la inmensa mayoria de los "ya lo tenemos" en O(1); solo los que NO aparecen ahi (los
	// genuinamente nuevos, un grupo chico) pasan por el chequeo lento y exacto de siempre.
	movieIndex := buildTitleYearIndex(catalog.movies)
	seriesIndex := buildTitleYearIndex(catalog.series)

	var newMovies, newSeries []CvtPost
	for _, m := range movies {
		if m.TMDbID == 0 || m.PHD2Slug == "" {
			continue
		}
		if movieIndex[normalizeTitle(m.Title)+"|"+yearOf(m.ReleaseDate)] {
			continue
		}
		if catalog.findMovie(flixLatestItem{Title: m.Title, ReleaseDate: m.ReleaseDate}) == nil {
			newMovies = append(newMovies, m)
		}
	}
	for _, s := range series {
		if s.TMDbID == 0 || s.PHD2Slug == "" {
			continue
		}
		if seriesIndex[normalizeTitle(s.Title)+"|"+yearOf(s.ReleaseDate)] {
			continue
		}
		if catalog.findSeries("", s.Title, s.ReleaseDate) == nil {
			newSeries = append(newSeries, s)
		}
	}
	log.Printf("[json-update] poseidon2: detectadas peliculas=%d series=%d", len(newMovies), len(newSeries))

	if cfg.DryRun {
		log.Printf("[json-update] poseidon2 dry-run finalizado; no se modificaron archivos")
		return 0
	}

	successful := 0
	var failures []error
	for _, m := range limitedCvtPosts(newMovies, cfg.MaxItems) {
		if err := addPHD2Movie(cfg, m.TMDbID, m.PHD2Slug); err != nil {
			failures = append(failures, fmt.Errorf("pelicula poseidon2 %s: %w", m.PHD2Slug, err))
			continue
		}
		successful++
	}
	for _, s := range limitedCvtPosts(newSeries, cfg.MaxItems) {
		if err := addPHD2Series(cfg, s.TMDbID, s.PHD2Slug); err != nil {
			failures = append(failures, fmt.Errorf("serie poseidon2 %s: %w", s.PHD2Slug, err))
			continue
		}
		successful++
	}

	for _, failure := range failures {
		log.Printf("[json-update] poseidon2 pendiente: %v", failure)
	}
	if len(failures) > 0 && successful == 0 && !onlyUnavailableFailures(failures) {
		return 1
	}
	log.Printf("[json-update] poseidon2: contenido nuevo procesado exitosos=%d pendientes=%d", successful, len(failures))
	return 0
}

// fetchRecentPHD2Items pagina un listado "/estrenos/page/N" de poseidonhd2 (confirmado:
// ordenado del estreno mas nuevo al mas viejo) y se DETIENE apenas una pagina entera no tiene
// ningun item con anio de estreno >= minYear - nunca baja el catalogo historico completo.
// maxPages es un limite de seguridad (por si el orden real no fuera el esperado).
func fetchRecentPHD2Items(listingPath string, isSeries bool, minYear int, maxPages int) []CvtPost {
	var out []CvtPost
	for page := 1; page <= maxPages; page++ {
		data, err := scrapeNextData(phd2Base + listingPath + strconv.Itoa(page))
		if err != nil {
			break
		}
		var nd Phd2NextData
		if json.Unmarshal(data, &nd) != nil {
			break
		}
		if len(nd.Props.PageProps.Movies) == 0 {
			break
		}
		anyRecent := false
		for _, item := range nd.Props.PageProps.Movies {
			post := phd2ItemToPost(item, isSeries)
			year := yearOf(post.ReleaseDate)
			if year == "" {
				continue
			}
			y, convErr := strconv.Atoi(year)
			if convErr != nil || y < minYear {
				continue
			}
			out = append(out, post)
			anyRecent = true
		}
		if !anyRecent {
			break
		}
	}
	return out
}

func buildTitleYearIndex(entries []repoCatalogEntry) map[string]bool {
	idx := make(map[string]bool, len(entries))
	for _, e := range entries {
		title := normalizeTitle(e.Titulo)
		year := yearOf(e.ReleaseDate)
		if title == "" || year == "" {
			continue
		}
		idx[title+"|"+year] = true
	}
	return idx
}

// poseidonhd2 es un catalogo grande y distinto al de FlixLatam: su listado de "estrenos" puede
// traer miles de titulos que nuestro catalogo no tiene (no es un goteo diario chico como
// FlixLatam). Sin un tope quieto, una sola corrida intentaria agregar todo de una - demasiado
// lento y arriesgado. defaultPoseidon2PerRun limita cuantos entran por corrida cuando no se
// paso --max-items explicito (0 = sin tope, que es el default de --max-items); el resto queda
// pendiente para las corridas siguientes, al ritmo de un puñado por dia, igual que FlixLatam.
const defaultPoseidon2PerRun = 25

func limitedCvtPosts(items []CvtPost, max int) []CvtPost {
	if max <= 0 {
		max = defaultPoseidon2PerRun
	}
	if len(items) <= max {
		return items
	}
	return items[:max]
}

func addPHD2Movie(cfg jsonUpdateConfig, tmdbID int, slug string) error {
	servers := fetchPHD2Servers(tmdbID, slug, false)
	if len(servers) == 0 {
		return errNoPlayableServers
	}
	meta, err := fetchPHD2FullMeta(tmdbID, slug, false)
	if err != nil || meta == nil {
		return fmt.Errorf("sin metadatos de poseidon2: %w", err)
	}
	id := slugToID("phd2:m:" + strconv.Itoa(tmdbID))
	path := filepath.Join(cfg.Root, "movies", strconv.Itoa(id)+".json")
	var previous repoMovieDetail
	_ = readJSONFile(path, &previous)
	detail := repoMovieDetail{
		repoCatalogEntry: repoCatalogEntry{
			ID: id, Titulo: meta.Titles.Name,
			Poster: meta.Images.Poster, ReleaseDate: releaseDate(meta.ReleaseDate),
			Genres: phd2MetaGenreSlugs(meta), Rating: phd2RatingString(meta.Rate.Average),
			Backdrop: meta.Images.Backdrop, Overview: meta.Overview,
			Slug: slug, AddedAt: cfg.AddedAt, Phd2TmdbId: tmdbID,
		},
		Servidores: mergeJSONMovieServers(previous.Servidores, servers),
	}
	mergeRepoMovieFields(&detail, previous)
	if err := writeJSONAtomic(path, detail); err != nil {
		return err
	}
	log.Printf("[json-update] pelicula poseidon2 escrita: %s servidores=%d", path, len(detail.Servidores))
	return nil
}

func addPHD2Series(cfg jsonUpdateConfig, tmdbID int, slug string) error {
	seasons, err := fetchPHD2SerieSeasons(tmdbID, slug)
	if err != nil {
		return err
	}
	id := slugToID("phd2:s:" + strconv.Itoa(tmdbID))
	playable := 0
	for _, season := range seasons {
		var episodes []EpisodeDetail
		for _, ep := range season.Episodes {
			servers := fetchPHD2EpisodeServers(tmdbID, slug, season.Number, ep.Number)
			if len(servers) == 0 {
				continue
			}
			episodes = append(episodes, EpisodeDetail{
				ID:         id*100000 + season.Number*1000 + ep.Number,
				Number:     ep.Number,
				Titulo:     fmt.Sprintf("Episodio %d", ep.Number),
				Servidores: servers,
			})
		}
		if len(episodes) == 0 {
			continue
		}
		seasonPath := filepath.Join(cfg.Root, "series", strconv.Itoa(id), fmt.Sprintf("t%d.json", season.Number))
		var previous SeasonDetail
		_ = readJSONFile(seasonPath, &previous)
		fresh := SeasonDetail{SerieID: id, Number: season.Number, Episodios: episodes}
		merged := mergeJSONSeason(previous, fresh)
		if err := writeJSONAtomic(seasonPath, merged); err != nil {
			return err
		}
		playable += len(merged.Episodios)
	}
	if playable == 0 {
		return errNoPlayableServers
	}
	meta, err := fetchPHD2FullMeta(tmdbID, slug, true)
	if err != nil || meta == nil {
		return fmt.Errorf("sin metadatos de poseidon2: %w", err)
	}
	path := filepath.Join(cfg.Root, "series", strconv.Itoa(id)+".json")
	var previous repoCatalogEntry
	_ = readJSONFile(path, &previous)
	detail := repoCatalogEntry{
		ID: id, Titulo: meta.Titles.Name,
		Poster: meta.Images.Poster, ReleaseDate: releaseDate(meta.ReleaseDate),
		Genres: phd2MetaGenreSlugs(meta), Rating: phd2RatingString(meta.Rate.Average),
		Backdrop: meta.Images.Backdrop, Overview: meta.Overview,
		Slug: slug, AddedAt: cfg.AddedAt, Phd2TmdbId: tmdbID,
	}
	mergeRepoEntryFields(&detail, previous)
	if err := writeJSONAtomic(path, detail); err != nil {
		return err
	}
	log.Printf("[json-update] serie poseidon2 escrita: %s episodios=%d", path, playable)
	return nil
}

func phd2MetaGenreSlugs(meta *phd2FullMeta) []string {
	out := make([]string, 0, len(meta.Genres))
	for _, g := range meta.Genres {
		if g.Slug != "" {
			out = append(out, g.Slug)
		}
	}
	return out
}

func phd2RatingString(avg float64) string {
	return strconv.Itoa(int(math.Round(avg)))
}
