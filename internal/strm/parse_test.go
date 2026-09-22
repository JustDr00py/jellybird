package strm

import "testing"

func TestParse(t *testing.T) {
	cases := []struct {
		in   string
		want Parsed
	}{
		// TV single episode
		{
			"The.Last.of.Us.S01E03.2160p.WEB-DL.DDP5.1.Atmos.H.265-MUX.mkv",
			Parsed{Kind: KindTV, Title: "The Last of Us", ShowTitle: "The Last of Us", Season: 1, Episode: 3},
		},
		{
			"breaking bad s02e05.mkv",
			Parsed{Kind: KindTV, Title: "breaking bad", ShowTitle: "breaking bad", Season: 2, Episode: 5},
		},
		{
			"Severance (2022) S02E01.mkv",
			Parsed{Kind: KindTV, Title: "Severance", ShowTitle: "Severance", Year: 2022, Season: 2, Episode: 1},
		},
		// dot-separated SxE
		{
			"Mr.Robot.S01.E04.1080p.BluRay.x264.mkv",
			Parsed{Kind: KindTV, Title: "Mr Robot", ShowTitle: "Mr Robot", Season: 1, Episode: 4},
		},
		// 1x04 style
		{
			"Firefly 1x02.mkv",
			Parsed{Kind: KindTV, Title: "Firefly", ShowTitle: "Firefly", Season: 1, Episode: 2},
		},
		// Multi-episode
		{
			"South.Park.S26E01-E02.1080p.WEB.h264.mkv",
			Parsed{Kind: KindTV, Title: "South Park", ShowTitle: "South Park", Season: 26, Episode: 1},
		},
		// Season pack — "Complete" is a release tag, show title stays clean
		{
			"The.Wire.Complete.Season.1.1080p.BluRay.x264",
			Parsed{Kind: KindTV, Title: "The Wire", ShowTitle: "The Wire", Season: 1},
		},
		// Movies
		{
			"Blade.Runner.2049.2017.2160p.UHD.BluRay.x265.HDR.mkv",
			Parsed{Kind: KindMovie, Title: "Blade Runner 2049", Year: 2017},
		},
		{
			"Dune.Part.Two.2024.1080p.WEB-DL.DDP5.1.Atmos.H.264.mkv",
			Parsed{Kind: KindMovie, Title: "Dune Part Two", Year: 2024},
		},
		{
			"Everything.Everywhere.All.at.Once.2022.mkv",
			Parsed{Kind: KindMovie, Title: "Everything Everywhere All at Once", Year: 2022},
		},
		// No year: truncate at first quality tag
		{
			"Some.Indie.Film.1080p.WEBRip.x264-RARBG.mkv",
			Parsed{Kind: KindMovie, Title: "Some Indie Film"},
		},
		// Movies that must NOT be mistaken for TV (regression: codec,
		// resolution and spaced-year fragments used to match SxxExx).
		{
			"Dune.2021.x265.1080p.WEB-DL",
			Parsed{Kind: KindMovie, Title: "Dune", Year: 2021},
		},
		{
			"Oceans 8 2018 1080p BluRay",
			Parsed{Kind: KindMovie, Title: "Oceans 8", Year: 2018},
		},
		{
			"Drive.2011.1920x1080.WEB-DL",
			Parsed{Kind: KindMovie, Title: "Drive", Year: 2011},
		},
		{
			"The.Batman.2022.2160p.WEB-DL.DDP5.1.Atmos.H.265",
			Parsed{Kind: KindMovie, Title: "The Batman", Year: 2022},
		},
		// Sequel numbers stay part of the title
		{
			"Deadpool.2.2018.1080p.WEB-DL.x264",
			Parsed{Kind: KindMovie, Title: "Deadpool 2", Year: 2018},
		},
		// Audio tag + codec ("DDP.7.1.x265") is not an episode marker
		// (regression: parsed as S01E265)
		{
			"Moana.2016.1080p.BluRay.DDP.7.1.x265-EDGE2020.mkv",
			Parsed{Kind: KindMovie, Title: "Moana", Year: 2016},
		},
		{
			"Some.Movie.2019.DTS.5.1.x264-BLURANiUM",
			Parsed{Kind: KindMovie, Title: "Some Movie", Year: 2019},
		},
		// Fansub releases: leading "[Group]" tag and dash-separated "S2 - 04"
		// (no E letter) must not pollute the title or hide the episode.
		{
			"[SubsPlease] Sousou no Frieren S2 - 04 (1080p) [698A157A].mkv",
			Parsed{Kind: KindTV, Title: "Sousou no Frieren", ShowTitle: "Sousou no Frieren", Season: 2, Episode: 4},
		},
		{
			"[Yameii] Frieren Beyond Journey's End - S02E04 [English Dub] [CR WEB-DL 1080p H264 AAC] [593CA663] (Sousou no Frieren Season 2 | S2).mkv",
			Parsed{Kind: KindTV, Title: "Frieren Beyond Journey's End", ShowTitle: "Frieren Beyond Journey's End", Season: 2, Episode: 4},
		},
	}
	for _, tc := range cases {
		got := Parse(tc.in)
		if got != tc.want {
			t.Errorf("Parse(%q)\n got  %+v\n want %+v", tc.in, got, tc.want)
		}
	}
}

func TestLayout(t *testing.T) {
	cases := []struct {
		p    Parsed
		file string
		want string
	}{
		{
			Parsed{Kind: KindMovie, Title: "Dune: Part Two", Year: 2024},
			"Dune.Part.Two.2024.1080p.mkv",
			"Movies/Dune Part Two (2024)/Dune Part Two (2024).strm",
		},
		{
			Parsed{Kind: KindTV, ShowTitle: "The Last of Us", Season: 1, Episode: 3},
			"whatever.mkv",
			"Shows/The Last of Us/Season 01/The Last of Us S01E03.strm",
		},
		{
			Parsed{Kind: KindTV, ShowTitle: "Severance", Season: 2, Episode: 10},
			"x.mkv",
			"Shows/Severance/Season 02/Severance S02E10.strm",
		},
		{
			Parsed{Kind: KindTV, ShowTitle: "The Wire", Season: 0, Episode: 0}, // season zero -> 1
			"The.Wire.S01.mkv",
			"Shows/The Wire/Season 01/The Wire S01.strm",
		},
		{
			Parsed{Kind: KindUnknown},
			"random.bin",
			"Movies/Unsorted/random.strm",
		},
	}
	for _, tc := range cases {
		got := tc.p.Layout(tc.file, "Movies", "Shows", ".strm")
		if got != tc.want {
			t.Errorf("Layout(%+v, %q)\n got  %q\n want %q", tc.p, tc.file, got, tc.want)
		}
	}
}

func TestParseFullPaths(t *testing.T) {
	got := Parse("packs/tv/Show.Name.S03E07.720p.mkv")
	if got.Kind != KindTV || got.Season != 3 || got.Episode != 7 || got.Title != "Show Name" {
		t.Errorf("path parse got %+v", got)
	}
}

func TestSanitize(t *testing.T) {
	cases := map[string]string{
		`What\: A "Quest"?`: "What A Quest",
		"  spaced   out  ":  "spaced out",
		"a/b\\c":             "a b c",
	}
	for in, want := range cases {
		if got := sanitize(in); got != want {
			t.Errorf("sanitize(%q) = %q, want %q", in, got, want)
		}
	}
}
