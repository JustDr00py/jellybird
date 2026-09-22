// Package watchlist polls Jellyseerr for approved requests and pushes them
// through the debrid search-and-add pipeline.
package watchlist

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"jellybird/internal/config"
	"jellybird/internal/debrid"
	"jellybird/internal/store"
)

// Client polls Jellyseerr.
type Client struct {
	cfg    config.Watchlist
	engine *debrid.Engine
	store  *store.Store
	log    *slog.Logger
	http   *http.Client
}

// New builds the poller.
func New(cfg config.Watchlist, engine *debrid.Engine, st *store.Store, log *slog.Logger) *Client {
	return &Client{
		cfg:    cfg,
		engine: engine,
		store:  st,
		log:    log,
		http:   &http.Client{Timeout: 30 * time.Second},
	}
}

// jellyseerrRequest is the subset of Jellyseerr's request+media payloads used.
type jellyseerrRequest struct {
	ID      int    `json:"id"`
	Status  int    `json:"status"` // 1=pending 2=approved 3=declined
	Season  int    `json:"season"`
	Episode int    `json:"episode"`
	Seasons []int  `json:"seasons"` // season-pack requests
	Media   jellyseerrMedia `json:"media"`
}

type jellyseerrMedia struct {
	ID        int    `json:"id"`
	MediaType string `json:"mediaType"` // movie|tv
	TMDBID    int    `json:"tmdbId"`
	IMDbID    string `json:"imdbId"`
}

// Run polls until ctx is cancelled.
func (c *Client) Run(ctx context.Context) {
	interval := c.cfg.Interval
	if interval < 30*time.Second {
		interval = 30 * time.Second
	}
	// Process pending work once at startup, then poll.
	c.poll(ctx)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.poll(ctx)
		}
	}
}

func (c *Client) poll(ctx context.Context) {
	reqs, err := c.fetchRequests(ctx)
	if err != nil {
		c.log.Warn("jellyseerr poll failed", "err", err)
		return
	}
	for _, r := range reqs {
		c.process(ctx, r)
	}
}

func (c *Client) fetchRequests(ctx context.Context) ([]jellyseerrRequest, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.cfg.JellyseerrURL+"/api/v1/request?take=100&sort=added", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-Api-Key", c.cfg.APIKey)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("jellyseerr HTTP %d", resp.StatusCode)
	}
	var page struct {
		Results []jellyseerrRequest `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		return nil, fmt.Errorf("decode: %w", err)
	}
	var out []jellyseerrRequest
	for _, r := range page.Results {
		// Approved and beyond; skip pending (1) and declined (3).
		if r.Status == 2 || (r.Status > 3 && r.Status != 5) {
			out = append(out, r)
		}
	}
	return out, nil
}

// process pushes one Jellyseerr request through the pipeline, tracking state
// in the store so it never double-adds.
func (c *Client) process(ctx context.Context, r jellyseerrRequest) {
	sourceID := strconv.Itoa(r.ID)
	tracked, err := c.store.ListRequests(ctx, "")
	if err == nil {
		for _, tr := range tracked {
			if tr.Source == "jellyseerr" && tr.SourceID == sourceID &&
				(tr.Status == "added" || tr.Status == "done") {
				return // already handled
			}
		}
	}

	if r.Media.IMDbID == "" {
		c.track(ctx, r, "failed", "request has no imdb id; refresh media in Jellyseerr")
		return
	}
	season := r.Season
	if season == 0 && len(r.Seasons) > 0 {
		season = r.Seasons[0]
	}

	c.track(ctx, r, "pending", "")
	res, _, err := c.engine.AddBest(ctx, r.Media.MediaType, r.Media.IMDbID, season, r.Episode)
	if err != nil {
		c.log.Warn("watchlist add failed", "request", sourceID, "err", err)
		c.track(ctx, r, "failed", err.Error())
		return
	}
	c.track(ctx, r, "added",
		fmt.Sprintf("%s torrent %s (cached=%v)", res.Provider, res.TorrentID, res.Cached))
	if c.cfg.MarkAvailable && r.Media.ID != 0 {
		if err := c.markAvailable(ctx, r); err != nil {
			c.log.Warn("mark available failed", "request", sourceID, "err", err)
		}
	}
}

func (c *Client) track(ctx context.Context, r jellyseerrRequest, status, detail string) {
	season := r.Season
	if season == 0 && len(r.Seasons) > 0 {
		season = r.Seasons[0]
	}
	err := c.store.UpsertRequest(ctx, store.Request{
		Source:    "jellyseerr",
		SourceID:  strconv.Itoa(r.ID),
		MediaType: r.Media.MediaType,
		Title:     fmt.Sprintf("%s#%d", r.Media.MediaType, r.Media.TMDBID),
		IMDbID:    r.Media.IMDbID,
		TMDBID:    strconv.Itoa(r.Media.TMDBID),
		Season:    season,
		Episode:   r.Episode,
		Status:    status,
		Detail:    detail,
	})
	if err != nil {
		c.log.Warn("track request failed", "err", err)
	}
}

// markAvailable flips Jellyseerr's media item to Available (mediaStatus=5
// in recent versions; the endpoint itself is version-stable).
func (c *Client) markAvailable(ctx context.Context, r jellyseerrRequest) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s/api/v1/media/%d/available", c.cfg.JellyseerrURL, r.Media.ID),
		strings.NewReader(`{"mediaStatus":5}`))
	if err != nil {
		return err
	}
	req.Header.Set("X-Api-Key", c.cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("jellyseerr HTTP %d", resp.StatusCode)
	}
	return nil
}
