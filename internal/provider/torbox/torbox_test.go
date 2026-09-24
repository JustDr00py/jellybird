package torbox

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

func writeEnvelope(w http.ResponseWriter, data any) {
	_ = json.NewEncoder(w).Encode(map[string]any{"success": true, "detail": "ok", "data": data})
}

func TestAccountInfo(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user/me" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("auth = %q", got)
		}
		writeEnvelope(w, map[string]any{
			"email": "user@example.com",
			"plan_expires_at": "2027-01-01T00:00:00Z",
		})
	})
	acct, err := c.AccountInfo(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if acct.Username != "user@example.com" || acct.PremiumUntil.IsZero() {
		t.Errorf("acct = %+v", acct)
	}
}

func TestListCloud(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/torrents/mylist" {
			t.Errorf("path = %s", r.URL.Path)
		}
		writeEnvelope(w, []map[string]any{
			{
				"id": 111, "name": "Movie.2023.1080p", "hash": "ABC123",
				"download_state": "completed", "size": 4000000000, "created_at": "2026-01-01T00:00:00Z",
				"files": []map[string]any{
					{"id": 1, "short_name": "Movie.2023.1080p.mkv", "size": 4000000000},
				},
			},
			{
				"id": 222, "name": "Still.Downloading", "hash": "DEF456",
				"download_state": "downloading",
			},
		})
	})
	torrents, err := c.ListCloud(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(torrents) != 2 {
		t.Fatalf("len = %d", len(torrents))
	}
	if torrents[0].ID != "111" || torrents[0].Status != "ready" || len(torrents[0].Files) != 1 {
		t.Errorf("first = %+v", torrents[0])
	}
	if torrents[0].Files[0].ID != "1" {
		t.Errorf("file id = %q", torrents[0].Files[0].ID)
	}
	if torrents[1].Status != "downloading" || torrents[1].Files != nil {
		t.Errorf("second = %+v", torrents[1])
	}
}

func TestAddMagnet(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/torrents/createtorrent" {
			t.Errorf("path = %s", r.URL.Path)
		}
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(r.PostForm.Get("magnet"), "magnet:") {
			t.Errorf("magnet = %q", r.PostForm.Get("magnet"))
		}
		writeEnvelope(w, map[string]any{"torrent_id": 333, "name": "New", "status": "cached"})
	})
	id, cached, err := c.AddMagnet(context.Background(), "magnet:?xt=urn:btih:hash")
	if err != nil {
		t.Fatal(err)
	}
	if id != "333" || !cached {
		t.Errorf("id=%q cached=%v", id, cached)
	}
}

func TestInstantCheck(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/torrents/checkcached") {
			t.Errorf("path = %s", r.URL.Path)
		}
		hashes := strings.Split(r.URL.Query().Get("hash"), ",")
		if len(hashes) == 0 {
			t.Error("no hashes")
		}
		// Report the first hash as cached.
		writeEnvelope(w, []map[string]any{
			{"hash": hashes[0], "torrent_id": 9, "files": []map[string]any{{"id": 5}, {"id": 6}}},
		})
	})
	res, err := c.InstantCheck(context.Background(), []string{"aaaa", "bbbb", "cccc"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 || !res[0].Cached || res[0].Hash != "aaaa" {
		t.Errorf("res = %+v", res)
	}
	if len(res[0].FileIDs) != 2 {
		t.Errorf("file ids = %v", res[0].FileIDs)
	}
}

func TestFileLink(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/torrents/requestdl" {
			t.Errorf("path = %s", r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("torrent_id") != "111" || q.Get("file_id") != "2" {
			t.Errorf("params = %v", q)
		}
		writeEnvelope(w, "https://api.torbox.app/torrentdl/STABLELINK")
	})
	link, _, err := c.FileLink(context.Background(), "111", "2")
	if err != nil {
		t.Fatal(err)
	}
	if link != "https://api.torbox.app/torrentdl/STABLELINK" {
		t.Errorf("link = %s", link)
	}
}

func TestErrorEnvelope(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"success": false, "error": "invalid api key"})
	})
	if _, err := c.AccountInfo(context.Background()); err == nil || !strings.Contains(err.Error(), "invalid api key") {
		t.Errorf("err = %v", err)
	}
}

func TestListCloudFreshBypassesCache(t *testing.T) {
	var got []string
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.URL.Query().Get("bypass_cache"))
		writeEnvelope(w, []map[string]any{})
	})
	if _, err := c.ListCloud(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListCloudFresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "false,true" {
		t.Errorf("bypass_cache = %v, want [false true]", got)
	}
}
