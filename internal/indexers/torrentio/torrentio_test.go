package torrentio

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestStreamUnmarshalTorrentioFormat(t *testing.T) {
	// Real Torrentio payload shape.
	raw := `{
		"name": "Torrentio\n1080p\nWEBRip",
		"title": "Dune.Part.Two.2024.1080p.WEBRip.x264-RARBG\n👤 245 💾 4.36 GB 📊 97.2%\nRARBG",
		"infoHash": "ABCDEF0123456789"
	}`
	var s Stream
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatal(err)
	}
	if s.Title != "Dune.Part.Two.2024.1080p.WEBRip.x264-RARBG" {
		t.Errorf("title = %q", s.Title)
	}
	if s.Seeders != 245 {
		t.Errorf("seeders = %d", s.Seeders)
	}
	const wantSize = int64(4_681_514_352) // 4.36 GiB
	if s.SizeBytes < wantSize-1024 || s.SizeBytes > wantSize+1024 {
		t.Errorf("size = %d, want ~%d", s.SizeBytes, wantSize)
	}
	if s.InfoHash != "abcdef0123456789" {
		t.Errorf("hash = %q", s.InfoHash)
	}
	if s.Source != "RARBG" {
		t.Errorf("source = %q", s.Source)
	}
}

func TestStreamUnmarshalPlainFormat(t *testing.T) {
	raw := `{
		"title": "Some.Release.2020\n3.2 GB\nWEB\n89\nUPLOADER",
		"infoHash": "feed"
	}`
	var s Stream
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		t.Fatal(err)
	}
	const wantSize = int64(3_435_973_836) // 3.2 GiB = 3.2 * 1024^3
	if s.SizeBytes < wantSize-1024 || s.SizeBytes > wantSize+1024 {
		t.Errorf("size = %d, want ~%d", s.SizeBytes, wantSize)
	}
}

func TestMagnet(t *testing.T) {
	s := Stream{Title: "A Movie", InfoHash: "abcd1234"}
	m := s.Magnet()
	if m != "magnet:?xt=urn:btih:abcd1234&dn=A%20Movie" {
		t.Errorf("magnet = %q", m)
	}
}

func TestSearchMovie(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/stream/movie/tt123.json" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"streams":[{"title":"X.2020\n👤 5 💾 1 GB 📊 99%\nYTS","infoHash":"aa11"}]}`))
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL)
	streams, err := c.SearchMovie(context.Background(), "tt123")
	if err != nil {
		t.Fatal(err)
	}
	if len(streams) != 1 || streams[0].InfoHash != "aa11" {
		t.Fatalf("streams = %+v", streams)
	}
}

func TestSearchSeries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/stream/series/tt456:2:3.json" {
			t.Errorf("path = %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"streams":[]}`))
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL)
	if _, err := c.SearchSeries(context.Background(), "tt456", 2, 3); err != nil {
		t.Fatal(err)
	}
}

func TestNotFoundIsEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	c := New(srv.URL)
	streams, err := c.SearchMovie(context.Background(), "tt0000000")
	if err != nil || streams != nil {
		t.Errorf("streams=%v err=%v", streams, err)
	}
}
