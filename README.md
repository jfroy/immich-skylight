# immich-skylight

Push photos from [Immich](https://immich.app) to a [Skylight](https://myskylight.com) frame.

Favorite a photo (or tag it) in Immich → it shows up on the frame a few minutes later.
Un-favorite it → optionally removed from the frame.

## How it works

```
┌────────┐  search/metadata   ┌─────────────────┐  upload_url + S3 PUT  ┌──────────┐
│ Immich │ ─────────────────▶ │ immich-skylight │ ────────────────────▶ │ Skylight │
│        │ ◀───────────────── │   (daemon)      │                       │  frame   │
└────────┘  thumbnail/original└───────┬─────────┘                       └──────────┘
                                      │ state.json: tokens + {assetID → messageIDs}
```

1. **Select** – queries Immich for favorites and/or assets carrying configured tags
   (`POST /api/search/metadata`), unioned and de-duplicated.
2. **Fetch** – downloads Immich's `preview` rendition by default: a JPEG that already
   handles HEIC/RAW conversion and is plenty for a frame's display. `fullsize` and
   `original` are available; anything Skylight can't accept falls back to `preview`.
3. **Upload** – uses the private Skylight app API (the one the official app uses):
   headless OAuth login with your email/password, `POST /api/upload_url` for a
   pre-signed URL, then a direct `PUT` of the bytes. Refresh tokens are persisted and
   rotated; on failure it re-logs-in with your password.
4. **Remember** – every uploaded asset is written to `state.json` with its Skylight
   message IDs so nothing is ever sent twice, and so it can be deleted later.
5. **Reverse sync** (optional, `REMOVE_UNSELECTED=true`) – photos no longer
   favorited/tagged in Immich are deleted from the frame.

> **Caveat:** Skylight has no public API. This talks to the same endpoints the Skylight
> web/mobile app uses and could break if Skylight changes them. Use against your own
> account only.

## Quick start (Docker)

```bash
git clone https://github.com/jfroy/immich-skylight && cd immich-skylight
cp .env.example .env && $EDITOR .env
docker compose run --rm immich-skylight frames      # find your frame ID (skip if you have one frame)
DRY_RUN=true docker compose run --rm immich-skylight sync   # see what would be sent
docker compose up -d                                # run the daemon
docker compose logs -f
```

## Quick start (binary)

```bash
make build
cp .env.example .env && $EDITOR .env
make run ARGS=frames
make run ARGS=sync      # one-shot
make run                # daemon
```

## Configuration

All configuration is via environment variables.

| Variable | Default | Description |
|---|---|---|
| `IMMICH_URL` | — | Immich server URL. **Required.** |
| `IMMICH_API_KEY` | — | Immich API key (Account Settings → API Keys). **Required.** Needs `asset.read`, `asset.download`, `tag.read`, `user.read`. |
| `IMMICH_FAVORITES` | `true` | Sync favorited photos. |
| `IMMICH_TAGS` | — | Comma-separated tag names or full paths (`Skylight`, `Family/Skylight`). Case-insensitive. Any listed tag qualifies. |
| `IMMICH_IMAGE_SOURCE` | `preview` | `preview` (~1440px JPEG), `fullsize`, or `original`. Falls back down the chain if unavailable/unsupported. |
| `INCLUDE_VIDEOS` | `false` | Also send MP4/MOV originals. |
| `SKYLIGHT_EMAIL` / `SKYLIGHT_PASSWORD` | — | Your Skylight account. **Required.** |
| `SKYLIGHT_FRAME_IDS` | — | Comma-separated frame IDs. Auto-selected if the account has exactly one frame. |
| `SKYLIGHT_FRAME_NAMES` | — | Alternative to IDs; case-insensitive match on the frame name. |
| `SKYLIGHT_CAPTION` | `true` | Use the Immich description as the photo caption. |
| `REMOVE_UNSELECTED` | `false` | Delete photos from the frame when they stop being selected in Immich. |
| `SYNC_INTERVAL` | `15m` | Poll interval for `run`. |
| `STATE_FILE` | `/data/state.json` | Where tokens and sent-asset records live. Back this up if you care about `REMOVE_UNSELECTED`. |
| `DRY_RUN` | `false` | Log what would be uploaded/removed without touching Skylight. |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error`. |

Either `IMMICH_FAVORITES=true` or at least one `IMMICH_TAGS` entry must be set.

## Commands

| Command | Description |
|---|---|
| `run` | Daemon: sync every `SYNC_INTERVAL` until SIGINT/SIGTERM. |
| `sync` | Single pass, then exit (for cron). |
| `frames` | Log in and print frame IDs/names. Only needs `SKYLIGHT_EMAIL`/`SKYLIGHT_PASSWORD`. |

## Notes

- Photos are uploaded oldest-first so the frame's feed stays chronological.
- Assets already on the frame before you started using this tool are unknown to it and
  never touched.
- `state.json` contains your Skylight refresh token; it's written `0600`. Treat it as a secret.
- If Skylight changes its login page, `frames`/`run` will fail with
  "did not contain a CSRF token" — that's the signal the auth flow needs updating
  (`internal/skylight/auth.go`).

## Development

```bash
go test -race ./...     # end-to-end tests against fake Immich + Skylight servers
go vet ./...
```

Stdlib only, no external dependencies.
