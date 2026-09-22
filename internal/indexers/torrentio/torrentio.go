// Package torrentio searches the Torrentio Stremio addon index by IMDb ID.
// Manifest format: https://github.com/TheBeastLT/torrentio-scraper
package torrentio

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const defaultBaseURL = "https://torrentio.strem.fun"

// Stream is one torrent stream offer.
type Stream struct {
	Title    string `json:"title"` // full release name incl. source/quality
	Name     string `json:"name"`  // "4k\nWEBRip\n..."
	InfoHash string `json:"infoHash"`
	FileIdx  *int   `json:"fileIdx,omitempty"`
	// Parsed from the first Title line by the addon: "Title\nsize\nsource\n seeders\n uploader"
	SizeBytes int64
	Seeders   int
	Source    string
}

// Torrentio renders the detail line like "👤 25 💾 4.36 GB 📊 97.2%" and the
// uploader on its own line; older/other addons use plain "4.36 GB\nweb\n25".
var (
	sizeRe    = regexp.MustCompile(`(?:💾\s*)?(\d+(?:\.\d+)?)\s*(KB|MB|GB|TB)`)
	seedersRe = regexp.MustCompile(`(?:👤\s*|^\s*)(\d+)\s*(?:👤|💾|$)`)
)

// UnmarshalJSON extracts size/seeders/source from Torrentio's detail text.
func (s *Stream) UnmarshalJSON(data []byte) error {
	type rawStream struct {
		Title    string `json:"title"`
		Name     string `json:"name"`
		InfoHash string `json:"infoHash"`
		FileIdx  *int   `json:"fileIdx"`
	}
	var raw rawStream
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	s.Title = raw.Title
	s.Name = raw.Name
	s.InfoHash = strings.ToLower(raw.InfoHash)
	s.FileIdx = raw.FileIdx
	// Title: "<release name>\n<detail lines...>". First line is the name;
	// the remainder carries size/seeders/source in varying shapes.
	lines := strings.Split(raw.Title, "\n")
	if len(lines) >= 2 {
		s.Title = strings.TrimSpace(lines[0])
		detail := strings.Join(lines[1:], "\n")
		if m := sizeRe.FindStringSubmatch(detail); m != nil {
			s.SizeBytes = sizeToBytes(m[1], m[2])
		}
		if m := seedersRe.FindStringSubmatch(detail); m != nil {
			s.Seeders, _ = strconv.Atoi(m[1])
		}
		s.Source = strings.TrimSpace(lines[len(lines)-1])
		if s.SizeBytes == 0 {
			// Detail line may BE the size ("4.36 GB") with source elsewhere.
			if v, err := parseSize(detail); err == nil {
				s.SizeBytes = v
			}
		}
	}
	return nil
}

func sizeToBytes(n, unit string) int64 {
	f, _ := strconv.ParseFloat(n, 64)
	switch strings.ToUpper(unit) {
	case "KB":
		return int64(f * 1024)
	case "MB":
		return int64(f * 1024 * 1024)
	case "GB":
		return int64(f * 1024 * 1024 * 1024)
	case "TB":
		return int64(f * 1024 * 1024 * 1024 * 1024)
	}
	return 0
}

func parseSize(v string) (int64, error) {
	m := sizeRe.FindStringSubmatch(strings.TrimSpace(v))
	if m == nil {
		return 0, fmt.Errorf("bad size %q", v)
	}
	return sizeToBytes(m[1], m[2]), nil
}

// Magnet builds a magnet link for the stream.
func (s Stream) Magnet() string {
	name := strings.ReplaceAll(s.Title, "&", "%26")
	return "magnet:?xt=urn:btih:" + s.InfoHash + "&dn=" + url.PathEscape(name)
}

// Client searches Torrentio. Safe for concurrent use.
type Client struct {
	baseURL string
	http    *http.Client
}

// New builds a Torrentio client; baseURL may be empty for the public instance.
func New(baseURL string) *Client {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	return &Client{
		baseURL: strings.TrimSuffix(baseURL, "/"),
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

type addonResp struct {
	Streams []Stream `json:"streams"`
}

func (c *Client) fetch(ctx context.Context, path string) ([]Stream, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	// Cloudflare (fronting the public instance) 403s Go's default UA.
	req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("torrentio: HTTP %d for %s", resp.StatusCode, path)
	}
	var out addonResp
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("torrentio: decode: %w", err)
	}
	return out.Streams, nil
}

// SearchMovie finds streams for a movie by IMDb id (e.g. "tt0111161").
func (c *Client) SearchMovie(ctx context.Context, imdbID string) ([]Stream, error) {
	return c.fetch(ctx, "/stream/movie/"+imdbID+".json")
}

// SearchSeries finds streams for one episode by IMDb id. Torrentio's addon
// API always requires a season AND episode — there is no season-only query.
func (c *Client) SearchSeries(ctx context.Context, imdbID string, season, episode int) ([]Stream, error) {
	if season <= 0 {
		season = 1
	}
	if episode <= 0 {
		episode = 1
	}
	return c.fetch(ctx, fmt.Sprintf("/stream/series/%s:%d:%d.json", imdbID, season, episode))
}
