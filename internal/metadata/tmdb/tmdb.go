// Package tmdb is a minimal client for The Movie Database search and external
// ID lookups used to bridge user queries to indexers.
package tmdb

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const baseURL = "https://api.themoviedb.org/3"

// Client talks to TMDB. Safe for concurrent use.
type Client struct {
	apiKey   string
	language string
	baseURL  string
	http     *http.Client
}

// New builds a TMDB client (v3 API key).
func New(apiKey, language string) *Client {
	if language == "" {
		language = "en-US"
	}
	return &Client{
		apiKey:   apiKey,
		language: language,
		baseURL:  baseURL,
		http:     &http.Client{Timeout: 20 * time.Second},
	}
}

// SetBaseURL overrides the API root (self-hosted mirrors, tests).
func (c *Client) SetBaseURL(u string) { c.baseURL = strings.TrimSuffix(u, "/") }

// Result is one media search hit.
type Result struct {
	ID           int    `json:"id"`
	Title        string `json:"title"`            // movies
	Name         string `json:"name"`             // tv
	OriginalTitle string `json:"original_title"`
	OriginalName string `json:"original_name"`
	ReleaseDate  string `json:"release_date"`     // movies
	FirstAirDate string `json:"first_air_date"`   // tv
	MediaType    string `json:"media_type"`
	Overview     string `json:"overview"`
	PosterPath   string `json:"poster_path"`
}

// Year extracts the release year.
func (r Result) Year() int {
	d := r.ReleaseDate
	if d == "" {
		d = r.FirstAirDate
	}
	if len(d) >= 4 {
		var y int
		if _, err := fmt.Sscanf(d[:4], "%d", &y); err == nil {
			return y
		}
	}
	return 0
}

// DisplayTitle picks the best title field.
func (r Result) DisplayTitle() string {
	if r.Title != "" {
		return r.Title
	}
	return r.Name
}

// Search finds movies and shows matching query (multi-search).
func (c *Client) Search(ctx context.Context, query string) ([]Result, error) {
	var out struct {
		Results []Result `json:"results"`
	}
	err := c.get(ctx, "/search/multi", url.Values{
		"query":              {query},
		"include_adult":      {"false"},
		"language":           {c.language},
	}, &out)
	if err != nil {
		return nil, err
	}
	// Keep only movies and shows; filter no-person results.
	var filtered []Result
	for _, r := range out.Results {
		if r.MediaType == "movie" || r.MediaType == "tv" {
			filtered = append(filtered, r)
		}
	}
	return filtered, nil
}

// Season is one entry from a TV show's season list.
type Season struct {
	SeasonNumber int    `json:"season_number"`
	Name         string `json:"name"`
	EpisodeCount int    `json:"episode_count"`
	PosterPath   string `json:"poster_path"`
	AirDate      string `json:"air_date"`
}

// Episode is one entry from a season's episode list.
type Episode struct {
	EpisodeNumber int    `json:"episode_number"`
	Name          string `json:"name"`
	Overview      string `json:"overview"`
	StillPath     string `json:"still_path"`
	AirDate       string `json:"air_date"`
}

// TVSeasons lists a show's seasons (from its details endpoint), dropping
// season 0 ("Specials") since that's not what a browsing user expects.
func (c *Client) TVSeasons(ctx context.Context, tvID int) ([]Season, error) {
	var out struct {
		Seasons []Season `json:"seasons"`
	}
	if err := c.get(ctx, fmt.Sprintf("/tv/%d", tvID), url.Values{"language": {c.language}}, &out); err != nil {
		return nil, err
	}
	var filtered []Season
	for _, s := range out.Seasons {
		if s.SeasonNumber > 0 {
			filtered = append(filtered, s)
		}
	}
	return filtered, nil
}

// TVSeasonEpisodes lists episodes for one season of a show.
func (c *Client) TVSeasonEpisodes(ctx context.Context, tvID, season int) ([]Episode, error) {
	var out struct {
		Episodes []Episode `json:"episodes"`
	}
	path := fmt.Sprintf("/tv/%d/season/%d", tvID, season)
	if err := c.get(ctx, path, url.Values{"language": {c.language}}, &out); err != nil {
		return nil, err
	}
	return out.Episodes, nil
}

// IMDbID resolves the IMDb identifier for a TMDB movie/show id.
func (c *Client) IMDbID(ctx context.Context, mediaType string, tmdbID int) (string, error) {
	var out struct {
		IMDbID string `json:"imdb_id"`
	}
	var path string
	switch mediaType {
	case "tv":
		path = fmt.Sprintf("/tv/%d/external_ids", tmdbID)
	default:
		path = fmt.Sprintf("/movie/%d/external_ids", tmdbID)
	}
	if err := c.get(ctx, path, nil, &out); err != nil {
		return "", err
	}
	if out.IMDbID == "" {
		return "", fmt.Errorf("tmdb: no imdb id for %s %d", mediaType, tmdbID)
	}
	return out.IMDbID, nil
}

func (c *Client) get(ctx context.Context, path string, q url.Values, out any) error {
	if q == nil {
		q = url.Values{}
	}
	q.Set("api_key", c.apiKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+path+"?"+q.Encode(), nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return fmt.Errorf("tmdb: HTTP %d for %s", resp.StatusCode, path)
	}
	if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
		return fmt.Errorf("tmdb: decode %s: %w", path, err)
	}
	return nil
}
