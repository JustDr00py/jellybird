// Package provider defines the debrid-agnostic API surface jellybird relies
// on, plus shared HTTP plumbing for the concrete clients.
package provider

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"jellybird/internal/ratelimit"
)

// Name identifies a provider in URLs, the database and logs.
type Name string

const (
	RealDebrid Name = "realdebrid"
	TorBox     Name = "torbox"
)

// ParseName converts a string into a Name, erroring on unknown values.
func ParseName(s string) (Name, error) {
	switch Name(s) {
	case RealDebrid, TorBox:
		return Name(s), nil
	}
	return "", fmt.Errorf("unknown provider %q", s)
}

// File is a single playable file inside a cloud torrent.
type File struct {
	// ID is the provider's file identifier (string to fit both APIs).
	ID string
	// Path is the file name or relative path inside the torrent.
	Path string
	// SizeBytes is the file size; 0 when unknown.
	SizeBytes int64
}

// Torrent is a torrent in the provider cloud.
type Torrent struct {
	// ID is the provider torrent identifier.
	ID string
	// Name is the torrent or release name.
	Name string
	// Hash is the BTIH info hash (lowercase hex) when known.
	Hash string
	// Status is a normalized provider status.
	Status TorrentStatus
	// Files lists selectable/playable files once known.
	Files []File
	// SizeBytes is the total torrent size when known.
	SizeBytes int64
	// AddedAt is when the torrent was added to the cloud.
	AddedAt time.Time
}

// TorrentStatus is a normalized torrent lifecycle state.
type TorrentStatus string

const (
	// StatusDownloading covers downloading, queued, magnet errors etc.
	StatusDownloading TorrentStatus = "downloading"
	// StatusDownloading covers provider-side seeding/uploading states.
	StatusSeeding TorrentStatus = "seeding"
	// StatusReady means files can be streamed now.
	StatusReady TorrentStatus = "ready"
	// StatusError covers provider-side failures.
	StatusError TorrentStatus = "error"
	// StatusUnknown maps unmapped provider statuses.
	StatusUnknown TorrentStatus = "unknown"
)

// Account describes the authenticated account.
type Account struct {
	Username string
	// PremiumUntil is zero for non-premium accounts.
	PremiumUntil time.Time
}

// InstantResult reports per-hash instant availability.
type InstantResult struct {
	Hash    string
	Cached  bool
	FileIDs []string
}

// Provider is a debrid service client. All calls must be safe for concurrent
// use and honor ctx cancellation.
type Provider interface {
	// Name returns the provider identifier.
	Name() Name
	// AccountInfo fetches the account behind the configured API key.
	AccountInfo(ctx context.Context) (Account, error)
	// ListCloud returns the torrents currently in the cloud.
	ListCloud(ctx context.Context) ([]Torrent, error)
	// AddMagnet adds a magnet link. It returns the new torrent ID and whether
	// the content was already cached (instantly available).
	AddMagnet(ctx context.Context, magnet string) (id string, cached bool, err error)
	// InstantCheck reports which of the hashes are instantly available.
	InstantCheck(ctx context.Context, hashes []string) ([]InstantResult, error)
	// FileLink returns a fresh, directly playable CDN URL for one file.
	FileLink(ctx context.Context, torrentID, fileID string) (link string, expiry time.Time, err error)
	// Delete removes a torrent from the cloud.
	Delete(ctx context.Context, torrentID string) error
}

// Client is shared HTTP plumbing for provider implementations.
type Client struct {
	HTTP      *http.Client
	Limiter   *ratelimit.Limiter
	UserAgent string
}

// MaxRetries is the number of 429/5xx retries before giving up.
const MaxRetries = 4

// Do performs rate-limited HTTP requests with 429 backoff. The body returned
// must be closed by the caller.
func (c *Client) Do(ctx context.Context, req *http.Request) (*http.Response, error) {
	if req.Header.Get("User-Agent") == "" && c.UserAgent != "" {
		req.Header.Set("User-Agent", c.UserAgent)
	}
	var lastErr error
	for attempt := 0; attempt <= MaxRetries; attempt++ {
		if err := c.Limiter.Wait(ctx); err != nil {
			return nil, err
		}
		// A prior attempt consumed the body; reset it so retries re-send it.
		if attempt > 0 && req.GetBody != nil {
			b, berr := req.GetBody()
			if berr != nil {
				return nil, berr
			}
			req.Body = b
		}
		resp, err := c.HTTP.Do(req)
		if err != nil {
			lastErr = err
		} else if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode < 500 {
			return resp, nil
		} else {
			readAndClose(resp)
			lastErr = fmt.Errorf("%s %s: HTTP %d", req.Method, req.URL.Redacted(), resp.StatusCode)
			if resp.Header.Get("Retry-After") != "" && attempt == 0 {
				// fallthrough to Backoff below with parsed header
				if d, perr := parseRetryAfter(resp.Header.Get("Retry-After")); perr == nil {
					// Backoff uses jitter; Retry-After needs honoring, so sleep it directly.
					if werr := sleepCtx(ctx, d); werr != nil {
						return nil, werr
					}
					continue
				}
			}
		}
		if attempt < MaxRetries {
			if werr := ratelimit.Backoff(ctx, attempt, 0); werr != nil {
				return nil, werr
			}
		}
	}
	return nil, fmt.Errorf("giving up after %d attempts: %w", MaxRetries+1, lastErr)
}

func readAndClose(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
}

func parseRetryAfter(v string) (time.Duration, error) {
	if secs, err := strconv.Atoi(v); err == nil {
		return time.Duration(secs) * time.Second, nil
	}
	if t, err := http.ParseTime(v); err == nil {
		d := time.Until(t)
		if d > 0 {
			return d, nil
		}
		return time.Second, nil
	}
	return 0, fmt.Errorf("bad Retry-After %q", v)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d > 5*time.Minute {
		d = 5 * time.Minute
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(d):
		return nil
	}
}

// FormPOST is a helper setting a form-encoded body.
func FormPOST(ctx context.Context, c *Client, rawURL string, form url.Values, bearer string) (*http.Response, error) {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, body)
	if err != nil {
		return nil, err
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	return c.Do(ctx, req)
}

// GetJSON is a helper issuing an authorized GET.
func GetJSON(ctx context.Context, c *Client, rawURL string, bearer string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	req.Header.Set("Accept", "application/json")
	return c.Do(ctx, req)
}
