package web

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-chi/chi/v5"

	"jellybird/internal/download"
	"jellybird/internal/provider"
	"jellybird/internal/store"
	"jellybird/internal/strm"
)

// maxIDLen bounds provider torrent/file IDs accepted from clients.
const maxIDLen = 128

func validID(s string) bool {
	return s != "" && len(s) <= maxIDLen && !strings.ContainsAny(s, "/\\\x00")
}

type localEntry struct {
	store.LocalFile
	// InLibrary is false once the file left the debrid cloud: the local
	// copy is then the only one left.
	InLibrary bool `json:"in_library"`
	// MoveTo is set for finished copies outside the download folder: where
	// POST /api/local/move would put them.
	MoveTo string `json:"move_to,omitempty"`
}

// localList is GET /api/local.
func (h *handlers) localList(w http.ResponseWriter, r *http.Request) {
	list, err := h.d.Store.ListLocal(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	out := make([]localEntry, 0, len(list))
	for _, lf := range list {
		_, err := h.d.Store.GetFile(r.Context(), lf.Provider, lf.TorrentID, lf.FileID)
		e := localEntry{LocalFile: lf, InLibrary: err == nil}
		if h.d.Downloads != nil {
			e.MoveTo, _ = h.d.Downloads.MoveTarget(lf)
		}
		out = append(out, e)
	}
	writeJSON(w, http.StatusOK, out)
}

// localAdd is POST /api/local {provider, torrent_id, file_id?}: queue one
// tracked file, or every tracked file of a torrent when file_id is empty.
// Failed entries are retried.
func (h *handlers) localAdd(w http.ResponseWriter, r *http.Request) {
	if h.d.Downloads == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "downloads are not enabled"})
		return
	}
	var body struct {
		Provider  string `json:"provider"`
		TorrentID string `json:"torrent_id"`
		FileID    string `json:"file_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxFormBytes)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
		return
	}
	if _, err := provider.ParseName(body.Provider); err != nil || !validID(body.TorrentID) ||
		(body.FileID != "" && !validID(body.FileID)) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "provider and torrent_id are required"})
		return
	}
	tracked, err := h.d.Store.ListFiles(r.Context(), body.Provider)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	var files []store.CloudFile
	for _, cf := range tracked {
		if cf.TorrentID == body.TorrentID && (body.FileID == "" || cf.FileID == body.FileID) {
			files = append(files, cf)
		}
	}
	if len(files) == 0 {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "nothing from this torrent is in the library yet"})
		return
	}
	n, err := h.d.Downloads.Enqueue(r.Context(), files)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"queued": n, "files": len(files)})
}

// localRemove is DELETE /api/local?provider=&torrent_id=&file_id=: cancel a
// download, or delete a local copy (the title goes back to streaming).
func (h *handlers) localRemove(w http.ResponseWriter, r *http.Request) {
	if h.d.Downloads == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "downloads are not enabled"})
		return
	}
	q := r.URL.Query()
	p, t, f := q.Get("provider"), q.Get("torrent_id"), q.Get("file_id")
	if _, err := provider.ParseName(p); err != nil || !validID(t) || !validID(f) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "provider, torrent_id and file_id are required"})
		return
	}
	err := h.d.Downloads.Remove(r.Context(), p, t, f)
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no local copy for that file"})
		return
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "removed"})
}

// localMove is POST /api/local/move {provider, torrent_id, file_id}: move a
// finished copy saved outside the download folder into it, in the
// background.
func (h *handlers) localMove(w http.ResponseWriter, r *http.Request) {
	if h.d.Downloads == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "downloads are not enabled"})
		return
	}
	var body struct {
		Provider  string `json:"provider"`
		TorrentID string `json:"torrent_id"`
		FileID    string `json:"file_id"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxFormBytes)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad json"})
		return
	}
	if _, err := provider.ParseName(body.Provider); err != nil || !validID(body.TorrentID) || !validID(body.FileID) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "provider, torrent_id and file_id are required"})
		return
	}
	err := h.d.Downloads.Move(r.Context(), body.Provider, body.TorrentID, body.FileID)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "no local copy for that file"})
	case errors.Is(err, download.ErrNotMovable):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
	case err != nil:
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
	default:
		writeJSON(w, http.StatusAccepted, map[string]string{"status": "moving"})
	}
}

// downloadFile is GET /api/download/{provider}/{torrentID}/{fileID}: save a
// library file to the viewer's device. A finished local copy is served from
// disk; otherwise the debrid file is proxied through jellybird, which keeps
// every debrid request on the server's IP (Real-Debrid flags accounts used
// from several IPs at once) and lets us name the file properly.
func (h *handlers) downloadFile(w http.ResponseWriter, r *http.Request) {
	p, t, f := chi.URLParam(r, "provider"), chi.URLParam(r, "torrentID"), chi.URLParam(r, "fileID")
	name, err := provider.ParseName(p)
	if err != nil || !validID(t) || !validID(f) {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	cf, cfErr := h.d.Store.GetFile(r.Context(), p, t, f)
	lf, lfErr := h.d.Store.GetLocal(r.Context(), p, t, f)
	haveLocal := lfErr == nil && lf.HasCopy() && lf.LocalPath != ""
	if cfErr != nil && !haveLocal {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	// Name the download after the tidy library entry, not the release.
	filename := filepath.Base(lf.LocalPath)
	if !haveLocal {
		filename = filepath.Base(strm.LocalPathFor(cf.StrmPath, cf.FilePath))
	}
	disposition := mime.FormatMediaType("attachment", map[string]string{"filename": filename})
	if disposition == "" {
		disposition = "attachment"
	}

	if haveLocal {
		if fh, err := os.Open(lf.LocalPath); err == nil {
			defer fh.Close()
			if st, err := fh.Stat(); err == nil && st.Mode().IsRegular() {
				w.Header().Set("Content-Disposition", disposition)
				w.Header().Set("Content-Type", "application/octet-stream")
				http.ServeContent(w, r, "", st.ModTime(), fh) // handles Range/resume
				return
			}
		} else {
			h.d.Log.Warn("local copy unreadable, falling back to debrid", "path", lf.LocalPath, "err", err)
		}
		if cfErr != nil {
			http.Error(w, "local copy missing and file is no longer in the cloud", http.StatusNotFound)
			return
		}
	}
	if h.d.Resolver == nil {
		http.Error(w, "streaming unavailable", http.StatusServiceUnavailable)
		return
	}

	for attempt := 0; attempt < 2; attempt++ {
		link, err := h.d.Resolver.Resolve(r.Context(), name, t, f)
		if err != nil {
			http.Error(w, "unable to get download link", http.StatusBadGateway)
			return
		}
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, link, nil)
		if err != nil {
			http.Error(w, "bad link", http.StatusBadGateway)
			return
		}
		if rg := r.Header.Get("Range"); rg != "" {
			req.Header.Set("Range", rg)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			http.Error(w, "debrid download failed", http.StatusBadGateway)
			return
		}
		switch resp.StatusCode {
		case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusGone:
			resp.Body.Close()
			h.d.Resolver.Invalidate(r.Context(), name, t, f) // stale cached link: retry fresh
			continue
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent &&
			resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
			http.Error(w, "debrid download failed", http.StatusBadGateway)
			return
		}
		// Pass through only transfer headers, never the CDN's cookies etc.
		for _, hd := range []string{"Content-Length", "Content-Range", "Accept-Ranges", "Last-Modified", "ETag"} {
			if v := resp.Header.Get(hd); v != "" {
				w.Header().Set(hd, v)
			}
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Disposition", disposition)
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		return
	}
	http.Error(w, "debrid rejected the download link", http.StatusBadGateway)
}
