package store

import (
	"context"
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
