package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
)

// flixlatam-refresh reutiliza el scraper de flixlatam.com que ya corre todos los dias
// (fetchFlixLatamMovieServers / fetchFlixLatamEpisodeServers / fetchFlixLatamSeasons) para
// volver a pedir los servidores de UN titulo puntual que ya esta en el catalogo (identificado
// por su id), sin esperar al cron diario. Por defecto SUMA los servidores nuevos a los que ya
// tenia (no borra nada); con --replace se dejan solo los de flixlatam para ese titulo/episodio.
func runFlixlatamRefresh(args []string) int {
	fs := flag.NewFlagSet("flixlatam-refresh", flag.ExitOnError)
	root := fs.String("root", ".", "raiz del repositorio (con movies/ y series/)")
	idFlag := fs.Int("id", 0, "id del catalogo (pelicula o serie) a reparar")
	seasonFlag := fs.Int("season", 0, "temporada (series; obligatorio si el id es una serie)")
	episodeFlag := fs.Int("episode", 0, "episodio puntual dentro de la temporada (0 = toda la temporada)")
	dryRun := fs.Bool("dry-run", false, "no escribe nada, solo muestra que encontraria")
	replace := fs.Bool("replace", false, "borrar los servidores viejos de este titulo/episodio y dejar SOLO los de flixlatam")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *idFlag == 0 {
		fmt.Fprintln(os.Stderr, "falta --id")
		return 1
	}

	moviePath := filepath.Join(*root, "movies", strconv.Itoa(*idFlag)+".json")
	seriesCatalogPath := filepath.Join(*root, "series", strconv.Itoa(*idFlag)+".json")
	if fileExistsFlix(moviePath) {
		return runFlixlatamRefreshMovie(*root, *idFlag, *dryRun, *replace)
	}
	if fileExistsFlix(seriesCatalogPath) {
		if *seasonFlag == 0 {
			fmt.Fprintln(os.Stderr, "ese id es una serie: falta --season")
			return 1
		}
		return runFlixlatamRefreshSeries(*root, *idFlag, *seasonFlag, *episodeFlag, *dryRun, *replace)
	}
	fmt.Fprintln(os.Stderr, "no se encontro ese id en movies/ ni series/ dentro de --root")
	return 1
}

func fileExistsFlix(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func replaceOrSumaWord(replace bool) string {
	if replace {
		return "reemplazar"
	}
	return "sumar"
}

func runFlixlatamRefreshMovie(root string, id int, dryRun, replace bool) int {
	path := filepath.Join(root, "movies", strconv.Itoa(id)+".json")
	var previous repoMovieDetail
	if err := readJSONFile(path, &previous); err != nil {
		fmt.Fprintln(os.Stderr, "no se pudo leer el catalogo existente:", err)
		return 1
	}
	if previous.Slug == "" {
		fmt.Fprintln(os.Stderr, "el catalogo no tiene guardado el slug de flixlatam para este id")
		return 1
	}
	fmt.Println("Buscando en flixlatam.com/pelicula/" + previous.Slug)
	servers := fetchFlixLatamMovieServers(previous.Slug)
	if len(servers) == 0 {
		fmt.Fprintln(os.Stderr, "flixlatam no devolvio ningun servidor para ese slug")
		return 1
	}
	var merged []Server
	if replace {
		merged = mergeJSONMovieServers(nil, servers)
	} else {
		merged = mergeJSONMovieServers(previous.Servidores, servers)
	}
	fmt.Printf("Encontrados %d servidor(es) en flixlatam. Total tras %s: %d\n", len(servers), replaceOrSumaWord(replace), len(merged))
	if dryRun {
		fmt.Println("(dry-run, no se escribio nada)")
		return 0
	}
	previous.Servidores = merged
	if err := writeJSONAtomic(path, previous); err != nil {
		fmt.Fprintln(os.Stderr, "error escribiendo:", err)
		return 1
	}
	fmt.Println("Listo:", path)
	return 0
}

func runFlixlatamRefreshSeries(root string, id, season, episode int, dryRun, replace bool) int {
	catalogPath := filepath.Join(root, "series", strconv.Itoa(id)+".json")
	var entry repoCatalogEntry
	if err := readJSONFile(catalogPath, &entry); err != nil {
		fmt.Fprintln(os.Stderr, "no se pudo leer el catalogo existente:", err)
		return 1
	}
	if entry.Slug == "" {
		fmt.Fprintln(os.Stderr, "el catalogo no tiene guardado el slug de flixlatam para este id")
		return 1
	}
	seasonPath := filepath.Join(root, "series", strconv.Itoa(id), fmt.Sprintf("t%d.json", season))
	var previousSeason SeasonDetail
	_ = readJSONFile(seasonPath, &previousSeason)
	if previousSeason.SerieID == 0 {
		previousSeason.SerieID = id
		previousSeason.Number = season
	}

	if episode > 0 {
		return refreshFlixlatamSingleEpisode(entry.Slug, seasonPath, &previousSeason, id, season, episode, dryRun, replace)
	}
	return refreshFlixlatamWholeSeason(entry.Slug, seasonPath, &previousSeason, id, season, dryRun, replace)
}

func refreshFlixlatamSingleEpisode(slug, seasonPath string, previousSeason *SeasonDetail, id, season, episode int, dryRun, replace bool) int {
	fmt.Printf("Buscando en flixlatam.com/serie/%s temporada %d capitulo %d\n", slug, season, episode)
	servers := fetchFlixLatamEpisodeServers(slug, season, episode)
	if len(servers) == 0 {
		fmt.Fprintln(os.Stderr, "flixlatam no devolvio ningun servidor para ese episodio")
		return 1
	}
	var existingServers []EpisodeServer
	existingIdx := -1
	for i := range previousSeason.Episodios {
		if previousSeason.Episodios[i].Number == episode {
			existingIdx = i
			existingServers = previousSeason.Episodios[i].Servidores
			break
		}
	}
	var mergedServers []EpisodeServer
	if replace {
		mergedServers = mergeJSONEpisodeServers(nil, servers)
	} else {
		mergedServers = mergeJSONEpisodeServers(existingServers, servers)
	}
	fmt.Printf("Encontrados %d servidor(es) en flixlatam. Total tras %s: %d\n", len(servers), replaceOrSumaWord(replace), len(mergedServers))
	if dryRun {
		fmt.Println("(dry-run, no se escribio nada)")
		return 0
	}
	if existingIdx >= 0 {
		previousSeason.Episodios[existingIdx].Servidores = mergedServers
	} else {
		previousSeason.Episodios = append(previousSeason.Episodios, EpisodeDetail{
			ID:         id*100000 + season*1000 + episode,
			Number:     episode,
			Titulo:     fmt.Sprintf("Episodio %d", episode),
			Servidores: mergedServers,
		})
		sort.Slice(previousSeason.Episodios, func(i, j int) bool {
			return previousSeason.Episodios[i].Number < previousSeason.Episodios[j].Number
		})
	}
	if err := writeJSONAtomic(seasonPath, previousSeason); err != nil {
		fmt.Fprintln(os.Stderr, "error escribiendo:", err)
		return 1
	}
	fmt.Println("Listo:", seasonPath)
	return 0
}

func refreshFlixlatamWholeSeason(slug, seasonPath string, previousSeason *SeasonDetail, id, season int, dryRun, replace bool) int {
	fmt.Printf("Buscando temporada %d completa en flixlatam.com/serie/%s\n", season, slug)
	seasons, err := fetchFlixLatamSeasons(slug)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error consultando temporadas en flixlatam:", err)
		return 1
	}
	var matched *flixlatamSeasonInfo
	for i := range seasons {
		if seasons[i].Number == season {
			matched = &seasons[i]
			break
		}
	}
	if matched == nil {
		fmt.Fprintln(os.Stderr, "flixlatam no tiene esa temporada para esta serie")
		return 1
	}
	fresh := fetchJSONSeason(id, slug, *matched, 4)
	if len(fresh.Episodios) == 0 {
		fmt.Fprintln(os.Stderr, "flixlatam no devolvio ningun episodio con servidores para esa temporada")
		return 1
	}
	for _, freshEp := range fresh.Episodios {
		if replace {
			for i := range previousSeason.Episodios {
				if previousSeason.Episodios[i].Number == freshEp.Number {
					previousSeason.Episodios[i].Servidores = nil
				}
			}
		}
		previousSeason.Episodios = mergeJSONEpisode(previousSeason.Episodios, freshEp)
	}
	fmt.Printf("Episodios con servidores encontrados en flixlatam: %d. Total en la temporada tras %s: %d\n", len(fresh.Episodios), replaceOrSumaWord(replace), len(previousSeason.Episodios))
	if dryRun {
		fmt.Println("(dry-run, no se escribio nada)")
		return 0
	}
	if err := writeJSONAtomic(seasonPath, previousSeason); err != nil {
		fmt.Fprintln(os.Stderr, "error escribiendo:", err)
		return 1
	}
	fmt.Println("Listo:", seasonPath)
	return 0
}
