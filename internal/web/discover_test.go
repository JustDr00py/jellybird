package web

import (
	"testing"

	"jellybird/internal/config"
	"jellybird/internal/metadata/tmdb"
	"jellybird/internal/store"
)

func TestNormTitle(t *testing.T) {
	cases := map[string]string{
		"Spider-Man: No Way Home": "spider man no way home",
		"Spider Man No Way Home":  "spider man no way home",
		"Ocean's Eleven":          "oceans eleven",
		"Law & Order":             "law and order",
		"  Amélie  ":              "amélie",
	}
	for in, want := range cases {
		if got := normTitle(in); got != want {
			t.Errorf("normTitle(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLibraryIndexLookup(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := t.Context()
	lib := config.Library{Path: "/media", MoviesDir: "Movies", TVDir: "Shows"}

	files := []store.CloudFile{
		{TorrentID: "T1", FileID: "1", StrmPath: "/media/Movies/Spider-Man No Way Home (2021)/Spider-Man No Way Home (2021).strm"},
		{TorrentID: "T2", FileID: "1", StrmPath: "/media/Movies/Nosferatu (2024)/Nosferatu (2024).strm"},
		{TorrentID: "T3", FileID: "1", StrmPath: "/media/Shows/Severance/Season 01/Severance S01E01.strm"},
		{TorrentID: "T3", FileID: "2", StrmPath: "/media/Shows/Severance/Season 01/Severance S01E02.strm"},
		{TorrentID: "T4", FileID: "1", StrmPath: "/media/Movies/Unsorted/Some Random Thing.strm"},
	}
	for _, f := range files {
		f.Provider, f.TorrentName, f.FilePath = "realdebrid", "x", "x.mkv"
		if _, err := st.UpsertFile(ctx, f); err != nil {
			t.Fatal(err)
		}
	}
	// Added through search: known by TMDB id, whatever its folder is called.
	if err := st.SetHint(ctx, store.Hint{Provider: "realdebrid", TorrentID: "T9", Kind: "movie", Title: "Renamed", TMDBID: "42"}); err != nil {
		t.Fatal(err)
	}

	idx, err := loadLibraryIndex(ctx, st, lib)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name     string
		r        tmdb.Result
		want     bool
		episodes int
	}{
		{"folder match, punctuation differs", tmdb.Result{ID: 1, MediaType: "movie", Title: "Spider-Man: No Way Home", ReleaseDate: "2021-12-15"}, true, 0},
		{"year one off", tmdb.Result{ID: 2, MediaType: "movie", Title: "Nosferatu", ReleaseDate: "2025-01-01"}, true, 0},
		{"same title, other remake", tmdb.Result{ID: 3, MediaType: "movie", Title: "Nosferatu", ReleaseDate: "1922-03-04"}, false, 0},
		{"hint by tmdb id", tmdb.Result{ID: 42, MediaType: "movie", Title: "Something Else"}, true, 0},
		{"unsorted is ignored", tmdb.Result{ID: 5, MediaType: "movie", Title: "Unsorted"}, false, 0},
		{"show counts episodes", tmdb.Result{ID: 6, MediaType: "tv", Name: "Severance"}, true, 2},
		{"missing show", tmdb.Result{ID: 7, MediaType: "tv", Name: "The Bear"}, false, 0},
		{"movie folder isn't a show", tmdb.Result{ID: 8, MediaType: "tv", Name: "Nosferatu"}, false, 0},
	}
	for _, c := range cases {
		got, eps := idx.lookup(c.r)
		if got != c.want || eps != c.episodes {
			t.Errorf("%s: lookup = (%v, %d), want (%v, %d)", c.name, got, eps, c.want, c.episodes)
		}
	}
}
