// Package web serves the jellybird dashboard and JSON API.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"jellybird/internal/config"
	"jellybird/internal/debrid"
	"jellybird/internal/provider"
	"jellybird/internal/store"
	"jellybird/internal/stream"
)

//go:embed templates/*.html
var templateFS embed.FS

// Deps carries everything the handlers need.
type Deps struct {
	Config    config.Config
	Store     *store.Store
	Engine    *debrid.Engine
	Resolver  *stream.Resolver
	Log       *slog.Logger
	Version   string
	SyncFlash chan struct{}
}

// Mount registers all routes on r.
func Mount(r chi.Router, d Deps) {
	h := &handlers{d: d}

	r.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": d.Version})
	})

	// Player-facing resolver (also used by media servers reading .strm).
	r.Handle("/stream/*", d.Resolver)

	// JSON API (same-origin from the UI; token-gated when configured).
	r.Route("/api", func(api chi.Router) {
		api.Use(d.apiAuth)
		api.Get("/cloud", h.cloud)
		api.Delete("/cloud", h.removeTorrent)
		api.Get("/library", h.library)
		api.Post("/sync", h.sync)
		api.Post("/library/wipe", h.wipeLibrary)
		api.Post("/library/rename", h.renameLibraryItem)
		api.Get("/library/check", h.libraryCheck)
		api.Get("/search", h.search)
		api.Get("/tv/seasons", h.tvSeasons)
		api.Get("/tv/episodes", h.tvEpisodes)
		api.Get("/torrents", h.torrents)
		api.Post("/add", h.add)
		api.Get("/requests", h.requests)
		api.Get("/accounts", h.accounts)
	})

	// Dashboard.
	r.Group(func(ui chi.Router) {
		ui.Use(d.uiAuth)
		ui.Get("/", h.index)
		ui.Get("/search", h.searchPage)
		ui.Get("/cloud", h.cloudPage)
		ui.Get("/settings", h.settingsPage)
	})
}

// --- auth helpers ---------------------------------------------------------

func (d Deps) apiAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !d.checkToken(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (d Deps) uiAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !d.checkToken(r) {
			w.Header().Set("WWW-Authenticate", `Basic realm="jellybird"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (d Deps) checkToken(r *http.Request) bool {
	if d.Config.Server.Token == "" {
		return true
	}
	// Bearer/JSON, query param, or basic-auth password.
	if r.URL.Query().Get("token") == d.Config.Server.Token {
		return true
	}
	if r.Header.Get("X-Jellybird-Token") == d.Config.Server.Token {
		return true
	}
	_, pass, ok := r.BasicAuth()
	if ok && pass == d.Config.Server.Token {
		return true
	}
	return false
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// --- handlers ------------------------------------------------------------

type handlers struct {
	d Deps
}

func (h *handlers) cloud(w http.ResponseWriter, r *http.Request) {
	providers := h.d.Engine.Providers()
	type result struct {
		name     provider.Name
		torrents []provider.Torrent
		err      error
	}
	results := make(chan result, len(providers))
	for name := range providers {
		go func(name provider.Name) {
			// Engine.ListCloud serves a short-lived cache shared with the
			// background sync, so reloading this page doesn't re-hit the
			// provider API every time.
			torrents, err := h.d.Engine.ListCloud(r.Context(), name)
			results <- result{name, torrents, err}
		}(name)
	}

	out := []map[string]any{}
	for range providers {
		res := <-results
		if res.err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]string{"error": string(res.name) + ": " + res.err.Error()})
			return
		}
		for _, t := range res.torrents {
			out = append(out, map[string]any{
				"provider": string(res.name),
				"id":       t.ID,
				"name":     t.Name,
				"status":   string(t.Status),
				"files":    len(t.Files),
				"size":     t.SizeBytes,
			})
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func (h *handlers) removeTorrent(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	providerName, torrentID := q.Get("provider"), q.Get("id")
	if providerName == "" || torrentID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "provider and id are required"})
		return
	}
	name, err := provider.ParseName(providerName)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := h.d.Engine.RemoveTorrent(r.Context(), name, torrentID); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "removed"})
}

func (h *handlers) library(w http.ResponseWriter, r *http.Request) {
	files, err := h.d.Store.ListFiles(r.Context(), r.URL.Query().Get("provider"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, files)
}

func (h *handlers) wipeLibrary(w http.ResponseWriter, r *http.Request) {
	n, err := h.d.Engine.WipeLibrary(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"removed": n})
}

// libraryCheck answers "is this TMDB title already in the library" for
// search results. type=movie ignores season/episode; type=tv requires both
// (a season-pack hint, episode 0, is treated as covering every episode).
func (h *handlers) libraryCheck(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	tmdbID := q.Get("tmdb_id")
	kind := q.Get("type")
	if tmdbID == "" || (kind != "movie" && kind != "tv") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "tmdb_id and type=movie|tv are required"})
		return
	}
	season := httpAtoiSafe(q.Get("season"))
	episode := httpAtoiSafe(q.Get("episode"))
	exists, err := h.d.Store.HintExists(r.Context(), kind, tmdbID, season, episode)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"exists": exists})
}

func (h *handlers) renameLibraryItem(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Provider  string `json:"provider"`
		TorrentID string `json:"torrent_id"`
		Title     string `json:"title"`
		Year      int    `json:"year"`
		MediaType string `json:"media_type"`
		Season    int    `json:"season"`
		Episode   int    `json:"episode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
		return
	}
	if body.Provider == "" || body.TorrentID == "" || body.Title == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "provider, torrent_id and title are required"})
		return
	}
	if _, err := provider.ParseName(body.Provider); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	kind := "movie"
	if body.MediaType == "tv" {
		kind = "tv"
	}
	hint := store.Hint{
		Provider: body.Provider, TorrentID: body.TorrentID,
		Kind: kind, Title: body.Title, Year: body.Year, Season: body.Season, Episode: body.Episode,
	}
	if err := h.d.Store.SetHint(r.Context(), hint); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	if _, err := h.d.Engine.SyncNow(r.Context()); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "renamed but resync failed: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "renamed"})
}

func (h *handlers) sync(w http.ResponseWriter, r *http.Request) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if _, err := h.d.Engine.SyncNow(ctx); err != nil {
			h.d.Log.Warn("manual sync error", "err", err)
		}
	}()
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "syncing"})
}

func (h *handlers) search(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "q is required"})
		return
	}
	results, err := h.d.Engine.SearchMeta(r.Context(), q)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, results)
}

func (h *handlers) tvSeasons(w http.ResponseWriter, r *http.Request) {
	meta := h.d.Engine.Metadata()
	if meta == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "search not configured"})
		return
	}
	tmdbID, ok := httpAtoi(r.URL.Query().Get("tmdb_id"))
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "tmdb_id is required"})
		return
	}
	seasons, err := meta.TVSeasons(r.Context(), tmdbID)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, seasons)
}

func (h *handlers) tvEpisodes(w http.ResponseWriter, r *http.Request) {
	meta := h.d.Engine.Metadata()
	if meta == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "search not configured"})
		return
	}
	q := r.URL.Query()
	tmdbID, ok := httpAtoi(q.Get("tmdb_id"))
	season, seasonOK := httpAtoi(q.Get("season"))
	if !ok || !seasonOK {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "tmdb_id and season are required"})
		return
	}
	episodes, err := meta.TVSeasonEpisodes(r.Context(), tmdbID, season)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, episodes)
}

func (h *handlers) torrents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	tmdbID := q.Get("tmdb_id")
	mediaType := q.Get("type")
	season, _ := httpAtoi(q.Get("season"))
	episode, _ := httpAtoi(q.Get("episode"))
	if tmdbID == "" || mediaType == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "tmdb_id and type are required"})
		return
	}
	// Resolve IMDb id through TMDB first.
	meta := h.d.Engine.Metadata()
	if meta == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "search not configured"})
		return
	}
	imdbID, err := meta.IMDbID(r.Context(), mediaType, httpAtoiSafe(tmdbID))
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	cands, err := h.d.Engine.SearchTorrents(r.Context(), mediaType, imdbID, season, episode)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, cands)
}

func (h *handlers) add(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Magnet    string `json:"magnet"`
		InfoHash  string `json:"info_hash"`
		Provider  string `json:"provider"`
		// Optional: the TMDB show/movie this candidate was picked for, so
		// sync can name the STRM file consistently regardless of how the
		// release group titled the actual file.
		Title     string `json:"title"`
		Year      int    `json:"year"`
		MediaType string `json:"media_type"`
		Season    int    `json:"season"`
		Episode   int    `json:"episode"`
		// Optional: lets HintExists later answer "is this TMDB title
		// already in the library" for search results. Empty when the
		// caller doesn't have (or doesn't pass) a TMDB id.
		TMDBID string `json:"tmdb_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
		return
	}
	if body.Magnet == "" || !strings.HasPrefix(body.Magnet, "magnet:") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "magnet is required"})
		return
	}
	var hint *store.Hint
	if body.Title != "" {
		kind := "movie"
		if body.MediaType == "tv" {
			kind = "tv"
		}
		hint = &store.Hint{Kind: kind, Title: body.Title, Year: body.Year, Season: body.Season, Episode: body.Episode, TMDBID: body.TMDBID}
	}
	res, err := h.d.Engine.AddMagnet(r.Context(), body.Magnet, body.InfoHash, body.Provider, hint)
	if err != nil {
		h.d.Log.Warn("add magnet failed", "title", body.Title, "provider", body.Provider, "err", err)
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, res)
}

func (h *handlers) requests(w http.ResponseWriter, r *http.Request) {
	reqs, err := h.d.Store.ListRequests(r.Context(), r.URL.Query().Get("status"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, reqs)
}

func (h *handlers) accounts(w http.ResponseWriter, r *http.Request) {
	out := []map[string]any{}
	for name, p := range h.d.Engine.Providers() {
		acct, err := p.AccountInfo(r.Context())
		entry := map[string]any{
			"provider": string(name),
			"username": acct.Username,
		}
		if err != nil {
			entry["error"] = err.Error()
		} else {
			if acct.PremiumUntil.IsZero() {
				entry["premium"] = false
			} else {
				entry["premium"] = acct.PremiumUntil.After(time.Now())
				entry["premium_until"] = acct.PremiumUntil.Format(time.RFC3339)
			}
		}
		out = append(out, entry)
	}
	lastSync, lastErr := h.d.Engine.LastSync()
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts":  out,
		"last_sync": lastSync.Format(time.RFC3339),
		"last_error": errString(lastErr),
		"version":   h.d.Version,
	})
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func httpAtoi(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
}

func httpAtoiSafe(s string) int {
	n, _ := httpAtoi(s)
	return n
}
