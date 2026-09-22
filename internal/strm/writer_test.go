package strm

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jellybird/internal/config"
	"jellybird/internal/provider"
	"jellybird/internal/store"
)

func testWriter(t *testing.T) (*Writer, *store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	cfg := config.Defaults()
	cfg.Library.Path = filepath.Join(dir, "media")
	cfg.Sync.MinFileMB = 0 // keep small test files
	w := NewWriter(cfg, st, slog.New(slog.DiscardHandler), "http://gw:8097", "")
	return w, st, cfg.Library.Path
}

func sampleTorrents() []provider.Torrent {
	return []provider.Torrent{
		{
			ID: "T1", Name: "Dune.Part.Two.2024.1080p", Hash: "aa", Status: provider.StatusReady,
			Files: []provider.File{
				{ID: "1", Path: "Dune.Part.Two.2024.1080p.mkv", SizeBytes: 8_000_000_000},
				{ID: "2", Path: "sample.mkv", SizeBytes: 1_000},
			},
		},
		{
			ID: "T2", Name: "Severance.S02E01", Hash: "bb", Status: provider.StatusReady,
			Files: []provider.File{
				{ID: "7", Path: "Severance.S02E01.2160p.mkv", SizeBytes: 9_000_000_000},
			},
		},
		{
			ID: "T3", Name: "Still.Downloading.S01E01", Status: provider.StatusDownloading,
			Files: []provider.File{{ID: "9", Path: "Still.Downloading.S01E01.mkv", SizeBytes: 900_000_000}},
		},
	}
}

func TestSyncProviderWritesStrm(t *testing.T) {
	w, st, libPath := testWriter(t)
	ctx := context.Background()

	res, err := w.SyncProvider(ctx, provider.RealDebrid, sampleTorrents())
	if err != nil {
		t.Fatal(err)
	}
	// T1 has 2 video files; "sample.mkv" is 1KB against an 8GB main file,
	// so the no-hint extras size-ratio filter drops it regardless of
	// MinFileMB. T3 skipped (downloading).
	if res.Created != 2 {
		t.Fatalf("created = %d, want 2 (sample filtered by size ratio)", res.Created)
	}

	// Check the actual files exist with the right URLs.
	dune := filepath.Join(libPath, "Movies", "Dune Part Two (2024)", "Dune Part Two (2024).strm")
	body, err := os.ReadFile(dune)
	if err != nil {
		t.Fatalf("movie strm missing: %v", err)
	}
	if want := "http://gw:8097/stream/realdebrid/T1/1"; string(body) != want {
		t.Errorf("strm body = %q, want %q", body, want)
	}
	ep := filepath.Join(libPath, "Shows", "Severance", "Season 02", "Severance S02E01.strm")
	if _, err := os.Stat(ep); err != nil {
		t.Fatalf("episode strm missing: %v", err)
	}

	// Idempotent second sync does not double-create.
	res, err = w.SyncProvider(ctx, provider.RealDebrid, sampleTorrents())
	if err != nil {
		t.Fatal(err)
	}
	if res.Created != 0 || res.Removed != 0 {
		t.Errorf("second sync created=%d removed=%d", res.Created, res.Removed)
	}
	files, _ := st.ListFiles(ctx, "")
	if len(files) != 2 {
		t.Errorf("tracked files = %d", len(files))
	}
}

// Regression: a season-pack member whose own S/E marker sits at position 0
// ("s01e08 Title.mkv") parses correctly as TV on its own but with an empty
// title, since there's no text left before the marker. The rescue switch
// used to discard that correct season/episode entirely and fall back to the
// torrent-level parse, which is a bare-title Movie for an untitled torrent
// name — filing every episode as its own "movie".
func TestSyncBareEpisodeMarkerBorrowsTorrentTitle(t *testing.T) {
	w, _, libPath := testWriter(t)
	ctx := context.Background()

	torrents := []provider.Torrent{
		{
			ID: "T1", Name: "Финес и Ферб", Status: provider.StatusReady,
			Files: []provider.File{{ID: "1", Path: "Season 1/s01e08 Болван Дю Солей.mkv", SizeBytes: 500_000_000}},
		},
	}
	if _, err := w.SyncProvider(ctx, provider.RealDebrid, torrents); err != nil {
		t.Fatal(err)
	}

	want := filepath.Join(libPath, "Shows", "Финес и Ферб", "Season 01", "Финес и Ферб S01E08.strm")
	if _, err := os.Stat(want); err != nil {
		t.Errorf("rescued path missing: %s (%v)", want, err)
	}
}

// Regression: neither the filename nor the torrent name carries any TV
// signal ("Phineas and Ferb" torrent, "130 - ... The Movie - ....mp4" file),
// but the file lives under a "Season NN" folder inside the torrent — the
// only season signal that exists lives in the directory itself.
func TestSyncSeasonFolderRescue(t *testing.T) {
	w, _, libPath := testWriter(t)
	ctx := context.Background()

	torrents := []provider.Torrent{
		{
			ID: "T1", Name: "Phineas and Ferb", Status: provider.StatusReady,
			Files: []provider.File{{ID: "2", Path: "Season 03/130 - Phineas and Ferb The Movie - Across the 2nd Dimension.mp4", SizeBytes: 500_000_000}},
		},
	}
	if _, err := w.SyncProvider(ctx, provider.RealDebrid, torrents); err != nil {
		t.Fatal(err)
	}

	want := filepath.Join(libPath, "Shows", "Phineas and Ferb", "Season 03", "Phineas and Ferb S03E130.strm")
	if _, err := os.Stat(want); err != nil {
		t.Errorf("rescued path missing: %s (%v)", want, err)
	}
}

func TestSyncProviderPrunesDeleted(t *testing.T) {
	w, _, libPath := testWriter(t)
	ctx := context.Background()

	// Full cloud, then a cloud where T2 vanished.
	if _, err := w.SyncProvider(ctx, provider.RealDebrid, sampleTorrents()); err != nil {
		t.Fatal(err)
	}
	shrunk := sampleTorrents()[:1] // only T1
	res, err := w.SyncProvider(ctx, provider.RealDebrid, shrunk)
	if err != nil {
		t.Fatal(err)
	}
	if res.Removed != 1 {
		t.Fatalf("removed = %d, want 1", res.Removed)
	}
	ep := filepath.Join(libPath, "Shows", "Severance", "Season 02")
	if _, err := os.Stat(ep); !os.IsNotExist(err) {
		t.Errorf("empty show dir should be pruned, stat err = %v", err)
	}
	dune := filepath.Join(libPath, "Movies", "Dune Part Two (2024)", "Dune Part Two (2024).strm")
	if _, err := os.Stat(dune); err != nil {
		t.Errorf("surviving movie missing: %v", err)
	}
}

func TestSizeFilterSkipsSamples(t *testing.T) {
	w, st, _ := testWriter(t)
	w.sync.MinFileMB = 50 // 50MB floor
	ctx := context.Background()
	res, err := w.SyncProvider(ctx, provider.RealDebrid, sampleTorrents())
	if err != nil {
		t.Fatal(err)
	}
	if res.Created != 2 {
		t.Fatalf("created = %d, want 2 (sample filtered)", res.Created)
	}
	files, _ := st.ListFiles(ctx, "")
	for _, f := range files {
		if f.FilePath == "sample.mkv" {
			t.Error("sample.mkv should be filtered")
		}
	}
}

func TestStreamURLToken(t *testing.T) {
	w, st, _ := testWriter(t)
	_ = st
	w.token = "s3cret"
	got := w.StreamURL(provider.TorBox, "9", "5")
	if want := "http://gw:8097/stream/torbox/9/5?token=s3cret"; got != want {
		t.Errorf("url = %q want %q", got, want)
	}
}

func TestSyncRelocatesOnLayoutChange(t *testing.T) {
	w, _, libPath := testWriter(t)
	ctx := context.Background()

	// First sync: episode lands under Shows/Severance/Season 02.
	if _, err := w.SyncProvider(ctx, provider.RealDebrid, sampleTorrents()); err != nil {
		t.Fatal(err)
	}
	oldPath := filepath.Join(libPath, "Shows", "Severance", "Season 02", "Severance S02E01.strm")
	if _, err := os.Stat(oldPath); err != nil {
		t.Fatalf("initial file missing: %v", err)
	}

	// Simulate a layout change (config edit / parser fix): same file now
	// computes a different path — the old one must not linger.
	w.cfg.TVDir = "Series"
	res, err := w.SyncProvider(ctx, provider.RealDebrid, sampleTorrents())
	if err != nil {
		t.Fatal(err)
	}
	newPath := filepath.Join(libPath, "Series", "Severance", "Season 02", "Severance S02E01.strm")
	if _, err := os.Stat(newPath); err != nil {
		t.Fatalf("relocated file missing: %v", err)
	}
	if _, err := os.Stat(oldPath); !os.IsNotExist(err) {
		t.Error("old strm should be removed after relocation")
	}
	// The abandoned Shows/Severance tree should be pruned too.
	showsDir := filepath.Join(libPath, "Shows")
	if _, err := os.Stat(showsDir); !os.IsNotExist(err) {
		t.Error("empty old TV dir should be pruned")
	}
	if res.Created != 0 {
		t.Errorf("relocation is an update, created = %d", res.Created)
	}
}

func TestSyncDuplicateEpisodesGetUniquePaths(t *testing.T) {
	w, st, libPath := testWriter(t)
	ctx := context.Background()

	// Same episode from two different release groups (like the user's
	// three Tulsa King sources): they must not overwrite each other.
	mk := func(id, group string) provider.Torrent {
		name := "Tulsa.King.S01E01.1080p.AMZN.WEB-DL.DDP5.1.H.264-" + group
		return provider.Torrent{
			ID: id, Name: name, Status: provider.StatusReady,
			Files: []provider.File{{ID: "1", Path: name + ".mkv", SizeBytes: 2 << 30}},
		}
	}
	if _, err := w.SyncProvider(ctx, provider.TorBox, []provider.Torrent{mk("A", "NTb")}); err != nil {
		t.Fatal(err)
	}
	if _, err := w.SyncProvider(ctx, provider.TorBox, []provider.Torrent{mk("A", "NTb"), mk("B", "GalaxyTV")}); err != nil {
		t.Fatal(err)
	}

	files, _ := st.ListFiles(ctx, "torbox")
	paths := map[string]bool{}
	for _, f := range files {
		paths[f.StrmPath] = true
	}
	if len(files) != 2 || len(paths) != 2 {
		t.Fatalf("expected 2 rows with distinct paths, got %d rows %d paths: %+v", len(files), len(paths), files)
	}
	plain := filepath.Join(libPath, "Shows", "Tulsa King", "Season 01", "Tulsa King S01E01.strm")
	alt := filepath.Join(libPath, "Shows", "Tulsa King", "Season 01", "Tulsa King S01E01 [GalaxyTV].strm")
	if _, err := os.Stat(plain); err != nil {
		t.Errorf("plain path missing: %v", err)
	}
	if _, err := os.Stat(alt); err != nil {
		t.Errorf("alternate path missing: %v", err)
	}

	// Idempotent: a third sync keeps both files at the same paths.
	if _, err := w.SyncProvider(ctx, provider.TorBox, []provider.Torrent{mk("A", "NTb"), mk("B", "GalaxyTV")}); err != nil {
		t.Fatal(err)
	}
	files2, _ := st.ListFiles(ctx, "torbox")
	if len(files2) != 2 {
		t.Errorf("sync created duplicates: %d rows", len(files2))
	}
	for _, p := range []string{plain, alt} {
		if _, err := os.Stat(p); err != nil {
			t.Errorf("path vanished after resync: %s (%v)", p, err)
		}
	}
}

func TestSyncUsesHintOverRawFilename(t *testing.T) {
	w, st, libPath := testWriter(t)
	ctx := context.Background()

	// Two different release groups' names for the same episode — without a
	// hint these would land in two different show folders.
	torrents := []provider.Torrent{
		{
			ID: "T1", Name: "SubsPlease Frieren", Status: provider.StatusReady,
			Files: []provider.File{{ID: "1", Path: "[SubsPlease] Sousou no Frieren S2 - 04 (1080p) [698A157A].mkv", SizeBytes: 500_000_000}},
		},
	}
	if err := st.SetHint(ctx, store.Hint{
		Provider: "realdebrid", TorrentID: "T1", Kind: "tv",
		Title: "Frieren: Beyond Journey's End", Season: 2, Episode: 4,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := w.SyncProvider(ctx, provider.RealDebrid, torrents); err != nil {
		t.Fatal(err)
	}

	// sanitize() replaces the colon with a space.
	want := filepath.Join(libPath, "Shows", "Frieren Beyond Journey's End", "Season 02", "Frieren Beyond Journey's End S02E04.strm")
	if _, err := os.Stat(want); err != nil {
		t.Errorf("hinted path missing: %s (%v)", want, err)
	}
}

func TestSyncHintIgnoresJunkFilesInSameTorrent(t *testing.T) {
	w, st, libPath := testWriter(t)
	ctx := context.Background()

	// A movie torrent bundled with tiny bonus/junk files — only the actual
	// movie (the largest file) should get the hinted name; the junk files
	// must not collide with it or overwrite its STRM.
	torrents := []provider.Torrent{
		{
			ID: "T1", Name: "Harry.Potter.Collection", Status: provider.StatusReady,
			Files: []provider.File{
				{ID: "1", Path: "001 - Rollercoaster.mkv", SizeBytes: 56},
				{ID: "2", Path: "Harry.Potter.and.the.Philosophers.Stone.2001.1080p.mkv", SizeBytes: 9_000_000_000},
			},
		},
	}
	if err := st.SetHint(ctx, store.Hint{
		Provider: "realdebrid", TorrentID: "T1", Kind: "movie",
		Title: "Harry Potter and the Philosopher's Stone", Year: 2001,
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := w.SyncProvider(ctx, provider.RealDebrid, torrents); err != nil {
		t.Fatal(err)
	}

	want := filepath.Join(libPath, "Movies", "Harry Potter and the Philosopher's Stone (2001)", "Harry Potter and the Philosopher's Stone (2001).strm")
	body, err := os.ReadFile(want)
	if err != nil {
		t.Fatalf("hinted movie path missing: %v", err)
	}
	if !strings.Contains(string(body), "/realdebrid/T1/2") {
		t.Errorf("strm points at wrong file: %s", body)
	}

	files, err := st.ListFiles(ctx, "realdebrid")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 {
		t.Fatalf("expected only the real movie tracked, got %d files: %+v", len(files), files)
	}
}

func TestWriterRemoveTorrent(t *testing.T) {
	w, st, libPath := testWriter(t)
	ctx := context.Background()

	if _, err := w.SyncProvider(ctx, provider.RealDebrid, sampleTorrents()); err != nil {
		t.Fatal(err)
	}
	dune := filepath.Join(libPath, "Movies", "Dune Part Two (2024)", "Dune Part Two (2024).strm")
	if _, err := os.Stat(dune); err != nil {
		t.Fatalf("setup: dune strm missing: %v", err)
	}

	if err := w.RemoveTorrent(ctx, provider.RealDebrid, "T1"); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(dune); !os.IsNotExist(err) {
		t.Errorf("strm still present after RemoveTorrent: err=%v", err)
	}
	remaining, err := st.ListFiles(ctx, "realdebrid")
	if err != nil {
		t.Fatal(err)
	}
	for _, cf := range remaining {
		if cf.TorrentID == "T1" {
			t.Errorf("T1 file row still tracked: %+v", cf)
		}
	}
}

func TestGroupTag(t *testing.T) {
	cases := map[string]string{
		"Tulsa.King.S01E01.720p.AMZN.WEBRip.x264-GalaxyTV.mkv": "GalaxyTV",
		"Moana.2016.1080p.BluRay.DDP.7.1.x265-EDGE2020.mkv":    "EDGE2020",
		"Tulsa King (2022) - S01E01 - Go West, Old Man.mkv":    "", // episode title, not a group
		"[SubsPlease] Sousou no Frieren S2 - 04 (1080p).mkv":   "",
		"Movie.2020.mkv":                                        "",
	}
	for in, want := range cases {
		if got := groupTag(in); got != want {
			t.Errorf("groupTag(%q) = %q, want %q", in, got, want)
		}
	}
}
