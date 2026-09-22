package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestDefaultsValidate(t *testing.T) {
	cfg := Defaults()
	cfg.Providers.RealDebrid.APIKey = "k"
	if err := cfg.Validate(); err != nil {
		t.Errorf("defaults with one key should validate: %v", err)
	}
}

func TestValidateRequiresProvider(t *testing.T) {
	cfg := Defaults()
	cfg.Library.Path = "/media"
	if err := cfg.Validate(); err == nil {
		t.Error("expected error with no providers")
	}
}

func TestLoadYAMLAndEnv(t *testing.T) {
	dir := t.TempDir()
	yaml := `
server:
  address: ":9001"
  token: "tok"
providers:
  realdebrid:
    api_key: "rd-key"
library:
  path: /data/media
  movies_dir: "Films"
sync:
  interval: 5m
`
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("JELLYBIRD_TORBOX_API_KEY", "tb-key")
	t.Setenv("JELLYBIRD_LIBRARY_PATH", "/other/media")

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Address != ":9001" || cfg.Server.Token != "tok" {
		t.Errorf("server = %+v", cfg.Server)
	}
	if cfg.Providers.RealDebrid.APIKey != "rd-key" {
		t.Errorf("rd key lost")
	}
	if cfg.Providers.TorBox.APIKey != "tb-key" {
		t.Error("env override for torbox key failed")
	}
	// env overrides file
	if cfg.Library.Path != "/other/media" {
		t.Errorf("env override for library.path failed: %s", cfg.Library.Path)
	}
	if cfg.Library.MoviesDir != "Films" {
		t.Errorf("movies_dir = %s", cfg.Library.MoviesDir)
	}
	if cfg.Sync.Interval != 5*time.Minute {
		t.Errorf("interval = %s", cfg.Sync.Interval)
	}
	if !cfg.Providers.TorBox.EnabledOrDefault() {
		t.Error("torbox should auto-enable with key")
	}
}

func TestEnvOnlyConfig(t *testing.T) {
	t.Setenv("JELLYBIRD_REALDEBRID_API_KEY", "k")
	t.Setenv("JELLYBIRD_LIBRARY_PATH", "/media")
	cfg, err := Load("/nonexistent/config.yaml")
	if err != nil {
		t.Fatalf("env-only should work: %v", err)
	}
	if !cfg.Providers.RealDebrid.EnabledOrDefault() {
		t.Error("realdebrid should be enabled")
	}
}

func TestWatchlistRequiresKey(t *testing.T) {
	cfg := Defaults()
	cfg.Providers.TorBox.APIKey = "k"
	cfg.Watchlist.JellyseerrURL = "http://js:5055"
	if err := cfg.Validate(); err == nil {
		t.Error("watchlist url without key should fail")
	}
}
