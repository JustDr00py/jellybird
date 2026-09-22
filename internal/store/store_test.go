package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestFileRoundTrip(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	created, err := s.UpsertFile(ctx, CloudFile{
		Provider: "realdebrid", TorrentID: "T1", TorrentName: "Dune",
		FileID: "1", FilePath: "Dune.2021.mkv", SizeBytes: 100, StrmPath: "/media/Movies/Dune (2021)/Dune (2021).strm",
	})
	if err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	created, err = s.UpsertFile(ctx, CloudFile{
		Provider: "realdebrid", TorrentID: "T1", TorrentName: "Dune",
		FileID: "1", FilePath: "Dune.2021.mkv", SizeBytes: 200, StrmPath: "/media/Movies/Dune (2021)/Dune (2021).strm",
	})
	if err != nil || created {
		t.Fatalf("second upsert created=%v err=%v", created, err)
	}

	files, err := s.ListFiles(ctx, "realdebrid")
	if err != nil || len(files) != 1 {
		t.Fatalf("files=%d err=%v", len(files), err)
	}
	if files[0].SizeBytes != 200 {
		t.Errorf("size = %d", files[0].SizeBytes)
	}

	got, err := s.GetFile(ctx, "realdebrid", "T1", "1")
	if err != nil || got.TorrentName != "Dune" {
		t.Fatalf("got=%+v err=%v", got, err)
	}

	if _, err := s.GetFile(ctx, "realdebrid", "T1", "9"); err != ErrNotFound {
		t.Errorf("missing file err = %v", err)
	}

	if err := s.DeleteFile(ctx, "realdebrid", "T1", "1"); err != nil {
		t.Fatal(err)
	}
	files, _ = s.ListFiles(ctx, "")
	if len(files) != 0 {
		t.Errorf("files after delete = %d", len(files))
	}
}

// Regression: requests.tmdb_id has been in CREATE TABLE IF NOT EXISTS since
// this repo's first commit, which is a no-op against a table that already
// exists — so a database from an even older, pre-history version of
// jellybird (long-running remote deployments) never gets that column and
// every INSERT/SELECT touching it fails with "no such column: tmdb_id".
func TestMigrateAddsRequestsTmdbID(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := raw.Exec(`CREATE TABLE requests (
		source TEXT NOT NULL, source_id TEXT NOT NULL, media_type TEXT NOT NULL,
		title TEXT NOT NULL, year INTEGER NOT NULL DEFAULT 0,
		imdb_id TEXT NOT NULL DEFAULT '', season INTEGER NOT NULL DEFAULT 0,
		episode INTEGER NOT NULL DEFAULT 0, status TEXT NOT NULL,
		detail TEXT NOT NULL DEFAULT '', updated_at INTEGER NOT NULL,
		PRIMARY KEY (source, source_id)
	)`); err != nil {
		t.Fatal(err)
	}
	if err := raw.Close(); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open on pre-tmdb_id database: %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	if err := s.UpsertRequest(ctx, Request{
		Source: "jellyseerr", SourceID: "1", MediaType: "movie",
		Title: "Dune", TMDBID: "438631", Status: "pending",
	}); err != nil {
		t.Fatalf("UpsertRequest after migration: %v", err)
	}
	reqs, err := s.ListRequests(ctx, "")
	if err != nil || len(reqs) != 1 || reqs[0].TMDBID != "438631" {
		t.Errorf("reqs=%+v err=%v", reqs, err)
	}
}

func TestStaleFiles(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	_, err := s.UpsertFile(ctx, CloudFile{Provider: "torbox", TorrentID: "1", TorrentName: "A", FileID: "1"})
	if err != nil {
		t.Fatal(err)
	}
	stale, err := s.StaleFiles(ctx, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 1 {
		t.Fatalf("stale = %d, want 1", len(stale))
	}
	fresh, err := s.StaleFiles(ctx, time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(fresh) != 0 {
		t.Fatalf("fresh = %d, want 0", len(fresh))
	}
}

func TestLinkCache(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	// Expired link behaves as missing.
	if err := s.PutLink(ctx, "rd", "T", "F", "http://old", time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetLink(ctx, "rd", "T", "F"); err != ErrNotFound {
		t.Errorf("expired err = %v", err)
	}
	// Fresh link round-trips.
	if err := s.PutLink(ctx, "rd", "T", "F", "http://cdn", time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	l, err := s.GetLink(ctx, "rd", "T", "F")
	if err != nil || l.URL != "http://cdn" {
		t.Fatalf("l=%+v err=%v", l, err)
	}
	if err := s.InvalidateLink(ctx, "rd", "T", "F"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetLink(ctx, "rd", "T", "F"); err != ErrNotFound {
		t.Errorf("invalidate err = %v", err)
	}
}

func TestRequests(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	err := s.UpsertRequest(ctx, Request{
		Source: "jellyseerr", SourceID: "42", MediaType: "movie",
		Title: "Dune", Status: "pending",
	})
	if err != nil {
		t.Fatal(err)
	}
	err = s.UpsertRequest(ctx, Request{
		Source: "jellyseerr", SourceID: "42", MediaType: "movie",
		Title: "Dune", Status: "added", Detail: "realdebrid T99",
	})
	if err != nil {
		t.Fatal(err)
	}
	reqs, err := s.ListRequests(ctx, "added")
	if err != nil || len(reqs) != 1 {
		t.Fatalf("reqs=%d err=%v", len(reqs), err)
	}
	if reqs[0].Detail != "realdebrid T99" {
		t.Errorf("detail = %q", reqs[0].Detail)
	}
}
