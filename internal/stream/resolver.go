// Package stream resolves /stream/{provider}/{torrentID}/{fileID} URLs into
// fresh debrid CDN links via 302 redirects.
package stream

import (
	"context"
	"crypto/subtle"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"jellybird/internal/provider"
	"jellybird/internal/store"
)

// Resolver redirects player requests to provider CDN links.
type Resolver struct {
	providers map[provider.Name]provider.Provider
	store     *store.Store
	log       *slog.Logger
	// token gates access when non-empty.
	token string
}

// NewResolver builds a Resolver over the enabled providers.
func NewResolver(providers map[provider.Name]provider.Provider, st *store.Store, log *slog.Logger, token string) *Resolver {
	return &Resolver{providers: providers, store: st, log: log, token: token}
}

// ServeHTTP implements GET /stream/{provider}/{torrentID}/{fileID}.
func (r *Resolver) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet && req.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.token != "" && subtle.ConstantTimeCompare([]byte(req.URL.Query().Get("token")), []byte(r.token)) != 1 {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	parts := strings.Split(strings.Trim(req.URL.Path, "/"), "/")
	// parts: stream, provider, torrentID, fileID
	if len(parts) != 4 {
		http.Error(w, "expected /stream/{provider}/{torrentID}/{fileID}", http.StatusBadRequest)
		return
	}
	name, err := provider.ParseName(parts[1])
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	torrentID, fileID := parts[2], parts[3]

	link, err := r.resolve(req.Context(), name, torrentID, fileID)
	if err != nil {
		r.log.Error("resolve failed", "provider", name, "torrent", torrentID, "file", fileID, "err", err)
		http.Error(w, "unable to resolve stream: "+err.Error(), http.StatusBadGateway)
		return
	}
	// Range/seek requests are honored by the CDN itself; the redirect
	// preserves the player's Range header.
	w.Header().Set("Location", link)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusFound)
}

func (r *Resolver) resolve(ctx context.Context, name provider.Name, torrentID, fileID string) (string, error) {
	p, ok := r.providers[name]
	if !ok {
		return "", errors.New("provider not enabled")
	}
	// 1. Fresh cached link?
	if cl, err := r.store.GetLink(ctx, string(name), torrentID, fileID); err == nil {
		return cl.URL, nil
	}
	// 2. Ask the provider.
	link, expiry, err := p.FileLink(ctx, torrentID, fileID)
	if err != nil {
		return "", err
	}
	if expiry.IsZero() || expiry.Before(time.Now()) {
		expiry = time.Now().Add(time.Hour)
	}
	expiry = clampExpiry(expiry)
	if err := r.store.PutLink(ctx, string(name), torrentID, fileID, link, expiry); err != nil {
		r.log.Warn("link cache write failed", "err", err)
	}
	return link, nil
}

// Invalidate drops the cached link; called when a redirect turns out dead.
func (r *Resolver) Invalidate(ctx context.Context, name provider.Name, torrentID, fileID string) {
	if err := r.store.InvalidateLink(ctx, string(name), torrentID, fileID); err != nil {
		r.log.Warn("link invalidate failed", "err", err)
	}
}

// clampExpiry keeps TTLs in a sane window regardless of provider claims.
func clampExpiry(t time.Time) time.Time {
	min := time.Now().Add(5 * time.Minute)
	max := time.Now().Add(24 * time.Hour)
	if t.Before(min) {
		return min
	}
	if t.After(max) {
		return max
	}
	return t
}

// Resolve is the exported form used by the API and tests.
func (r *Resolver) Resolve(ctx context.Context, name provider.Name, torrentID, fileID string) (string, error) {
	return r.resolve(ctx, name, torrentID, fileID)
}
