// Package torbox implements provider.Provider against the TorBox API
// (https://docs.torbox.app/).
package torbox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"jellybird/internal/provider"
	"jellybird/internal/ratelimit"
)

const (
	defaultBaseURL = "https://api.torbox.app/v1/api"
	defaultRPM     = 280 // documented limit 300/min; stay under it
)

// Client talks to TorBox. It is safe for concurrent use.
type Client struct {
	apiKey  string
	baseURL string
	http    *provider.Client
}

// New builds a TorBox client with an API key from TorBox settings.
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
			UserAgent: "jellybird/0.1",
		},
	}
}

// Name implements provider.Provider.
func (c *Client) Name() provider.Name { return provider.TorBox }

// SetBaseURL overrides the API root (self-hosted proxies, tests).
func (c *Client) SetBaseURL(u string) { c.baseURL = strings.TrimSuffix(u, "/") }

// envelope is the common TorBox response wrapper.
type envelope struct {
	Success bool            `json:"success"`
	Detail  string          `json:"detail,omitempty"`
	Error   string          `json:"error,omitempty"`
	Data    json.RawMessage `json:"data"`
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, form url.Values, out any) error {
	raw := c.baseURL + path
	if len(query) > 0 {
		raw += "?" + query.Encode()
	}
	var req *http.Request
	var err error
	switch method {
	case http.MethodGet:
		req, err = http.NewRequestWithContext(ctx, method, raw, nil)
	default:
		body := strings.NewReader("")
		if form != nil {
			body = strings.NewReader(form.Encode())
		}
		req, err = http.NewRequestWithContext(ctx, method, raw, body)
	}
	if err != nil {
		return err
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(ctx, req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("torbox: HTTP %d %s", resp.StatusCode, resp.Status)
	}
	if out == nil {
		return nil
	}
	var env envelope
	dec := json.NewDecoder(resp.Body)
	if err := dec.Decode(&env); err != nil {
		return fmt.Errorf("torbox: decode response: %w", err)
	}
	if !env.Success {
		msg := env.Error
		if msg == "" {
			msg = env.Detail
		}
		return fmt.Errorf("torbox: %s", msg)
	}
	if len(env.Data) == 0 || string(env.Data) == "null" || out == nil {
		return nil
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return fmt.Errorf("torbox: decode data: %w", err)
	}
	return nil
}

type tbTorrent struct {
	// IDs arrive as JSON numbers; fileID() normalizes them to strings.
	ID        any    `json:"id"`
	Name      string `json:"name"`
	Hash      string `json:"hash"`
	Status    string `json:"download_state"`
	Size      int64  `json:"size"`
	CreatedAt string `json:"created_at"`
	Files     []struct {
		ID   any    `json:"id"`
		Name string `json:"short_name"`
		Size int64  `json:"size"`
	} `json:"files"`
}

func mapStatus(s string) provider.TorrentStatus {
	switch s {
	case "cached", "completed":
		return provider.StatusReady
	case "downloading", "queued", "metaDL", "checking", "resolving":
		return provider.StatusDownloading
	case "uploading", "seeding", "stalled":
		return provider.StatusSeeding
	case "paused", "error", "failed":
		return provider.StatusError
	}
	return provider.StatusUnknown
}

func fileID(v any) string {
	switch t := v.(type) {
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case string:
		return t
	}
	return fmt.Sprintf("%v", v)
}

// AccountInfo implements provider.Provider.
func (c *Client) AccountInfo(ctx context.Context) (provider.Account, error) {
	// Envelope data for /user/me is the user object itself.
	var u struct {
		Email       string `json:"email"`
		PlanExpires string `json:"plan_expires_at"`
	}
	if err := c.do(ctx, http.MethodGet, "/user/me", nil, nil, &u); err != nil {
		return provider.Account{}, err
	}
	acct := provider.Account{Username: u.Email}
	if t, err := time.Parse(time.RFC3339, u.PlanExpires); err == nil {
		acct.PremiumUntil = t
	}
	return acct, nil
}

// ListCloud implements provider.Provider.
func (c *Client) ListCloud(ctx context.Context) ([]provider.Torrent, error) {
	var list []tbTorrent
	// bypass_cache=false uses TorBox's own 600s cache to save quota.
	q := url.Values{"bypass_cache": {"false"}}
	if err := c.do(ctx, http.MethodGet, "/torrents/mylist", q, nil, &list); err != nil {
		return nil, err
	}
	out := make([]provider.Torrent, 0, len(list))
	for _, t := range list {
		pt := provider.Torrent{
			ID:        fileID(t.ID),
			Name:      t.Name,
			Hash:      strings.ToLower(t.Hash),
			Status:    mapStatus(t.Status),
			SizeBytes: t.Size,
		}
		if pt.Status == provider.StatusReady {
			for _, f := range t.Files {
				pt.Files = append(pt.Files, provider.File{
					ID:        fileID(f.ID),
					Path:      f.Name,
					SizeBytes: f.Size,
				})
			}
		}
		if t.CreatedAt != "" {
			if ts, err := time.Parse(time.RFC3339, t.CreatedAt); err == nil {
				pt.AddedAt = ts.UTC()
			}
		}
		out = append(out, pt)
	}
	return out, nil
}

// AddMagnet implements provider.Provider. TorBox queues instantly when the
// content is cached.
func (c *Client) AddMagnet(ctx context.Context, magnet string) (string, bool, error) {
	form := url.Values{
		"magnet":    {magnet},
		"seed":      {"3"},
		"as_queued": {"true"},
	}
	var created struct {
		TorrentID any    `json:"torrent_id"`
		Name      string `json:"name"`
		Status    string `json:"status"`
	}
	if err := c.do(ctx, http.MethodPost, "/torrents/createtorrent", nil, form, &created); err != nil {
		return "", false, err
	}
	id := fileID(created.TorrentID)
	if id == "" || id == "<nil>" {
		return "", false, fmt.Errorf("torbox: createtorrent returned no torrent id")
	}
	cached := mapStatus(created.Status) == provider.StatusReady
	return id, cached, nil
}

type checkCached struct {
	Hash       string `json:"hash"`
	TorrentID  any    `json:"torrent_id"`
	FileIDs    []struct {
		ID any `json:"id"`
	} `json:"files"`
}

// InstantCheck implements provider.Provider. TorBox accepts ~100 hashes per
// call and caches results for an hour.
func (c *Client) InstantCheck(ctx context.Context, hashes []string) ([]provider.InstantResult, error) {
	var out []provider.InstantResult
	const batch = 50
	for start := 0; start < len(hashes); start += batch {
		end := min(start+batch, len(hashes))
		q := url.Values{
			"hash":   {strings.Join(hashes[start:end], ",")},
			"format": {"object"},
		}
		var results []checkCached
		if err := c.do(ctx, http.MethodGet, "/torrents/checkcached", q, nil, &results); err != nil {
			continue // a failed batch must not sink the rest
		}
		for _, r := range results {
			ir := provider.InstantResult{Hash: strings.ToLower(r.Hash), Cached: true}
			for _, f := range r.FileIDs {
				ir.FileIDs = append(ir.FileIDs, fileID(f.ID))
			}
			out = append(out, ir)
		}
	}
	return out, nil
}

// FileLink implements provider.Provider. requestdl returns a stable CDN URL
// that redirects to the current download host.
func (c *Client) FileLink(ctx context.Context, torrentID, fileID string) (string, time.Time, error) {
	q := url.Values{
		"token":     {c.apiKey},
		"torrent_id": {torrentID},
		"file_id":   {fileID},
		"zip_link":  {"false"},
		"user_ip":   {"0.0.0.0"},
	}
	var link string
	if err := c.do(ctx, http.MethodGet, "/torrents/requestdl", q, nil, &link); err != nil {
		return "", time.Time{}, err
	}
	if link == "" {
		return "", time.Time{}, fmt.Errorf("torbox: requestdl returned empty link")
	}
	// requestdl permalinks stay valid; refresh periodically anyway.
	return link, time.Now().Add(12 * time.Hour), nil
}

// Delete implements provider.Provider via the controltorrent endpoint.
func (c *Client) Delete(ctx context.Context, torrentID string) error {
	form := url.Values{
		"torrent_id": {torrentID},
		"action":     {"delete"},
	}
	return c.do(ctx, http.MethodPost, "/torrents/controltorrent", nil, form, nil)
}
