// Package realdebrid implements provider.Provider against the Real-Debrid
// REST API (https://api-docs.real-debrid.com/).
package realdebrid

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"jellybird/internal/provider"
	"jellybird/internal/ratelimit"
)

const (
	defaultBaseURL = "https://api.real-debrid.com/rest/1.0"
	defaultRPM     = 240 // documented limit is 250/min; stay under it
	providerAgent  = "jellybird/0.1 (debrid gateway for jellyfin/emby)"
)

// Client talks to Real-Debrid. It is safe for concurrent use.
type Client struct {
	apiKey  string
	baseURL string
	http    *provider.Client

	// fileMu/fileCache cache per-torrent file listings. RD's bulk list
	// endpoint never carries file data (only /torrents/info/{id} does), and
	// a ready torrent's files never change, so caching them avoids one
	// extra rate-limited API call per ready torrent on every page load.
	fileMu    sync.Mutex
	fileCache map[string][]provider.File
}

// New builds a Real-Debrid client with an API token from
// https://real-debrid.com/apitoken.
func New(apiKey string, rpm int) *Client {
	if rpm <= 0 {
		rpm = defaultRPM
	}
	return &Client{
		apiKey:  apiKey,
		baseURL: defaultBaseURL,
		http: &provider.Client{
			HTTP:      &http.Client{Timeout: 60 * time.Second},
			Limiter:   ratelimit.New(rpm),
			UserAgent: providerAgent,
		},
		fileCache: make(map[string][]provider.File),
	}
}

// Name implements provider.Provider.
func (c *Client) Name() provider.Name { return provider.RealDebrid }

// SetBaseURL overrides the API root (self-hosted proxies, tests).
func (c *Client) SetBaseURL(u string) { c.baseURL = strings.TrimSuffix(u, "/") }

// apiError is the RD error envelope.
type apiError struct {
	ErrorCode int    `json:"error_code"`
	Error     string `json:"error"`
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	resp, err := provider.GetJSON(ctx, c.http, c.baseURL+path, c.apiKey)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return decode(resp, out)
}

func (c *Client) post(ctx context.Context, path string, form url.Values, out any) error {
	resp, err := provider.FormPOST(ctx, c.http, c.baseURL+path, form, c.apiKey)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	return decode(resp, out)
}

func decode(resp *http.Response, out any) error {
	if resp.StatusCode == http.StatusNoContent {
		return nil
	}
	dec := json.NewDecoder(resp.Body)
	if resp.StatusCode >= 400 {
		var apiErr apiError
		if err := dec.Decode(&apiErr); err == nil && apiErr.Error != "" {
			return fmt.Errorf("realdebrid: HTTP %d (code %d): %s", resp.StatusCode, apiErr.ErrorCode, apiErr.Error)
		}
		return fmt.Errorf("realdebrid: HTTP %d", resp.StatusCode)
	}
	if out == nil {
		return nil
	}
	if err := dec.Decode(out); err != nil {
		return fmt.Errorf("realdebrid: decode response: %w", err)
	}
	return nil
}

type user struct {
	Username   string `json:"username"`
	Premium    int    `json:"premium"` // seconds of premium remaining, 0 if none
	Expiration string `json:"expiration"`
}

// AccountInfo implements provider.Provider.
func (c *Client) AccountInfo(ctx context.Context) (provider.Account, error) {
	var u user
	if err := c.get(ctx, "/user", &u); err != nil {
		return provider.Account{}, err
	}
	acct := provider.Account{Username: u.Username}
	if u.Premium > 0 && u.Expiration != "" {
		if t, err := time.Parse(time.RFC3339, u.Expiration); err == nil {
			acct.PremiumUntil = t
		}
	}
	return acct, nil
}

type torrent struct {
	ID      string `json:"id"`
	Hash    string `json:"hash"`
	Filename string `json:"filename"`
	Status  string `json:"status"`
	Added   string `json:"added"`
	Bytes   int64  `json:"bytes"`
	Links   []string `json:"links"`
	Files   []struct {
		ID   int    `json:"id"`
		Path string `json:"path"`
		Bytes int64 `json:"bytes"`
		Selected int `json:"selected"`
	} `json:"files"`
}

func mapStatus(s string) provider.TorrentStatus {
	switch s {
	case "magnet_error", "error", "virus_error", "dead_torrent":
		return provider.StatusError
	case "downloaded":
		return provider.StatusReady
	case "downloading", "magnet_conversion", "queued", "checking":
		return provider.StatusDownloading
	case "uploading", "stalled":
		return provider.StatusSeeding
	}
	return provider.StatusUnknown
}

// ListCloud implements provider.Provider. RD returns full file listings only
// for torrents whose files were already selected, which is what we play.
//
// RD caps each page at 5000 and does not document sort order, so a library
// over one page could silently drop a just-added torrent from every sync if
// we only fetched page 1 — page through everything instead.
func (c *Client) ListCloud(ctx context.Context) ([]provider.Torrent, error) {
	const pageSize = 5000
	var raw []torrent
	for page := 1; ; page++ {
		var batch []torrent
		path := fmt.Sprintf("/torrents?limit=%d&page=%d", pageSize, page)
		if err := c.get(ctx, path, &batch); err != nil {
			return nil, err
		}
		raw = append(raw, batch...)
		if len(batch) < pageSize {
			break
		}
	}
	out := make([]provider.Torrent, len(raw))
	for i, t := range raw {
		out[i] = provider.Torrent{
			ID:        t.ID,
			Name:      t.Filename,
			Hash:      strings.ToLower(t.Hash),
			Status:    mapStatus(t.Status),
			SizeBytes: t.Bytes,
		}
		if t.Added != "" {
			if secs, err := strconv.ParseInt(t.Added, 10, 64); err == nil {
				out[i].AddedAt = time.Unix(secs, 0).UTC()
			}
		}
	}

	// The bulk list endpoint never carries per-file data (RD only returns
	// "files" from /torrents/info/{id}); fetch it for torrents that are
	// actually ready, since that's the only case the sync writer uses files
	// for. Cached, since a ready torrent's files never change — without
	// this, an account with 100+ ready torrents means 100+ rate-limited API
	// calls on every single page load. The cache is empty right after
	// startup, so fetches for uncached torrents run concurrently (bounded,
	// and still throttled overall by the shared rate limiter) instead of one
	// full network round-trip at a time — otherwise a large library's first
	// load after a restart can take tens of seconds.
	const fileFetchConcurrency = 20
	sem := make(chan struct{}, fileFetchConcurrency)
	var wg sync.WaitGroup
	for i, t := range raw {
		if mapStatus(t.Status) != provider.StatusReady {
			continue
		}
		c.fileMu.Lock()
		cached, ok := c.fileCache[t.ID]
		c.fileMu.Unlock()
		if ok {
			out[i].Files = cached
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, id string) {
			defer wg.Done()
			defer func() { <-sem }()
			full, err := c.info(ctx, id)
			if err != nil {
				return
			}
			c.fileMu.Lock()
			c.fileCache[id] = full.Files
			c.fileMu.Unlock()
			out[i].Files = full.Files
		}(i, t.ID)
	}
	wg.Wait()
	return out, nil
}

type addResp struct {
	ID string `json:"id"`
}

// AddMagnet implements provider.Provider.
func (c *Client) AddMagnet(ctx context.Context, magnet string) (string, bool, error) {
	form := url.Values{"magnet": {magnet}}
	var ar addResp
	if err := c.post(ctx, "/torrents/addMagnet", form, &ar); err != nil {
		return "", false, err
	}
	if ar.ID == "" {
		return "", false, fmt.Errorf("realdebrid: addMagnet returned no id")
	}
	// Select all files; cached torrents become ready immediately.
	if err := c.post(ctx, "/torrents/selectFiles/"+ar.ID, url.Values{"files": {"all"}}, nil); err != nil {
		return ar.ID, false, err
	}
	// Discover whether it was instant; probe failure here is not fatal —
	// the sync engine will pick the status up later.
	cached := false
	if info, err := c.info(ctx, ar.ID); err == nil {
		cached = info.Status == provider.StatusReady
	}
	return ar.ID, cached, nil
}

func (c *Client) info(ctx context.Context, id string) (provider.Torrent, error) {
	var t torrent
	if err := c.get(ctx, "/torrents/info/"+id, &t); err != nil {
		return provider.Torrent{}, err
	}
	pt := provider.Torrent{
		ID:        t.ID,
		Name:      t.Filename,
		Hash:      strings.ToLower(t.Hash),
		Status:    mapStatus(t.Status),
		SizeBytes: t.Bytes,
	}
	for _, f := range t.Files {
		if f.Selected == 0 {
			continue
		}
		pt.Files = append(pt.Files, provider.File{
			ID:        strconv.Itoa(f.ID),
			Path:      strings.TrimPrefix(f.Path, "/"),
			SizeBytes: f.Bytes,
		})
	}
	return pt, nil
}

// InstantCheck implements provider.Provider. Real-Debrid permanently
// disabled its instant-availability endpoint in late 2024 under
// anti-piracy pressure (it now returns error_code 37 "disabled_endpoint"
// for every call, undocumented in their current API reference), so there
// is no way to ask RD which hashes are cached anymore. Returning no
// results immediately avoids burning API quota on a call that is
// guaranteed to fail for every hash, once per search; callers already
// treat an empty result as "cached status unknown" and fall back
// accordingly (SearchTorrents just won't mark RD candidates cached,
// pickProvider skips to its next fallback).
func (c *Client) InstantCheck(ctx context.Context, hashes []string) ([]provider.InstantResult, error) {
	return nil, nil
}

type unrestrict struct {
	Download string `json:"download"`
	Link     string `json:"link"`
}

// FileLink implements provider.Provider. RD torrents expose per-file download
// URLs (the links[] array, ordered like the selected files) that must be
// unrestricted into a direct CDN link.
func (c *Client) FileLink(ctx context.Context, torrentID, fileID string) (string, time.Time, error) {
	var t torrent
	if err := c.get(ctx, "/torrents/info/"+torrentID, &t); err != nil {
		return "", time.Time{}, err
	}
	// links[] corresponds to selected files only: walk files counting
	// selected ones until we hit the requested file.
	linkIdx := -1
	selected := 0
	for _, f := range t.Files {
		if f.Selected == 0 {
			continue
		}
		if strconv.Itoa(f.ID) == fileID {
			linkIdx = selected
			break
		}
		selected++
	}
	if linkIdx < 0 || linkIdx >= len(t.Links) {
		return "", time.Time{}, fmt.Errorf("realdebrid: file %s not playable on torrent %s", fileID, torrentID)
	}
	form := url.Values{"link": {t.Links[linkIdx]}}
	var ur unrestrict
	if err := c.post(ctx, "/unrestrict/link", form, &ur); err != nil {
		return "", time.Time{}, err
	}
	if ur.Download == "" {
		return "", time.Time{}, fmt.Errorf("realdebrid: unrestrict returned no download link")
	}
	// RD direct links are valid ~4h; refetch a bit before.
	return ur.Download, time.Now().Add(3 * time.Hour), nil
}

// Delete implements provider.Provider.
func (c *Client) Delete(ctx context.Context, torrentID string) error {
	// Real-Debrid only accepts the DELETE method here; a POST is rejected
	// with 403 wrong_parameter.
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		c.baseURL+"/torrents/delete/"+url.PathEscape(torrentID), nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	resp, err := c.http.Do(ctx, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// 404 means it's already gone, which is what the caller wanted.
	if resp.StatusCode >= 400 && resp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("realdebrid: delete HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	c.fileMu.Lock()
	delete(c.fileCache, torrentID)
	c.fileMu.Unlock()
	return nil
}
