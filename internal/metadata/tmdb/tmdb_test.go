package tmdb

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBrowse(t *testing.T) {
	var gotPath string
	var gotQuery map[string][]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.Query()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"page": 2, "total_pages": 9,
			"results": []map[string]any{{"id": 1, "name": "Severance", "first_air_date": "2022-02-18"}},
		})
	}))
	defer srv.Close()
	c := New("key", "")
	c.SetBaseURL(srv.URL)
	ctx := t.Context()

	p, err := c.Browse(ctx, "tv", "top_rated", 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/tv/top_rated" || gotQuery["page"][0] != "2" {
		t.Fatalf("requested %s %v", gotPath, gotQuery)
	}
	// List endpoints omit media_type; Browse fills it in.
	if p.Page != 2 || p.TotalPages != 9 || len(p.Results) != 1 || p.Results[0].MediaType != "tv" {
		t.Fatalf("page = %+v", p)
	}

	if _, err := c.Browse(ctx, "movie", "trending", 878, 1); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/discover/movie" || gotQuery["with_genres"][0] != "878" {
		t.Fatalf("genre browse requested %s %v", gotPath, gotQuery)
	}

	if _, err := c.Browse(ctx, "tv", "now_playing", 0, 1); err == nil {
		t.Fatal("now_playing is a movie list; want an error for tv")
	}
	if _, err := c.Browse(ctx, "person", "popular", 0, 1); err == nil {
		t.Fatal("want an error for an unknown media type")
	}
}
