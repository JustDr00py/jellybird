package web

import (
	"io"
	"net/http"
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
