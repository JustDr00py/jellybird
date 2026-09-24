# 🐦 jellybird

**One debrid gateway for every media server.** jellybird bridges
[Real-Debrid](https://real-debrid.com) and [TorBox](https://torbox.app) into
**Jellyfin**, **Emby** and **Silo** using the battle-tested *STRM + resolver*
pattern — no FUSE mounts, no rclone, no arr stack required.

```
            your debrid cloud                jellybird                    your media server
   ┌──────────────────────────┐    ┌────────────────────────────┐    ┌──────────────────────┐
   │ Real-Debrid  │  TorBox   │───▶│ cloud sync  →  .strm files │───▶│ Jellyfin / Emby /Silo│
   │  (cached torrents)       │◀───│ search+add  ←  web UI      │    │ plays .strm natively │
   └──────────────────────────┘    │ /stream/*   →  302 → CDN   │◀───│ on playback          │
                                   └────────────────────────────┘    └──────────────────────┘
```

## How it works

1. **Sync** — every few minutes jellybird lists the torrents in your debrid
   cloud and writes `.strm` files into a normal `Movies/` + `Shows/` folder
   structure (release names are parsed into `Movies/Title (Year)/` and
   `Shows/Title/Season NN/` layouts your server already understands).
   Torrents you delete from your debrid disappear from the library too.
2. **Play** — each `.strm` contains a `http://jellybird:8097/stream/…` URL.
   When a player hits it, jellybird fetches a fresh CDN link from your
   debrid (cached in SQLite) and answers with a `302` redirect. Seeking
   works via the CDN's native `Range` support.
3. **Search & add** — the built-in dashboard searches TMDB + Torrentio,
   marks which torrents are *instantly available* on your debrid, and adds
   them with one click. Cached content shows up in your library within
   seconds.
4. **Watchlist automation** *(optional)* — point jellybird at Jellyseerr and
   approved requests are downloaded to your debrid and marked Available
   automatically.
5. **Offline copies** *(optional)* — one click downloads a title onto the
   server in place of its `.strm`, so it keeps playing without the internet
   or your debrid. See [Offline copies](#offline-copies).

## No big drives needed

Your media lives on your debrid; jellybird only writes tiny `.strm`
pointers (about 100 bytes each). A real library of 146 movies and 35 shows,
**about 12 TB of media, takes about 12 MB on disk**. No NAS, no RAID, no
drive upgrades as the library grows — a mini PC or an old laptop is plenty
for jellybird and a media server that direct-plays.

The trade-offs: you need a debrid subscription, playback depends on your
internet connection (4K remuxes want a solid one), and transcoding still
happens on your media server's hardware. For titles you can't live without
offline, use **Keep local** to store just those on disk.

## Quick start (Docker)

```bash
cp .env.example .env             # shipped as-is; paste your debrid API key(s)
# optional: cp config.example.yaml config.yaml   (tune sync/library there)
docker compose up -d             # jellybird + Jellyfin
docker logs jellybird            # confirm "realdebrid enabled" / "torbox enabled"
```

`.env` takes the keys (at least **one** of the debrid keys is required):

```env
JELLYBIRD_REALDEBRID_API_KEY=xxxxx   # from https://real-debrid.com/apitoken
JELLYBIRD_TORBOX_API_KEY=            # or TorBox → Settings → API
JELLYBIRD_TMDB_API_KEY=              # free key, enables the search UI
```

If neither debrid key is set, the container exits with
`no provider enabled: set realdebrid.api_key or torbox.api_key` — check
`docker logs jellybird`.

Then in **Jellyfin**: Dashboard → Libraries → add `/media/Movies` (Movies)
and `/media/Shows` (Shows). Done — your debrid cloud is now your library.

## Quick start (binary)

```bash
go build ./cmd/jellybird
export JELLYBIRD_REALDEBRID_API_KEY=xxxxx    # and/or JELLYBIRD_TORBOX_API_KEY
export JELLYBIRD_LIBRARY_PATH=/srv/media
export JELLYBIRD_SERVER_EXTERNAL_URL=http://$(hostname):8097
./jellybird
```

Dashboard: `http://localhost:8097` · Health: `/healthz`

## Updating

```bash
git pull
docker compose up -d --build jellybird
```

With **podman-compose**, add `--force-recreate --no-deps`: podman-compose
only recreates a container when the compose file changes, so a rebuilt
image would otherwise be ignored and the old version keeps running.

```bash
podman compose up -d --build --force-recreate --no-deps jellybird
```

The first sync after an update applies any naming fixes: affected `.strm`
files are moved (never duplicated) and Jellyfin picks up the changes.

## The dashboard

Sign in at `http://<host>:8097` (see [Dashboard login](#dashboard-login)).

- **Library** — every tracked file with its `.strm` path, filterable by
  title and provider. **Edit** fixes a title jellybird misnamed (title,
  year, movie/TV, season/episode) and re-files it immediately. **Keep
  local** / **Save** / **Remove local** per file, a summary of local copies
  linking to **Local files**, **Sync now**, and **Wipe library** to rebuild
  every `.strm` from scratch (your debrid cloud is untouched).
- **Search & Add** — search TMDB, pick a season and episode for shows, and
  see which releases are cached on your debrid (instant) and which titles
  are already in your library. One click adds a release, named after the
  TMDB title regardless of how the release group spelled it.
- **Cloud** — your live Real-Debrid / TorBox torrents, filterable by name,
  provider, status and file count. **Remove** deletes a torrent from your
  debrid *and* its `.strm` files in one go; **Keep local** downloads a whole
  torrent (e.g. a season pack). Paste a magnet link to add it directly,
  optionally with a clean title to file it under.
- **Local files** — every file downloaded to the server: where downloads
  are saved, disk usage, free space, download progress, and the "only copy" flag for titles gone from
  your cloud. Filter by name or status, remove files one at a time or in
  bulk, cancel downloads, and retry or clear failed ones.
- **Settings** — debrid account and premium status, last sync, the
  API/stream token (for the Jellyfin plugin), **change password**, and the
  state of Jellyseerr watchlist requests.

## Jellyfin plugin

`plugin/` ships the **Jellybird** Jellyfin plugin (built from
[jellyfin-plugin-jellybird](https://github.com/JustDr00py/jellyfin-plugin-jellybird),
GPL-3.0), and the example `docker-compose.yml` mounts it into Jellyfin's
plugin folder. In Jellyfin,
open **Dashboard → Plugins → Jellybird** and set:

- **jellybird base URL** — e.g. `http://jellybird:8097`
- **Token** — your `server.token` (shown on jellybird's Settings page)

It lets you search and add debrid content without leaving Jellyfin, and
adds a **Trigger Sync** scheduled task. It talks to jellybird's API with
the `X-Jellybird-Token` header; if you change the token, update it here.

## Naming and filtering

Release names are chaos, so jellybird works hard to file things where your
media server expects them:

- **Movies vs. shows** — `S01E02`, `1x02`, multi-episode `S01E02E03`,
  fansub `Show - 02` and `S2 - 04`, double episodes `Show - 88-89`,
  spelled-out `Episode 01` / `Ep 3v2`, `Season NN` folders, and "Season(s)
  1-14" / "Complete Series" batch packs all file as TV. Titles that merely
  contain "Episode" and a year (`Star Wars Episode 1 … 1999`) stay movies.
- **Seasons** — a season named in the file, its folder or the torrent name
  is used; explicit `S00` specials go to `Season 00` (Jellyfin's Specials).
- **Extras** — samples, trailers and bonus features (`Featurettes/`,
  `Extras/`, `Behind the Scenes/`, `-trailer` style names …) never become
  library entries, however large.
- **Duplicates** — the same episode from several releases gets distinct
  names (`… [GROUP].strm`) so nothing overwrites anything; Jellyfin shows
  them as versions of one episode.
- **Added via search** — the TMDB title, year and episode you picked are
  used instead of the release name.
- **Stable** — a sync that finds nothing new rewrites no files, so
  Jellyfin's real-time monitor isn't triggered every few minutes.

Something still landing in the wrong place? Fix it with **Edit** on the
Library page, and please open an issue with the file name.

## Setup per media server

All three servers play STRM the same way — point them at the library root.

| Server | Setup |
|---|---|
| **Jellyfin** | Libraries → Add → `/media/Movies` + `/media/Shows`. Set "Real time monitoring" on. |
| **Emby** | Libraries → Add Movies/TV from the same folders. |
| **Silo** | Add `/media/Movies` + `/media/Shows` as library sources in the web UI. |

> **Docker note:** the media server and jellybird must reach each other.
> Set `server.external_url` to a hostname the *server* can resolve, e.g.
> `http://jellybird:8097` in the same compose network, or
> `http://192.168.1.50:8097` on bare metal.

## Configuration

See [`config.example.yaml`](config.example.yaml) — everything is commented.
Highlights:

| Key | Purpose |
|---|---|
| `providers.realdebrid.api_key` / `providers.torbox.api_key` | debrid credentials (either or both) |
| `library.path` | STRM tree root — your server scans this |
| `server.external_url` | URL baked into `.strm` files |
| `server.token` | shared secret for `/stream` links and the Jellyfin plugin's API calls |
| `server.admin_username` / `server.admin_password` | optional: seed (or reset) the dashboard login |
| `sync.interval` | cloud polling cadence (min 30s) |
| `tmdb.api_key` | enables the search UI (free key) |
| `watchlist.*` | Jellyseerr auto-download |
| `downloads.concurrency` / `downloads.min_free_gb` | offline copies: parallel downloads (default 1) and the free-space reserve (default 5 GB) |

Environment overrides: `JELLYBIRD_<SECTION>_<KEY>` — e.g.
`JELLYBIRD_REALDEBRID_API_KEY`, `JELLYBIRD_TORBOX_API_KEY`,
`JELLYBIRD_TMDB_API_KEY`, `JELLYBIRD_JELLYSEERR_URL`,
`JELLYBIRD_JELLYSEERR_API_KEY`, `JELLYBIRD_LIBRARY_PATH`,
`JELLYBIRD_SERVER_TOKEN`, `JELLYBIRD_ADMIN_USERNAME`,
`JELLYBIRD_ADMIN_PASSWORD`, `JELLYBIRD_DATABASE_PATH`.

### Dashboard login

The dashboard uses username/password sign-in with server-side sessions
(HttpOnly cookie, 30 days). On first run, open `http://<host>:8097/setup`
to create the admin account — if `server.token` is set, the setup form asks
for it so nobody else on the network can claim the account first. Or set
`JELLYBIRD_ADMIN_USERNAME` / `JELLYBIRD_ADMIN_PASSWORD` to create it at
startup; restarting with a new `JELLYBIRD_ADMIN_PASSWORD` resets a forgotten
password. Passwords are stored as PBKDF2-SHA256 hashes, and failed logins
are rate-limited (5 per 15 minutes per IP and per username).

`/api/*` accepts either a dashboard session or the server token in the
`X-Jellybird-Token` header, which is what the Jellybird Jellyfin plugin
uses. `/stream/*` URLs in `.strm` files carry a per-file `?sig=` signature
derived from the token, never the token itself — Jellyfin shows `.strm`
targets to every user under Media Info, and a signature only lets someone
stream that one file.

## Downloads on a NAS

By default **Keep local** copies are saved inside the library, next to the
`.strm` files. Set `downloads.path` (env `JELLYBIRD_DOWNLOADS_PATH`) to save
them somewhere else instead, such as a NAS. The library and its `.strm`
files stay where they are. Copies keep the library layout under that path:

```
/downloads/Movies/Dune Part Two (2024)/Dune Part Two (2024).mkv
/downloads/Shows/Severance/Season 02/Severance S02E03.mkv
```

1. **Mount the share.** In `docker-compose.yml`, uncomment one of the
   `downloads` volume blocks (NFS or SMB), the `downloads:/downloads` mount
   in both services and `JELLYBIRD_DOWNLOADS_PATH`, then fill in the
   `JELLYBIRD_NAS_*` values in `.env`. Rootless Podman can't mount NFS/SMB
   inside a container: mount the share on the host (e.g. in `/etc/fstab`)
   and bind it instead, `/mnt/nas/jellybird:/downloads:z` (`:ro,z` for
   Jellyfin).
2. **Point Jellyfin at it.** Add `/downloads/Movies` to your Movies library
   and `/downloads/Shows` to your Shows library, next to the `/media`
   folders. Jellyfin must see the share at the same path jellybird uses.
3. **Move older copies** (optional). Copies saved before you set the path
   stay where they are and are flagged "in library folder" on the **Local
   files** page. **Move** (per file or for a selection) copies them to the
   NAS one at a time. Each keeps playing from its old location until the
   move finishes, then the old file is deleted.

Notes:
- The share must be writable by the container (it runs as root; with SMB
  the `uid`/`gid`/`file_mode` options decide ownership).
- Partial downloads and moves are staged in
  `<downloads.path>/.jellybird-downloads/`, so finishing one is a quick
  rename, and `min_free_gb` is checked against the NAS's free space.
- If you later unset `downloads.path`, copies already on the NAS are left
  alone, but jellybird can only remove copies under the library or the
  current `downloads.path`.

## Offline copies

Everything jellybird adds is streamed from your debrid service by default.
To keep something playable without it — an internet outage, a lapsed
subscription, a torrent that gets removed — use **Keep local** on the
Library page (per file) or the Cloud page (a whole torrent, e.g. a season).

jellybird downloads the file into `<library>/.jellybird-downloads/` (outside
the folders Jellyfin scans), then moves it into place next to where the
`.strm` was — `Dune Part Two (2024).strm` becomes `Dune Part Two (2024).mkv`
— and deletes the `.strm`. Jellyfin picks up the real file on its next
library scan and can direct-play or transcode it like any local media.

- Downloads resume after restarts and dropped connections, refresh expired
  debrid links, verify the final size and refuse to start when the disk
  would drop below `downloads.min_free_gb`.
- Local copies are never deleted by sync: if the title leaves your debrid
  cloud, its local copy stays (flagged "only copy") until you remove it.
- **Remove local** deletes the file and, if the title is still in your
  cloud, puts the `.strm` back. The **Local files** page does this in bulk
  and warns before deleting an only copy.
- Copies can live on another disk or a NAS: see
  [Downloads on a NAS](#downloads-on-a-nas).
- **Save** downloads a file to the device you're browsing from. It's
  proxied through jellybird so the debrid service only ever sees the
  server's IP (Real-Debrid can flag accounts used from several IPs).

Debrid services have fair-use limits on bandwidth; downloading whole
libraries may get throttled.

## API

| Endpoint | Purpose |
|---|---|
| `GET /stream/{provider}/{torrentID}/{fileID}` | 302 redirect to CDN link |
| `GET /api/cloud` | live debrid cloud listing |
| `DELETE /api/cloud?provider=&id=` | remove a torrent from the debrid and its `.strm` files |
| `GET /api/library` | tracked STRM files |
| `GET /api/library/check?type=&tmdb_id=&season=&episode=` | is this TMDB title already in the library |
| `POST /api/library/rename` `{provider, torrent_id, title, year, media_type, season, episode}` | re-file a misnamed title |
| `POST /api/library/wipe` | delete every `.strm` and resync from scratch |
| `POST /api/sync` | trigger a sync |
| `GET /api/search?q=…` | TMDB search |
| `GET /api/tv/seasons?tmdb_id=` / `GET /api/tv/episodes?tmdb_id=&season=` | season/episode pickers |
| `GET /api/torrents?type=&tmdb_id=&season=&episode=` | cached-annotated torrents |
| `POST /api/add` `{magnet, info_hash, provider, title?, year?, media_type?, season?, episode?, tmdb_id?}` | add a magnet (optionally with the title to file it under) |
| `GET /api/accounts` | debrid accounts, premium status, last sync |
| `GET /api/requests` | watchlist pipeline state |
| `POST /api/account/password` `{current, new}` | change the dashboard password (session only) |
| `GET /api/local` | "keep local" copies and download progress |
| `POST /api/local` `{provider, torrent_id, file_id?}` | download a file (or a whole torrent) onto the server; retries failed ones |
| `POST /api/local/move` `{provider, torrent_id, file_id}` | move a copy saved in the library into `downloads.path` (runs in the background) |
| `DELETE /api/local?provider=&torrent_id=&file_id=` | cancel a download / delete a local copy (the title goes back to streaming) |
| `GET /api/download/{provider}/{torrentID}/{fileID}` | save a file to your device (served from the local copy if there is one) |
| `GET /healthz` | liveness |

`/api/*` needs a dashboard session or the `X-Jellybird-Token` header.

## FAQ

**Is a STRM a real file?** Yes — a one-line text file containing a URL.
Jellyfin/Emby/Silo treat it as a video and hand the URL to the player;
jellybird redirects to your debrid's CDN on each playback, so links never
go stale.

**Do downloads need to finish first?** No — cached (instantly available)
torrents on your debrid stream immediately. Uncached ones appear at the
next sync after the provider finishes downloading.

**Both Real-Debrid and TorBox?** Yes — enable both; the dashboard shows
which provider has each result cached and adds to the best one.

**Rate limits?** jellybird stays under the documented limits (RD 250/min,
TorBox 300/min) with token buckets, and backs off on HTTP 429.

**I deleted something but Jellyfin still shows it.** jellybird removes the
`.strm` files right away, but Jellyfin's real-time monitor often misses a
whole show folder disappearing. Run **Dashboard → Libraries → Scan All
Libraries** and it's gone.

**Do I need to sync or wipe after deleting from my debrid?** No. Removing
on the Cloud page cleans up immediately; deleting on the debrid's website
is picked up by the next sync (or **Sync now**). **Wipe library** is only
for rebuilding every path from scratch.

**Can other Jellyfin users see my token?** No. Jellyfin shows a `.strm`'s
URL under Media Info, so `.strm` files only carry a per-file signature that
lets someone stream that one file. If you ran a version from before
signatures, change `server.token` once (and update the plugin).

## Development

```bash
go test ./...       # unit + httptest suites (no network)
go build ./cmd/jellybird
```

Structure: `internal/provider` (debrid clients) · `internal/strm` (release
parser + sync) · `internal/stream` (resolver) · `internal/debrid` (engine)
· `internal/download` (offline copies) · `internal/auth` (passwords,
sessions, stream signatures) · `internal/store` (SQLite) · `internal/web`
(dashboard/API) · `internal/watchlist` (Jellyseerr).

## License

jellybird is free software, licensed under the
[GNU General Public License v3.0](LICENSE). You can use, modify and share
it; if you distribute a modified version, it must stay under the GPL with
its source available.
