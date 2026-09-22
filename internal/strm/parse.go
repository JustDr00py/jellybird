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
	// leadingTagRe strips a "[ReleaseGroup]" prefix some fansub groups use
	// (e.g. "[SubsPlease] Show..."). It has no space before the "]", so the
	// normal edge-trimming in cleanTitle can't remove it on its own.
	leadingTagRe = regexp.MustCompile(`^\[[^\[\]]{1,40}\]\s*`)
	// yearRe finds a plausible (1930-2039) year token.
	yearRe = regexp.MustCompile(`(?:^|[\.\_\s\-\[\(])((?:19[3-9]\d|20[0-3]\d))(?:$|[\.\_\s\-\]\)])`)
	// trashRe strips release tags that pollute titles.
	trashRe = regexp.MustCompile(`(?i)\b(?:480p|720p|1080[pi]|2160p|4k|uhd|web[\.\- ]?dl|web[\.\- ]?rip|web|br?rip|blu[\.\- ]?ray|bdrip|dvdrip|dvd|hdtv|hdts|cam|screener|x264|x265|h\.?264|h\.?265|hevc|avc|xvid|divx|aac2?\.?0|ac3|eac3|dts(\-hd)?|truehd|atmos|ddp?5?\.?1|10bit|8bit|hdr10(\+)?|dv|dolby[\.\- ]?vision|dolby[\.\- ]?atmos|repack|proper|real|extended|remastered|unrated|imax|multi|dubbed|subbed|internal|limited|remux|nordic|german|french|italian|spanish|dutch|complete|season|part\s?\d+)\b`)
	// tokenRe splits release name into tokens.
	tokenRe = regexp.MustCompile(`[\.\_ ]+`)
)

// Parse extracts title/season/episode/year from a release or file name.
// The name may be a full path; only the base name (extension stripped) is used.
func Parse(name string) Parsed {
	base := strings.TrimSuffix(filepath.Base(name), filepath.Ext(name))
	base = strings.TrimSpace(base)
	base = leadingTagRe.ReplaceAllString(base, "")
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

	// Season pack (no episode).
	if m := seasonPackRe.FindStringSubmatch(base); m != nil {
		p.Kind = KindTV
		p.Season = atoi(m[1])
		p.ShowTitle = cleanTitle(base, m[0])
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
