package main

// Herramienta manual para agregar o reparar UN titulo puntual desde poseidonhd2.co, sin
// necesitar ningun servidor propio con base de datos: reutiliza las mismas funciones de
// lectura de poseidonhd2 (fetchPHD2Servers, fetchPHD2SerieSeasons, fetchPHD2EpisodeServers)
// que ya estaban escritas en este archivo, y las mismas funciones de mezcla/escritura que ya
// usa el actualizador diario de FlixLatam (mergeJSONMovieServers, mergeJSONEpisode,
// writeJSONAtomic, etc.) - asi el archivo final queda IGUAL de bien formado que si lo hubiera
// escrito el actualizador de siempre.
//
// Uso:
//   stremcore-monitor poseidon-refresh --root .. --url https://www.poseidonhd2.co/serie/312017/a-knight-in-the-making [--id 312017] [--dry-run]
//
// Si no se pasa --id, se usa el TMDbId de la URL como id nuevo en el catalogo (alta de un
// titulo que todavia no existe). Si se pasa --id (el id que ya tiene un titulo roto en el
// catalogo), se AGREGAN los servidores de poseidonhd2 a los que ya tenia ese titulo - no se
// borra nada de lo que ya estaba.

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
)

var phd2URLPattern = regexp.MustCompile(`poseidonhd2\.co/(pelicula|serie)/(\d+)/([a-zA-Z0-9-]+)`)

// Poseidon expone mas datos de los que fetchPHD2Servers/fetchPHD2SerieSeasons ya usaban
// (esas dos solo leian "videos"/"seasons"). Para poder dar de alta un titulo NUEVO hace falta
// tambien titulo/poster/sinopsis/generos, que si vienen en la misma pagina.
type phd2FullNextData struct {
	Props struct {
		PageProps struct {
			ThisMovie *phd2FullMeta `json:"thisMovie"`
			ThisSerie *phd2FullMeta `json:"thisSerie"`
		} `json:"pageProps"`
	} `json:"props"`
}

type phd2FullMeta struct {
	Titles struct {
		Name string `json:"name"`
	} `json:"titles"`
	Images struct {
		Poster   string `json:"poster"`
		Backdrop string `json:"backdrop"`
	} `json:"images"`
	Overview    string `json:"overview"`
	ReleaseDate string `json:"releaseDate"`
	Rate        struct {
		Average float64 `json:"average"`
	} `json:"rate"`
	Genres []struct {
		Name string `json:"name"`
		Slug string `json:"slug"`
	} `json:"genres"`
}

func fetchPHD2FullMeta(tmdbID int, slug string, isSeries bool) (*phd2FullMeta, error) {
	path := "/pelicula/" + strconv.Itoa(tmdbID) + "/" + slug
	if isSeries {
		path = "/serie/" + strconv.Itoa(tmdbID) + "/" + slug
	}
	data, err := scrapeNextData(phd2Base + path)
	if err != nil {
		return nil, err
	}
	var nd phd2FullNextData
	if err := json.Unmarshal(data, &nd); err != nil {
		return nil, err
	}
	if isSeries {
		return nd.Props.PageProps.ThisSerie, nil
	}
	return nd.Props.PageProps.ThisMovie, nil
}

func phd2GenreSlugs(meta *phd2FullMeta) []string {
	if meta == nil {
		return nil
	}
	out := make([]string, 0, len(meta.Genres))
	for _, g := range meta.Genres {
		if g.Slug != "" {
			out = append(out, g.Slug)
		}
	}
	return out
}

func runPoseidonRefresh(args []string) int {
	fs := flag.NewFlagSet("poseidon-refresh", flag.ExitOnError)
	root := fs.String("root", ".", "raiz del repositorio (carpeta que contiene movies/ y series/)")
	pageURL := fs.String("url", "", "URL de la ficha en poseidonhd2.co (pelicula o serie)")
	idFlag := fs.Int("id", 0, "id del catalogo a reparar; si se omite, se usa el TMDbId de la URL como titulo nuevo")
	dryRun := fs.Bool("dry-run", false, "no escribir nada, solo mostrar lo que se haria")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	if *pageURL == "" {
		log.Println("falta --url (la pagina de poseidonhd2.co de la pelicula o serie)")
		return 1
	}
	m := phd2URLPattern.FindStringSubmatch(*pageURL)
	if m == nil {
		log.Println("la url no tiene el formato esperado: https://www.poseidonhd2.co/pelicula/ID/slug o .../serie/ID/slug")
		return 1
	}
	isSeries := m[1] == "serie"
	tmdbID, _ := strconv.Atoi(m[2])
	slug := m[3]

	catalogID := tmdbID
	repairing := false
	if *idFlag > 0 {
		catalogID = *idFlag
		repairing = true
	}

	kindLabel := "pelicula"
	if isSeries {
		kindLabel = "serie"
	}
	action := "alta nueva"
	if repairing {
		action = fmt.Sprintf("reparacion del id existente %d", catalogID)
	}
	log.Printf("poseidon-refresh: %s (tmdb=%d slug=%s) -> %s%s", kindLabel, tmdbID, slug, action, dryRunSuffix(*dryRun))

	if isSeries {
		return runPoseidonRefreshSeries(*root, tmdbID, slug, catalogID, *dryRun)
	}
	return runPoseidonRefreshMovie(*root, tmdbID, slug, catalogID, *dryRun)
}

func dryRunSuffix(dryRun bool) string {
	if dryRun {
		return " (simulacion, no se escribe nada)"
	}
	return ""
}

func runPoseidonRefreshMovie(root string, tmdbID int, slug string, catalogID int, dryRun bool) int {
	servers := fetchPHD2Servers(tmdbID, slug, false)
	if len(servers) == 0 {
		log.Println("poseidonhd2 no devolvio ningun servidor jugable para esta pelicula (revisa la url)")
		return 1
	}

	path := filepath.Join(root, "movies", strconv.Itoa(catalogID)+".json")
	var previous repoMovieDetail
	hadPrevious := readJSONFile(path, &previous) == nil

	detail := repoMovieDetail{}
	if hadPrevious {
		detail = previous
	}
	detail.ID = catalogID
	before := len(previous.Servidores)
	detail.Servidores = mergeJSONMovieServers(previous.Servidores, servers)

	if !hadPrevious {
		meta, err := fetchPHD2FullMeta(tmdbID, slug, false)
		if err != nil || meta == nil {
			log.Printf("no se pudieron traer titulo/poster/sinopsis de poseidonhd2: %v", err)
			return 1
		}
		detail.Titulo = meta.Titles.Name
		detail.Poster = meta.Images.Poster
		detail.Backdrop = meta.Images.Backdrop
		detail.Overview = meta.Overview
		detail.ReleaseDate = releaseDate(meta.ReleaseDate)
		detail.Rating = fmt.Sprintf("%.1f", meta.Rate.Average)
		detail.Genres = phd2GenreSlugs(meta)
		detail.Slug = slug
	}
	mergeRepoMovieFields(&detail, previous)

	log.Printf("pelicula %d (%s): %d servidores antes -> %d despues de sumar poseidonhd2", catalogID, detail.Titulo, before, len(detail.Servidores))

	if dryRun {
		printDryRun(path, detail)
		return 0
	}
	if err := writeJSONAtomic(path, detail); err != nil {
		log.Printf("error escribiendo %s: %v", path, err)
		return 1
	}
	log.Printf("escrito: %s", path)
	return 0
}

func runPoseidonRefreshSeries(root string, tmdbID int, slug string, catalogID int, dryRun bool) int {
	seasons, err := fetchPHD2SerieSeasons(tmdbID, slug)
	if err != nil || len(seasons) == 0 {
		log.Printf("poseidonhd2 no devolvio temporadas para esta serie (revisa la url): %v", err)
		return 1
	}

	entryPath := filepath.Join(root, "series", strconv.Itoa(catalogID)+".json")
	var previousEntry repoCatalogEntry
	hadPreviousEntry := readJSONFile(entryPath, &previousEntry) == nil

	entry := repoCatalogEntry{}
	if hadPreviousEntry {
		entry = previousEntry
	}
	entry.ID = catalogID
	if !hadPreviousEntry {
		meta, err := fetchPHD2FullMeta(tmdbID, slug, true)
		if err != nil || meta == nil {
			log.Printf("no se pudieron traer titulo/poster/sinopsis de poseidonhd2: %v", err)
			return 1
		}
		entry.Titulo = meta.Titles.Name
		entry.Poster = meta.Images.Poster
		entry.Backdrop = meta.Images.Backdrop
		entry.Overview = meta.Overview
		entry.ReleaseDate = releaseDate(meta.ReleaseDate)
		entry.Rating = fmt.Sprintf("%.1f", meta.Rate.Average)
		entry.Genres = phd2GenreSlugs(meta)
		entry.Slug = slug
	}
	mergeRepoEntryFields(&entry, previousEntry)

	totalBefore, totalAfter := 0, 0
	touchedSeasons := 0
	for _, season := range seasons {
		if season.Number <= 0 || len(season.Episodes) == 0 {
			continue
		}
		seasonPath := filepath.Join(root, "series", strconv.Itoa(catalogID), fmt.Sprintf("t%d.json", season.Number))
		var previousSeason SeasonDetail
		_ = readJSONFile(seasonPath, &previousSeason)
		totalBefore += len(previousSeason.Episodios)

		newSeason := SeasonDetail{SerieID: catalogID, Number: season.Number}
		if previousSeason.SerieID != 0 {
			newSeason = previousSeason
			newSeason.SerieID = catalogID
		}

		anyEpisode := false
		for _, ep := range season.Episodes {
			servers := fetchPHD2EpisodeServers(tmdbID, slug, season.Number, ep.Number)
			if len(servers) == 0 {
				continue
			}
			anyEpisode = true
			title := ep.Title
			if title == "" {
				title = fmt.Sprintf("Episodio %d", ep.Number)
			}
			fresh := EpisodeDetail{
				ID:     catalogID*100000 + season.Number*1000 + ep.Number,
				Number: ep.Number,
				Titulo: title,
				Imagen: ep.Image,
			}
			newSeason.Episodios = mergeJSONEpisode(newSeason.Episodios, fresh)
			for i := range newSeason.Episodios {
				if newSeason.Episodios[i].Number == ep.Number {
					newSeason.Episodios[i].Servidores = mergeJSONEpisodeServers(newSeason.Episodios[i].Servidores, servers)
					if newSeason.Episodios[i].Imagen == "" {
						newSeason.Episodios[i].Imagen = ep.Image
					}
					break
				}
			}
		}
		if !anyEpisode {
			continue
		}
		touchedSeasons++
		totalAfter += len(newSeason.Episodios)

		if dryRun {
			printDryRun(seasonPath, newSeason)
			continue
		}
		if err := writeJSONAtomic(seasonPath, newSeason); err != nil {
			log.Printf("error escribiendo %s: %v", seasonPath, err)
			return 1
		}
		log.Printf("escrito: %s (%d episodios)", seasonPath, len(newSeason.Episodios))
	}

	if touchedSeasons == 0 {
		log.Println("poseidonhd2 no tenia ningun episodio jugable para esta serie ahora mismo - no se escribio nada")
		return 1
	}

	log.Printf("serie %d (%s): %d episodios con datos antes -> %d despues, en %d temporada(s)", catalogID, entry.Titulo, totalBefore, totalAfter, touchedSeasons)

	if dryRun {
		printDryRun(entryPath, entry)
		return 0
	}
	if err := writeJSONAtomic(entryPath, entry); err != nil {
		log.Printf("error escribiendo %s: %v", entryPath, err)
		return 1
	}
	log.Printf("escrito: %s", entryPath)
	return 0
}

func printDryRun(path string, value any) {
	b, _ := json.MarshalIndent(value, "", "  ")
	fmt.Fprintf(os.Stderr, "\n--- (simulacion) se escribiria en %s ---\n%s\n", path, string(b))
}
