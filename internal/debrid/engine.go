// Package debrid orchestrates providers: cloud sync into STRM files and the
// search-and-add pipeline shared by the web UI and the watchlist.
package debrid

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"sync"
	"time"

	"jellybird/internal/config"
	"jellybird/internal/indexers/torrentio"
	"jellybird/internal/metadata/tmdb"
	"jellybird/internal/provider"
	"jellybird/internal/store"
	"jellybird/internal/strm"
)

// cloudCacheTTL bounds how often the periodic sync loop and the /api/cloud
// page each hit a provider's cloud-listing API. Both share one cache entry
// per provider, so back-to-back page loads (or a page load landing right
// after a sync tick) reuse the same fetch instead of doubling API traffic.
const cloudCacheTTL = 30 * time.Second

// cloudCacheEntry holds one provider's most recent cloud listing. Failed
// fetches are never cached, so a transient provider error always retries on
// the next call instead of being pinned for the TTL.
type cloudCacheEntry struct {
	torrents  []provider.Torrent
	fetchedAt time.Time
}

// Engine coordinates all providers.
type Engine struct {
	providers map[provider.Name]provider.Provider
	writer    *strm.Writer
	store     *store.Store
	log       *slog.Logger
	meta      *tmdb.Client
	indexer   *torrentio.Client
	lastSync  time.Time
	lastErr   error

	cloudMu    sync.Mutex
	cloudCache map[provider.Name]cloudCacheEntry
}

// NewEngine builds the engine.
func NewEngine(providers map[provider.Name]provider.Provider, writer *strm.Writer, st *store.Store, log *slog.Logger) *Engine {
	return &Engine{
		providers:  providers,
		writer:     writer,
		store:      st,
		log:        log,
		cloudCache: make(map[provider.Name]cloudCacheEntry),
	}
}

// EnableSearch wires the search pipeline (TMDB + Torrentio).
func (e *Engine) EnableSearch(meta *tmdb.Client, idx *torrentio.Client) {
	e.meta = meta
	e.indexer = idx
}

// Metadata exposes the TMDB client (nil when search is off).
func (e *Engine) Metadata() *tmdb.Client { return e.meta }

// SearchEnabled reports whether the search pipeline is wired.
func (e *Engine) SearchEnabled() bool { return e.meta != nil && e.indexer != nil }

// Providers exposes the enabled provider map.
func (e *Engine) Providers() map[provider.Name]provider.Provider { return e.providers }

// ListCloud returns one provider's cloud listing, serving a cached copy when
// it's fresh enough. Used by both the periodic sync and the /api/cloud page
// so neither pays for a separate round trip when the other just fetched.
func (e *Engine) ListCloud(ctx context.Context, name provider.Name) ([]provider.Torrent, error) {
	p, ok := e.providers[name]
	if !ok {
		return nil, fmt.Errorf("provider %q not enabled", name)
	}
	return e.listCloud(ctx, name, p)
}

func (e *Engine) listCloud(ctx context.Context, name provider.Name, p provider.Provider) ([]provider.Torrent, error) {
	e.cloudMu.Lock()
	entry, ok := e.cloudCache[name]
	e.cloudMu.Unlock()
	if ok && time.Since(entry.fetchedAt) < cloudCacheTTL {
		return entry.torrents, nil
	}
	torrents, err := p.ListCloud(ctx)
	if err != nil {
		return nil, err
	}
	e.cloudMu.Lock()
	e.cloudCache[name] = cloudCacheEntry{torrents: torrents, fetchedAt: time.Now()}
	e.cloudMu.Unlock()
	return torrents, nil
}

// invalidateCloudCache drops a provider's cached listing so the next read
// (sync or cloud page) fetches fresh data instead of waiting out the TTL —
// used right after an action that changes cloud state (add/delete) so it's
// reflected immediately.
func (e *Engine) invalidateCloudCache(name provider.Name) {
	e.cloudMu.Lock()
	delete(e.cloudCache, name)
	e.cloudMu.Unlock()
}

// SyncNow runs one cloud sync across all providers.
func (e *Engine) SyncNow(ctx context.Context) (strm.SyncResult, error) {
	total := strm.SyncResult{}
	for name, p := range e.providers {
		torrents, err := e.listCloud(ctx, name, p)
		if err != nil {
			e.log.Error("list cloud failed", "provider", name, "err", err)
			e.lastErr = err
			continue
		}
		res, err := e.writer.SyncProvider(ctx, name, torrents)
		if err != nil {
			e.log.Error("sync failed", "provider", name, "err", err)
			e.lastErr = err
			continue
		}
		total.Created += res.Created
		total.Updated += res.Updated
		total.Removed += res.Removed
		e.log.Info("provider synced",
			"provider", name,
			"torrents", len(torrents),
			"created", res.Created, "updated", res.Updated, "removed", res.Removed)
	}
	e.lastSync = time.Now()
	_ = e.store.SetMeta(ctx, "last_sync", e.lastSync.Format(time.RFC3339))
	if e.lastErr == nil {
		_ = e.store.SetMeta(ctx, "last_sync_error", "")
	}
	return total, e.lastErr
}

// RunSyncLoop syncs on schedule until ctx is cancelled. done receives a
// signal after each completed pass (used to wake manual sync waiters).
func (e *Engine) RunSyncLoop(ctx context.Context, cfg config.Sync, done chan<- struct{}) {
	if cfg.RunOnStart {
		if _, err := e.SyncNow(ctx); err != nil {
			e.log.Warn("initial sync had errors", "err", err)
		}
	}
	ticker := time.NewTicker(cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := e.SyncNow(ctx); err != nil {
				e.log.Warn("sync had errors", "err", err)
			}
			if done != nil {
				select {
				case done <- struct{}{}:
				default:
				}
			}
		}
	}
}

// LastSync reports the last sync time and error state for the UI.
func (e *Engine) LastSync() (time.Time, error) { return e.lastSync, e.lastErr }

// SearchCandidate is one searchable torrent result.
type SearchCandidate struct {
	Title    string `json:"title"`
	SizeBytes int64 `json:"size_bytes"`
	Seeders  int    `json:"seeders"`
	Source   string `json:"source"`
	Provider string `json:"provider"` // which debrid has it cached ("" = none yet)
	Hash     string `json:"hash"`
	Magnet   string `json:"magnet"`
	Cached   bool   `json:"cached"`
}

// SearchMeta resolves a query to media, returning options for the UI.
func (e *Engine) SearchMeta(ctx context.Context, query string) ([]tmdb.Result, error) {
	if e.meta == nil {
		return nil, errors.New("search not configured (set tmdb.api_key)")
	}
	return e.meta.Search(ctx, query)
}

// SearchTorrents finds candidates for a media item, annotating which debrid
// provider has them cached.
func (e *Engine) SearchTorrents(ctx context.Context, mediaType string, imdbID string, season, episode int) ([]SearchCandidate, error) {
	if e.indexer == nil {
		return nil, errors.New("search not configured")
	}
	var (
		streams []torrentio.Stream
		err     error
	)
	switch mediaType {
	case "tv":
		streams, err = e.indexer.SearchSeries(ctx, imdbID, season, episode)
	default:
		streams, err = e.indexer.SearchMovie(ctx, imdbID)
	}
	if err != nil {
		return nil, err
	}
	// Instant-check across every enabled provider.
	hashes := make([]string, 0, len(streams))
	byHash := map[string]*SearchCandidate{}
	cands := make([]SearchCandidate, 0, len(streams))
	for _, s := range streams {
		c := SearchCandidate{
			Title:     s.Title,
			SizeBytes: s.SizeBytes,
			Seeders:   s.Seeders,
			Source:    s.Source,
			Hash:      s.InfoHash,
			Magnet:    s.Magnet(),
		}
		cands = append(cands, c)
		if s.InfoHash != "" {
			hashes = append(hashes, s.InfoHash)
			byHash[s.InfoHash] = &cands[len(cands)-1]
		}
	}
	for name, p := range e.providers {
		results, err := p.InstantCheck(ctx, hashes)
		if err != nil {
			e.log.Warn("instant check failed", "provider", name, "err", err)
			continue
		}
		for _, r := range results {
			if r.Cached {
				if c, ok := byHash[r.Hash]; ok {
					c.Cached = true
					c.Provider = string(name)
				}
			}
		}
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].Cached != cands[j].Cached {
			return cands[i].Cached
		}
		return cands[i].Seeders > cands[j].Seeders
	})
	return cands, nil
}

// AddResult reports the outcome of adding a magnet.
type AddResult struct {
	Provider string `json:"provider"`
	TorrentID string `json:"torrent_id"`
	Cached   bool   `json:"cached"`
	Synced   bool   `json:"synced"`
}

// AddMagnet adds a magnet to the first provider that has it cached, or a
// preferred fallback provider, then syncs STRM files. hint, when non-nil,
// records the TMDB show/movie this torrent was picked for so sync can use
// that canonical title instead of parsing the raw release filename.
func (e *Engine) AddMagnet(ctx context.Context, magnet, infoHash, preferred string, hint *store.Hint) (AddResult, error) {
	// Pick target: preferred provider, else first cached, else first enabled.
	target, err := e.pickProvider(ctx, infoHash, preferred)
	if err != nil {
		return AddResult{}, err
	}
	id, cached, err := target.p.AddMagnet(ctx, magnet)
	if err != nil {
		return AddResult{}, fmt.Errorf("add to %s: %w", target.name, err)
	}
	res := AddResult{Provider: string(target.name), TorrentID: id, Cached: cached}
	e.invalidateCloudCache(target.name)
	if hint != nil {
		hint.Provider = string(target.name)
		hint.TorrentID = id
		if err := e.store.SetHint(ctx, *hint); err != nil {
			e.log.Warn("save hint failed", "provider", target.name, "torrent", id, "err", err)
		}
	}
	// If instantly available, the STRM files can be written right away.
	if cached {
		if _, err := e.SyncNow(ctx); err == nil {
			res.Synced = true
		}
	}
	return res, nil
}

// RemoveTorrent deletes a torrent from its debrid cloud and cleans up any
// STRM files/hints jellybird had written for it.
func (e *Engine) RemoveTorrent(ctx context.Context, name provider.Name, torrentID string) error {
	p, ok := e.providers[name]
	if !ok {
		return fmt.Errorf("provider %q not enabled", name)
	}
	// TorBox in particular can answer a delete with HTTP 500
	// (DATABASE_ERROR) after actually removing the torrent, so a failed
	// delete is not retried blindly: the cloud listing decides the outcome.
	if err := p.Delete(provider.WithoutServerRetries(ctx), torrentID); err != nil {
		gone, verr := torrentGone(ctx, p, torrentID)
		if verr != nil || !gone {
			e.log.Warn("cloud delete failed", "provider", name, "torrent", torrentID, "err", err, "verify_err", verr)
			return err
		}
		e.log.Info("cloud delete reported an error but the torrent is gone", "provider", name, "torrent", torrentID, "err", err)
	}
	e.invalidateCloudCache(name)
	if err := e.writer.RemoveTorrent(ctx, name, torrentID); err != nil {
		e.log.Warn("strm cleanup after delete failed", "provider", name, "torrent", torrentID, "err", err)
	}
	if err := e.store.DeleteHint(ctx, string(name), torrentID); err != nil {
		e.log.Warn("hint cleanup after delete failed", "provider", name, "torrent", torrentID, "err", err)
	}
	return nil
}

// torrentGone reports whether torrentID is absent from the provider's cloud,
// preferring an uncached listing when the provider offers one.
func torrentGone(ctx context.Context, p provider.Provider, torrentID string) (bool, error) {
	list := p.ListCloud
	if fl, ok := p.(provider.FreshLister); ok {
		list = fl.ListCloudFresh
	}
	torrents, err := list(ctx)
	if err != nil {
		return false, err
	}
	for _, t := range torrents {
		if t.ID == torrentID {
			return false, nil
		}
	}
	return true, nil
}

// WipeLibrary clears every tracked STRM file and its database row (but not
// hints, and nothing in the debrid cloud), then triggers a fresh sync so the
// library rebuilds immediately using the current parser/naming logic.
func (e *Engine) WipeLibrary(ctx context.Context) (int, error) {
	n, err := e.writer.WipeLibrary(ctx)
	if err != nil {
		return n, err
	}
	if _, err := e.SyncNow(ctx); err != nil {
		e.log.Warn("resync after library wipe failed", "err", err)
	}
	return n, nil
}

type picked struct {
	name provider.Name
	p    provider.Provider
}

func (e *Engine) pickProvider(ctx context.Context, infoHash, preferred string) (picked, error) {
	if preferred != "" {
		name, err := provider.ParseName(preferred)
		if err != nil {
			return picked{}, err
		}
		if p, ok := e.providers[name]; ok {
			return picked{name, p}, nil
		}
		return picked{}, fmt.Errorf("provider %s not enabled", preferred)
	}
	if infoHash != "" {
		for name, p := range e.providers {
			results, err := p.InstantCheck(ctx, []string{infoHash})
			if err != nil || len(results) == 0 {
				continue
			}
			if results[0].Cached {
				return picked{name, p}, nil
			}
		}
	}
	// Fallback: first enabled (deterministic order).
	for _, name := range []provider.Name{provider.RealDebrid, provider.TorBox} {
		if p, ok := e.providers[name]; ok {
			return picked{name, p}, nil
		}
	}
	return picked{}, errors.New("no provider available")
}

// AddBest searches, picks the best cached candidate (or falls back to
// best-seeded) and adds it. Used by the watchlist pipeline. tmdbID, when
// non-zero, is resolved to a canonical title/year so the resulting STRM
// lands under the same name Jellyseerr/TMDB use, instead of whatever the
// raw release name parses to.
func (e *Engine) AddBest(ctx context.Context, mediaType, imdbID string, tmdbID, season, episode int) (AddResult, SearchCandidate, error) {
	cands, err := e.SearchTorrents(ctx, mediaType, imdbID, season, episode)
	if err != nil {
		return AddResult{}, SearchCandidate{}, err
	}
	if len(cands) == 0 {
		return AddResult{}, SearchCandidate{}, errors.New("no candidates found")
	}
	best := cands[0] // sorted cached-first, then seeders

	var hint *store.Hint
	if e.meta != nil && tmdbID != 0 {
		if d, err := e.meta.Details(ctx, mediaType, tmdbID); err != nil {
			e.log.Warn("tmdb details lookup failed; falling back to release-name parsing",
				"media_type", mediaType, "tmdb_id", tmdbID, "err", err)
		} else {
			kind := "movie"
			if mediaType == "tv" {
				kind = "tv"
			}
			hint = &store.Hint{
				Kind: kind, Title: d.DisplayTitle(), Year: d.Year(),
				Season: season, Episode: episode, TMDBID: strconv.Itoa(tmdbID),
			}
		}
	}

	res, err := e.AddMagnet(ctx, best.Magnet, best.Hash, best.Provider, hint)
	return res, best, err
}
