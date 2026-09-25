package web

import (
	"fmt"
	"html/template"
	"net/http"
	"path/filepath"
	"strconv"
)

var pageFuncs = template.FuncMap{
	"pill": func(text, class string) template.HTML {
		return template.HTML(fmt.Sprintf(`<span class="pill %s">%s</span>`, class, template.HTMLEscapeString(text)))
	},
	"fmtSize": func(n int64) string {
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
	},
	"base": func(p string) string { return filepath.Base(p) },
}

// render executes a page template, which itself includes the layout blocks.
// The logged-in username (if any) is injected as .User for the header.
func (h *handlers) render(w http.ResponseWriter, r *http.Request, status int, page string, data map[string]any) {
	if u, ok := userFrom(r.Context()); ok {
		data["User"] = u.Username
	}
	t, err := template.New(page).Funcs(pageFuncs).ParseFS(templateFS,
		"templates/layout.html", "templates/"+page)
	if err != nil {
		http.Error(w, "template error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	if err := t.ExecuteTemplate(w, page, data); err != nil {
		h.d.Log.Error("template execute failed", "page", page, "err", err)
	}
}

const libraryPageSize = 150

func (h *handlers) index(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	if page < 1 {
		page = 1
	}
	query, provider := q.Get("q"), q.Get("provider")
	files, total, err := h.d.Store.ListFilesPage(r.Context(), provider, query, libraryPageSize, (page-1)*libraryPageSize)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	totalPages := (total + libraryPageSize - 1) / libraryPageSize
	if totalPages < 1 {
		totalPages = 1
	}
	h.render(w, r, http.StatusOK, "index.html", map[string]any{
		"Files": files, "Total": total,
		"Page": page, "TotalPages": totalPages,
		"HasPrev": page > 1, "HasNext": page < totalPages,
		"PrevPage": page - 1, "NextPage": page + 1,
		"Query": query, "Provider": provider,
	})
}

func (h *handlers) searchPage(w http.ResponseWriter, r *http.Request) {
	h.render(w, r, http.StatusOK, "search.html", map[string]any{})
}

func (h *handlers) discoverPage(w http.ResponseWriter, r *http.Request) {
	h.render(w, r, http.StatusOK, "discover.html", map[string]any{})
}

func (h *handlers) cloudPage(w http.ResponseWriter, r *http.Request) {
	h.render(w, r, http.StatusOK, "cloud.html", map[string]any{})
}

func (h *handlers) localPage(w http.ResponseWriter, r *http.Request) {
	data := map[string]any{"Enabled": h.d.Downloads != nil}
	if h.d.Downloads != nil {
		if free, ok := h.d.Downloads.FreeSpace(); ok {
			data["FreeBytes"] = free
		}
		data["ReserveBytes"] = h.d.Downloads.MinFreeBytes()
		if h.d.Downloads.SeparateLocalRoot() {
			data["DownloadsPath"] = h.d.Downloads.LocalRoot()
		}
	}
	h.render(w, r, http.StatusOK, "local.html", data)
}

func (h *handlers) settingsPage(w http.ResponseWriter, r *http.Request) {
	data := map[string]any{
		"Config":  &h.d.Config, // pointer: HasSearch/HasWatchlist have pointer receivers
		"Version": h.d.Version,
	}
	h.render(w, r, http.StatusOK, "settings.html", data)
}
