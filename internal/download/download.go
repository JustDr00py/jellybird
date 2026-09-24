// Package download implements "keep local": it downloads debrid files into
// the library in place of their .strm entries so they play offline.
package download

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	"jellybird/internal/config"
	"jellybird/internal/provider"
	"jellybird/internal/store"
	"jellybird/internal/strm"
)

// tempDirName holds partial downloads and moves. It sits inside the
// download root so the final rename never crosses filesystems, but outside
// the Movies/Shows folders Jellyfin scans, and its ".part" files aren't
// media anyway.
const tempDirName = ".jellybird-downloads"

var (
	maxAttempts      = 4
	retryBackoff     = 5 * time.Second
	progressInterval = 2 * time.Second
	// stallTimeout aborts (and later resumes) a transfer that stops moving.
	stallTimeout = 90 * time.Second
)

// LinkSource resolves debrid files to direct download URLs.
// *stream.Resolver implements it.
type LinkSource interface {
	Resolve(ctx context.Context, name provider.Name, torrentID, fileID string) (string, error)
	Invalidate(ctx context.Context, name provider.Name, torrentID, fileID string)
}

// Manager runs the download queue.
type Manager struct {
	store  *store.Store
	writer *strm.Writer
	links  LinkSource
	log    *slog.Logger
	cfg    config.Downloads
	tmpDir string
	client *http.Client
	// freeBytes reports free space for a path (stubbed in tests).
	freeBytes func(path string) (int64, bool)

	wake chan struct{}
	// moveTurn lets one Move copy at a time; the rest wait their turn.
	moveTurn chan struct{}

	mu      sync.Mutex
	running map[string]context.CancelFunc // key -> cancel for in-flight jobs
	runCtx  context.Context               // Run's context, parent of moves
}

// New builds a Manager.
func New(st *store.Store, w *strm.Writer, links LinkSource, cfg config.Downloads, log *slog.Logger) *Manager {
	return &Manager{
		store:  st,
		writer: w,
		links:  links,
		log:    log,
		cfg:    cfg,
		tmpDir: filepath.Join(w.LocalRoot(), tempDirName),
		client: &http.Client{
			Transport: &http.Transport{
				Proxy:                 http.ProxyFromEnvironment,
				ResponseHeaderTimeout: 60 * time.Second,
				TLSHandshakeTimeout:   15 * time.Second,
			},
		},
		freeBytes: diskFree,
		wake:      make(chan struct{}, 8),
		moveTurn:  make(chan struct{}, 1),
		running:   map[string]context.CancelFunc{},
	}
}

func key(p, t, f string) string { return p + "/" + t + "/" + f }

// Run processes the queue until ctx is cancelled.
func (m *Manager) Run(ctx context.Context) {
	m.mu.Lock()
	m.runCtx = ctx
	m.mu.Unlock()
	if err := m.store.ResetInterruptedLocal(ctx); err != nil {
		m.log.Warn("reset interrupted downloads failed", "err", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < m.cfg.Concurrency; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.worker(ctx)
		}()
	}
	wg.Wait()
}

func (m *Manager) worker(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		lf, err := m.store.ClaimNextLocal(ctx)
		if err == nil {
			m.process(ctx, lf)
			continue
		}
		if !errors.Is(err, store.ErrNotFound) && ctx.Err() == nil {
			m.log.Warn("claim download failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-m.wake:
		case <-time.After(30 * time.Second):
		}
	}
}

// Enqueue queues every given tracked file; it returns how many were newly
// queued (already-queued, in-progress or finished files are skipped).
func (m *Manager) Enqueue(ctx context.Context, files []store.CloudFile) (int, error) {
	n := 0
	for _, cf := range files {
		ok, err := m.store.EnqueueLocal(ctx, cf)
		if err != nil {
			return n, err
		}
		if ok {
			n++
		}
	}
	for i := 0; i < n && i < m.cfg.Concurrency; i++ {
		select {
		case m.wake <- struct{}{}:
		default:
		}
	}
	return n, nil
}

// Remove cancels an in-flight download or deletes a finished local copy
// (restoring the .strm when the file is still in the cloud), then forgets
// the entry.
func (m *Manager) Remove(ctx context.Context, providerName, torrentID, fileID string) error {
	lf, err := m.store.GetLocal(ctx, providerName, torrentID, fileID)
	if err != nil {
		return err
	}
	// Forget the entry first so an in-flight worker sees it's gone.
	if err := m.store.DeleteLocal(ctx, providerName, torrentID, fileID); err != nil {
		return err
	}
	m.mu.Lock()
	cancel := m.running[key(providerName, torrentID, fileID)]
	m.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if err := os.Remove(m.tempPath(lf)); err != nil && !os.IsNotExist(err) {
		m.log.Warn("remove partial download failed", "err", err)
	}
	if lf.HasCopy() {
		return m.writer.RemoveLocalCopy(ctx, lf)
	}
	return nil
}

// ErrNotMovable is returned by Move for entries that aren't a finished
// copy outside the download folder.
var ErrNotMovable = errors.New("only finished local copies outside the download folder can be moved")

// MoveTarget reports where Move would put a finished local copy, or ok
// false when it's already in the download folder.
func (m *Manager) MoveTarget(lf store.LocalFile) (string, bool) {
	if lf.Status != store.LocalDone || lf.LocalPath == "" {
		return "", false
	}
	return m.writer.MoveTarget(lf.LocalPath)
}

// Move copies a finished local copy saved outside the download folder
// (e.g. before downloads.path pointed at a NAS) into it, in the background.
// The copy keeps playing from its old path until the move completes. Moves
// run one at a time.
func (m *Manager) Move(ctx context.Context, providerName, torrentID, fileID string) error {
	lf, err := m.store.GetLocal(ctx, providerName, torrentID, fileID)
	if err != nil {
		return err
	}
	dest, ok := m.MoveTarget(lf)
	if !ok {
		return ErrNotMovable
	}
	if started, err := m.store.StartMove(ctx, providerName, torrentID, fileID); err != nil {
		return err
	} else if !started {
		return ErrNotMovable
	}

	m.mu.Lock()
	parent := m.runCtx
	if parent == nil {
		parent = context.Background()
	}
	mctx, cancel := context.WithCancel(parent)
	k := key(providerName, torrentID, fileID)
	m.running[k] = cancel
	m.mu.Unlock()

	go func() {
		defer func() {
			cancel()
			m.mu.Lock()
			delete(m.running, k)
			m.mu.Unlock()
		}()
		select {
		case m.moveTurn <- struct{}{}:
			defer func() { <-m.moveTurn }()
		case <-mctx.Done():
			return
		}
		m.log.Info("move started", "from", lf.LocalPath, "to", dest)
		err := m.move(mctx, lf, dest)
		switch {
		case err == nil:
			m.log.Info("move finished", "path", dest)
		case mctx.Err() != nil:
			// Removed by the user (row gone) or shutting down (reset on start).
			_ = os.Remove(m.tempPath(lf))
		default:
			m.log.Warn("move failed", "from", lf.LocalPath, "err", err)
			_ = os.Remove(m.tempPath(lf))
			_ = m.store.EndMove(context.WithoutCancel(mctx), lf.Provider, lf.TorrentID, lf.FileID, "move failed: "+err.Error())
		}
	}()
	return nil
}

// move copies lf's file to a temp file beside dest, renames it into place
// and only then deletes the original.
func (m *Manager) move(ctx context.Context, lf store.LocalFile, dest string) error {
	src, err := os.Open(lf.LocalPath)
	if err != nil {
		return err
	}
	defer src.Close()
	st, err := src.Stat()
	if err != nil {
		return err
	}
	if err := m.checkSpace(st.Size()); err != nil {
		return err
	}
	if _, err := os.Lstat(dest); err == nil {
		return fmt.Errorf("%s already exists", dest)
	}
	if err := os.MkdirAll(m.tmpDir, 0o755); err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	tmp := m.tempPath(lf)
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	pw := &progressWriter{
		w: out, last: time.Now(),
		report: func(n int64) {
			_ = m.store.SetLocalProgress(context.WithoutCancel(ctx), lf.Provider, lf.TorrentID, lf.FileID, n)
		},
		activity: make(chan struct{}, 1),
	}
	_, err = io.Copy(pw, ctxReader{ctx, src})
	pw.report(pw.done)
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if pw.done != st.Size() {
		return fmt.Errorf("size mismatch: copied %d bytes, expected %d", pw.done, st.Size())
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return err
	}
	if err := os.Rename(tmp, dest); err != nil {
		return err
	}
	return m.writer.FinishMove(context.WithoutCancel(ctx), lf, lf.LocalPath, dest)
}

// ctxReader stops a copy once ctx is cancelled.
type ctxReader struct {
	ctx context.Context
	r   io.Reader
}

func (c ctxReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// LocalRoot is where local copies are stored.
func (m *Manager) LocalRoot() string { return m.writer.LocalRoot() }

// SeparateLocalRoot reports whether local copies go outside the library.
func (m *Manager) SeparateLocalRoot() bool { return m.writer.SeparateLocalRoot() }

// FreeSpace reports free bytes where local copies are stored; ok is false
// when the platform can't tell.
func (m *Manager) FreeSpace() (free int64, ok bool) {
	return m.freeBytes(m.writer.LocalRoot())
}

// MinFreeBytes is the reserve downloads refuse to eat into.
func (m *Manager) MinFreeBytes() int64 { return m.cfg.MinFreeGB << 30 }

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9_-]`)

// tempPath is the staging file for one entry. IDs come from the provider,
// so they're reduced to a safe charset before touching the filesystem.
func (m *Manager) tempPath(lf store.LocalFile) string {
	name := unsafeName.ReplaceAllString(lf.Provider, "_") + "-" +
		unsafeName.ReplaceAllString(lf.TorrentID, "_") + "-" +
		unsafeName.ReplaceAllString(lf.FileID, "_") + ".part"
	return filepath.Join(m.tmpDir, name)
}

func (m *Manager) process(parent context.Context, lf store.LocalFile) {
	k := key(lf.Provider, lf.TorrentID, lf.FileID)
	ctx, cancel := context.WithCancel(parent)
	m.mu.Lock()
	m.running[k] = cancel
	m.mu.Unlock()
	defer func() {
		cancel()
		m.mu.Lock()
		delete(m.running, k)
		m.mu.Unlock()
	}()

	m.log.Info("download started", "file", lf.FilePath, "provider", lf.Provider, "torrent", lf.TorrentID)
	dest, err := m.download(ctx, lf)
	switch {
	case err == nil:
		m.log.Info("download finished", "file", lf.FilePath, "path", dest)
	case parent.Err() != nil:
		// Shutting down: stays "downloading" and is re-queued on start.
	case ctx.Err() != nil:
		// Removed by the user mid-download; its row is already gone.
		_ = os.Remove(m.tempPath(lf))
	default:
		m.log.Warn("download failed", "file", lf.FilePath, "err", err)
		_ = m.store.SetLocalFailed(parent, lf.Provider, lf.TorrentID, lf.FileID, err.Error())
	}
}

func (m *Manager) download(ctx context.Context, lf store.LocalFile) (string, error) {
	name, err := provider.ParseName(lf.Provider)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(m.tmpDir, 0o755); err != nil {
		return "", fmt.Errorf("create temp dir: %w", err)
	}
	tmp := m.tempPath(lf)

	var have int64
	if st, err := os.Stat(tmp); err == nil {
		have = st.Size()
	}
	if err := m.checkSpace(lf.SizeBytes - have); err != nil {
		return "", err
	}

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if attempt > 1 {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(time.Duration(attempt*attempt) * retryBackoff):
			}
		}
		lastErr = m.fetch(ctx, name, lf, tmp)
		if lastErr == nil || ctx.Err() != nil {
			break
		}
		var perm permanentError
		if errors.As(lastErr, &perm) {
			break
		}
		m.log.Info("download interrupted, will resume", "file", lf.FilePath, "attempt", attempt, "err", lastErr)
	}
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if lastErr != nil {
		return "", lastErr
	}

	st, err := os.Stat(tmp)
	if err != nil {
		return "", err
	}
	if lf.SizeBytes > 0 && st.Size() != lf.SizeBytes {
		_ = os.Remove(tmp)
		return "", fmt.Errorf("size mismatch: got %d bytes, expected %d", st.Size(), lf.SizeBytes)
	}

	// Removed while the last bytes arrived? Don't resurrect it.
	if _, err := m.store.GetLocal(ctx, lf.Provider, lf.TorrentID, lf.FileID); err != nil {
		_ = os.Remove(tmp)
		return "", context.Canceled
	}
	// The file's library path may have changed while downloading, and it
	// may have left the cloud entirely — look it up fresh.
	cf, err := m.store.GetFile(ctx, lf.Provider, lf.TorrentID, lf.FileID)
	if err != nil {
		return "", fmt.Errorf("file is no longer in the library: %w", err)
	}
	dest, err := m.writer.PromoteLocal(ctx, cf, tmp)
	if err != nil {
		return "", err
	}
	if err := m.store.SetLocalDone(ctx, lf.Provider, lf.TorrentID, lf.FileID, dest, st.Size()); err != nil {
		return "", err
	}
	return dest, nil
}

// permanentError stops retries (disk full, bad request...).
type permanentError struct{ error }

func (e permanentError) Unwrap() error { return e.error }

// fetch downloads (or resumes) into tmp.
func (m *Manager) fetch(ctx context.Context, name provider.Name, lf store.LocalFile, tmp string) error {
	link, err := m.links.Resolve(ctx, name, lf.TorrentID, lf.FileID)
	if err != nil {
		return fmt.Errorf("get download link: %w", err)
	}

	var offset int64
	if st, err := os.Stat(tmp); err == nil {
		offset = st.Size()
	}
	if lf.SizeBytes > 0 && offset == lf.SizeBytes {
		return nil // already complete (e.g. stopped right before the move)
	}
	if lf.SizeBytes > 0 && offset > lf.SizeBytes {
		_ = os.Remove(tmp)
		offset = 0
	}

	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, link, nil)
	if err != nil {
		return permanentError{err}
	}
	if offset > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", offset))
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	flags := os.O_CREATE | os.O_WRONLY
	switch {
	case resp.StatusCode == http.StatusPartialContent && offset > 0:
		flags |= os.O_APPEND
	case resp.StatusCode == http.StatusOK:
		flags |= os.O_TRUNC // server ignored Range: start over
		offset = 0
	case resp.StatusCode == http.StatusRequestedRangeNotSatisfiable:
		_ = os.Remove(tmp) // partial file doesn't match: restart clean
		return errors.New("resume rejected, restarting")
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden ||
		resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
		m.links.Invalidate(ctx, name, lf.TorrentID, lf.FileID) // expired link: next attempt gets a fresh one
		return fmt.Errorf("download link rejected (HTTP %d)", resp.StatusCode)
	default:
		return fmt.Errorf("unexpected HTTP %d from debrid CDN", resp.StatusCode)
	}

	out, err := os.OpenFile(tmp, flags, 0o644)
	if err != nil {
		return permanentError{err}
	}
	defer out.Close()

	pw := &progressWriter{
		w: out, done: offset, last: time.Now(),
		report: func(n int64) {
			_ = m.store.SetLocalProgress(context.WithoutCancel(ctx), lf.Provider, lf.TorrentID, lf.FileID, n)
		},
		activity: make(chan struct{}, 1),
	}
	var stalled atomic.Bool
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		t := time.NewTimer(stallTimeout)
		defer t.Stop()
		for {
			select {
			case <-stop:
				return
			case <-pw.activity:
				t.Reset(stallTimeout)
			case <-t.C:
				stalled.Store(true)
				cancel()
				return
			}
		}
	}()

	_, err = io.Copy(pw, resp.Body)
	pw.report(pw.done)
	if err != nil {
		if stalled.Load() {
			return fmt.Errorf("transfer stalled for %s", stallTimeout)
		}
		return err
	}
	return out.Sync()
}

// checkSpace refuses downloads that would leave less than MinFreeGB free.
func (m *Manager) checkSpace(need int64) error {
	if need <= 0 {
		return nil
	}
	free, ok := m.freeBytes(m.writer.LocalRoot())
	if !ok {
		return nil
	}
	reserve := m.cfg.MinFreeGB << 30
	if free-need < reserve {
		return permanentError{fmt.Errorf("not enough disk space: need %s plus a %d GB reserve, %s free",
			humanBytes(need), m.cfg.MinFreeGB, humanBytes(free))}
	}
	return nil
}

// progressWriter counts bytes, reports them periodically and signals
// activity for stall detection.
type progressWriter struct {
	w        io.Writer
	done     int64
	last     time.Time
	report   func(int64)
	activity chan struct{}
}

func (p *progressWriter) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	p.done += int64(n)
	select {
	case p.activity <- struct{}{}:
	default:
	}
	if time.Since(p.last) >= progressInterval {
		p.last = time.Now()
		p.report(p.done)
	}
	return n, err
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
