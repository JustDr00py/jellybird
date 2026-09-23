package store

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Local copy states.
const (
	LocalQueued      = "queued"
	LocalDownloading = "downloading"
	LocalDone        = "done"
	LocalFailed      = "failed"
)

// LocalFile is a "keep local" request: a debrid file downloaded into the
// library in place of its .strm, for offline playback.
type LocalFile struct {
	Provider    string    `json:"provider"`
	TorrentID   string    `json:"torrent_id"`
	FileID      string    `json:"file_id"`
	TorrentName string    `json:"torrent_name"`
	FilePath    string    `json:"file_path"`
	SizeBytes   int64     `json:"size_bytes"`
	BytesDone   int64     `json:"bytes_done"`
	Status      string    `json:"status"`
	LocalPath   string    `json:"local_path"`
	Error       string    `json:"error"`
	UpdatedAt   time.Time `json:"updated_at"`
}

const localCols = `provider, torrent_id, file_id, torrent_name, file_path, size_bytes, bytes_done, status, local_path, error, updated_at`

func scanLocal(row interface{ Scan(...any) error }) (LocalFile, error) {
	var lf LocalFile
	var updated int64
	err := row.Scan(&lf.Provider, &lf.TorrentID, &lf.FileID, &lf.TorrentName, &lf.FilePath,
		&lf.SizeBytes, &lf.BytesDone, &lf.Status, &lf.LocalPath, &lf.Error, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return LocalFile{}, ErrNotFound
	}
	lf.UpdatedAt = time.Unix(updated, 0)
	return lf, err
}

// EnqueueLocal queues a tracked file for download. A failed entry is
// re-queued; queued, in-progress and finished entries are left alone.
// It reports whether the file is now (newly) queued.
func (s *Store) EnqueueLocal(ctx context.Context, f CloudFile) (bool, error) {
	now := time.Now().Unix()
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO local_files (provider, torrent_id, file_id, torrent_name, file_path, size_bytes, status, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT (provider, torrent_id, file_id) DO UPDATE
			SET status = excluded.status, error = '', updated_at = excluded.updated_at
			WHERE local_files.status = ?`,
		f.Provider, f.TorrentID, f.FileID, f.TorrentName, f.FilePath, f.SizeBytes, LocalQueued, now, now, LocalFailed)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// GetLocal returns one local-copy entry.
func (s *Store) GetLocal(ctx context.Context, provider, torrentID, fileID string) (LocalFile, error) {
	return scanLocal(s.db.QueryRowContext(ctx,
		`SELECT `+localCols+` FROM local_files WHERE provider = ? AND torrent_id = ? AND file_id = ?`,
		provider, torrentID, fileID))
}

// ListLocal returns every local-copy entry, active ones first.
func (s *Store) ListLocal(ctx context.Context) ([]LocalFile, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+localCols+` FROM local_files
		ORDER BY CASE status WHEN 'downloading' THEN 0 WHEN 'queued' THEN 1 WHEN 'failed' THEN 2 ELSE 3 END,
		         created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LocalFile{}
	for rows.Next() {
		lf, err := scanLocal(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, lf)
	}
	return out, rows.Err()
}

// ClaimNextLocal atomically moves the oldest queued entry to downloading
// and returns it, so concurrent workers never grab the same file.
func (s *Store) ClaimNextLocal(ctx context.Context) (LocalFile, error) {
	return scanLocal(s.db.QueryRowContext(ctx, `
		UPDATE local_files SET status = ?, error = '', updated_at = ?
		WHERE rowid = (SELECT rowid FROM local_files WHERE status = ? ORDER BY created_at LIMIT 1)
		RETURNING `+localCols,
		LocalDownloading, time.Now().Unix(), LocalQueued))
}

// ResetInterruptedLocal re-queues downloads cut off by a restart; their
// partial files are resumed.
func (s *Store) ResetInterruptedLocal(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE local_files SET status = ? WHERE status = ?`, LocalQueued, LocalDownloading)
	return err
}

// SetLocalProgress records downloaded bytes.
func (s *Store) SetLocalProgress(ctx context.Context, provider, torrentID, fileID string, done int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE local_files SET bytes_done = ?, updated_at = ? WHERE provider = ? AND torrent_id = ? AND file_id = ?`,
		done, time.Now().Unix(), provider, torrentID, fileID)
	return err
}

// SetLocalFailed marks a download failed with a reason.
func (s *Store) SetLocalFailed(ctx context.Context, provider, torrentID, fileID, reason string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE local_files SET status = ?, error = ?, updated_at = ? WHERE provider = ? AND torrent_id = ? AND file_id = ?`,
		LocalFailed, reason, time.Now().Unix(), provider, torrentID, fileID)
	return err
}

// SetLocalDone marks a download finished at localPath.
func (s *Store) SetLocalDone(ctx context.Context, provider, torrentID, fileID, localPath string, size int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE local_files SET status = ?, local_path = ?, bytes_done = ?, error = '', updated_at = ?
		 WHERE provider = ? AND torrent_id = ? AND file_id = ?`,
		LocalDone, localPath, size, time.Now().Unix(), provider, torrentID, fileID)
	return err
}

// SetLocalPath records a finished local copy's new location after the
// library layout moved it.
func (s *Store) SetLocalPath(ctx context.Context, provider, torrentID, fileID, localPath string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE local_files SET local_path = ?, updated_at = ? WHERE provider = ? AND torrent_id = ? AND file_id = ?`,
		localPath, time.Now().Unix(), provider, torrentID, fileID)
	return err
}

// DeleteLocal forgets a local-copy entry (the caller removes files).
func (s *Store) DeleteLocal(ctx context.Context, provider, torrentID, fileID string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM local_files WHERE provider = ? AND torrent_id = ? AND file_id = ?`,
		provider, torrentID, fileID)
	return err
}
