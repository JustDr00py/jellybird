// Package strm parses release names into Jellyfin-friendly library layouts
// and writes .strm files pointing at the jellybird gateway.
package strm

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// MediaKind classifies a parsed release.
type MediaKind string

const (
	KindMovie MediaKind = "movie"
	KindTV    MediaKind = "tv"
	KindUnknown MediaKind = "unknown"
)

// Parsed is the result of release-name parsing.
type Parsed struct {
	Kind    MediaKind
	Title   string
	Year    int
	Season  int
	Episode int
	// ShowTitle is set for TV episodes (may equal Title).
	ShowTitle string
	// SeasonAssumed marks a Season of 1 that was guessed because the name
	// had an episode number but no season at all. Callers with better
	// context (torrent name, "Season NN" folder) should override it.
	SeasonAssumed bool
}

var (
	// sxeRe matches S01E02 / s01.e02 / 1x02 / "S01 E04" markers. Token
	// boundaries are required on both sides and an E/X letter between the
	// numbers is mandatory, so resolution ("1920x1080"), codec
	// ("2021.x265") and spaced-year ("8 2018") fragments never match.
	sxeRe = regexp.MustCompile(`(?i)(?:^|[\.\_\s\-\[\(])s?(\d{1,2})[\.\_\- ]?([ex])(\d{1,3})(?:$|[\.\_\s\-\]\)])`)
	// multiEpRe matches S01E02E03 / S01E02-E04 multi-episode markers.
	multiEpRe = regexp.MustCompile(`(?i)(?:^|[\.\_\s\-\[\(])s?(\d{1,2})[\.\_\- ]?e(\d{1,3})(?:[\.\_\- ]?e(\d{1,3}))+(?:$|[\.\_\s\-\]\)])`)
	// dashEpRe matches fansub-style "S2 - 04" (season/episode joined by a
	// bare dash, no E letter) as used by anime release groups.
	dashEpRe = regexp.MustCompile(`(?i)(?:^|[\.\_\s\-\[\(])s(\d{1,2})\s*-\s*(\d{1,3})(?:$|[\.\_\s\-\[\(\)\]])`)
	// seasonPackRe matches "Season 1", "S01", "Season.1".
	seasonPackRe = regexp.MustCompile(`(?i)(?:^|[\.\_\s\-\[])(?:s|season[\.\_\s]?)(\d{1,2})(?:$|[\.\_\s\-\]])`)
	// seriesRangeRe matches "Season(s) N-M" / "Series N-M": batch/complete
	// releases that describe an episode or season range instead of the
	// single-number form seasonPackRe handles ("Seasons 1-14", "Complete
	// Series 1-6"). The range itself isn't used (per-file episode numbers
	// come from bareEpisodeRe via EpisodeFromFileName); it only confirms TV.
	seriesRangeRe = regexp.MustCompile(`(?i)(?:^|[\.\_\s\-\[\(])(?:seasons?|series)[\.\_\s]+(\d{1,3})[\.\_\s\-]+(\d{1,3})(?:$|[\.\_\s\-\]\)])`)
	// rangeBatchRe matches the same episode-range idiom with the range
	// before the keyword instead of after ("1-51 Batch", "1-6 Complete").
	rangeBatchRe = regexp.MustCompile(`(?i)(?:^|[\.\_\s\-\[\(])(\d{1,3})[\.\_\s\-]+(\d{1,3})[\.\_\s]+(?:batch|complete)(?:$|[\.\_\s\-\]\)])`)
	// completeSeriesRe is a keyword-only fallback for batch releases that
	// name no numeric range at all ("... Complete TV Series ...").
	completeSeriesRe = regexp.MustCompile(`(?i)\bcomplete[\.\_\s]+tv[\.\_\s]+series\b`)
	// fansubDashEpRe matches the common fansub convention of a bare
	// dash-separated episode number with no season marker at all
	// ("Show - 02 (1080p) [hash]", "Show - 73 - Episode Title"). Season is
	// assumed to be 1. Unlike dashEpRe, no leading "s\d" is required, so
	// this must stay narrower (separators required on both sides of the
	// dash) to avoid swallowing "Title - Subtitle"-style movie names. A
	// double-episode range ("Show - 88-89 (720p)") is accepted and filed
	// under its first episode.
	fansubDashEpRe = regexp.MustCompile(`(?i)[\.\_\s]-[\.\_\s](\d{1,3})(?:-\d{1,3})?(?:[\.\_\s]|$)`)
	// episodeWordRe matches a spelled-out "Episode 01" / "Ep.3" / "Episode
	// 3v2" marker with no season at all (season assumed 1), as used by some
	// batch encoders. The keyword is required, so bare numbers never match.
	episodeWordRe = regexp.MustCompile(`(?i)(?:^|[\.\_\s\-\[\(])(?:episode|ep)[\.\_\s]?(\d{1,3})(?:v\d)?(?:$|[\.\_\s\-\]\)])`)
	// leadingTagRe strips a "[ReleaseGroup]" prefix some fansub groups use
	// (e.g. "[SubsPlease] Show..."). It has no space before the "]", so the
	// normal edge-trimming in cleanTitle can't remove it on its own.
	leadingTagRe = regexp.MustCompile(`^\[[^\[\]]{1,40}\]\s*`)
	// leadingIndexRe strips a zero-padded track/disc index some box-set and
	// collection torrents prefix each file with ("01 - Title", "002 Title").
	// Real movie/show titles never start with a leading zero, so this is
	// safe to remove unconditionally.
	leadingIndexRe = regexp.MustCompile(`^0\d{1,2}[\.\-\s]+`)
	// yearRe finds a plausible (1930-2039) year token.
	yearRe = regexp.MustCompile(`(?:^|[\.\_\s\-\[\(])((?:19[3-9]\d|20[0-3]\d))(?:$|[\.\_\s\-\]\)])`)
	// trashRe strips release tags that pollute titles.
	trashRe = regexp.MustCompile(`(?i)\b(?:480p|720p|1080[pi]|2160p|4k|uhd|web[\.\- ]?dl|web[\.\- ]?rip|web|br?rip|blu[\.\- ]?ray|bdrip|dvdrip|dvd|hdtv|hdts|cam|screener|x264|x265|h\.?264|h\.?265|hevc|avc|xvid|divx|aac2?\.?0|ac3|eac3|dts(\-hd)?|truehd|atmos|ddp?5?\.?1|10bit|8bit|hdr10(\+)?|dv|dolby[\.\- ]?vision|dolby[\.\- ]?atmos|repack|proper|real|extended|remastered|unrated|imax|multi|dubbed|subbed|internal|limited|remux|enhanced|nordic|german|french|italian|spanish|dutch|complete|season|part\s?\d+)\b`)
	// tokenRe splits release name into tokens.
	tokenRe = regexp.MustCompile(`[\.\_ ]+`)
	// bareEpisodeRe matches a standalone episode number in a filename that
	// carries no S/E letter marker of its own (e.g. "01.mkv", "Episode
	// 03.mkv", "3 - Title.mkv"). Only meaningful once the torrent-level
	// name has already confirmed the release is TV, so it's used solely by
	// EpisodeFromFileName.
	bareEpisodeRe = regexp.MustCompile(`(?i)(?:^|[\.\_\s\-\[\(])(?:e|ep|episode)?[\.\_\s]?(\d{1,3})(?:$|[\.\_\s\-\]\)])`)
)

// EpisodeFromFileName extracts a bare episode number from a season-pack
// member file that carries no season/episode marker of its own (the torrent
// name is what identified it as TV). Returns 0 when none is found.
func EpisodeFromFileName(name string) int {
	base := strings.TrimSuffix(filepath.Base(name), filepath.Ext(name))
	if m := bareEpisodeRe.FindStringSubmatch(base); m != nil {
		return atoi(m[1])
	}
	return 0
}

// SeasonFromPath reports the season number from the file's immediate parent
// directory when it's named "Season NN" (or "SNN"). Some torrents nest
// absolute-numbered specials/movies under a season folder without repeating
// the season anywhere in the filename or torrent name itself, leaving the
// directory as the only machine-readable TV signal at all.
func SeasonFromPath(path string) (season int, ok bool) {
	dir := filepath.Base(filepath.Dir(path))
	if dir == "." || dir == "/" || dir == "" {
		return 0, false
	}
	if m := seasonPackRe.FindStringSubmatch(dir); m != nil {
		return atoi(m[1]), true
	}
	return 0, false
}

// Parse extracts title/season/episode/year from a release or file name.
// The name may be a full path; only the base name (extension stripped) is used.
func Parse(name string) Parsed {
	base := strings.TrimSuffix(filepath.Base(name), filepath.Ext(name))
	base = strings.TrimSpace(base)
	base = leadingTagRe.ReplaceAllString(base, "")
	base = leadingIndexRe.ReplaceAllString(base, "")
	p := Parsed{Kind: KindUnknown}

	if base == "" {
		return p
	}

	// Multi-episode releases are TV.
	if m := multiEpRe.FindStringSubmatch(base); m != nil {
		p.Kind = KindTV
		p.Season = atoi(m[1])
		p.Episode = atoi(m[2])
		p.ShowTitle = cleanTitle(base, m[0])
		p.Title = p.ShowTitle
		return p
	}

	if m := sxeRe.FindStringSubmatch(base); m != nil && !isCodecFragment(m[2], m[3]) {
		p.Kind = KindTV
		p.Season = atoi(m[1])
		p.Episode = atoi(m[3])
		p.ShowTitle = cleanTitle(base, m[0])
		// Episode files can carry the show year too.
		p.Year = findYear(p.ShowTitle)
		p.ShowTitle = stripYear(p.ShowTitle)
		p.Title = p.ShowTitle
		return p
	}

	// Fansub-style "S2 - 04" (dash joins season/episode, no E letter).
	if m := dashEpRe.FindStringSubmatch(base); m != nil {
		p.Kind = KindTV
		p.Season = atoi(m[1])
		p.Episode = atoi(m[2])
		p.ShowTitle = cleanTitle(base, m[0])
		p.Title = p.ShowTitle
		return p
	}

	// Fansub-style "Show - 02 (1080p) [hash]" (bare dash episode, no
	// season digit at all — season assumed 1).
	if m := fansubDashEpRe.FindStringSubmatch(base); m != nil {
		p.Kind = KindTV
		p.Season = 1
		p.Episode = atoi(m[1])
		p.ShowTitle = cleanTitle(base, m[0])
		p.Title = p.ShowTitle
		return p
	}

	// Spelled-out "Episode 01" with no season. A release year after the
	// marker means a movie title that happens to contain the word
	// ("Star.Wars.Episode.1.The.Phantom.Menace.1999"), so that's skipped.
	// A season named earlier in the same name ("Show Season 04. Episode
	// 02") wins; otherwise season 1 is only a guess (SeasonAssumed).
	if m := episodeWordRe.FindStringSubmatchIndex(base); m != nil && !yearRe.MatchString(base[m[1]-1:]) {
		marker := base[m[0]:m[1]]
		p.Kind = KindTV
		p.Episode = atoi(base[m[2]:m[3]])
		if sm := seasonPackRe.FindStringSubmatchIndex(base[:m[0]+1]); sm != nil {
			p.Season = atoi(base[sm[2]:sm[3]])
			p.ShowTitle = stripYear(cleanTitle(base, base[sm[0]:sm[1]]))
		} else {
			p.Season = 1
			p.SeasonAssumed = true
			p.ShowTitle = cleanTitle(base, marker)
		}
		p.Title = p.ShowTitle
		return p
	}

	// Season pack (no episode).
	if m := seasonPackRe.FindStringSubmatch(base); m != nil {
		p.Kind = KindTV
		p.Season = atoi(m[1])
		p.ShowTitle = cleanTitle(base, m[0])
		p.ShowTitle = stripYear(p.ShowTitle)
		p.Title = p.ShowTitle
		return p
	}

	// Batch/complete-series releases that name an episode or season range
	// instead of a single season number ("Seasons 1-14", "1-51 Batch",
	// "Complete TV Series"). Per-file episode numbers still come from
	// EpisodeFromFileName in the writer's rescue path; this only confirms
	// the release is TV so that rescue actually fires.
	if m := seriesRangeRe.FindStringSubmatch(base); m != nil {
		p.Kind = KindTV
		p.ShowTitle = cleanTitle(base, m[0])
		p.Year = findYear(p.ShowTitle)
		p.ShowTitle = stripYear(p.ShowTitle)
		p.Title = p.ShowTitle
		return p
	}
	if m := rangeBatchRe.FindStringSubmatch(base); m != nil {
		p.Kind = KindTV
		p.ShowTitle = cleanTitle(base, m[0])
		p.Year = findYear(p.ShowTitle)
		p.ShowTitle = stripYear(p.ShowTitle)
		p.Title = p.ShowTitle
		return p
	}
	if m := completeSeriesRe.FindStringSubmatch(base); m != nil {
		p.Kind = KindTV
		p.ShowTitle = cleanTitle(base, m[0])
		p.Year = findYear(p.ShowTitle)
		p.ShowTitle = stripYear(p.ShowTitle)
		p.Title = p.ShowTitle
		return p
	}

	// Movie: title up to year, else first trash tag.
	if y := yearRe.FindStringSubmatch(base); y != nil {
		p.Kind = KindMovie
		p.Year = atoi(y[1])
		p.Title = cleanTitle(base, y[1])
		return p
	}
	if loc := trashRe.FindStringIndex(base); loc != nil {
		p.Kind = KindMovie
		p.Title = cleanTitle(base, base[loc[0]:])
		return p
	}
	p.Kind = KindMovie
	p.Title = cleanTitle(base, "")
	return p
}

// cleanTitle removes junk before/after the matched marker and normalizes
// separators. marker is the part of the name that ended the title.
func cleanTitle(name, marker string) string {
	t := name
	if marker != "" {
		if i := strings.Index(t, marker); i >= 0 {
			t = t[:i]
		}
	}
	t = strings.Trim(t, "._-[]() ")
	tokens := tokenRe.Split(t, -1)
	var keep []string
	for _, tok := range tokens {
		if tok == "" || trashRe.MatchString(tok) {
			continue
		}
		keep = append(keep, tok)
	}
	// Trailing bare digits are NOT trimmed: they are usually part of the
	// title ("Deadpool 2", "Zombieland 2"); episode markers were already
	// consumed above.
	title := strings.Join(keep, " ")
	title = strings.Map(func(r rune) rune {
		switch r {
		case '_', '.':
			return ' '
		}
		return r
	}, title)
	return strings.TrimSpace(title)
}

// findYear locates a year token inside an already-extracted title fragment.
func findYear(s string) int {
	if m := yearRe.FindStringSubmatch(s); m != nil {
		return atoi(m[1])
	}
	return 0
}

// stripYear removes a trailing year from a show title.
func stripYear(s string) string {
	if m := yearRe.FindStringIndex(s); m != nil {
		return strings.TrimSpace(strings.TrimSuffix(s[:m[0]], " ._-"))
	}
	return s
}

func atoi(s string) int {
	n, _ := strconv.Atoi(strings.TrimLeft(s, "0"))
	return n
}

// isCodecFragment rejects NxNN matches where the letter is "x" and the
// episode digits are a video codec version — the classic false positive is
// an audio tag followed by the codec: "DDP.7.1.x265" reads as "1x265".
// Legit episodes formatted "1x264" (season 1, episode 264) are vanishingly
// rare compared to codec collisions, so they are sacrificed.
func isCodecFragment(letter, episode string) bool {
	return (letter == "x" || letter == "X") &&
		(episode == "264" || episode == "265")
}

// Layout returns the library-relative path for a parsed release file.
// For movies: Movies/Title (Year)/Title (Year).strm
// For TV:     Shows/Title/Season NN/Title SxxExx.strm
// ext should include the dot.
func (p Parsed) Layout(fileName, moviesDir, tvDir, ext string) string {
	switch p.Kind {
	case KindTV:
		show := sanitize(p.ShowTitle)
		if show == "" {
			show = "Unknown Show"
		}
		season := p.Season
		if season == 0 {
			season = 1
		}
		ep := p.Episode
		var base string
		if ep > 0 {
			base = sanitize(p.ShowTitle) + " " + epCode(season, ep) + ext
		} else {
			// Whole-file season pack entry: keep original name, prettified.
			stem := strings.NewReplacer(".", " ", "_", " ").Replace(
				strings.TrimSuffix(fileName, filepath.Ext(fileName)))
			base = sanitize(stem) + ext
		}
		return filepath.Join(tvDir, show, "Season "+pad2(season), base)
	case KindMovie:
		title := sanitize(p.Title)
		if title == "" {
			title = "Unknown"
		}
		if p.Year > 0 {
			return filepath.Join(moviesDir, title+" ("+strconv.Itoa(p.Year)+")", title+" ("+strconv.Itoa(p.Year)+")"+ext)
		}
		return filepath.Join(moviesDir, title, title+ext)
	default:
		return filepath.Join(moviesDir, "Unsorted", sanitize(strings.TrimSuffix(fileName, filepath.Ext(fileName)))+ext)
	}
}

func epCode(season, episode int) string {
	return "S" + pad2(season) + "E" + pad2(episode)
}

func pad2(n int) string {
	if n < 10 {
		return "0" + strconv.Itoa(n)
	}
	return strconv.Itoa(n)
}

// sanitize makes a string safe for file names on Linux and SMB shares.
func sanitize(s string) string {
	s = strings.TrimSpace(s)
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|', '#', '\x00':
			b.WriteRune(' ')
		default:
			b.WriteRune(r)
		}
	}
	out := strings.Join(strings.Fields(b.String()), " ")
	return out
}
