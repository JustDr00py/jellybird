// Command jellybird is the debrid-to-media-server gateway.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"

	"jellybird/internal/auth"
	"jellybird/internal/config"
	"jellybird/internal/debrid"
	"jellybird/internal/indexers/torrentio"
	"jellybird/internal/metadata/tmdb"
	"jellybird/internal/provider"
	"jellybird/internal/provider/realdebrid"
	"jellybird/internal/provider/torbox"
	"jellybird/internal/store"
	"jellybird/internal/stream"
	"jellybird/internal/strm"
	"jellybird/internal/watchlist"
	"jellybird/internal/web"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "jellybird:", err)
		os.Exit(1)
	}
}

func run() error {
	configPath := flag.String("config", "config.yaml", "path to config.yaml")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		return err
	}

	level := slog.LevelInfo
	switch cfg.LogLevel {
	case "debug":
		level = slog.LevelDebug
	case "warn":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)

	log.Info("jellybird starting", "version", version, "library", cfg.Library.Path)

	// Database.
	dbPath := cfg.Database.Path
	if dbPath == "" {
		dbPath = filepath.Join("data", "jellybird.db")
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o755); err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}
	st, err := store.Open(dbPath)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer st.Close()

	if err := seedAdmin(context.Background(), st, cfg.Server, log); err != nil {
		return fmt.Errorf("seed admin account: %w", err)
	}
	if n, err := st.CountUsers(context.Background()); err == nil && n == 0 {
		log.Warn("no dashboard account yet: open /setup in a browser to create one")
	}

	// Providers (base URLs overridable for self-hosted proxies/tests).
	providers := map[provider.Name]provider.Provider{}
	if cfg.Providers.RealDebrid.EnabledOrDefault() {
		rd := realdebrid.New(cfg.Providers.RealDebrid.APIKey, cfg.Providers.RealDebrid.RequestsPerMinute)
		if v := os.Getenv("JELLYBIRD_REALDEBRID_URL"); v != "" {
			rd.SetBaseURL(v)
		}
		providers[provider.RealDebrid] = rd
		log.Info("realdebrid enabled")
	}
	if cfg.Providers.TorBox.EnabledOrDefault() {
		tb := torbox.New(cfg.Providers.TorBox.APIKey, cfg.Providers.TorBox.RequestsPerMinute)
		if v := os.Getenv("JELLYBIRD_TORBOX_URL"); v != "" {
			tb.SetBaseURL(v)
		}
		providers[provider.TorBox] = tb
		log.Info("torbox enabled")
	}
	if len(providers) == 0 {
		return errors.New("no provider enabled: set realdebrid.api_key or torbox.api_key")
	}

	// Gateway base URL baked into .strm files.
	externalBase := cfg.Server.ExternalURL
	if externalBase == "" {
		externalBase = "http://127.0.0.1" + cfg.Server.Address
	}

	writer := strm.NewWriter(cfg, st, log, externalBase, cfg.Server.Token)
	resolver := stream.NewResolver(providers, st, log, cfg.Server.Token)

	// Debrid engine: cloud sync + search + add pipeline.
	engine := debrid.NewEngine(providers, writer, st, log)
	if cfg.HasSearch() {
		meta := tmdb.New(cfg.TMDB.APIKey, cfg.TMDB.Language)
		idx := torrentio.New(cfg.Indexers.Torrentio.BaseURL)
		engine.EnableSearch(meta, idx)
		log.Info("search pipeline enabled (tmdb + torrentio)")
	} else {
		log.Warn("search disabled: set tmdb.api_key to enable")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// Cloud sync loop.
	syncDone := make(chan struct{}, 1)
	go engine.RunSyncLoop(ctx, cfg.Sync, syncDone)

	// Watchlist loop.
	if cfg.HasWatchlist() {
		wl := watchlist.New(cfg.Watchlist, engine, st, log)
		go wl.Run(ctx)
		log.Info("jellyseerr watchlist enabled", "url", cfg.Watchlist.JellyseerrURL)
	}

	// HTTP server.
	r := chi.NewRouter()
	web.Mount(r, web.Deps{
		Config:    cfg,
		Store:     st,
		Engine:    engine,
		Resolver:  resolver,
		Log:       log,
		Version:   version,
		SyncFlash: syncDone,
	})
	srv := &http.Server{
		Addr:              cfg.Server.Address,
		Handler:           r,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		log.Info("listening", "address", cfg.Server.Address)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case <-ctx.Done():
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// seedAdmin creates the configured admin account, or resets its password if
// it already exists (lost-password recovery).
func seedAdmin(ctx context.Context, st *store.Store, srv config.Server, log *slog.Logger) error {
	if srv.AdminUsername == "" {
		return nil
	}
	hash, err := auth.HashPassword(srv.AdminPassword)
	if err != nil {
		return err
	}
	u, err := st.GetUserByName(ctx, srv.AdminUsername)
	switch {
	case errors.Is(err, store.ErrNotFound):
		if _, err := st.CreateUser(ctx, srv.AdminUsername, hash); err != nil {
			return err
		}
		log.Info("dashboard admin account created from config", "user", srv.AdminUsername)
		return nil
	case err != nil:
		return err
	}
	if auth.VerifyPassword(u.PasswordHash, srv.AdminPassword) {
		return nil
	}
	if err := st.SetPassword(ctx, u.ID, hash); err != nil {
		return err
	}
	log.Info("dashboard admin password reset from config", "user", u.Username)
	return nil
}
