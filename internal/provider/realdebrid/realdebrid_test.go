package realdebrid

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func testClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	c := New("test-key", 0)
	c.baseURL = srv.URL
	return c
}

func TestAccountInfo(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("auth = %q", got)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"username": "alice", "premium": 5820653, "expiration": "2027-01-01T00:00:00Z",
		})
	})
	acct, err := c.AccountInfo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if acct.Username != "alice" || acct.PremiumUntil.IsZero() {
		t.Errorf("acct = %+v", acct)
	}
}

func TestAccountInfoFree(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"username": "bob", "premium": 0, "expiration": "",
		})
	})
	acct, err := c.AccountInfo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if acct.Username != "bob" || !acct.PremiumUntil.IsZero() {
		t.Errorf("acct = %+v", acct)
	}
}

func TestListCloud(t *testing.T) {
	// Mirrors the real API: the bulk list never carries "files" — only
	// /torrents/info/{id} does — so ListCloud must fetch that separately
	// for ready torrents.
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/torrents":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{
					"id": "12345", "hash": "ABCDEF0123", "filename": "Dune.2021.1080p.mkv",
					"status": "downloaded", "added": "1700000000", "bytes": 8000000000,
					"links": []string{"https://real-debrid.com/d/XXX"},
				},
				{
					"id": "67890", "filename": "Still.Downloading", "status": "downloading",
				},
			})
		case r.URL.Path == "/torrents/info/12345":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "12345", "hash": "ABCDEF0123", "filename": "Dune.2021.1080p.mkv",
				"status": "downloaded", "bytes": 8000000000,
				"files": []map[string]any{
					{"id": 1, "path": "/Dune.2021.1080p.mkv", "bytes": 8000000000, "selected": 1},
					{"id": 2, "path": "/sample.mkv", "bytes": 5000000, "selected": 0},
				},
			})
		default:
			t.Errorf("path = %s", r.URL.Path)
		}
	})
	torrents, err := c.ListCloud(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(torrents) != 2 {
		t.Fatalf("len = %d", len(torrents))
	}
	first := torrents[0]
	if first.ID != "12345" || first.Status != "ready" {
		t.Errorf("first = %+v", first)
	}
	if len(first.Files) != 1 { // unselected sample skipped
		t.Fatalf("files = %+v", first.Files)
	}
	if first.Files[0].Path != "Dune.2021.1080p.mkv" || first.Files[0].ID != "1" {
		t.Errorf("file = %+v", first.Files[0])
	}
	if torrents[1].Status != "downloading" {
		t.Errorf("second status = %s", torrents[1].Status)
	}
}

func TestListCloudCachesFileInfo(t *testing.T) {
	var infoCalls int
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/torrents":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": "12345", "hash": "ABCDEF0123", "filename": "Dune.2021.1080p.mkv", "status": "downloaded", "bytes": 8000000000},
			})
		case r.URL.Path == "/torrents/info/12345":
			infoCalls++
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "12345", "status": "downloaded", "bytes": 8000000000,
				"files": []map[string]any{{"id": 1, "path": "/Dune.2021.1080p.mkv", "bytes": 8000000000, "selected": 1}},
			})
		case r.URL.Path == "/torrents/delete/12345":
			// Real-Debrid rejects anything but DELETE here.
			if r.Method != http.MethodDelete {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("path = %s", r.URL.Path)
		}
	})

	if _, err := c.ListCloud(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListCloud(context.Background()); err != nil {
		t.Fatal(err)
	}
	if infoCalls != 1 {
		t.Errorf("info calls = %d, want 1 (second ListCloud should hit the cache)", infoCalls)
	}

	// Deleting the torrent must drop it from the cache.
	if err := c.Delete(context.Background(), "12345"); err != nil {
		t.Fatal(err)
	}
	c.fileMu.Lock()
	_, stillCached := c.fileCache["12345"]
	c.fileMu.Unlock()
	if stillCached {
		t.Error("file cache still has entry after Delete")
	}
}

func TestAddMagnet(t *testing.T) {
	var selected bool
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/torrents/addMagnet":
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			if !strings.HasPrefix(r.PostForm.Get("magnet"), "magnet:") {
				t.Errorf("magnet = %q", r.PostForm.Get("magnet"))
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "ABC"})
		case "/torrents/selectFiles/ABC":
			selected = true
			w.WriteHeader(http.StatusNoContent)
		case "/torrents/info/ABC":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "ABC", "filename": "Cached.Torrent", "status": "downloaded",
			})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})
	id, cached, err := c.AddMagnet(context.Background(), "magnet:?xt=urn:btih:hash&dn=X")
	if err != nil {
		t.Fatal(err)
	}
	if id != "ABC" || !cached || !selected {
		t.Errorf("id=%q cached=%v selected=%v", id, cached, selected)
	}
}

// InstantCheck is a deliberate no-op: RD disabled its instant-availability
// endpoint in late 2024, so calling it would just burn API quota on a
// request guaranteed to fail for every hash.
func TestInstantCheck(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected request to disabled RD endpoint: %s", r.URL.Path)
	})
	res, err := c.InstantCheck(context.Background(), []string{"aaaa", "bbbb"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 0 {
		t.Errorf("res = %+v, want empty", res)
	}
}

func TestFileLinkMapsSelectedFilesToLinks(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/torrents/info/T1":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "T1", "filename": "Show.S01", "status": "downloaded",
				"files": []map[string]any{
					{"id": 1, "path": "/artwork.nfo", "selected": 0},
					{"id": 2, "path": "/Show.S01E01.mkv", "selected": 1},
					{"id": 3, "path": "/Show.S01E02.mkv", "selected": 1},
				},
				"links": []string{
					"https://real-debrid.com/d/LINK1",
					"https://real-debrid.com/d/LINK2",
				},
			})
		case "/unrestrict/link":
			if err := r.ParseForm(); err != nil {
				t.Fatal(err)
			}
			link := r.PostForm.Get("link")
			direct := "https://cdn.example.com/direct2"
			if link == "https://real-debrid.com/d/LINK1" {
				direct = "https://cdn.example.com/direct1"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"download": direct, "link": link,
			})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})
	// File 3 is the second *selected* file, so it must map to LINK2.
	link, _, err := c.FileLink(context.Background(), "T1", "3")
	if err != nil {
		t.Fatal(err)
	}
	if link != "https://cdn.example.com/direct2" {
		t.Errorf("file 3 (second selected) should map to LINK2, got %s", link)
	}
}

func TestErrorEnvelope(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{"error_code": 8, "error": "bad_token"})
	})
	if _, err := c.AccountInfo(context.Background()); err == nil || !strings.Contains(err.Error(), "bad_token") {
		t.Errorf("err = %v", err)
	}
}
