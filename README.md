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
| `server.token` | optional shared secret for streaming + dashboard |
| `sync.interval` | cloud polling cadence (min 30s) |
| `tmdb.api_key` | enables the search UI (free key) |
| `watchlist.*` | Jellyseerr auto-download |

Environment overrides: `JELLYBIRD_<SECTION>_<KEY>` — e.g.
`JELLYBIRD_REALDEBRID_API_KEY`, `JELLYBIRD_TORBOX_API_KEY`,
`JELLYBIRD_TMDB_API_KEY`, `JELLYBIRD_JELLYSEERR_URL`,
`JELLYBIRD_JELLYSEERR_API_KEY`, `JELLYBIRD_LIBRARY_PATH`,
`JELLYBIRD_SERVER_TOKEN`, `JELLYBIRD_DATABASE_PATH`.

## API

| Endpoint | Purpose |
|---|---|
| `GET /stream/{provider}/{torrentID}/{fileID}` | 302 redirect to CDN link |
| `GET /api/cloud` | live debrid cloud listing |
| `GET /api/library` | tracked STRM files |
| `POST /api/sync` | trigger a sync |
| `GET /api/search?q=…` | TMDB search |
| `GET /api/torrents?type=&tmdb_id=&season=&episode=` | cached-annotated torrents |
| `POST /api/add` `{magnet, info_hash, provider}` | add a magnet |
| `GET /api/requests` | watchlist pipeline state |
| `GET /healthz` | liveness |

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

## Development

```bash
go test ./...       # unit + httptest suites (no network)
go build ./cmd/jellybird
```

Structure: `internal/provider` (debrid clients) · `internal/strm` (release
parser + sync) · `internal/stream` (resolver) · `internal/debrid` (engine)
· `internal/web` (dashboard/API) · `internal/watchlist` (Jellyseerr).

## License

TBD — pick before publishing (GPLv3 if you want Jellyfin-repo distribution).
