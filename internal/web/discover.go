package web

import (
	"context"
	"net/http"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"jellybird/internal/config"
	"jellybird/internal/metadata/tmdb"
	"jellybird/internal/store"
)

// discoverItem is a TMDB result labelled with what the library already has.
type discoverItem struct {
	tmdb.Result
	InLibrary bool `json:"in_library"`
	// Episodes is how many episode files a show has in the library (TV only).
	Episodes int `json:"episodes,omitempty"`
}

// discoverLists returns the lists and genres the Discover page offers for
// ?type=movie|tv.
func (h *handlers) discoverLists(w http.ResponseWriter, r *http.Request) {
	meta := h.d.Engine.Metadata()
	if meta == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "discover needs a TMDB key (set tmdb.api_key)"})
		return
	}
	kind := r.URL.Query().Get("type")
	lists, ok := tmdb.Lists[kind]
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "type=movie|tv is required"})
		return
	}
	genres, err := meta.Genres(r.Context(), kind)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	out := []map[string]string{}
	for _, l := range lists {
		out = append(out, map[string]string{"name": l.Name, "label": l.Label})
	}
	writeJSON(w, http.StatusOK, map[string]any{"lists": out, "genres": genres})
}

// discover returns one page of a TMDB list (?type=, ?list= or ?genre=,
// ?page=), each title labelled with whether it's already in the library.
func (h *handlers) discover(w http.ResponseWriter, r *http.Request) {
	meta := h.d.Engine.Metadata()
	if meta == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "discover needs a TMDB key (set tmdb.api_key)"})
		return
	}
	q := r.URL.Query()
	genre, _ := strconv.Atoi(q.Get("genre"))
	page, _ := strconv.Atoi(q.Get("page"))
	res, err := meta.Browse(r.Context(), q.Get("type"), q.Get("list"), genre, page)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": err.Error()})
		return
	}
	idx, err := loadLibraryIndex(r.Context(), h.d.Store, h.d.Config.Library)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	items := make([]discoverItem, 0, len(res.Results))
	for _, m := range res.Results {
		it := discoverItem{Result: m}
		it.InLibrary, it.Episodes = idx.lookup(m)
		items = append(items, it)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"results": items, "page": res.Page, "total_pages": res.TotalPages,
	})
}

// libraryIndex answers "do we have this TMDB title" for a batch of results.
// TMDB ids only exist on hints (titles added through search or the
// watchlist), so titles that arrived by cloud sync are matched by the
// "Title (Year)" / "Show" folder names the .strm writer lays out.
type libraryIndex struct {
	hintMovies, hintShows map[string]bool
	movies                map[string][]int // normalized title -> years (0 = none)
	shows                 map[string]int   // normalized show title -> episode files
}

var folderYear = regexp.MustCompile(`^(.*?)\s*\((\d{4})\)$`)

func loadLibraryIndex(ctx context.Context, st *store.Store, lib config.Library) (*libraryIndex, error) {
	idx := &libraryIndex{movies: map[string][]int{}, shows: map[string]int{}}
	var err error
	if idx.hintMovies, idx.hintShows, err = st.HintTMDBIDs(ctx); err != nil {
		return nil, err
	}
	paths, err := st.ListStrmPaths(ctx)
	if err != nil {
		return nil, err
	}
	moviesRoot := filepath.Join(lib.Path, lib.MoviesDir)
	showsRoot := filepath.Join(lib.Path, lib.TVDir)
	for _, p := range paths {
		if dir, ok := firstDirUnder(showsRoot, p); ok {
			title, _ := splitYear(dir)
			idx.shows[normTitle(title)]++
			continue
		}
		if dir, ok := firstDirUnder(moviesRoot, p); ok && dir != "Unsorted" {
			title, year := splitYear(dir)
			key := normTitle(title)
			idx.movies[key] = append(idx.movies[key], year)
		}
	}
	return idx, nil
}

// lookup reports whether r is in the library and, for shows, how many
// episode files it has there.
func (idx *libraryIndex) lookup(r tmdb.Result) (bool, int) {
	id := strconv.Itoa(r.ID)
	if r.MediaType == "tv" {
		n := idx.shows[normTitle(r.Name)]
		if n == 0 && r.OriginalName != "" {
			n = idx.shows[normTitle(r.OriginalName)]
		}
		return n > 0 || idx.hintShows[id], n
	}
	if idx.hintMovies[id] {
		return true, 0
	}
	year := r.Year()
	for _, t := range []string{r.Title, r.OriginalTitle} {
		for _, y := range idx.movies[normTitle(t)] {
			// Release names often carry the festival or home-release year,
			// one off from TMDB's date.
			if y == 0 || year == 0 || (y >= year-1 && y <= year+1) {
				return true, 0
			}
		}
	}
	return false, 0
}

// firstDirUnder returns the first path element of p below root.
func firstDirUnder(root, p string) (string, bool) {
	rel, err := filepath.Rel(root, p)
	if err != nil || rel == "." || strings.HasPrefix(rel, "..") {
		return "", false
	}
	dir, _, ok := strings.Cut(filepath.ToSlash(rel), "/")
	return dir, ok
}

// splitYear splits "Title (2021)" into its title and year.
func splitYear(s string) (string, int) {
	if m := folderYear.FindStringSubmatch(s); m != nil {
		y, _ := strconv.Atoi(m[2])
		return m[1], y
	}
	return s, 0
}

// normTitle folds a title to lowercase letters and digits separated by
// single spaces, so "Spider-Man: No Way Home" (TMDB) and "Spider Man No Way
// Home" (a parsed release name) compare equal.
func normTitle(s string) string {
	s = strings.ReplaceAll(strings.ToLower(s), "&", " and ")
	s = strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return r
		}
		if r == '\'' || r == '’' {
			return -1 // "Ocean's" == "Oceans"
		}
		return ' '
	}, s)
	return strings.Join(strings.Fields(s), " ")
}
