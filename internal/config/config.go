// Package config loads jellybird configuration from YAML with environment
// variable overrides.
package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"jellybird/internal/auth"
)

// Config is the root configuration for the jellybird service.
type Config struct {
	Server     Server     `yaml:"server"`
	Providers  Providers  `yaml:"providers"`
	Library    Library    `yaml:"library"`
	Sync       Sync       `yaml:"sync"`
	TMDB       TMDB       `yaml:"tmdb"`
	Indexers   Indexers   `yaml:"indexers"`
	Watchlist  Watchlist  `yaml:"watchlist"`
	Database   Database   `yaml:"database"`
	LogLevel   string     `yaml:"log_level"`
}

// Server configures the HTTP gateway itself.
type Server struct {
	// Address to listen on, e.g. ":8097" or "0.0.0.0:8097".
	Address string `yaml:"address"`
	// ExternalURL is the base URL written into .strm files. If empty the
	// request Host header is used at write time.
	ExternalURL string `yaml:"external_url"`
	// Token protects /stream (embedded in .strm URLs) and lets non-browser
	// clients such as the Jellyfin plugin call /api via the
	// X-Jellybird-Token header. The dashboard itself uses username/password
	// login.
	Token string `yaml:"token"`
	// AdminUsername/AdminPassword optionally seed the dashboard account at
	// startup. If the user exists its password is reset to AdminPassword,
	// which doubles as lost-password recovery. Leave empty to create the
	// account through the first-run /setup page instead.
	AdminUsername string `yaml:"admin_username"`
	AdminPassword string `yaml:"admin_password"`
}

// Providers holds per-debrid-service credentials and tuning.
type Providers struct {
	RealDebrid Provider `yaml:"realdebrid"`
	TorBox     Provider `yaml:"torbox"`
}

// Provider is a single debrid service configuration.
type Provider struct {
	// Enabled toggles the provider. Defaults to enabled when an API key is set.
	Enabled *bool `yaml:"enabled,omitempty"`
	// APIKey is the private API token (env: JELLYBIRD_REALDEBRID_API_KEY / JELLYBIRD_TORBOX_API_KEY).
	APIKey string `yaml:"api_key"`
	// RequestsPerMinute caps outbound API calls; 0 uses the provider default.
	RequestsPerMinute int `yaml:"requests_per_minute"`
}

// EnabledOrDefault reports whether the provider should run.
func (p Provider) EnabledOrDefault() bool {
	if p.Enabled != nil {
		return *p.Enabled
	}
	return p.APIKey != ""
}

// Library configures where .strm files are written.
type Library struct {
	// Path is the root media directory jellybird manages.
	Path string `yaml:"path"`
	// MoviesDir and TVDir are relative to Path.
	MoviesDir string `yaml:"movies_dir"`
	TVDir     string `yaml:"tv_dir"`
	// StrmDirDepth mirrors the debrid folder structure instead of parsing
	// release names when true.
	PreserveStructure bool `yaml:"preserve_structure"`
}

// Sync configures the cloud -> STRM sync engine.
type Sync struct {
	// Interval between cloud syncs.
	Interval time.Duration `yaml:"interval"`
	// RunSyncOnStart triggers an immediate sync at boot.
	RunOnStart bool `yaml:"run_on_start"`
	// VideoExtensions filters which cloud files become .strm entries.
	VideoExtensions []string `yaml:"video_extensions"`
	// MaxFileMB skips video files larger than this (0 = unlimited).
	MaxFileMB int64 `yaml:"max_file_mb"`
	// MinFileMB skips video files smaller than this (skips samples).
	MinFileMB int64 `yaml:"min_file_mb"`
}

// TMDB configures The Movie Database metadata lookups.
type TMDB struct {
	// APIKey is a free TMDB v3 key (env: JELLYBIRD_TMDB_API_KEY).
	APIKey string `yaml:"api_key"`
	// Language for metadata, e.g. "en-US".
	Language string `yaml:"language"`
}

// Indexers configures torrent search sources.
type Indexers struct {
	Torrentio Torrentio `yaml:"torrentio"`
}

// Torrentio is the Torrentio Stremio-addon indexer.
type Torrentio struct {
	// Enabled turns on Torrentio search.
	Enabled bool `yaml:"enabled"`
	// BaseURL overrides the default public instance.
	BaseURL string `yaml:"base_url"`
}

// Watchlist configures request-stack polling.
type Watchlist struct {
	// Jellyseerr base URL, e.g. http://jellyseerr:5055.
	JellyseerrURL string `yaml:"jellyseerr_url"`
	// APIKey for the Jellyseerr API (env: JELLYBIRD_JELLYSEERR_API_KEY).
	APIKey string `yaml:"api_key"`
	// Interval between polls.
	Interval time.Duration `yaml:"interval"`
	// MarkAvailable tells Jellyseerr media is available after STRM creation.
	MarkAvailable bool `yaml:"mark_available"`
}

// Database configures the SQLite store.
type Database struct {
	// Path to the SQLite file. Empty uses "<data dir>/jellybird.db".
	Path string `yaml:"path"`
}

// Defaults returns a Config populated with sane defaults.
func Defaults() Config {
	moviesDir := "Movies"
	tvDir := "Shows"
	return Config{
		Server: Server{
			Address: ":8097",
		},
		Library: Library{
			Path:      "/media",
			MoviesDir: moviesDir,
			TVDir:     tvDir,
		},
		Sync: Sync{
			Interval:      10 * time.Minute,
			RunOnStart:    true,
			VideoExtensions: []string{".mkv", ".mp4", ".avi", ".m4v", ".mov", ".wmv", ".flv", ".webm", ".mpg", ".mpeg", ".ts"},
			MinFileMB:     50,
		},
		TMDB: TMDB{
			Language: "en-US",
		},
		Indexers: Indexers{
			Torrentio: Torrentio{
				Enabled: true,
			},
		},
		Watchlist: Watchlist{
			Interval: 2 * time.Minute,
		},
		LogLevel: "info",
	}
}

// Load reads the YAML file at path, applies defaults, then environment
// overrides, and validates the result.
func Load(path string) (Config, error) {
	cfg := Defaults()
	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			// A missing file is fine when env vars carry the config.
			if !errors.Is(err, os.ErrNotExist) {
				return cfg, fmt.Errorf("read config: %w", err)
			}
		} else if err := yaml.Unmarshal(data, &cfg); err != nil {
			return cfg, fmt.Errorf("parse config %s: %w", path, err)
		}
	}
	applyEnv(&cfg)
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

var envOverrides = []struct {
	env   string
	apply func(*Config, string)
}{
	{"JELLYBIRD_SERVER_ADDRESS", func(c *Config, v string) { c.Server.Address = v }},
	{"JELLYBIRD_SERVER_EXTERNAL_URL", func(c *Config, v string) { c.Server.ExternalURL = v }},
	{"JELLYBIRD_SERVER_TOKEN", func(c *Config, v string) { c.Server.Token = v }},
	{"JELLYBIRD_ADMIN_USERNAME", func(c *Config, v string) { c.Server.AdminUsername = v }},
	{"JELLYBIRD_ADMIN_PASSWORD", func(c *Config, v string) { c.Server.AdminPassword = v }},
	{"JELLYBIRD_REALDEBRID_API_KEY", func(c *Config, v string) { c.Providers.RealDebrid.APIKey = v }},
	{"JELLYBIRD_TORBOX_API_KEY", func(c *Config, v string) { c.Providers.TorBox.APIKey = v }},
	{"JELLYBIRD_LIBRARY_PATH", func(c *Config, v string) { c.Library.Path = v }},
	{"JELLYBIRD_TMDB_API_KEY", func(c *Config, v string) { c.TMDB.APIKey = v }},
	{"JELLYBIRD_JELLYSEERR_URL", func(c *Config, v string) { c.Watchlist.JellyseerrURL = v }},
	{"JELLYBIRD_JELLYSEERR_API_KEY", func(c *Config, v string) { c.Watchlist.APIKey = v }},
	{"JELLYBIRD_DATABASE_PATH", func(c *Config, v string) { c.Database.Path = v }},
	{"JELLYBIRD_LOG_LEVEL", func(c *Config, v string) { c.LogLevel = v }},
}

func applyEnv(cfg *Config) {
	for _, o := range envOverrides {
		if v, ok := os.LookupEnv(o.env); ok && v != "" {
			o.apply(cfg, v)
		}
	}
}

// Validate checks that the configuration is usable.
func (c *Config) Validate() error {
	var errs []error
	if c.Server.Address == "" {
		errs = append(errs, errors.New("server.address is required"))
	}
	if c.Library.Path == "" {
		errs = append(errs, errors.New("library.path is required"))
	}
	if !c.Providers.RealDebrid.EnabledOrDefault() && !c.Providers.TorBox.EnabledOrDefault() {
		errs = append(errs, errors.New("at least one provider (realdebrid or torbox) needs an api_key"))
	}
	if (c.Server.AdminUsername == "") != (c.Server.AdminPassword == "") {
		errs = append(errs, errors.New("server.admin_username and server.admin_password must be set together"))
	} else if c.Server.AdminUsername != "" {
		if err := auth.ValidateUsername(c.Server.AdminUsername); err != nil {
			errs = append(errs, fmt.Errorf("server.admin_username: %w", err))
		}
		if err := auth.ValidatePassword(c.Server.AdminPassword); err != nil {
			errs = append(errs, fmt.Errorf("server.admin_password: %w", err))
		}
	}
	if c.Sync.Interval < 30*time.Second {
		errs = append(errs, errors.New("sync.interval must be at least 30s to respect API rate limits"))
	}
	if c.Watchlist.JellyseerrURL != "" && c.Watchlist.APIKey == "" {
		errs = append(errs, errors.New("watchlist.api_key is required when watchlist.jellyseerr_url is set"))
	}
	if len(errs) > 0 {
		msgs := make([]string, len(errs))
		for i, e := range errs {
			msgs[i] = e.Error()
		}
		return fmt.Errorf("invalid configuration: %s", strings.Join(msgs, "; "))
	}
	return nil
}

// HasWatchlist reports whether Jellyseerr polling is configured.
func (c *Config) HasWatchlist() bool {
	return c.Watchlist.JellyseerrURL != "" && c.Watchlist.APIKey != ""
}

// HasSearch reports whether the search pipeline can run.
func (c *Config) HasSearch() bool {
	return c.TMDB.APIKey != "" && c.Indexers.Torrentio.Enabled
}
