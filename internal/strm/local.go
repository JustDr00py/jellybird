package strm

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"jellybird/internal/provider"
	"jellybird/internal/store"
)

// LocalPathFor is where a downloaded copy of srcPath lives: the file's .strm
// path with the real extension, so Jellyfin sees the same title/folder.
func LocalPathFor(strmPath, srcPath string) string {
	ext := strings.ToLower(filepath.Ext(srcPath))
	if ext == "" || ext == ".strm" {
		ext = ".mkv"
	}
	return strings.TrimSuffix(strmPath, ".strm") + ext
}

// withinLibrary reports whether path resolves inside the library root. It
// guards every filesystem write/delete driven by stored paths.
func (w *Writer) withinLibrary(path string) bool {
	rel, err := filepath.Rel(filepath.Clean(w.cfg.Path), filepath.Clean(path))
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// LibraryRoot is the library directory (downloads stage their temp files
// beneath it so the final rename stays on one filesystem).
func (w *Writer) LibraryRoot() string { return w.cfg.Path }

// PromoteLocal moves a completed download from tmpPath into the library in
// place of the file's .strm and deletes the .strm. It returns the final
// path.
func (w *Writer) PromoteLocal(ctx context.Context, cf store.CloudFile, tmpPath string) (string, error) {
	if cf.StrmPath == "" {
		return "", errors.New("file has no library path yet")
	}
	dest := LocalPathFor(cf.StrmPath, cf.FilePath)
	if !w.withinLibrary(dest) || !w.withinLibrary(cf.StrmPath) {
		return "", fmt.Errorf("refusing path outside library: %s", dest)
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return "", fmt.Errorf("mkdir: %w", err)
	}
	if err := os.Rename(tmpPath, dest); err != nil {
		return "", fmt.Errorf("move into library: %w", err)
	}
	if err := os.Remove(cf.StrmPath); err != nil && !os.IsNotExist(err) {
		w.log.Warn("remove strm after local download failed", "path", cf.StrmPath, "err", err)
	}
	return dest, nil
}

// RemoveLocalCopy deletes a finished local copy and, if the file is still
// in the cloud, writes its .strm back so the title keeps streaming.
func (w *Writer) RemoveLocalCopy(ctx context.Context, lf store.LocalFile) error {
	if lf.LocalPath != "" {
		if !w.withinLibrary(lf.LocalPath) {
			return fmt.Errorf("refusing path outside library: %s", lf.LocalPath)
		}
		if err := os.Remove(lf.LocalPath); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	cf, err := w.store.GetFile(ctx, lf.Provider, lf.TorrentID, lf.FileID)
	if errors.Is(err, store.ErrNotFound) {
		// Gone from the cloud: nothing to stream any more.
		if lf.LocalPath != "" {
			w.pruneEmptyDirs(filepath.Dir(lf.LocalPath))
		}
		return nil
	}
	if err != nil {
		return err
	}
	name, err := provider.ParseName(cf.Provider)
	if err != nil {
		return err
	}
	if err := w.writeStrm(cf.StrmPath, w.StreamURL(name, cf.TorrentID, cf.FileID)); err != nil {
		return err
	}
	if lf.LocalPath != "" {
		w.pruneEmptyDirs(filepath.Dir(lf.LocalPath))
	}
	return nil
}

// keepLocal is SyncProvider's hook for a file with a finished local copy:
// keep the copy at the path the current layout wants (moving it after
// parser/naming changes) and make sure no .strm duplicates it. It returns
// false when there is no finished local copy and the .strm should be
// written as usual.
func (w *Writer) keepLocal(ctx context.Context, name provider.Name, torrentID string, f provider.File, strmPath string) bool {
	lf, err := w.store.GetLocal(ctx, string(name), torrentID, f.ID)
	if err != nil || lf.Status != store.LocalDone {
		return false
	}
	want := LocalPathFor(strmPath, f.Path)
	if lf.LocalPath != want && w.withinLibrary(want) && w.withinLibrary(lf.LocalPath) {
		if err := os.MkdirAll(filepath.Dir(want), 0o755); err != nil {
			w.log.Warn("relocate local copy failed", "to", want, "err", err)
		} else if err := os.Rename(lf.LocalPath, want); err != nil {
			w.log.Warn("relocate local copy failed", "from", lf.LocalPath, "to", want, "err", err)
		} else {
			if err := w.store.SetLocalPath(ctx, lf.Provider, lf.TorrentID, lf.FileID, want); err != nil {
				w.log.Warn("record relocated local copy failed", "err", err)
			}
			w.pruneEmptyDirs(filepath.Dir(lf.LocalPath))
			w.log.Info("relocated local copy", "from", lf.LocalPath, "to", want)
		}
	}
	if err := os.Remove(strmPath); err != nil && !os.IsNotExist(err) {
		w.log.Warn("remove strm shadowing local copy failed", "path", strmPath, "err", err)
	}
	return true
}

// pruneEmptyDirs removes dir and its empty parents up to the library root.
func (w *Writer) pruneEmptyDirs(dir string) {
	for dir != w.cfg.Path && strings.HasPrefix(dir, w.cfg.Path) {
		entries, err := os.ReadDir(dir)
		if err != nil || len(entries) > 0 {
			return
		}
		if err := os.Remove(dir); err != nil {
			return
		}
		dir = filepath.Dir(dir)
	}
}
