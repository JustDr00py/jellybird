package stream

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"jellybird/internal/provider"
	"jellybird/internal/store"
)

type fakeProvider struct {
	linkCall int
}

func (f *fakeProvider) Name() provider.Name { return provider.TorBox }
func (f *fakeProvider) AccountInfo(ctx context.Context) (provider.Account, error) {
	return provider.Account{}, nil
}
func (f *fakeProvider) ListCloud(ctx context.Context) ([]provider.Torrent, error) { return nil, nil }
func (f *fakeProvider) AddMagnet(ctx context.Context, m string) (string, bool, error) {
	return "", false, nil
}
func (f *fakeProvider) InstantCheck(ctx context.Context, h []string) ([]provider.InstantResult, error) {
	return nil, nil
}
func (f *fakeProvider) FileLink(ctx context.Context, torrentID, fileID string) (string, time.Time, error) {
	f.linkCall++
	return "https://cdn.example.com/file.mkv", time.Now().Add(time.Hour), nil
}
func (f *fakeProvider) Delete(ctx context.Context, id string) error { return nil }

func testResolver(t *testing.T, token string) (*Resolver, *fakeProvider, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	fp := &fakeProvider{}
	provs := map[provider.Name]provider.Provider{provider.TorBox: fp}
	return NewResolver(provs, st, slog.New(slog.DiscardHandler), token), fp, st
}

func TestRedirectHappyPath(t *testing.T) {
	r, fp, _ := testResolver(t, "")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/stream/torbox/123/456", nil)
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("code = %d", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "https://cdn.example.com/file.mkv" {
		t.Errorf("location = %q", loc)
	}
	if fp.linkCall != 1 {
		t.Errorf("provider calls = %d", fp.linkCall)
	}
}

func TestLinkCacheAvoidsRefetch(t *testing.T) {
	r, fp, _ := testResolver(t, "")
	for i := 0; i < 3; i++ {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/stream/torbox/123/456", nil))
		if rec.Code != http.StatusFound {
			t.Fatalf("code = %d", rec.Code)
		}
	}
	if fp.linkCall != 1 {
		t.Errorf("provider calls = %d, want 1 (cached)", fp.linkCall)
	}
}

func TestRangeHeaderPassthrough(t *testing.T) {
	r, _, _ := testResolver(t, "")
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/stream/torbox/123/456", nil)
	req.Header.Set("Range", "bytes=0-1000")
	r.ServeHTTP(rec, req)
	// The redirect itself is 302; Range handling happens at the CDN.
	if rec.Code != http.StatusFound {
		t.Fatalf("code = %d", rec.Code)
	}
}

func TestTokenAuth(t *testing.T) {
	r, _, _ := testResolver(t, "s3cret")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/stream/torbox/123/456", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no-token code = %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/stream/torbox/123/456?token=s3cret", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("with-token code = %d", rec.Code)
	}
}

func TestBadPaths(t *testing.T) {
	r, _, _ := testResolver(t, "")
	for _, path := range []string{"/stream/", "/stream/unknownp/1/2", "/stream/torbox/onlytwo"} {
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code == http.StatusFound {
			t.Errorf("path %q should not redirect", path)
		}
	}
}
