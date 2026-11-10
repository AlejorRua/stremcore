package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSeriesMissingSeasonsYNumeroDeEpisodio(t *testing.T) {
	root := t.TempDir()
	seriesDir := filepath.Join(root, "series")
	if err := os.MkdirAll(seriesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "movies"), 0o755); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	write := func(id string, added time.Time, slug string) {
		body := `{"id":` + id + `,"titulo":"Serie ` + id + `","slug":"` + slug + `","added_at":"` + added.Format(time.RFC3339) + `"}`
		if err := os.WriteFile(filepath.Join(seriesDir, id+".json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("1", now.Add(-48*time.Hour), "sin-temporadas-reciente")
	write("2", now.Add(-72*time.Hour), "con-temporadas")
	write("3", now.Add(-90*24*time.Hour), "sin-temporadas-vieja")
	write("4", now.Add(-24*time.Hour), "")
	if err := os.MkdirAll(filepath.Join(seriesDir, "2"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(seriesDir, "2", "t1.json"), []byte(`{"serie_id":2,"number":1,"episodios":[]}`), 0o644); err != nil {
		t.Fatal(err)
	}

	catalog, err := loadJSONCatalogIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	got := catalog.seriesMissingSeasons(10, seriesRetryWindow)
	if len(got) != 1 || got[0].ID != 1 {
		t.Fatalf("se esperaba solo la serie 1 (reciente y sin temporadas), salio %+v", got)
	}

	casos := map[string]int{
		`<li><div class="numerando">1 - 3</div><a href="/serie/x#s1-e3" class="episode-link">`: 3,
		`<a href="/serie/x#s2-e12">`:                                        12,
		`<a href="/serie/x/temporada/1/capitulo/7">`:                        7,
		`<li><div class="numerando">2 - 5</div><div class="episodiotitle">`: 5,
		`<li>sin numero</li>`:                                               0,
	}
	for html, want := range casos {
		if n := episodeNumberFrom(html); n != want {
			t.Fatalf("episodeNumberFrom(%q) = %d, se esperaba %d", html, n, want)
		}
	}

	nuevo := `<div class="pframe source-box on"><iframe title="Reproductor de video" src="/vidurl/tt1-1x01/" frameborder="0">`
	if s := iframeSourceFrom(nuevo); s != "/vidurl/tt1-1x01/" {
		t.Fatalf("iframe nuevo: %q", s)
	}
	viejo := `<div><iframe src="https://e.example/abc"></iframe>`
	if s := iframeSourceFrom(viejo); s != "https://e.example/abc" {
		t.Fatalf("iframe viejo: %q", s)
	}
	lazy := `<div><iframe data-src="https://e.example/lazy" loading="lazy"></iframe>`
	if s := iframeSourceFrom(lazy); s != "https://e.example/lazy" {
		t.Fatalf("iframe lazy: %q", s)
	}
}
