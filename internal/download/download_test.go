package download

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"jellybird/internal/config"
	"jellybird/internal/provider"
	"jellybird/internal/store"
	"jellybird/internal/strm"
)

func init() {
	retryBackoff = 10 * time.Millisecond
	progressInterval = 10 * time.Millisecond
}

// fakeLinks hands out the CDN URL; the first `stale` resolutions return a
// link that the CDN rejects, like an expired debrid link.
type fakeLinks struct {
	good, bad   string
	stale       atomic.Int32
	invalidated atomic.Int32
}

func (f *fakeLinks) Resolve(context.Context, provider.Name, string, string) (string, error) {
	if f.stale.Add(-1) >= 0 {
		return f.bad, nil
	}
	return f.good, nil
}
func (f *fakeLinks) Invalidate(context.Context, provider.Name, string, string) { f.invalidated.Add(1) }

type env struct {
	st      *store.Store
	w       *strm.Writer
	m       *Manager
	links   *fakeLinks
	lib     string
	content []byte
	rmu     sync.Mutex
	ranges  []string
	torrent provider.Torrent
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := config.Defaults()
	cfg.Library.Path = filepath.Join(dir, "media")
	cfg.Sync.MinFileMB = 0
	log := slog.New(slog.DiscardHandler)
	w := strm.NewWriter(cfg, st, log, "http://gw:8097", "")

	e := &env{st: st, w: w, lib: cfg.Library.Path, content: bytes.Repeat([]byte("jellybird!"), 50_000)}
	cdn := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/expired" {
			http.Error(rw, "expired", http.StatusForbidden)
			return
		}
		e.rmu.Lock()
		e.ranges = append(e.ranges, r.Header.Get("Range"))
		e.rmu.Unlock()
		http.ServeContent(rw, r, "", time.Time{}, bytes.NewReader(e.content))
	}))
	t.Cleanup(cdn.Close)
	e.links = &fakeLinks{good: cdn.URL + "/file", bad: cdn.URL + "/expired"}
	e.m = New(st, w, e.links, cfg.Downloads, log)
	e.m.freeBytes = func(string) (int64, bool) { return 1 << 50, true }

	e.torrent = provider.Torrent{
		ID: "T1", Name: "Dune.Part.Two.2024.1080p", Status: provider.StatusReady,
		Files: []provider.File{{ID: "1", Path: "Dune.Part.Two.2024.1080p.mkv", SizeBytes: int64(len(e.content))}},
	}
	e.sync(t, e.torrent)
	return e
}

func (e *env) sync(t *testing.T, ts ...provider.Torrent) {
	t.Helper()
	if _, err := e.w.SyncProvider(context.Background(), provider.RealDebrid, ts); err != nil {
		t.Fatal(err)
	}
}

func (e *env) strmPath() string {
	return filepath.Join(e.lib, "Movies", "Dune Part Two (2024)", "Dune Part Two (2024).strm")
}
func (e *env) localPath() string { return strings.TrimSuffix(e.strmPath(), ".strm") + ".mkv" }

func (e *env) enqueue(t *testing.T) {
	t.Helper()
	cf, err := e.st.GetFile(context.Background(), "realdebrid", "T1", "1")
	if err != nil {
		t.Fatal(err)
	}
	if n, err := e.m.Enqueue(context.Background(), []store.CloudFile{cf}); err != nil || n != 1 {
		t.Fatalf("enqueue = %d, %v", n, err)
	}
}

// runUntil runs the manager until the entry reaches a terminal state.
func (e *env) runUntil(t *testing.T, want string) store.LocalFile {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.m.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		lf, err := e.st.GetLocal(context.Background(), "realdebrid", "T1", "1")
		if err == nil && (lf.Status == store.LocalDone || lf.Status == store.LocalFailed) {
			if lf.Status != want {
				t.Fatalf("status = %s (%s), want %s", lf.Status, lf.Error, want)
			}
			return lf
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("download did not finish")
	return store.LocalFile{}
}

func TestKeepLocalLifecycle(t *testing.T) {
	e := newEnv(t)
	ctx := context.Background()
	if _, err := os.Stat(e.strmPath()); err != nil {
		t.Fatalf("strm missing before download: %v", err)
	}

	e.enqueue(t)
	lf := e.runUntil(t, store.LocalDone)

	if lf.LocalPath != e.localPath() {
		t.Fatalf("local path = %q, want %q", lf.LocalPath, e.localPath())
	}
	got, err := os.ReadFile(e.localPath())
	if err != nil || !bytes.Equal(got, e.content) {
		t.Fatalf("local copy content mismatch (err %v)", err)
	}
	if _, err := os.Stat(e.strmPath()); !os.IsNotExist(err) {
		t.Fatal(".strm should be replaced by the local copy")
	}
	if entries, _ := os.ReadDir(filepath.Join(e.lib, tempDirName)); len(entries) != 0 {
		t.Fatalf("temp dir not cleaned: %v", entries)
	}

	// Later syncs must not write the .strm back next to the local copy.
	e.sync(t, e.torrent)
	if _, err := os.Stat(e.strmPath()); !os.IsNotExist(err) {
		t.Fatal("sync recreated .strm over a local copy")
	}

	// Gone from the cloud: the tracked row goes, the local copy stays.
	e.sync(t)
	if _, err := e.st.GetFile(ctx, "realdebrid", "T1", "1"); err == nil {
		t.Fatal("file row should be pruned")
	}
	if _, err := os.Stat(e.localPath()); err != nil {
		t.Fatalf("local copy deleted when the torrent left the cloud: %v", err)
	}

	// Removing it deletes the file and its now-empty folder.
	if err := e.m.Remove(ctx, "realdebrid", "T1", "1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Dir(e.localPath())); !os.IsNotExist(err) {
		t.Fatal("local copy / folder not removed")
	}
}

func TestRemoveLocalRestoresStrm(t *testing.T) {
	e := newEnv(t)
	e.enqueue(t)
	e.runUntil(t, store.LocalDone)
	if err := e.m.Remove(context.Background(), "realdebrid", "T1", "1"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(e.localPath()); !os.IsNotExist(err) {
		t.Fatal("local copy not deleted")
	}
	body, err := os.ReadFile(e.strmPath())
	if err != nil || string(body) != "http://gw:8097/stream/realdebrid/T1/1" {
		t.Fatalf("strm not restored: %q %v", body, err)
	}
}

func TestResumesPartialDownload(t *testing.T) {
	e := newEnv(t)
	half := len(e.content) / 2
	if err := os.MkdirAll(e.m.tmpDir, 0o755); err != nil {
		t.Fatal(err)
	}
	lf := store.LocalFile{Provider: "realdebrid", TorrentID: "T1", FileID: "1"}
	if err := os.WriteFile(e.m.tempPath(lf), e.content[:half], 0o644); err != nil {
		t.Fatal(err)
	}
	e.enqueue(t)
	e.runUntil(t, store.LocalDone)
	got, _ := os.ReadFile(e.localPath())
	if !bytes.Equal(got, e.content) {
		t.Fatal("resumed file corrupt")
	}
	e.rmu.Lock()
	defer e.rmu.Unlock()
	if want := "bytes=" + strconv.Itoa(half) + "-"; len(e.ranges) != 1 || e.ranges[0] != want {
		t.Fatalf("expected one request with Range %q, got %q", want, e.ranges)
	}
}

func TestExpiredLinkIsRefreshed(t *testing.T) {
	e := newEnv(t)
	e.links.stale.Store(1)
	e.enqueue(t)
	e.runUntil(t, store.LocalDone)
	if e.links.invalidated.Load() != 1 {
		t.Fatalf("invalidated = %d, want 1", e.links.invalidated.Load())
	}
}

func TestRefusesWhenDiskIsFull(t *testing.T) {
	e := newEnv(t)
	e.m.freeBytes = func(string) (int64, bool) { return 1 << 30, true } // 1 GB free, 5 GB reserve
	e.enqueue(t)
	lf := e.runUntil(t, store.LocalFailed)
	if !strings.Contains(lf.Error, "not enough disk space") {
		t.Fatalf("error = %q", lf.Error)
	}
	if _, err := os.Stat(e.strmPath()); err != nil {
		t.Fatal("failed download must leave the .strm in place")
	}
	// Queuing again retries a failed entry.
	e.m.freeBytes = func(string) (int64, bool) { return 1 << 50, true }
	e.enqueue(t)
	e.runUntil(t, store.LocalDone)
}

func TestLocalCopyFollowsLayoutChanges(t *testing.T) {
	e := newEnv(t)
	e.enqueue(t)
	e.runUntil(t, store.LocalDone)

	// Simulate a copy that sits where an older parser put it.
	old := filepath.Join(e.lib, "Movies", "Old Name", "Old Name.mkv")
	if err := os.MkdirAll(filepath.Dir(old), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(e.localPath(), old); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := e.st.SetLocalPath(ctx, "realdebrid", "T1", "1", old); err != nil {
		t.Fatal(err)
	}

	e.sync(t, e.torrent)
	if _, err := os.Stat(e.localPath()); err != nil {
		t.Fatalf("local copy not moved to the current layout: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(old)); !os.IsNotExist(err) {
		t.Fatal("old folder not pruned")
	}
	lf, _ := e.st.GetLocal(ctx, "realdebrid", "T1", "1")
	if lf.LocalPath != e.localPath() {
		t.Fatalf("stored path = %q", lf.LocalPath)
	}
}

func TestTempPathIsSanitized(t *testing.T) {
	m := &Manager{tmpDir: "/lib/.jellybird-downloads"}
	got := m.tempPath(store.LocalFile{Provider: "realdebrid", TorrentID: "../../etc", FileID: "a/b"})
	if filepath.Dir(got) != "/lib/.jellybird-downloads" {
		t.Fatalf("temp path escaped: %q", got)
	}
}
