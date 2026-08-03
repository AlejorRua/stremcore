package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	htmlpkg "html"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	providerCineCalidad = "cinecalidad"
	providerCuevana     = "cuevana"
	providerDoramasflix = "doramasflix"
	providerSoloLatino  = "sololatino"
	providerAnimeFLV    = "animeflv"

	cineCalidadBase = "https://www.cinecalidad.ec"
	cuevanaBase     = "https://cuevana.gs"
	doramasflixBase = "https://doramasflix.in"
	doramasflixAPI  = "https://sv1.fluxcedene.net/api/gql"
	soloLatinoBase  = "https://sololatino.net"
	animeFLVBase    = "https://www3.animeflv.net"

	externalProviderMaxPages = 120
)

type externalProviderSpec struct {
	ID          string
	Name        string
	FetchMovies func(page int) []CvtPost
	FetchSeries func(page int) []CvtPost
}

var externalProviderSpecs = []externalProviderSpec{
	{ID: providerCineCalidad, Name: "CineCalidad", FetchMovies: fetchCineCalidadMoviesPage, FetchSeries: fetchCineCalidadSeriesPage},
	{ID: providerCuevana, Name: "Cuevana", FetchMovies: fetchCuevanaMoviesPage, FetchSeries: fetchCuevanaSeriesPage},
	{ID: providerDoramasflix, Name: "Doramasflix", FetchMovies: fetchDoramasflixMoviesPage, FetchSeries: fetchDoramasflixSeriesPage},
	{ID: providerSoloLatino, Name: "SoloLatino", FetchMovies: fetchSoloLatinoMoviesPage, FetchSeries: fetchSoloLatinoSeriesPage},
	{ID: providerAnimeFLV, Name: "AnimeFLV", FetchMovies: fetchAnimeFLVMoviesPage, FetchSeries: fetchAnimeFLVSeriesPage},
}

func cloneProviderLinks(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		if v != "" {
			out[k] = v
		}
	}
	return out
}

func addProviderLink(post *CvtPost, provider, link string) {
	link = strings.TrimSpace(link)
	if provider == "" || link == "" {
		return
	}
	if post.ProviderLinks == nil {
		post.ProviderLinks = map[string]string{}
	}
	post.ProviderLinks[provider] = link
}

func mergeProviderLinks(base, extra map[string]string) map[string]string {
	out := cloneProviderLinks(base)
	if len(extra) == 0 {
		return out
	}
	if out == nil {
		out = map[string]string{}
	}
	for k, v := range extra {
		if _, exists := out[k]; !exists && strings.TrimSpace(v) != "" {
			out[k] = v
		}
	}
	return out
}

func hasProviderLinks(post CvtPost) bool {
	return len(post.ProviderLinks) > 0
}

func providerLink(post CvtPost, provider string) string {
	if post.ProviderLinks == nil {
		return ""
	}
	return post.ProviderLinks[provider]
}

func firstProviderURL(post CvtPost) string {
	if len(post.ProviderLinks) == 0 {
		return ""
	}
	keys := make([]string, 0, len(post.ProviderLinks))
	for k := range post.ProviderLinks {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if post.ProviderLinks[k] != "" {
			return post.ProviderLinks[k]
		}
	}
	return ""
}

func fullPosterURL(path string) string {
	return fullImageURL(path)
}

func externalPost(provider, link, title, poster, backdrop, overview, rating, runtime, releaseDate string, genres []int, isSeries bool) CvtPost {
	title = cleanText(title)
	if title == "" {
		title = titleFromSlug(link)
	}
	link = strings.TrimSpace(link)
	if releaseDate == "" {
		if year := firstYear(link + " " + title); year != "" {
			releaseDate = year + "-01-01"
		}
	}
	p := CvtPost{
		ID:           slugToID(provider + ":" + link),
		Title:        title,
		Overview:     cleanText(overview),
		Images:       CvtImages{Poster: poster, Backdrop: backdrop},
		Rating:       rating,
		Genres:       genres,
		Type:         "movies",
		ReleaseDate:  releaseDate,
		Runtime:      runtime,
		ProviderOnly: true,
	}
	if isSeries {
		p.Type = "tvshows"
	}
	addProviderLink(&p, provider, link)
	return p
}

func fetchAllExternalMoviesPages() []CvtPost {
	return fetchAllExternalPages(false)
}

func fetchAllExternalSeriesPages() []CvtPost {
	return fetchAllExternalPages(true)
}

func fetchAllExternalPages(isSeries bool) []CvtPost {
	var all []CvtPost
	for _, spec := range externalProviderSpecs {
		fetch := spec.FetchMovies
		kind := "películas"
		if isSeries {
			fetch = spec.FetchSeries
			kind = "series"
		}
		if fetch == nil {
			continue
		}
		items := fetchExternalProviderPages(spec.ID, spec.Name, kind, fetch)
		all = append(all, items...)
	}
	return dedupeMergedPosts(all)
}

func fetchExternalProviderPages(providerID, name, kind string, fetch func(page int) []CvtPost) []CvtPost {
	var out []CvtPost
	emptyStreak := 0
	for page := 1; page <= externalProviderMaxPages; page++ {
		items := fetch(page)
		if len(items) == 0 {
			emptyStreak++
			if page == 1 || emptyStreak >= 3 {
				break
			}
			continue
		}
		emptyStreak = 0
		out = append(out, items...)
		time.Sleep(buildDelay)
	}
	log.Printf("[%s] %d %s descargadas", providerID, len(out), kind)
	return out
}

func fetchExternalMovieServers(post CvtPost) []Server {
	if len(post.ProviderLinks) == 0 {
		return nil
	}
	type result struct {
		servers []Server
	}
	keys := make([]string, 0, len(post.ProviderLinks))
	for k := range post.ProviderLinks {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	results := make([]result, len(keys))
	var wg sync.WaitGroup
	for i, provider := range keys {
		link := post.ProviderLinks[provider]
		wg.Add(1)
		go func(i int, provider, link string) {
			defer wg.Done()
			switch provider {
			case providerCineCalidad:
				results[i].servers = fetchCineCalidadMovieServers(link)
			case providerCuevana:
				results[i].servers = fetchCuevanaMovieServers(link)
			case providerDoramasflix:
				results[i].servers = fetchDoramasflixMovieServers(link)
			case providerSoloLatino:
				results[i].servers = fetchSoloLatinoMovieServers(link)
			case providerAnimeFLV:
				results[i].servers = fetchAnimeFLVMovieServers(link)
			}
		}(i, provider, link)
	}
	wg.Wait()

	var out []Server
	for _, res := range results {
		for _, s := range res.servers {
			s.ID = len(out) + 1
			out = append(out, s)
		}
	}
	return sanitizeMovieServers(out)
}

func fetchExternalSeasonEpisodes(post CvtPost, seasonNum int) []EpisodeDetail {
	if len(post.ProviderLinks) == 0 {
		return nil
	}
	type result struct {
		episodes []EpisodeDetail
	}
	keys := make([]string, 0, len(post.ProviderLinks))
	for k := range post.ProviderLinks {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	results := make([]result, len(keys))
	var wg sync.WaitGroup
	for i, provider := range keys {
		link := post.ProviderLinks[provider]
		wg.Add(1)
		go func(i int, provider, link string) {
			defer wg.Done()
			switch provider {
			case providerCineCalidad:
				results[i].episodes = fetchCineCalidadSeasonEpisodes(link, post.ID, seasonNum)
			case providerCuevana:
				results[i].episodes = fetchCuevanaSeasonEpisodes(link, post.ID, seasonNum)
			case providerDoramasflix:
				results[i].episodes = fetchDoramasflixSeasonEpisodes(link, post.ID, seasonNum)
			case providerSoloLatino:
				results[i].episodes = fetchSoloLatinoSeasonEpisodes(link, post.ID, seasonNum)
			case providerAnimeFLV:
				results[i].episodes = fetchAnimeFLVSeasonEpisodes(link, post.ID, seasonNum)
			}
		}(i, provider, link)
	}
	wg.Wait()

	byNumber := map[int]EpisodeDetail{}
	for _, res := range results {
		for _, ep := range res.episodes {
			if ep.Number <= 0 {
				continue
			}
			current := byNumber[ep.Number]
			if current.ID == 0 {
				current = ep
			} else {
				if current.Titulo == "" {
					current.Titulo = ep.Titulo
				}
				if current.Imagen == "" {
					current.Imagen = ep.Imagen
				}
			}
			current.Servidores = append(current.Servidores, ep.Servidores...)
			byNumber[ep.Number] = current
		}
	}
	nums := make([]int, 0, len(byNumber))
	for n := range byNumber {
		nums = append(nums, n)
	}
	sort.Ints(nums)
	out := make([]EpisodeDetail, 0, len(nums))
	for _, n := range nums {
		ep := byNumber[n]
		ep.Servidores = sanitizeEpisodeServers(ep.Servidores)
		out = append(out, ep)
	}
	return out
}

func appendExternalSeasonServers(post CvtPost, season SeasonDetail) SeasonDetail {
	external := fetchExternalSeasonEpisodes(post, season.Number)
	if len(external) == 0 {
		return season
	}
	byNumber := map[int]int{}
	for i, ep := range season.Episodios {
		byNumber[ep.Number] = i
	}
	for _, ext := range external {
		if idx, ok := byNumber[ext.Number]; ok {
			if season.Episodios[idx].Titulo == "" {
				season.Episodios[idx].Titulo = ext.Titulo
			}
			if season.Episodios[idx].Imagen == "" {
				season.Episodios[idx].Imagen = ext.Imagen
			}
			season.Episodios[idx].Servidores = append(season.Episodios[idx].Servidores, ext.Servidores...)
			continue
		}
		season.Episodios = append(season.Episodios, ext)
		byNumber[ext.Number] = len(season.Episodios) - 1
	}
	sort.Slice(season.Episodios, func(i, j int) bool {
		return season.Episodios[i].Number < season.Episodios[j].Number
	})
	return season
}

func buildExternalSerieDetail(post CvtPost) (SerieDetail, bool) {
	seasonCounts := map[int]int{}
	for season := 1; season <= 3; season++ {
		eps := fetchExternalSeasonEpisodes(post, season)
		if len(eps) == 0 {
			if season == 1 {
				return SerieDetail{}, false
			}
			break
		}
		seasonCounts[season] = len(eps)
	}
	var nums []int
	for n := range seasonCounts {
		nums = append(nums, n)
	}
	sort.Ints(nums)
	temporadas := make([]SeasonInfo, 0, len(nums))
	for _, n := range nums {
		temporadas = append(temporadas, SeasonInfo{
			ID:             post.ID*1000 + n,
			Number:         n,
			TotalEpisodios: seasonCounts[n],
		})
	}
	if len(temporadas) == 0 {
		return SerieDetail{}, false
	}
	return SerieDetail{
		ID:             post.ID,
		TmdbID:         post.TMDbID,
		Titulo:         post.Title,
		TituloOriginal: post.OriginalTitle,
		PosterURL:      fullPosterURL(post.Images.Poster),
		BannerURL:      fullPosterURL(post.Images.Backdrop),
		Descripcion:    post.Overview,
		Rating:         parseRating(post.Rating),
		ReleaseDate:    releaseDate(post.ReleaseDate),
		Generos:        resolveGenres(post.Genres),
		Temporadas:     temporadas,
	}, true
}

func serverFromExternalURL(id int, lang, name, quality, playerURL string) Server {
	playerURL = normalizeURL("", playerURL)
	if name == "" {
		name = serverName(playerURL)
	}
	embedID := lastURLSegment(playerURL)
	return Server{ID: id, Idioma: lang, Nombre: name, Calidad: quality, PlayerURL: playerURL, URL: playerURL, EmbedID: embedID}
}

func episodeServerFromExternalURL(id int, lang, name, quality, playerURL string) EpisodeServer {
	playerURL = normalizeURL("", playerURL)
	if name == "" {
		name = serverName(playerURL)
	}
	embedID := lastURLSegment(playerURL)
	return EpisodeServer{ID: id, Idioma: lang, Nombre: name, Calidad: quality, PlayerURL: playerURL, URL: playerURL, EmbedID: embedID}
}

func movieServersFromEpisodeServers(servers []EpisodeServer) []Server {
	out := make([]Server, 0, len(servers))
	for _, s := range servers {
		out = append(out, Server{
			ID: s.ID, Idioma: s.Idioma, Nombre: s.Nombre,
			Calidad: s.Calidad, PlayerURL: s.PlayerURL, URL: s.URL, EmbedID: s.EmbedID,
		})
	}
	return sanitizeMovieServers(out)
}

func normalizeURL(base, raw string) string {
	raw = strings.TrimSpace(htmlpkg.UnescapeString(raw))
	if raw == "" {
		return ""
	}
	if strings.HasPrefix(raw, "//") {
		return "https:" + raw
	}
	u, err := url.Parse(raw)
	if err == nil && u.IsAbs() {
		return raw
	}
	if base == "" {
		return raw
	}
	b, err := url.Parse(base)
	if err != nil {
		return raw
	}
	rel, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	return b.ResolveReference(rel).String()
}

func fetchHTML(pageURL string, referer ...string) (string, error) {
	req, err := http.NewRequest("GET", pageURL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", userAgent)
	if len(referer) > 0 && referer[0] != "" {
		req.Header.Set("Referer", referer[0])
	}
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

func postJSON(endpoint string, body []byte, headers map[string]string) ([]byte, error) {
	req, err := http.NewRequest("POST", endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(resp.Body)
}

var (
	tagRE        = regexp.MustCompile(`(?is)<[^>]+>`)
	spaceRE      = regexp.MustCompile(`\s+`)
	yearRE       = regexp.MustCompile(`\b(19|20)\d{2}\b`)
	articleRE    = regexp.MustCompile(`(?is)<article\b[^>]*>.*?</article>`)
	anchorHrefRE = regexp.MustCompile(`(?is)<a\b[^>]*\bhref=["']([^"']+)["'][^>]*>`)
)

func cleanText(s string) string {
	s = tagRE.ReplaceAllString(s, " ")
	s = htmlpkg.UnescapeString(s)
	return strings.TrimSpace(spaceRE.ReplaceAllString(s, " "))
}

func firstYear(s string) string {
	return yearRE.FindString(s)
}

func titleFromSlug(link string) string {
	link = strings.TrimRight(link, "/")
	if u, err := url.Parse(link); err == nil {
		link = u.Path
	}
	part := strings.Trim(strings.TrimPrefix(link[strings.LastIndex(link, "/")+1:], "/"), " ")
	part = strings.ReplaceAll(part, "-", " ")
	part = strings.ReplaceAll(part, "_", " ")
	return cleanText(part)
}

func attrValue(block, attr string) string {
	re := regexp.MustCompile(`(?is)\b` + regexp.QuoteMeta(attr) + `\s*=\s*["']([^"']+)["']`)
	if m := re.FindStringSubmatch(block); len(m) > 1 {
		return htmlpkg.UnescapeString(m[1])
	}
	return ""
}

func attrValueInTag(block, tag, attr string) string {
	re := regexp.MustCompile(`(?is)<` + regexp.QuoteMeta(tag) + `\b[^>]*\b` + regexp.QuoteMeta(attr) + `\s*=\s*["']([^"']+)["'][^>]*>`)
	if m := re.FindStringSubmatch(block); len(m) > 1 {
		return htmlpkg.UnescapeString(m[1])
	}
	return ""
}

func firstMatch(block, expr string) string {
	re := regexp.MustCompile(expr)
	if m := re.FindStringSubmatch(block); len(m) > 1 {
		return htmlpkg.UnescapeString(m[1])
	}
	return ""
}

func lastURLSegment(raw string) string {
	raw = strings.TrimRight(raw, "/")
	if raw == "" {
		return ""
	}
	if u, err := url.Parse(raw); err == nil && u.Path != "" {
		raw = u.Path
	}
	if i := strings.LastIndex(raw, "/"); i >= 0 {
		return raw[i+1:]
	}
	return raw
}

func surroundingBlock(s string, start, before, after int) string {
	if start < 0 {
		return ""
	}
	from := start - before
	if from < 0 {
		from = 0
	}
	to := start + after
	if to > len(s) {
		to = len(s)
	}
	return s[from:to]
}

func parseYearDate(year string) string {
	year = strings.TrimSpace(year)
	if len(year) == 4 {
		if _, err := strconv.Atoi(year); err == nil {
			return year + "-01-01"
		}
	}
	return ""
}

func htmlScriptByID(page, id string) string {
	re := regexp.MustCompile(`(?is)<script\b[^>]*\bid=["']` + regexp.QuoteMeta(id) + `["'][^>]*>(.*?)</script>`)
	if m := re.FindStringSubmatch(page); len(m) > 1 {
		return htmlpkg.UnescapeString(m[1])
	}
	return ""
}

func jsonObjectFromScriptVar(page, varName string) string {
	idx := strings.Index(page, varName)
	if idx < 0 {
		return ""
	}
	rest := page[idx+len(varName):]
	if eq := strings.Index(rest, "="); eq >= 0 {
		rest = rest[eq+1:]
	}
	rest = strings.TrimSpace(rest)
	if !strings.HasPrefix(rest, "{") && !strings.HasPrefix(rest, "[") {
		return ""
	}
	depth := 0
	inString := false
	escape := false
	for i, r := range rest {
		if escape {
			escape = false
			continue
		}
		if r == '\\' {
			escape = true
			continue
		}
		if r == '"' {
			inString = !inString
			continue
		}
		if inString {
			continue
		}
		if r == '{' || r == '[' {
			depth++
		}
		if r == '}' || r == ']' {
			depth--
			if depth == 0 {
				return rest[:i+1]
			}
		}
	}
	return ""
}

func languageName(raw string) string {
	raw = strings.ToUpper(strings.TrimSpace(raw))
	raw = strings.Trim(raw, "[] ")
	switch raw {
	case "LAT", "LATINO":
		return "Latino"
	case "ESP", "CAST", "CASTELLANO":
		return "Castellano"
	case "SUB", "SUBTITULADO":
		return "Subtitulado"
	case "ENG", "INGLES", "INGLÉS":
		return "Inglés"
	case "JAP":
		return "Japonés"
	case "COR":
		return "Coreano"
	default:
		if raw == "" {
			return "Latino"
		}
		return raw
	}
}

// ── CineCalidad ────────────────────────────────────────────────────────────────

func fetchCineCalidadMoviesPage(page int) []CvtPost {
	pageURL := cineCalidadBase
	if page > 1 {
		pageURL = fmt.Sprintf("%s/page/%d", cineCalidadBase, page)
	}
	html, err := fetchHTML(pageURL, cineCalidadBase)
	if err != nil {
		return nil
	}
	return parseCineCalidadListing(html, false)
}

func fetchCineCalidadSeriesPage(page int) []CvtPost {
	pageURL := fmt.Sprintf("%s/ver-serie/page/%d", cineCalidadBase, page)
	html, err := fetchHTML(pageURL, cineCalidadBase)
	if err != nil {
		return nil
	}
	return parseCineCalidadListing(html, true)
}

func parseCineCalidadListing(page string, wantSeries bool) []CvtPost {
	var posts []CvtPost
	seen := map[string]bool{}
	for _, art := range articleRE.FindAllString(page, -1) {
		href := normalizeURL(cineCalidadBase, attrValueInTag(art, "a", "href"))
		if href == "" || seen[href] {
			continue
		}
		isSeries := strings.Contains(href, "/ver-serie/")
		isMovie := strings.Contains(href, "/ver-pelicula/")
		if wantSeries && !isSeries {
			continue
		}
		if !wantSeries && !isMovie {
			continue
		}
		poster := attrValueInTag(art, "img", "data-src")
		if poster == "" {
			poster = attrValueInTag(art, "img", "src")
		}
		poster = normalizeURL(cineCalidadBase, poster)
		title := attrValueInTag(art, "img", "alt")
		if title == "" {
			title = cleanText(firstMatch(art, `(?is)<h[23][^>]*>(.*?)</h[23]>`))
		}
		rd := parseYearDate(firstYear(art))
		posts = append(posts, externalPost(providerCineCalidad, href, title, poster, "", "", "", "", rd, nil, isSeries))
		seen[href] = true
	}
	return posts
}

func fetchCineCalidadMovieServers(link string) []Server {
	return movieServersFromEpisodeServers(fetchCineCalidadPageServers(link))
}

func fetchCineCalidadPageServers(link string) []EpisodeServer {
	link = normalizeURL(cineCalidadBase, link)
	page, err := fetchHTML(link, cineCalidadBase)
	if err != nil {
		return nil
	}
	re := regexp.MustCompile(`(?is)<li\b[^>]*\bdata-option=["']([^"']+)["'][^>]*>(.*?)</li>`)
	var out []EpisodeServer
	for _, m := range re.FindAllStringSubmatch(page, -1) {
		if len(m) < 3 {
			continue
		}
		name := cleanText(m[2])
		if strings.Contains(strings.ToLower(name), "trailer") {
			continue
		}
		playerURL := normalizeURL(cineCalidadBase, m[1])
		out = append(out, episodeServerFromExternalURL(len(out)+1, "Latino", name, "HD", playerURL))
	}
	return sanitizeEpisodeServers(out)
}

func fetchCineCalidadSeasonEpisodes(link string, serieID, seasonNum int) []EpisodeDetail {
	link = normalizeURL(cineCalidadBase, link)
	page, err := fetchHTML(link, cineCalidadBase)
	if err != nil {
		return nil
	}
	re := regexp.MustCompile(`(?is)<div\b[^>]*class=["'][^"']*mark-1[^"']*["'][^>]*>(.*?)</div>\s*</div>`)
	var out []EpisodeDetail
	for _, m := range re.FindAllStringSubmatch(page, -1) {
		block := m[1]
		numerando := cleanText(firstMatch(block, `(?is)<[^>]*class=["'][^"']*numerando[^"']*["'][^>]*>(.*?)</[^>]+>`))
		if !strings.HasPrefix(strings.ToUpper(numerando), fmt.Sprintf("S%d-", seasonNum)) {
			continue
		}
		epNum, _ := strconv.Atoi(firstMatch(numerando, `(?i)-E(\d+)`))
		if epNum <= 0 {
			continue
		}
		href := normalizeURL(cineCalidadBase, attrValueInTag(block, "a", "href"))
		title := cleanText(firstMatch(block, `(?is)<[^>]*class=["'][^"']*episodiotitle[^"']*["'][^>]*>(.*?)</[^>]+>`))
		poster := normalizeURL(cineCalidadBase, attrValueInTag(block, "img", "data-src"))
		if poster == "" {
			poster = normalizeURL(cineCalidadBase, attrValueInTag(block, "img", "src"))
		}
		out = append(out, EpisodeDetail{
			ID:         serieID*100000 + seasonNum*1000 + epNum,
			Number:     epNum,
			Titulo:     title,
			Imagen:     poster,
			Servidores: fetchCineCalidadPageServers(href),
		})
	}
	return out
}

// ── Cuevana ───────────────────────────────────────────────────────────────────

type cuevanaEnvelope[T any] struct {
	Error   bool   `json:"error"`
	Message string `json:"message"`
	Data    T      `json:"data"`
}

type cuevanaImages struct {
	Poster   string `json:"poster"`
	Backdrop string `json:"backdrop"`
}

type cuevanaPost struct {
	ID            int           `json:"_id"`
	Title         string        `json:"title"`
	Overview      string        `json:"overview"`
	Slug          string        `json:"slug"`
	Images        cuevanaImages `json:"images"`
	Poster        string        `json:"poster"`
	Backdrop      string        `json:"backdrop"`
	Rating        string        `json:"rating"`
	CommunityRate string        `json:"community_rating"`
	Genres        []int         `json:"genres"`
	Years         []int         `json:"years"`
	Type          string        `json:"type"`
	ReleaseDate   string        `json:"release_date"`
	Runtime       string        `json:"runtime"`
	OriginalTitle string        `json:"original_title"`
}

type cuevanaPageData struct {
	Posts []cuevanaPost `json:"posts"`
}

type cuevanaPlayerData struct {
	Embeds []cuevanaPlayerEntry `json:"embeds"`
}

type cuevanaPlayerEntry struct {
	URL        string `json:"url"`
	Server     string `json:"server"`
	Lang       string `json:"lang"`
	Quality    string `json:"quality"`
	Resolution string `json:"resolution"`
}

type cuevanaEpisodeList struct {
	Posts   []cuevanaEpisode `json:"posts"`
	Seasons []string         `json:"seasons"`
}

type cuevanaEpisodeData struct {
	Episode cuevanaEpisode `json:"episode"`
}

type cuevanaEpisode struct {
	ID            int    `json:"_id"`
	Title         string `json:"title"`
	Slug          string `json:"slug"`
	Overview      string `json:"overview"`
	StillPath     string `json:"still_path"`
	SeasonNumber  int    `json:"season_number"`
	EpisodeNumber int    `json:"episode_number"`
}

func cuevanaFastAPI[T any](path string, params map[string]string) (T, bool) {
	var zero T
	u, err := url.Parse(cuevanaBase + "/wp-api/v1/" + strings.TrimPrefix(path, "/"))
	if err != nil {
		return zero, false
	}
	q := u.Query()
	for k, v := range params {
		q.Set(k, v)
	}
	u.RawQuery = q.Encode()
	req, err := http.NewRequest("GET", u.String(), nil)
	if err != nil {
		return zero, false
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Referer", cuevanaBase)
	resp, err := httpClient.Do(req)
	if err != nil {
		return zero, false
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return zero, false
	}
	var envelope cuevanaEnvelope[T]
	if json.Unmarshal(body, &envelope) != nil || envelope.Error {
		return zero, false
	}
	return envelope.Data, true
}

func cuevanaPostToCvt(post cuevanaPost, isSeries bool) CvtPost {
	prefix := "peliculas"
	if isSeries {
		prefix = "series"
		if post.Type == "animes" {
			prefix = "animes"
		}
	}
	link := prefix + "/" + post.Slug
	poster := post.Poster
	if poster == "" {
		poster = post.Images.Poster
	}
	backdrop := post.Backdrop
	if backdrop == "" {
		backdrop = post.Images.Backdrop
	}
	rating := post.CommunityRate
	if rating == "" {
		rating = post.Rating
	}
	rd := post.ReleaseDate
	if rd == "" && len(post.Years) > 0 {
		rd = strconv.Itoa(post.Years[0]) + "-01-01"
	}
	cv := externalPost(providerCuevana, link, post.Title, normalizeURL(cuevanaBase, poster), normalizeURL(cuevanaBase, backdrop), post.Overview, rating, post.Runtime, rd, post.Genres, isSeries)
	cv.TMDbID = post.ID
	cv.OriginalTitle = post.OriginalTitle
	return cv
}

func fetchCuevanaMoviesPage(page int) []CvtPost {
	data, ok := cuevanaFastAPI[cuevanaPageData]("/listing/movies", map[string]string{
		"page": strconv.Itoa(page), "orderBy": "latest", "order": "desc", "postType": "movies", "postsPerPage": "24",
	})
	if !ok {
		return nil
	}
	out := make([]CvtPost, 0, len(data.Posts))
	for _, p := range data.Posts {
		out = append(out, cuevanaPostToCvt(p, false))
	}
	return out
}

func fetchCuevanaSeriesPage(page int) []CvtPost {
	var out []CvtPost
	for _, postType := range []string{"tvshows", "animes"} {
		data, ok := cuevanaFastAPI[cuevanaPageData]("/listing/"+postType, map[string]string{
			"page": strconv.Itoa(page), "orderBy": "latest", "order": "desc", "postType": postType, "postsPerPage": "24",
		})
		if !ok {
			continue
		}
		for _, p := range data.Posts {
			if p.Type == "" {
				p.Type = postType
			}
			out = append(out, cuevanaPostToCvt(p, true))
		}
	}
	return out
}

func cuevanaSlug(link string) string {
	link = strings.Trim(strings.TrimPrefix(link, cuevanaBase), "/")
	return strings.Trim(strings.TrimPrefix(link[strings.LastIndex(link, "/")+1:], "/"), " ")
}

func cuevanaShowPostType(link string) string {
	link = strings.Trim(strings.TrimPrefix(link, cuevanaBase), "/")
	if strings.HasPrefix(link, "anime") || strings.HasPrefix(link, "animes") {
		return "animes"
	}
	return "tvshows"
}

func cuevanaResolvePlayerURL(playerURL string) string {
	if playerURL == "" || !strings.Contains(playerURL, "cuevana.gs/player.php") {
		return playerURL
	}
	req, err := http.NewRequest("GET", playerURL, nil)
	if err != nil {
		return playerURL
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Referer", cuevanaBase)
	resp, err := httpClient.Do(req)
	if err != nil {
		return playerURL
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return playerURL
	}
	iframe := attrValueInTag(string(body), "iframe", "src")
	if iframe == "" {
		return playerURL
	}
	return normalizeURL(playerURL, htmlpkg.UnescapeString(iframe))
}

func cuevanaServersFromPlayerData(data cuevanaPlayerData) []EpisodeServer {
	var out []EpisodeServer
	for _, entry := range data.Embeds {
		if entry.URL == "" {
			continue
		}
		playerURL := cuevanaResolvePlayerURL(entry.URL)
		name := entry.Server
		if name == "" || strings.EqualFold(name, "online") {
			name = serverName(playerURL)
		}
		quality := entry.Quality
		if quality == "" {
			quality = entry.Resolution
		}
		if quality == "" {
			quality = "HD"
		}
		out = append(out, episodeServerFromExternalURL(len(out)+1, languageName(entry.Lang), name, quality, playerURL))
	}
	return sanitizeEpisodeServers(out)
}

func fetchCuevanaPlayerServers(postID int) []EpisodeServer {
	data, ok := cuevanaFastAPI[cuevanaPlayerData]("/player", map[string]string{"postId": strconv.Itoa(postID), "demo": "0"})
	if !ok {
		return nil
	}
	return cuevanaServersFromPlayerData(data)
}

func fetchCuevanaMovieServers(link string) []Server {
	post, ok := cuevanaFastAPI[cuevanaPost]("/single/movies", map[string]string{
		"slug": cuevanaSlug(link), "postType": "movies",
	})
	if !ok || post.ID == 0 {
		return nil
	}
	return movieServersFromEpisodeServers(fetchCuevanaPlayerServers(post.ID))
}

func fetchCuevanaSeasonEpisodes(link string, serieID, seasonNum int) []EpisodeDetail {
	showType := cuevanaShowPostType(link)
	show, ok := cuevanaFastAPI[cuevanaPost]("/single/"+showType, map[string]string{
		"slug": cuevanaSlug(link), "postType": showType,
	})
	if !ok || show.ID == 0 {
		return nil
	}
	list, ok := cuevanaFastAPI[cuevanaEpisodeList]("/single/episodes/list", map[string]string{
		"_id": strconv.Itoa(show.ID), "season": strconv.Itoa(seasonNum), "page": "1", "postsPerPage": "100",
	})
	if !ok {
		return nil
	}
	var out []EpisodeDetail
	for _, ep := range list.Posts {
		if ep.SeasonNumber != seasonNum || ep.EpisodeNumber <= 0 {
			continue
		}
		out = append(out, EpisodeDetail{
			ID:         serieID*100000 + seasonNum*1000 + ep.EpisodeNumber,
			Number:     ep.EpisodeNumber,
			Titulo:     ep.Title,
			Imagen:     normalizeURL(cuevanaBase, ep.StillPath),
			Servidores: fetchCuevanaPlayerServers(ep.ID),
		})
	}
	return out
}

// ── Doramasflix ────────────────────────────────────────────────────────────────

type doramasAPIResponse struct {
	Data struct {
		PaginationDorama struct {
			Items []doramasItem `json:"items"`
		} `json:"paginationDorama"`
		PaginationMovie struct {
			Items []doramasItem `json:"items"`
		} `json:"paginationMovie"`
		ListSeasons  []doramasSeason  `json:"listSeasons"`
		ListEpisodes []doramasEpisode `json:"listEpisodes"`
	} `json:"data"`
}

type doramasItem struct {
	ID         string `json:"_id"`
	Name       string `json:"name"`
	NameES     string `json:"name_es"`
	Slug       string `json:"slug"`
	Overview   string `json:"overview"`
	PosterPath string `json:"poster_path"`
	Poster     string `json:"poster"`
	TypeName   string `json:"__typename"`
}

type doramasSeason struct {
	Slug         string `json:"slug"`
	SeasonNumber int    `json:"season_number"`
	PosterPath   string `json:"poster_path"`
}

type doramasEpisode struct {
	ID            string `json:"_id"`
	Name          string `json:"name"`
	Slug          string `json:"slug"`
	EpisodeNumber int    `json:"episode_number"`
	SeasonNumber  int    `json:"season_number"`
	StillPath     string `json:"still_path"`
}

func doramasGraphQL(query string) (doramasAPIResponse, bool) {
	var out doramasAPIResponse
	body, err := postJSON(doramasflixAPI, []byte(query), map[string]string{
		"accept":            "application/json, text/plain, */*",
		"platform":          "doramasflix",
		"authorization":     "Bear",
		"x-access-platform": "RxARncfg1S_MdpSrCvreoLu_SikCGMzE1NzQzODc3NjE2MQ==",
	})
	if err != nil {
		return out, false
	}
	if json.Unmarshal(body, &out) != nil {
		return out, false
	}
	return out, true
}

func doramasPoster(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if strings.HasPrefix(path, "http") {
		return path
	}
	return "https://image.tmdb.org/t/p/w500" + path
}

func doramasTitle(item doramasItem) string {
	if item.NameES != "" {
		return strings.TrimSpace(item.Name + " (" + item.NameES + ")")
	}
	return item.Name
}

func fetchDoramasflixMoviesPage(page int) []CvtPost {
	query := fmt.Sprintf(`{"operationName":"listMovies","variables":{"perPage":20,"sort":"POPULARITY_DESC","filter":{},"page":%d},"query":"query listMovies($page: Int, $perPage: Int, $sort: SortFindManyMovieInput, $filter: FilterFindManyMovieInput) {\n  paginationMovie(page: $page, perPage: $perPage, sort: $sort, filter: $filter) {\n    items {\n      _id\n      name\n      name_es\n      slug\n      poster_path\n      poster\n      __typename\n    }\n  }\n}\n"}`, page)
	resp, ok := doramasGraphQL(query)
	if !ok {
		return nil
	}
	out := make([]CvtPost, 0, len(resp.Data.PaginationMovie.Items))
	for _, item := range resp.Data.PaginationMovie.Items {
		link := "peliculas-online/" + item.Slug
		out = append(out, externalPost(providerDoramasflix, link, doramasTitle(item), doramasPoster(firstNonEmpty(item.PosterPath, item.Poster)), "", item.Overview, "", "", "", []int{157}, false))
	}
	return out
}

func fetchDoramasflixSeriesPage(page int) []CvtPost {
	query := fmt.Sprintf(`{"operationName":"listDoramas","variables":{"page":%d,"sort":"POPULARITY_DESC","perPage":20,"filter":{"isTVShow":false}},"query":"query listDoramas($page: Int, $perPage: Int, $sort: SortFindManyDoramaInput, $filter: FilterFindManyDoramaInput) {\n  paginationDorama(page: $page, perPage: $perPage, sort: $sort, filter: $filter) {\n    items {\n      _id\n      name\n      name_es\n      slug\n      poster_path\n      poster\n      __typename\n    }\n  }\n}\n"}`, page)
	resp, ok := doramasGraphQL(query)
	if !ok {
		return nil
	}
	out := make([]CvtPost, 0, len(resp.Data.PaginationDorama.Items))
	for _, item := range resp.Data.PaginationDorama.Items {
		link := "doramas-online/" + item.Slug
		out = append(out, externalPost(providerDoramasflix, link, doramasTitle(item), doramasPoster(firstNonEmpty(item.PosterPath, item.Poster)), "", item.Overview, "", "", "", []int{157}, true))
	}
	return out
}

func doramasApolloState(link string) map[string]any {
	pageURL := normalizeURL(doramasflixBase+"/", link)
	page, err := fetchHTML(pageURL, doramasflixBase)
	if err != nil {
		return nil
	}
	script := htmlScriptByID(page, "__NEXT_DATA__")
	if script == "" {
		return nil
	}
	var root map[string]any
	if json.Unmarshal([]byte(script), &root) != nil {
		return nil
	}
	props, _ := root["props"].(map[string]any)
	pageProps, _ := props["pageProps"].(map[string]any)
	apollo, _ := pageProps["apolloState"].(map[string]any)
	return apollo
}

func doramasIDFromApollo(apollo map[string]any) string {
	for key, value := range apollo {
		if !strings.HasPrefix(key, "Dorama:") && !strings.HasPrefix(key, "Movie:") {
			continue
		}
		if obj, ok := value.(map[string]any); ok {
			if id, ok := obj["_id"].(string); ok {
				return id
			}
		}
	}
	return ""
}

func fetchDoramasflixMovieServers(link string) []Server {
	return movieServersFromEpisodeServers(fetchDoramasflixServersFromPage(link))
}

func fetchDoramasflixServersFromPage(link string) []EpisodeServer {
	apollo := doramasApolloState(link)
	if len(apollo) == 0 {
		return nil
	}
	var out []EpisodeServer
	for key, value := range apollo {
		obj, ok := value.(map[string]any)
		if !ok {
			continue
		}
		if strings.HasPrefix(key, "Episode:") || strings.HasPrefix(key, "Movie:") {
			if linksObj, ok := obj["links_online"].(map[string]any); ok {
				if arr, ok := linksObj["json"].([]any); ok {
					for _, item := range arr {
						if serverObj, ok := item.(map[string]any); ok {
							out = append(out, doramasServerFromMap(len(out)+1, serverObj))
						}
					}
				}
			}
		}
		if strings.HasPrefix(key, "ROOT_QUERY.listProblems") {
			if serverWrap, ok := obj["server"].(map[string]any); ok {
				if serverObj, ok := serverWrap["json"].(map[string]any); ok {
					out = append(out, doramasServerFromMap(len(out)+1, serverObj))
				}
			}
		}
	}
	return sanitizeEpisodeServers(out)
}

func doramasServerFromMap(id int, obj map[string]any) EpisodeServer {
	link, _ := obj["link"].(string)
	lang, _ := obj["lang"].(string)
	finalURL := doramasRealLink(link)
	return episodeServerFromExternalURL(id, languageName(lang), serverName(finalURL), "HD", finalURL)
}

func doramasRealLink(link string) string {
	link = normalizeURL("", link)
	if !strings.Contains(link, "fkplayer.xyz") {
		return link
	}
	page, err := fetchHTML(link, doramasflixBase)
	if err != nil {
		return link
	}
	script := htmlScriptByID(page, "__NEXT_DATA__")
	token := firstMatch(script, `"token"\s*:\s*"([^"]+)"`)
	if token == "" {
		return link
	}
	body, err := postJSON("https://fkplayer.xyz/api/decoding", []byte(fmt.Sprintf(`{"token":%q}`, token)), nil)
	if err != nil {
		return link
	}
	var resp struct {
		Link string `json:"link"`
	}
	if json.Unmarshal(body, &resp) != nil || resp.Link == "" {
		return link
	}
	decoded, err := base64.StdEncoding.DecodeString(resp.Link)
	if err != nil {
		return link
	}
	return string(decoded)
}

func fetchDoramasflixSeasonEpisodes(link string, serieID, seasonNum int) []EpisodeDetail {
	doramaID := doramasIDFromApollo(doramasApolloState(link))
	if doramaID == "" {
		return nil
	}
	query := fmt.Sprintf(`{"operationName":"listEpisodes","variables":{"serie_id":%q,"season_number":%d},"query":"query listEpisodes($season_number: Float!, $serie_id: MongoID!) {\n  listEpisodes(sort: NUMBER_ASC, filter: {type_serie: \"dorama\", serie_id: $serie_id, season_number: $season_number}) {\n    _id\n    name\n    slug\n    episode_number\n    season_number\n    still_path\n    __typename\n  }\n}\n"}`, doramaID, seasonNum)
	resp, ok := doramasGraphQL(query)
	if !ok {
		return nil
	}
	var out []EpisodeDetail
	for _, ep := range resp.Data.ListEpisodes {
		if ep.EpisodeNumber <= 0 {
			continue
		}
		epLink := "episodios/" + ep.Slug
		title := fmt.Sprintf("Episodio %d", ep.EpisodeNumber)
		if ep.Name != "" {
			title += ": " + ep.Name
		}
		out = append(out, EpisodeDetail{
			ID:         serieID*100000 + seasonNum*1000 + ep.EpisodeNumber,
			Number:     ep.EpisodeNumber,
			Titulo:     title,
			Imagen:     doramasPoster(ep.StillPath),
			Servidores: fetchDoramasflixServersFromPage(epLink),
		})
	}
	return out
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// ── SoloLatino ────────────────────────────────────────────────────────────────

type soloLatinoDataLinkItem struct {
	FileID        int                   `json:"file_id"`
	VideoLanguage string                `json:"video_language"`
	SortedEmbeds  []soloLatinoDataEmbed `json:"sortedEmbeds"`
}

type soloLatinoDataEmbed struct {
	Servername string `json:"servername"`
	Link       string `json:"link"`
	Type       string `json:"type"`
}

func fetchSoloLatinoMoviesPage(page int) []CvtPost {
	pageURL := soloLatinoBase + "/peliculas"
	if page > 1 {
		pageURL = fmt.Sprintf("%s/peliculas/page/%d", soloLatinoBase, page)
	}
	pageHTML, err := fetchHTML(pageURL, soloLatinoBase)
	if err != nil {
		return nil
	}
	return parseSoloLatinoListing(pageHTML, false)
}

func fetchSoloLatinoSeriesPage(page int) []CvtPost {
	pageURL := soloLatinoBase + "/series"
	if page > 1 {
		pageURL = fmt.Sprintf("%s/series/page/%d", soloLatinoBase, page)
	}
	pageHTML, err := fetchHTML(pageURL, soloLatinoBase)
	if err != nil {
		return nil
	}
	return parseSoloLatinoListing(pageHTML, true)
}

func parseSoloLatinoListing(page string, wantSeries bool) []CvtPost {
	re := regexp.MustCompile(`(?is)<a\b[^>]*href=["']([^"']*(?:/pelicula/|/serie/|/series/)[^"']*)["'][^>]*>`)
	matches := re.FindAllStringSubmatchIndex(page, -1)
	seen := map[string]bool{}
	var out []CvtPost
	for _, m := range matches {
		href := normalizeURL(soloLatinoBase, page[m[2]:m[3]])
		if href == "" || seen[href] {
			continue
		}
		isSeries := strings.Contains(href, "/serie/") || strings.Contains(href, "/series/")
		if wantSeries != isSeries {
			continue
		}
		block := surroundingBlock(page, m[0], 700, 2500)
		title := cleanText(firstMatch(block, `(?is)<[^>]*class=["'][^"']*card__title[^"']*["'][^>]*>(.*?)</[^>]+>`))
		if title == "" {
			title = attrValueInTag(block, "img", "alt")
		}
		poster := normalizeURL(soloLatinoBase, firstNonEmpty(
			firstMatch(block, `(?is)<img\b[^>]*class=["'][^"']*card__poster[^"']*["'][^>]*src=["']([^"']+)["']`),
			attrValueInTag(block, "img", "src"),
		))
		year := cleanText(firstMatch(block, `(?is)<[^>]*class=["'][^"']*card__year[^"']*["'][^>]*>(.*?)</[^>]+>`))
		out = append(out, externalPost(providerSoloLatino, href, title, poster, "", "", "", "", parseYearDate(firstYear(year)), nil, isSeries))
		seen[href] = true
	}
	return out
}

func fetchSoloLatinoMovieServers(link string) []Server {
	return movieServersFromEpisodeServers(fetchSoloLatinoPageServers(link))
}

func fetchSoloLatinoPageServers(link string) []EpisodeServer {
	link = normalizeURL(soloLatinoBase, link)
	page, err := fetchHTML(link, soloLatinoBase)
	if err != nil {
		return nil
	}
	buttonRE := regexp.MustCompile(`(?is)<button\b[^>]*class=["'][^"']*server-btn[^"']*["'][^>]*\bdata-server-url=["']([^"']+)["'][^>]*>`)
	var out []EpisodeServer
	for _, m := range buttonRE.FindAllStringSubmatch(page, -1) {
		if len(m) < 2 || m[1] == "" {
			continue
		}
		for _, s := range processSoloLatinoIframe(normalizeURL(soloLatinoBase, m[1]), link) {
			s.ID = len(out) + 1
			out = append(out, s)
		}
	}
	return sanitizeEpisodeServers(out)
}

func processSoloLatinoIframe(iframeURL, referer string) []EpisodeServer {
	page, err := fetchHTML(iframeURL, referer)
	if err != nil {
		return nil
	}
	var out []EpisodeServer
	if dataJSON := firstMatch(page, `(?is)dataLink\s*=\s*(\[.+?\]);`); dataJSON != "" {
		var items []soloLatinoDataLinkItem
		if json.Unmarshal([]byte(dataJSON), &items) == nil {
			for _, item := range items {
				lang := languageName(item.VideoLanguage)
				for _, embed := range item.SortedEmbeds {
					if strings.EqualFold(embed.Servername, "download") || strings.EqualFold(embed.Type, "download") {
						continue
					}
					finalURL := decodeJWTLink(embed.Link)
					if finalURL == "" {
						continue
					}
					out = append(out, episodeServerFromExternalURL(len(out)+1, lang, embed.Servername, "HD", finalURL))
				}
			}
		}
	}
	domRE := regexp.MustCompile(`(?is)<li\b[^>]*onclick=["'][^"']*go_to_playerVast\(\s*\\?'([^'"]+)\\?'[^"']*["'][^>]*>(.*?)</li>`)
	for _, m := range domRE.FindAllStringSubmatch(page, -1) {
		if len(m) < 3 {
			continue
		}
		finalURL := normalizeURL(iframeURL, m[1])
		name := cleanText(firstMatch(m[2], `(?is)<span[^>]*>(.*?)</span>`))
		if strings.EqualFold(name, "1fichier") || strings.EqualFold(name, "download") {
			continue
		}
		out = append(out, episodeServerFromExternalURL(len(out)+1, "Latino", name, "HD", finalURL))
	}
	if iframe := attrValueInTag(page, "iframe", "src"); iframe != "" {
		iframe = normalizeURL(iframeURL, iframe)
		out = append(out, episodeServerFromExternalURL(len(out)+1, "Latino", serverName(iframe), "HD", iframe))
	}
	return sanitizeEpisodeServers(out)
}

func fetchSoloLatinoSeasonEpisodes(link string, serieID, seasonNum int) []EpisodeDetail {
	link = normalizeURL(soloLatinoBase, link)
	page, err := fetchHTML(link, soloLatinoBase)
	if err != nil {
		return nil
	}
	hrefRE := regexp.MustCompile(`(?is)<a\b[^>]*class=["'][^"']*ep-item[^"']*["'][^>]*href=["']([^"']+)["'][^>]*>`)
	locs := hrefRE.FindAllStringSubmatchIndex(page, -1)
	var out []EpisodeDetail
	for _, loc := range locs {
		href := normalizeURL(soloLatinoBase, page[loc[2]:loc[3]])
		block := surroundingBlock(page, loc[0], 3500, 1600)
		panelIdx := strings.LastIndex(block, "data-season-panel")
		if panelIdx < 0 {
			continue
		}
		panelBlock := block[panelIdx:]
		panelNum, _ := strconv.Atoi(firstMatch(panelBlock, `data-season-panel=["']?(\d+)`))
		if panelNum != seasonNum {
			continue
		}
		epNum, _ := strconv.Atoi(firstMatch(block, `(?is)<[^>]*class=["'][^"']*ep-num[^"']*["'][^>]*>[^0-9]*(\d+)`))
		if epNum <= 0 {
			epNum, _ = strconv.Atoi(firstMatch(href, `(?:capitulo|episodio|episode|ep)-?(\d+)`))
		}
		if epNum <= 0 {
			continue
		}
		title := cleanText(firstMatch(block, `(?is)<p\b[^>]*class=["'][^"']*leading-tight[^"']*["'][^>]*>(.*?)</p>`))
		if title == "" {
			title = fmt.Sprintf("Episodio %d", epNum)
		}
		poster := normalizeURL(soloLatinoBase, firstMatch(block, `(?is)<img\b[^>]*class=["'][^"']*ep-thumb[^"']*["'][^>]*src=["']([^"']+)["']`))
		out = append(out, EpisodeDetail{
			ID:         serieID*100000 + seasonNum*1000 + epNum,
			Number:     epNum,
			Titulo:     title,
			Imagen:     poster,
			Servidores: fetchSoloLatinoPageServers(href),
		})
	}
	return out
}

// ── AnimeFLV ──────────────────────────────────────────────────────────────────

type animeFLVServerModel struct {
	Sub []struct {
		Title string `json:"title"`
		Code  string `json:"code"`
	} `json:"SUB"`
}

func fetchAnimeFLVMoviesPage(page int) []CvtPost {
	pageURL := fmt.Sprintf("%s/browse?type[]=movie&page=%d", animeFLVBase, page)
	pageHTML, err := fetchHTML(pageURL, animeFLVBase)
	if err != nil {
		return nil
	}
	return parseAnimeFLVListing(pageHTML, false)
}

func fetchAnimeFLVSeriesPage(page int) []CvtPost {
	pageURL := fmt.Sprintf("%s/browse?order=rating&page=%d", animeFLVBase, page)
	pageHTML, err := fetchHTML(pageURL, animeFLVBase)
	if err != nil {
		return nil
	}
	return parseAnimeFLVListing(pageHTML, true)
}

func parseAnimeFLVListing(page string, wantSeries bool) []CvtPost {
	var out []CvtPost
	seen := map[string]bool{}
	for _, art := range articleRE.FindAllString(page, -1) {
		href := normalizeURL(animeFLVBase, attrValueInTag(art, "a", "href"))
		if !strings.Contains(href, "/anime/") || seen[href] {
			continue
		}
		typeText := cleanText(firstMatch(art, `(?is)<span\b[^>]*class=["'][^"']*Type[^"']*["'][^>]*>(.*?)</span>`))
		isMovie := strings.EqualFold(typeText, "Película")
		if wantSeries == isMovie {
			continue
		}
		title := cleanText(firstMatch(art, `(?is)<h3[^>]*>(.*?)</h3>`))
		poster := normalizeURL(animeFLVBase, attrValueInTag(art, "img", "src"))
		out = append(out, externalPost(providerAnimeFLV, href, title, poster, "", "", "", "", "", []int{51}, wantSeries))
		seen[href] = true
	}
	return out
}

func fetchAnimeFLVMovieServers(link string) []Server {
	episodes := fetchAnimeFLVSeasonEpisodes(link, slugToID(providerAnimeFLV+":"+link), 1)
	if len(episodes) == 0 {
		return nil
	}
	return movieServersFromEpisodeServers(episodes[0].Servidores)
}

func fetchAnimeFLVSeasonEpisodes(link string, serieID, seasonNum int) []EpisodeDetail {
	if seasonNum != 1 {
		return nil
	}
	link = normalizeURL(animeFLVBase, link)
	page, err := fetchHTML(link, animeFLVBase)
	if err != nil {
		return nil
	}
	script := firstMatch(page, `(?is)<script[^>]*>([^<]*(?:var\s+episodes\s*=|var\s+anime_info\s*=).*?)</script>`)
	if script == "" {
		script = page
	}
	episodesRaw := firstMatch(script, `(?is)var\s+episodes\s*=\s*\[(.*?)\];`)
	animeInfoRaw := firstMatch(script, `(?is)var\s+anime_info\s*=\s*(\[[^\]]+\])`)
	var animeInfo []string
	_ = json.Unmarshal([]byte(animeInfoRaw), &animeInfo)
	animeID := ""
	animeURI := ""
	if len(animeInfo) > 0 {
		animeID = animeInfo[0]
	}
	if len(animeInfo) > 2 {
		animeURI = animeInfo[2]
	}
	if episodesRaw == "" || animeURI == "" {
		return nil
	}
	pieces := strings.Split(episodesRaw, "],[")
	var out []EpisodeDetail
	for _, piece := range pieces {
		piece = strings.Trim(piece, "[] ")
		cols := strings.Split(piece, ",")
		if len(cols) == 0 {
			continue
		}
		epNum, _ := strconv.Atoi(strings.Trim(cols[0], ` "'`))
		if epNum <= 0 {
			continue
		}
		epLink := fmt.Sprintf("%s/ver/%s-%d", animeFLVBase, animeURI, epNum)
		poster := ""
		if animeID != "" {
			poster = fmt.Sprintf("https://cdn.animeflv.net/screenshots/%s/%d/th_3.jpg", animeID, epNum)
		}
		out = append(out, EpisodeDetail{
			ID:         serieID*100000 + seasonNum*1000 + epNum,
			Number:     epNum,
			Titulo:     fmt.Sprintf("Episodio %d", epNum),
			Imagen:     poster,
			Servidores: fetchAnimeFLVEpisodeServers(epLink),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Number < out[j].Number })
	return out
}

func fetchAnimeFLVEpisodeServers(link string) []EpisodeServer {
	link = normalizeURL(animeFLVBase, link)
	page, err := fetchHTML(link, animeFLVBase)
	if err != nil {
		return nil
	}
	jsonStr := jsonObjectFromScriptVar(page, "var videos")
	if jsonStr == "" {
		return nil
	}
	var model animeFLVServerModel
	if json.Unmarshal([]byte(jsonStr), &model) != nil {
		return nil
	}
	var out []EpisodeServer
	for _, sub := range model.Sub {
		if strings.TrimSpace(sub.Code) == "" {
			continue
		}
		name := sub.Title
		if name == "" {
			name = serverName(sub.Code)
		}
		out = append(out, episodeServerFromExternalURL(len(out)+1, "Subtitulado", name, "HD", sub.Code))
	}
	return sanitizeEpisodeServers(out)
}
