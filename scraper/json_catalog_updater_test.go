package main

import (
	"path/filepath"
	"testing"
)

func TestAppendUniqueJSONItemDeduplicatesTitleAndYear(t *testing.T) {
	items := []flixLatestItem{{Title: "Kaidan Horror Classics", Slug: "first", ReleaseDate: "2026-01-01"}}
	items = appendUniqueJSONItem(items, flixLatestItem{Title: "Kaidan Horror Classics", Slug: "second", ReleaseDate: "2026-01-01"})
	if len(items) != 1 {
		t.Fatalf("expected one item, got %d", len(items))
	}
}

func TestMergeFlixLatestItemsDeduplicatesSlug(t *testing.T) {
	current := []flixLatestItem{{Title: "Existente", Slug: "misma-pelicula"}}
	incoming := []flixLatestItem{
		{Title: "Existente repetida", Slug: "MISMA-PELICULA"},
		{Title: "Nueva", Slug: "pelicula-nueva"},
	}

	merged := mergeFlixLatestItems(current, incoming)
	if len(merged) != 2 {
		t.Fatalf("expected 2 unique items, got %d", len(merged))
	}
	if merged[1].Slug != "pelicula-nueva" {
		t.Fatalf("unexpected appended item: %#v", merged[1])
	}
}

func TestMergeJSONMovieServersPreservesProviders(t *testing.T) {
	existing := []Server{
		{ID: 1, Nombre: "Proveedor A", URL: "https://example.com/a"},
		{ID: 2, Nombre: "Proveedor B", URL: "https://example.com/b"},
	}
	fresh := []Server{
		{ID: 1, Nombre: "Proveedor B", URL: "https://example.com/b"},
		{ID: 2, Nombre: "Proveedor C", URL: "https://example.com/c"},
	}

	merged := mergeJSONMovieServers(existing, fresh)
	if len(merged) != 3 {
		t.Fatalf("expected 3 unique servers, got %d", len(merged))
	}
	for i, server := range merged {
		if server.ID != i+1 {
			t.Fatalf("server %d has id %d", i, server.ID)
		}
	}
	if merged[0].Nombre != "Proveedor A" || merged[2].Nombre != "Proveedor C" {
		t.Fatalf("unexpected provider order: %#v", merged)
	}
}

func TestEpisodeExistsReadsRepositorySeason(t *testing.T) {
	root := t.TempDir()
	seriesDir := filepath.Join(root, "series")
	entry := repoCatalogEntry{ID: 42, Titulo: "Serie de prueba", Slug: "serie-prueba"}
	if err := writeJSONAtomic(filepath.Join(seriesDir, "42.json"), entry); err != nil {
		t.Fatal(err)
	}
	season := SeasonDetail{
		SerieID: 42,
		Number:  1,
		Episodios: []EpisodeDetail{{
			ID: 4201001, Number: 1,
			Servidores: []EpisodeServer{{ID: 1, URL: "https://example.com/video"}},
		}},
	}
	if err := writeJSONAtomic(filepath.Join(seriesDir, "42", "t1.json"), season); err != nil {
		t.Fatal(err)
	}
	index, err := loadJSONCatalogIndexWithEmptyMovies(root)
	if err != nil {
		t.Fatal(err)
	}
	if !index.episodeExists(flixLatestEpisode{Slug: "serie-prueba", Season: 1, Episode: 1}) {
		t.Fatal("expected episode to exist")
	}
}

func loadJSONCatalogIndexWithEmptyMovies(root string) (*jsonCatalogIndex, error) {
	if err := writeJSONAtomic(filepath.Join(root, "movies", "placeholder.json"), repoCatalogEntry{}); err != nil {
		return nil, err
	}
	return loadJSONCatalogIndex(root)
}

func TestAbsoluteFlixLatamPlayerURL(t *testing.T) {
	got := absoluteFlixLatamPlayerURL("/vidurl/tt123/")
	want := "https://flixlatam.com/vidurl/tt123/"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}
