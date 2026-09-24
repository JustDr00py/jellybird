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

// within reports whether path resolves strictly inside root.
func within(root, path string) bool {
	rel, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// withinLibrary reports whether path resolves inside the library root. It
// guards every .strm write/delete driven by stored paths.
func (w *Writer) withinLibrary(path string) bool { return within(w.cfg.Path, path) }

// localCopyRoot returns the root (download folder or library) a local copy
// lives under, or "" when it is under neither. It guards every local-copy
// move/delete driven by stored paths.
func (w *Writer) localCopyRoot(path string) string {
	// The download folder may sit inside the library: check it first.
	for _, root := range []string{w.localRoot, w.cfg.Path} {
		if within(root, path) {
			return root
		}
	}
	return ""
}

// LocalRoot is where local copies are stored. Downloads stage their temp
// files beneath it so the final rename stays on one filesystem.
func (w *Writer) LocalRoot() string { return w.localRoot }

// SeparateLocalRoot reports whether local copies go somewhere other than
// the library (downloads.path is set).
func (w *Writer) SeparateLocalRoot() bool {
	return filepath.Clean(w.localRoot) != filepath.Clean(w.cfg.Path)
}

// localRel is the path of a file's local copy relative to whichever root
// holds it: the .strm's library-relative path with the real extension.
func (w *Writer) localRel(strmPath, srcPath string) (string, error) {
	if !w.withinLibrary(strmPath) {
		return "", fmt.Errorf("refusing path outside library: %s", strmPath)
	}
	return filepath.Rel(w.cfg.Path, LocalPathFor(strmPath, srcPath))
}

// localDest is where a new local copy of the file goes.
func (w *Writer) localDest(strmPath, srcPath string) (string, error) {
	rel, err := w.localRel(strmPath, srcPath)
	if err != nil {
		return "", err
	}
	return filepath.Join(w.localRoot, rel), nil
}

// MoveTarget is where an existing local copy belongs under the current
// download root. ok is false when it's already there or its location is
// not one jellybird manages.
func (w *Writer) MoveTarget(localPath string) (string, bool) {
	root := w.localCopyRoot(localPath)
	if root == "" || root == w.localRoot {
		return "", false
	}
	rel, err := filepath.Rel(root, localPath)
	if err != nil {
		return "", false
	}
	return filepath.Join(w.localRoot, rel), true
}

// FinishMove records a local copy moved from oldPath to newPath and
// deletes the old file. If the entry was removed meanwhile, the new file
// is deleted instead.
func (w *Writer) FinishMove(ctx context.Context, lf store.LocalFile, oldPath, newPath string) error {
	if w.localCopyRoot(oldPath) == "" || w.localCopyRoot(newPath) == "" {
		return fmt.Errorf("refusing path outside library/download folder: %s", newPath)
	}
	done, err := w.store.FinishMove(ctx, lf.Provider, lf.TorrentID, lf.FileID, newPath)
	if err != nil {
		return err
	}
	stale := oldPath
	if !done {
		stale = newPath
	}
	if err := os.Remove(stale); err != nil && !os.IsNotExist(err) {
		return err
	}
	w.pruneEmptyDirs(filepath.Dir(stale))
	return nil
}

// PromoteLocal moves a completed download from tmpPath into the download
// root in place of the file's .strm and deletes the .strm. It returns the final
// path.
func (w *Writer) PromoteLocal(ctx context.Context, cf store.CloudFile, tmpPath string) (string, error) {
	if cf.StrmPath == "" {
		return "", errors.New("file has no library path yet")
	}
	dest, err := w.localDest(cf.StrmPath, cf.FilePath)
	if err != nil {
		return "", err
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
		if w.localCopyRoot(lf.LocalPath) == "" {
			return fmt.Errorf("refusing path outside library/download folder: %s", lf.LocalPath)
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
	if err != nil || !lf.HasCopy() {
		return false
	}
	// Follow layout changes within the root the copy is already under
	// (moving to another disk is an explicit, slow Move), and never while a
	// Move is copying it.
	rel, err := w.localRel(strmPath, f.Path)
	root := w.localCopyRoot(lf.LocalPath)
	if want := filepath.Join(root, rel); err == nil && root != "" && lf.Status == store.LocalDone && lf.LocalPath != want {
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

// pruneEmptyDirs removes dir and its empty parents up to the root
// (library or download folder) it sits under.
func (w *Writer) pruneEmptyDirs(dir string) {
	root := w.localCopyRoot(dir)
	for root != "" && within(root, dir) {
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
