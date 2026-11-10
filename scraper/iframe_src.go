package main

import "regexp"

// FlixLatam cambio el HTML del reproductor: ahora el iframe trae otros atributos ANTES de src
// (<iframe title="Reproductor de video" src="...">) y el scraper buscaba "<iframe src=" pegado, asi que desde
// ese cambio no encontraba ningun servidor en los episodios/peliculas nuevos. Esto lee el src (o data-src) del
// primer iframe sin importar el orden de los atributos.
var (
	iframeTagRe  = regexp.MustCompile(`(?is)<iframe\b[^>]*>`)
	iframeSrcRe  = regexp.MustCompile(`(?is)\ssrc\s*=\s*["']([^"']*)["']`)
	iframeDataRe = regexp.MustCompile(`(?is)\sdata-src\s*=\s*["']([^"']*)["']`)
)

// Tambien cambio el listado de episodios de la pagina de la serie: el enlace ya no es /serie/<slug>/temporada/N/capitulo/M
// sino /serie/<slug>#s1-e3, y el numero tambien viene en <div class="numerando">1 - 3</div>. Se aceptan los tres formatos.
var (
	episodeAnchorRe    = regexp.MustCompile(`(?i)#s\d+-e(\d+)`)
	episodeNumerandoRe = regexp.MustCompile(`(?is)<div class="numerando">\s*\d+\s*-\s*(\d+)\s*</div>`)
	episodeCapituloRe  = regexp.MustCompile(`(?i)/capitulo/(\d+)`)
)

func episodeNumberFrom(item string) int {
	for _, re := range []*regexp.Regexp{episodeCapituloRe, episodeAnchorRe, episodeNumerandoRe} {
		if m := re.FindStringSubmatch(item); len(m) > 1 {
			n := 0
			for _, c := range m[1] {
				n = n*10 + int(c-'0')
			}
			if n > 0 {
				return n
			}
		}
	}
	return 0
}

func iframeSourceFrom(part string) string {
	tag := iframeTagRe.FindString(part)
	if tag == "" {
		return ""
	}
	if m := iframeSrcRe.FindStringSubmatch(tag); len(m) > 1 && m[1] != "" && m[1] != "about:blank" {
		return m[1]
	}
	if m := iframeDataRe.FindStringSubmatch(tag); len(m) > 1 && m[1] != "" && m[1] != "about:blank" {
		return m[1]
	}
	return ""
}
