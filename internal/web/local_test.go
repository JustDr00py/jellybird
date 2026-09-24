package web

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"jellybird/internal/store"
)

func TestDownloadServesLocalCopy(t *testing.T) {
	srv, st := newTestServer(t, true)
	ctx := t.Context()

	path := filepath.Join(t.TempDir(), "Dune Part Two (2024).mkv")
	content := strings.Repeat("0123456789", 1000)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	cf := store.CloudFile{Provider: "realdebrid", TorrentID: "T1", FileID: "1", TorrentName: "Dune", FilePath: "dune.mkv", SizeBytes: int64(len(content))}
	if _, err := st.EnqueueLocal(ctx, cf); err != nil {
		t.Fatal(err)
	}
	if err := st.SetLocalDone(ctx, "realdebrid", "T1", "1", path, int64(len(content))); err != nil {
		t.Fatal(err)
	}

	get := func(url, rng string, auth bool) *http.Response {
		req, _ := http.NewRequest("GET", srv.URL+url, nil)
		if auth {
			req.Header.Set("X-Jellybird-Token", testToken)
		}
		if rng != "" {
			req.Header.Set("Range", rng)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	resp := get("/api/download/realdebrid/T1/1", "", true)
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != content {
		t.Fatalf("download: %d, %d bytes", resp.StatusCode, len(body))
	}
	if cd := resp.Header.Get("Content-Disposition"); !strings.HasPrefix(cd, "attachment") || !strings.Contains(cd, "Dune Part Two (2024).mkv") {
		t.Fatalf("Content-Disposition = %q", cd)
	}

	resp = get("/api/download/realdebrid/T1/1", "bytes=10-19", true)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent || string(body) != "0123456789" {
		t.Fatalf("range: %d %q", resp.StatusCode, body)
	}

	for url, want := range map[string]int{
		"/api/download/realdebrid/NOPE/1": http.StatusNotFound,
		"/api/download/evilprovider/T1/1": http.StatusBadRequest,
	} {
		resp := get(url, "", true)
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("%s: %d, want %d", url, resp.StatusCode, want)
		}
	}

	resp = get("/api/download/realdebrid/T1/1", "", false)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated download: %d", resp.StatusCode)
	}

	// The listing reports the copy as the only one left (no files row).
	resp = get("/api/local", "", true)
	body, _ = io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `"in_library":false`) || !strings.Contains(string(body), `"status":"done"`) {
		t.Fatalf("local list = %s", body)
	}
}

func TestLocalPageRenders(t *testing.T) {
	srv, _ := newTestServer(t, true)
	c := newClient()
	resp, _ := c.PostForm(srv.URL+"/login", url.Values{"username": {"admin"}, "password": {"hunter2hunter2"}})
	resp.Body.Close()

	resp, err := c.Get(srv.URL + "/local")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	// The test server has no download manager: the page says so instead
	// of offering controls that would fail.
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "Downloads are disabled") {
		t.Fatalf("local page: %d\n%s", resp.StatusCode, body)
	}
}

func TestLocalPageTemplateEnabled(t *testing.T) {
	h := &handlers{d: Deps{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}}
	rec := httptest.NewRecorder()
	h.render(rec, httptest.NewRequest("GET", "/local", nil), http.StatusOK, "local.html",
		map[string]any{"Enabled": true, "FreeBytes": int64(50 << 30), "ReserveBytes": int64(10 << 30), "DownloadsPath": "/downloads"})
	body := rec.Body.String()
	if rec.Code != http.StatusOK || !strings.Contains(body, "50.0 GB") || !strings.Contains(body, "keeps 10.0 GB free") ||
		!strings.Contains(body, `id="rmsel"`) || !strings.Contains(body, `id="mvsel" data-path="/downloads"`) {
		t.Fatalf("local page: %d\n%s", rec.Code, body)
	}
}
