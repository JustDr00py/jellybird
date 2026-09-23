package strm

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"jellybird/internal/config"
	"jellybird/internal/provider"
	"jellybird/internal/store"
)

// extrasSizeRatio is the minimum fraction of a torrent's largest video file
// size a file must reach to be treated as a main feature rather than a
// bundled extra, for torrents without a hint. Bonus shorts/featurettes are
// normally well under this relative to the real feature(s); legitimate
// multi-movie box sets and season-pack episodes normally clear it.
const extrasSizeRatio = 0.3

// extrasDirs are folder names (normalized: lowercase, separators as spaces)
// that releases and Jellyfin's own conventions use for bonus material. A
// video anywhere under one of these is never a main feature, however large
// — a feature-length storyboard or making-of can easily clear
// extrasSizeRatio. "Specials" is deliberately absent: it's TV season 0.
var extrasDirs = map[string]bool{
	"extras": true, "extra": true, "bonus": true, "bonus features": true, "bonus material": true,
	"featurettes": true, "featurette": true, "behind the scenes": true, "making of": true,
	"deleted scenes": true, "interviews": true, "scenes": true, "shorts": true,
	"trailers": true, "trailer": true, "samples": true, "sample": true,
}

// extrasSuffixes are Jellyfin's "<name>-<type>" extras file-name suffixes.
var extrasSuffixes = []string{
	"-trailer", "-sample", "-featurette", "-behindthescenes", "-deleted",
	"-deletedscene", "-interview", "-scene", "-short", "-extra",
}

// isExtra reports whether a file inside a torrent is bonus material by its
// folder or file name.
func isExtra(path string) bool {
	parts := strings.Split(filepath.ToSlash(path), "/")
	norm := strings.NewReplacer(".", " ", "_", " ", "-", " ")
	for _, dir := range parts[:len(parts)-1] {
		if extrasDirs[strings.Join(strings.Fields(norm.Replace(strings.ToLower(dir))), " ")] {
			return true
		}
	}
	stem := strings.ToLower(strings.TrimSuffix(parts[len(parts)-1], filepath.Ext(path)))
	if stem == "sample" {
		return true
	}
	for _, suf := range extrasSuffixes {
		if strings.HasSuffix(stem, suf) {
			return true
		}
	}
	return false
}

// Writer manages the on-disk STRM tree and its database mapping.
type Writer struct {
	cfg   config.Library
	sync  config.Sync
	store *store.Store
	log   *slog.Logger
	// externalBase is the gateway base URL written into .strm files.
	externalBase string
	// token is appended as a query parameter when set.
	token string
}

// NewWriter builds a Writer.
func NewWriter(cfg config.Config, st *store.Store, log *slog.Logger, externalBase, token string) *Writer {
	return &Writer{
		cfg:          cfg.Library,
		sync:         cfg.Sync,
		store:        st,
		log:          log,
		externalBase: strings.TrimSuffix(externalBase, "/"),
		token:        token,
	}
}

// StreamURL returns the gateway URL for a cloud file.
func (w *Writer) StreamURL(p provider.Name, torrentID, fileID string) string {
	u := fmt.Sprintf("%s/stream/%s/%s/%s", w.externalBase, p, torrentID, fileID)
	if w.token != "" {
		u += "?token=" + w.token
	}
	return u
}

// isVideo reports whether a file should become a STRM entry.
func (w *Writer) isVideo(path string, size int64) bool {
	if isExtra(path) {
		return false
	}
	ext := strings.ToLower(filepath.Ext(path))
	allowed := false
	for _, okExt := range w.sync.VideoExtensions {
		if ext == okExt {
			allowed = true
			break
		}
	}
	if !allowed {
		return false
	}
	if w.sync.MinFileMB > 0 && size > 0 && size < w.sync.MinFileMB*1024*1024 {
		return false
	}
	if w.sync.MaxFileMB > 0 && size > 0 && size > w.sync.MaxFileMB*1024*1024 {
		return false
	}
	return true
}

// SyncResult summarizes one sync pass.
type SyncResult struct {
	Created int
	Updated int
	Removed int
}

// SyncProvider writes STRM files for one provider's cloud snapshot and prunes
// entries whose torrents disappeared. It returns the per-provider result.
func (w *Writer) SyncProvider(ctx context.Context, name provider.Name, torrents []provider.Torrent) (SyncResult, error) {
	res := SyncResult{}
	syncStart := time.Now()

	seen := make(map[string]bool) // "torrentID/fileID"

	for _, t := range torrents {
		// Only torrents with playable files map to STRM; downloading ones
		// will appear on a later sync.
		if t.Status != provider.StatusReady {
			continue
		}
		// A hint (set when this torrent was added via search) carries the
		// canonical TMDB title/season/episode, so different release groups
		// for the same show land in the same folder. search-and-add targets
		// one movie/episode, but the torrent itself may bundle extras,
		// samples or junk files alongside it — applying the hint to every
		// video file would make them all collide on the same STRM path, so
		// it's restricted to the single largest file (the actual content).
		hint, hasHint, err := w.store.GetHint(ctx, string(name), t.ID)
		if err != nil {
			w.log.Warn("hint lookup failed", "provider", name, "torrent", t.ID, "err", err)
			hasHint = false
		}
		primaryFileID := ""
		var maxVideoSize int64
		if hasHint {
			var bestSize int64 = -1
			for _, f := range t.Files {
				if w.isVideo(f.Path, f.SizeBytes) && f.SizeBytes > bestSize {
					bestSize = f.SizeBytes
					primaryFileID = f.ID
				}
			}
		} else {
			// No hint means this torrent wasn't added through jellybird's
			// search flow (pre-existing cloud content, manual adds), so
			// every video file would otherwise become its own library
			// entry. Bonus shorts/featurettes/samples bundled alongside a
			// real feature are typically a small fraction of its size, so
			// filter those out relative to the biggest file in the torrent.
			// Legitimate multi-movie/box-set files stay: they're normally
			// within the same order of magnitude of each other.
			for _, f := range t.Files {
				if w.isVideo(f.Path, f.SizeBytes) && f.SizeBytes > maxVideoSize {
					maxVideoSize = f.SizeBytes
				}
			}
		}
		for _, f := range t.Files {
			if !w.isVideo(f.Path, f.SizeBytes) {
				continue
			}
			if hasHint && f.ID != primaryFileID {
				continue // extras/sample/junk bundled with a single-title add
			}
			if !hasHint && maxVideoSize > 0 && f.SizeBytes > 0 &&
				float64(f.SizeBytes) < float64(maxVideoSize)*extrasSizeRatio {
				continue // bonus short/sample bundled with the main feature(s)
			}
			key := t.ID + "/" + f.ID
			seen[key] = true

			var parsed Parsed
			if hasHint {
				parsed = Parsed{
					Kind:      MediaKind(hint.Kind),
					Title:     hint.Title,
					ShowTitle: hint.Title,
					Year:      hint.Year,
					Season:    hint.Season,
					Episode:   hint.Episode,
				}
			} else {
				// Prefer the torrent-level release name for parsing context.
				parsed = Parse(f.Path)
				tParsed := Parse(t.Name)
				switch {
				case parsed.Kind == KindTV && parsed.Title == "" && tParsed.Title != "":
					// The file's own season/episode marker sits at the very
					// start of the name ("s01e08 Episode Title.mkv"), so
					// there's no text left over for a title. The season and
					// episode are already correct — just borrow the show
					// name from the torrent instead of discarding them by
					// falling through to the torrent-level parse wholesale.
					parsed.Title = tParsed.Title
					parsed.ShowTitle = tParsed.Title
				case parsed.Kind == KindUnknown || parsed.Title == "":
					parsed = tParsed
					if parsed.Kind == KindTV && parsed.Episode == 0 {
						parsed.Episode = EpisodeFromFileName(f.Path)
					}
				case parsed.Kind == KindMovie && tParsed.Kind == KindTV:
					// The file itself carries no S/E marker of its own
					// (season-pack torrents often just name members
					// "01.mkv"), so it read as a bare-title movie. The
					// torrent name says TV — trust that instead of filing
					// every episode as its own "movie".
					parsed = tParsed
					parsed.Episode = EpisodeFromFileName(f.Path)
				case parsed.Kind == KindMovie && tParsed.Kind == KindMovie:
					// Neither the file nor the torrent name carries any TV
					// signal, but some torrents nest absolute-numbered
					// specials/movies under a "Season NN" folder without
					// repeating the season anywhere else — the last resort.
					if season, ok := SeasonFromPath(f.Path); ok {
						show := tParsed.Title
						if show == "" {
							show = parsed.Title
						}
						parsed = Parsed{Kind: KindTV, Title: show, ShowTitle: show, Season: season}
						parsed.Episode = EpisodeFromFileName(f.Path)
					}
				}
			}

			// The file named an episode but no season ("Episode 03", season
			// 1 guessed): a season from the folder or the torrent name is
			// better evidence.
			if !hasHint && parsed.Kind == KindTV && parsed.SeasonAssumed {
				if season, ok := SeasonFromPath(f.Path); ok {
					parsed.Season = season
				} else if tParsed := Parse(t.Name); tParsed.Kind == KindTV && tParsed.Season > 0 {
					parsed.Season = tParsed.Season
				}
			}

			var relPath string
			if w.cfg.PreserveStructure {
				relPath = filepath.Join(w.cfg.MoviesDir, sanitizePath(t.Name), sanitizePath(f.Path))
				relPath = strings.TrimSuffix(relPath, filepath.Ext(relPath)) + ".strm"
			} else {
				relPath = parsed.Layout(filepath.Base(f.Path), w.cfg.MoviesDir, w.cfg.TVDir, ".strm")
			}
			absPath := w.uniquePath(ctx, string(name), t, f, filepath.Join(w.cfg.Path, relPath))

			// Layout changes (parser fixes, config edits) must not leave
			// the old .strm behind: remove it when the path moves.
			if old, err := w.store.GetFile(ctx, string(name), t.ID, f.ID); err == nil &&
				old.StrmPath != "" && old.StrmPath != absPath {
				if err := w.removeStrmFile(old.StrmPath); err != nil {
					w.log.Warn("old strm removal failed", "path", old.StrmPath, "err", err)
				} else {
					w.log.Info("relocated strm",
						"from", old.StrmPath, "to", absPath)
					res.Updated++
				}
			}

			created, err := w.store.UpsertFile(ctx, store.CloudFile{
				Provider:    string(name),
				TorrentID:   t.ID,
				TorrentName: t.Name,
				FileID:      f.ID,
				FilePath:    f.Path,
				SizeBytes:   f.SizeBytes,
				StrmPath:    absPath,
			})
			if err != nil {
				w.log.Error("store upsert failed", "provider", name, "torrent", t.ID, "file", f.ID, "err", err)
				continue
			}

			// A finished "keep local" copy replaces the .strm for good.
			if w.keepLocal(ctx, name, t.ID, f, absPath) {
				if created {
					res.Created++
				}
				continue
			}

			if err := w.writeStrm(absPath, w.StreamURL(name, t.ID, f.ID)); err != nil {
				w.log.Error("write strm failed", "path", absPath, "err", err)
				continue
			}
			if created {
				res.Created++
			} else {
				res.Updated++
			}
		}
	}

	// Prune files that vanished from this provider's cloud.
	tracked, err := w.store.ListFiles(ctx, string(name))
	if err != nil {
		return res, err
	}
	for _, cf := range tracked {
		key := cf.TorrentID + "/" + cf.FileID
		if seen[key] || cf.UpdatedAt.After(syncStart) {
			continue
		}
		if err := w.removeStrm(ctx, cf); err != nil {
			w.log.Warn("prune failed", "path", cf.StrmPath, "err", err)
			continue
		}
		res.Removed++
	}
	return res, nil
}

// uniquePath returns the library path for a file, disambiguating when a
// different torrent already claims the same path (e.g. the same episode
// added from three different release groups). Duplicate entries get a
// "[group]" suffix instead of silently overwriting each other.
func (w *Writer) uniquePath(ctx context.Context, provName string, t provider.Torrent, f provider.File, want string) string {
	if w.pathIsFree(ctx, provName, t.ID, want) {
		return want
	}
	ext := filepath.Ext(want)
	stem := strings.TrimSuffix(want, ext)
	var candidates []string
	if label := groupTag(f.Path); label != "" {
		candidates = append(candidates, stem+" ["+label+"]"+ext)
	}
	candidates = append(candidates, stem+" ["+provName+"-"+t.ID+"]"+ext)
	for _, cand := range candidates {
		if cand == want {
			continue
		}
		if w.pathIsFree(ctx, provName, t.ID, cand) {
			w.log.Info("duplicate library entry, using alternate path",
				"path", cand, "wanted", want)
			return cand
		}
	}
	return want // exhausted alternatives; last writer wins as before
}

func (w *Writer) pathIsFree(ctx context.Context, provName, torrentID, path string) bool {
	owner, err := w.store.FindByStrmPath(ctx, path)
	if errors.Is(err, store.ErrNotFound) {
		return true
	}
	if err != nil {
		return true // cannot tell; prefer progress over blocking
	}
	return owner.Provider == provName && owner.TorrentID == torrentID
}

// groupTag extracts a short release-group tag ("NTb", "GalaxyTV",
// "EDGE2020") from the final dash-separated token of a release name;
// "" when the tail is not a plausible group tag (e.g. episode titles).
func groupTag(name string) string {
	parts := strings.Split(name, "-")
	if len(parts) < 2 {
		return ""
	}
	tag := strings.TrimSuffix(strings.TrimSpace(parts[len(parts)-1]), filepath.Ext(parts[len(parts)-1]))
	if len(tag) < 2 || len(tag) > 24 {
		return ""
	}
	for _, r := range tag {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')) {
			return ""
		}
	}
	return tag
}

// writeStrm writes one .strm file (and its parents) if content changed.
func (w *Writer) writeStrm(path, url string) error {
	if existing, err := os.ReadFile(path); err == nil && string(existing) == url {
		return nil // idempotent
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir: %w", err)
	}
	if err := os.WriteFile(path, []byte(url), 0o644); err != nil {
		return fmt.Errorf("write: %w", err)
	}
	return nil
}

// removeStrmFile deletes a .strm file and prunes empty leaf directories up
// to the library root.
func (w *Writer) removeStrmFile(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	dir := filepath.Dir(path)
	for dir != w.cfg.Path && strings.HasPrefix(dir, w.cfg.Path) {
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) > 0 {
			break
		}
		if err := os.Remove(dir); err != nil {
			break
		}
		dir = filepath.Dir(dir)
	}
	return nil
}

// removeStrm deletes the STRM file and its database row. A "keep local"
// copy beside it is deliberately left in place (it only goes away via
// RemoveLocalCopy): losing the cloud copy is exactly when it matters.
func (w *Writer) removeStrm(ctx context.Context, cf store.CloudFile) error {
	if cf.StrmPath != "" {
		if err := w.removeStrmFile(cf.StrmPath); err != nil {
			return err
		}
	}
	return w.store.DeleteFile(ctx, cf.Provider, cf.TorrentID, cf.FileID)
}

// RemoveTorrent deletes the STRM files and database rows tracked for one
// torrent — used when the user manually removes it from the debrid cloud.
func (w *Writer) RemoveTorrent(ctx context.Context, name provider.Name, torrentID string) error {
	tracked, err := w.store.ListFiles(ctx, string(name))
	if err != nil {
		return err
	}
	for _, cf := range tracked {
		if cf.TorrentID != torrentID {
			continue
		}
		if err := w.removeStrm(ctx, cf); err != nil {
			return err
		}
	}
	return nil
}

// WipeLibrary deletes every tracked STRM file and its database row across
// all providers, without touching anything in the debrid cloud itself. Use
// it to force a clean resync — e.g. after a naming/parser fix — that
// regenerates every file's path from scratch instead of leaving stale
// layouts behind.
func (w *Writer) WipeLibrary(ctx context.Context) (int, error) {
	tracked, err := w.store.ListFiles(ctx, "")
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, cf := range tracked {
		if err := w.removeStrm(ctx, cf); err != nil {
			return removed, err
		}
		removed++
	}
	return removed, nil
}

// LibraryTree returns a sorted listing of STRM paths for the UI.
func (w *Writer) LibraryTree(ctx context.Context) ([]store.CloudFile, error) {
	files, err := w.store.ListFiles(ctx, "")
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].TorrentName != files[j].TorrentName {
			return files[i].TorrentName < files[j].TorrentName
		}
		return files[i].FilePath < files[j].FilePath
	})
	return files, nil
}

// sanitizePath flattens a torrent path for PreserveStructure mode.
func sanitizePath(p string) string {
	parts := strings.FieldsFunc(p, func(r rune) bool {
		return r == '/' || r == '\\'
	})
	var out []string
	for _, part := range parts {
		if s := sanitize(part); s != "" {
			out = append(out, s)
		}
	}
	return filepath.Join(out...)
}
