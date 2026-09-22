// Package store persists cloud snapshots, stream link cache and watchlist
// state in SQLite.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// likeEscape escapes SQLite LIKE wildcards in user input so a search term
// like "50%" or "a_b" is matched literally.
func likeEscape(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

// Store wraps the SQLite database.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the SQLite database at path.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // modernc/sqlite is happiest single-writer
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS files (
			provider      TEXT NOT NULL,
			torrent_id    TEXT NOT NULL,
			torrent_name  TEXT NOT NULL,
			file_id       TEXT NOT NULL,
			file_path     TEXT NOT NULL,
			size_bytes    INTEGER NOT NULL DEFAULT 0,
			strm_path     TEXT NOT NULL DEFAULT '',
			created_at    INTEGER NOT NULL,
			updated_at    INTEGER NOT NULL,
			PRIMARY KEY (provider, torrent_id, file_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_files_strm ON files(strm_path)`,
		`CREATE TABLE IF NOT EXISTS link_cache (
			provider   TEXT NOT NULL,
			torrent_id TEXT NOT NULL,
			file_id    TEXT NOT NULL,
			url        TEXT NOT NULL,
			expires_at INTEGER NOT NULL,
			PRIMARY KEY (provider, torrent_id, file_id)
		)`,
		`CREATE TABLE IF NOT EXISTS requests (
			source      TEXT NOT NULL,          -- e.g. "jellyseerr"
			source_id   TEXT NOT NULL,          -- request id
			media_type  TEXT NOT NULL,          -- movie|tv
			title       TEXT NOT NULL,
			year        INTEGER NOT NULL DEFAULT 0,
			imdb_id     TEXT NOT NULL DEFAULT '',
			tmdb_id     TEXT NOT NULL DEFAULT '',
			season      INTEGER NOT NULL DEFAULT 0,
			episode     INTEGER NOT NULL DEFAULT 0,
			status      TEXT NOT NULL,          -- pending|added|failed|done
			detail      TEXT NOT NULL DEFAULT '',
			updated_at  INTEGER NOT NULL,
			PRIMARY KEY (source, source_id)
		)`,
		`CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS hints (
			provider   TEXT NOT NULL,
			torrent_id TEXT NOT NULL,
			kind       TEXT NOT NULL,
			title      TEXT NOT NULL,
			year       INTEGER NOT NULL DEFAULT 0,
			season     INTEGER NOT NULL DEFAULT 0,
			episode    INTEGER NOT NULL DEFAULT 0,
			PRIMARY KEY (provider, torrent_id)
		)`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	return nil
}

// CloudFile is a tracked cloud file and its STRM mapping.
type CloudFile struct {
	Provider    string
	TorrentID   string
	TorrentName string
	FileID      string
	FilePath    string
	SizeBytes   int64
	StrmPath    string
	UpdatedAt   time.Time
}

// UpsertFile inserts or updates a cloud file record and returns true when the
// record was created (new content).
func (s *Store) UpsertFile(ctx context.Context, f CloudFile) (created bool, err error) {
	var exists bool
	err = s.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM files WHERE provider = ? AND torrent_id = ? AND file_id = ?)`,
		f.Provider, f.TorrentID, f.FileID).Scan(&exists)
	if err != nil {
		return false, err
	}
	now := time.Now().Unix()
	if exists {
		_, err = s.db.ExecContext(ctx, `
			UPDATE files SET torrent_name = ?, file_path = ?, size_bytes = ?, strm_path = ?, updated_at = ?
			WHERE provider = ? AND torrent_id = ? AND file_id = ?`,
			f.TorrentName, f.FilePath, f.SizeBytes, f.StrmPath, now, f.Provider, f.TorrentID, f.FileID)
		return false, err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO files (provider, torrent_id, torrent_name, file_id, file_path, size_bytes, strm_path, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		f.Provider, f.TorrentID, f.TorrentName, f.FileID, f.FilePath, f.SizeBytes, f.StrmPath, now, now)
	return true, err
}

// SetStrmPath records the STRM location chosen for a file.
func (s *Store) SetStrmPath(ctx context.Context, provider, torrentID, fileID, strmPath string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE files SET strm_path = ?, updated_at = ? WHERE provider = ? AND torrent_id = ? AND file_id = ?`,
		strmPath, time.Now().Unix(), provider, torrentID, fileID)
	return err
}

// StaleFiles lists tracked files whose provider/torrent pair disappeared from
// the cloud since the given sync started.
func (s *Store) StaleFiles(ctx context.Context, since time.Time) ([]CloudFile, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT provider, torrent_id, torrent_name, file_id, file_path, size_bytes, strm_path, updated_at
		FROM files WHERE updated_at < ?`, since.Unix())
	if err != nil {
		return nil, err
	}
	return scanFiles(rows)
}

// DeleteFile removes one tracked file.
func (s *Store) DeleteFile(ctx context.Context, provider, torrentID, fileID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM files WHERE provider = ? AND torrent_id = ? AND file_id = ?`,
		provider, torrentID, fileID)
	return err
}

// Hint is metadata captured when a torrent was added via search (its TMDB
// title/season/episode), so sync can use it instead of blindly re-parsing
// the raw release filename — different release groups for the same show
// often use different, inconsistent titles.
type Hint struct {
	Provider  string
	TorrentID string
	Kind      string // "movie" | "tv"
	Title     string
	Year      int
	Season    int
	Episode   int
}

// SetHint records (or replaces) the hint for one torrent.
func (s *Store) SetHint(ctx context.Context, h Hint) error {
	var exists bool
	err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM hints WHERE provider = ? AND torrent_id = ?)`,
		h.Provider, h.TorrentID).Scan(&exists)
	if err != nil {
		return err
	}
	if exists {
		_, err = s.db.ExecContext(ctx, `
			UPDATE hints SET kind = ?, title = ?, year = ?, season = ?, episode = ?
			WHERE provider = ? AND torrent_id = ?`,
			h.Kind, h.Title, h.Year, h.Season, h.Episode, h.Provider, h.TorrentID)
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO hints (provider, torrent_id, kind, title, year, season, episode)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		h.Provider, h.TorrentID, h.Kind, h.Title, h.Year, h.Season, h.Episode)
	return err
}

// GetHint looks up the hint for one torrent, if any.
func (s *Store) GetHint(ctx context.Context, provider, torrentID string) (Hint, bool, error) {
	var h Hint
	err := s.db.QueryRowContext(ctx, `
		SELECT provider, torrent_id, kind, title, year, season, episode
		FROM hints WHERE provider = ? AND torrent_id = ?`, provider, torrentID).
		Scan(&h.Provider, &h.TorrentID, &h.Kind, &h.Title, &h.Year, &h.Season, &h.Episode)
	if errors.Is(err, sql.ErrNoRows) {
		return Hint{}, false, nil
	}
	if err != nil {
		return Hint{}, false, err
	}
	return h, true, nil
}

// DeleteHint removes the hint recorded for one torrent, if any.
func (s *Store) DeleteHint(ctx context.Context, provider, torrentID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM hints WHERE provider = ? AND torrent_id = ?`, provider, torrentID)
	return err
}

// ListFiles returns all tracked files, optionally filtered by provider.
func (s *Store) ListFiles(ctx context.Context, provider string) ([]CloudFile, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if provider == "" {
		rows, err = s.db.QueryContext(ctx, `
			SELECT provider, torrent_id, torrent_name, file_id, file_path, size_bytes, strm_path, updated_at
			FROM files ORDER BY torrent_name, file_path`)
	} else {
		rows, err = s.db.QueryContext(ctx, `
			SELECT provider, torrent_id, torrent_name, file_id, file_path, size_bytes, strm_path, updated_at
			FROM files WHERE provider = ? ORDER BY torrent_name, file_path`, provider)
	}
	if err != nil {
		return nil, err
	}
	return scanFiles(rows)
}

// ListFilesPage returns one page of tracked files plus the total count,
// optionally filtered by provider and/or a case-insensitive substring match
// against the torrent name or file path. Use ListFiles (unpaginated) instead
// when the full set is actually needed, e.g. sync/prune/wipe.
func (s *Store) ListFilesPage(ctx context.Context, provider, query string, limit, offset int) ([]CloudFile, int, error) {
	var conds []string
	var args []any
	if provider != "" {
		conds = append(conds, "provider = ?")
		args = append(args, provider)
	}
	if query != "" {
		conds = append(conds, "(torrent_name LIKE ? ESCAPE '\\' OR file_path LIKE ? ESCAPE '\\')")
		like := "%" + likeEscape(query) + "%"
		args = append(args, like, like)
	}
	where := ""
	if len(conds) > 0 {
		where = " WHERE " + strings.Join(conds, " AND ")
	}

	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM files`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	q := `SELECT provider, torrent_id, torrent_name, file_id, file_path, size_bytes, strm_path, updated_at FROM files` +
		where + ` ORDER BY torrent_name, file_path LIMIT ? OFFSET ?`
	qargs := append(append([]any{}, args...), limit, offset)
	rows, err := s.db.QueryContext(ctx, q, qargs...)
	if err != nil {
		return nil, 0, err
	}
	files, err := scanFiles(rows)
	if err != nil {
		return nil, 0, err
	}
	return files, total, nil
}

// FindByStrmPath resolves a STRM path back to a cloud file.
func (s *Store) FindByStrmPath(ctx context.Context, strmPath string) (CloudFile, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT provider, torrent_id, torrent_name, file_id, file_path, size_bytes, strm_path, updated_at
		FROM files WHERE strm_path = ? LIMIT 1`, strmPath)
	if err != nil {
		return CloudFile{}, err
	}
	files, err := scanFiles(rows)
	if err != nil {
		return CloudFile{}, err
	}
	if len(files) == 0 {
		return CloudFile{}, ErrNotFound
	}
	return files[0], nil
}

// GetFile fetches a single file by provider coordinates.
func (s *Store) GetFile(ctx context.Context, providerName, torrentID, fileID string) (CloudFile, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT provider, torrent_id, torrent_name, file_id, file_path, size_bytes, strm_path, updated_at
		FROM files WHERE provider = ? AND torrent_id = ? AND file_id = ?`, providerName, torrentID, fileID)
	if err != nil {
		return CloudFile{}, err
	}
	files, err := scanFiles(rows)
	if err != nil {
		return CloudFile{}, err
	}
	if len(files) == 0 {
		return CloudFile{}, ErrNotFound
	}
	return files[0], nil
}

func scanFiles(rows *sql.Rows) ([]CloudFile, error) {
	defer rows.Close()
	var out []CloudFile
	for rows.Next() {
		var f CloudFile
		var updated int64
		if err := rows.Scan(&f.Provider, &f.TorrentID, &f.TorrentName, &f.FileID, &f.FilePath, &f.SizeBytes, &f.StrmPath, &updated); err != nil {
			return nil, err
		}
		f.UpdatedAt = time.Unix(updated, 0).UTC()
		out = append(out, f)
	}
	return out, rows.Err()
}

// CachedLink is a cached CDN link.
type CachedLink struct {
	URL       string
	ExpiresAt time.Time
}

// GetLink returns a cached link if still fresh.
func (s *Store) GetLink(ctx context.Context, providerName, torrentID, fileID string) (CachedLink, error) {
	var cl CachedLink
	var exp int64
	err := s.db.QueryRowContext(ctx,
		`SELECT url, expires_at FROM link_cache WHERE provider = ? AND torrent_id = ? AND file_id = ?`,
		providerName, torrentID, fileID).Scan(&cl.URL, &exp)
	if errors.Is(err, sql.ErrNoRows) {
		return cl, ErrNotFound
	}
	if err != nil {
		return cl, err
	}
	cl.ExpiresAt = time.Unix(exp, 0).UTC()
	if time.Now().After(cl.ExpiresAt) {
		return cl, ErrNotFound
	}
	return cl, nil
}

// PutLink caches a CDN link with its expiry.
func (s *Store) PutLink(ctx context.Context, providerName, torrentID, fileID, url string, expires time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO link_cache (provider, torrent_id, file_id, url, expires_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(provider, torrent_id, file_id) DO UPDATE SET url = excluded.url, expires_at = excluded.expires_at`,
		providerName, torrentID, fileID, url, expires.Unix())
	return err
}

// InvalidateLink drops a cached link (e.g. after a redirect failure).
func (s *Store) InvalidateLink(ctx context.Context, providerName, torrentID, fileID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM link_cache WHERE provider = ? AND torrent_id = ? AND file_id = ?`,
		providerName, torrentID, fileID)
	return err
}

// Request is a watchlist-derived request being tracked through the pipeline.
type Request struct {
	Source    string
	SourceID  string
	MediaType string // movie|tv
	Title     string
	Year      int
	IMDbID    string
	TMDBID    string
	Season    int
	Episode   int
	Status    string // pending|added|failed|done
	Detail    string
	UpdatedAt time.Time
}

// UpsertRequest inserts or updates a tracked request.
func (s *Store) UpsertRequest(ctx context.Context, r Request) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO requests (source, source_id, media_type, title, year, imdb_id, tmdb_id, season, episode, status, detail, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(source, source_id) DO UPDATE SET
			status = excluded.status, detail = excluded.detail, updated_at = excluded.updated_at`,
		r.Source, r.SourceID, r.MediaType, r.Title, r.Year, r.IMDbID, r.TMDBID, r.Season, r.Episode, r.Status, r.Detail, time.Now().Unix())
	return err
}

// ListRequests returns tracked requests filtered by status (empty = all).
func (s *Store) ListRequests(ctx context.Context, status string) ([]Request, error) {
	var (
		rows *sql.Rows
		err  error
	)
	if status == "" {
		rows, err = s.db.QueryContext(ctx, `SELECT source, source_id, media_type, title, year, imdb_id, tmdb_id, season, episode, status, detail, updated_at FROM requests ORDER BY updated_at DESC`)
	} else {
		rows, err = s.db.QueryContext(ctx, `SELECT source, source_id, media_type, title, year, imdb_id, tmdb_id, season, episode, status, detail, updated_at FROM requests WHERE status = ? ORDER BY updated_at DESC`, status)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Request
	for rows.Next() {
		var r Request
		var updated int64
		if err := rows.Scan(&r.Source, &r.SourceID, &r.MediaType, &r.Title, &r.Year, &r.IMDbID, &r.TMDBID, &r.Season, &r.Episode, &r.Status, &r.Detail, &updated); err != nil {
			return nil, err
		}
		r.UpdatedAt = time.Unix(updated, 0).UTC()
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetMeta reads a metadata value.
func (s *Store) GetMeta(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return v, err
}

// SetMeta writes a metadata value.
func (s *Store) SetMeta(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO meta (key, value) VALUES (?, ?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}
