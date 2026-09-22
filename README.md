# immich-skylight

Push photos from [Immich](https://immich.app) to a [Skylight](https://myskylight.com) frame.

Favorite a photo (or tag it) in Immich → it shows up on the frame a few minutes later.
Un-favorite it → optionally removed from the frame.

## How it works

```
┌────────┐   search/metadata    ┌─────────────────┐   upload_url + S3 PUT   ┌──────────┐
│        │ ──────────────────▶ │                 │ ─────────────────────▶ │          │
│ Immich │                      │ immich-skylight │                         │ Skylight │
│        │ ◀────────────────── │     (daemon)    │ ─────────────────────▶ │  frame   │
│        │  thumbnail/original  │                 │  delete (reverse sync)  │          │
└────────┘                      └────────┬────────┘                         └──────────┘
                                         │
                                ┌────────┴────────┐
                                │    state.db     │
                                │  tokens, asset  │
                                │ → frame → msgs  │
                                └─────────────────┘
```

1. **Select** – queries Immich for favorites and/or assets carrying configured tags
   (`POST /api/search/metadata`, cursor-paginated). These go to every target frame. In addition, a
   **per-frame tag** (default `Skylight/<frame name>`) is created in Immich for each
   frame; tagging a photo with it sends it to that frame only.
2. **Fetch** – downloads Immich's `preview` rendition by default: a JPEG that already
   handles HEIC/RAW conversion and is plenty for a frame's display. `fullsize` and
   `original` are available; anything Skylight can't accept falls back to `preview`.
3. **Upload** – via [go-skylight](https://github.com/sebrandon1/go-skylight): headless
   OAuth login with your email/password, `POST /api/upload_url` for a pre-signed URL,
   then a direct `PUT` of the bytes. This daemon owns the credential lifecycle: refresh
   tokens are persisted and rotated, sessions refresh proactively before expiry, a 401
   triggers one re-auth + retry, and if refresh fails it re-logs-in with your password.
4. **Remember** – every uploaded asset is committed to a SQLite database (`state.db`,
   pure-Go driver, WAL mode) with its per-frame Skylight message IDs, so nothing is ever
   sent twice and photos can be deleted later. Each upload is its own transaction; a
   crash mid-pass cannot lose or duplicate a record.
5. **Reverse sync** (optional, `REMOVE_UNSELECTED=true`) – photos no longer
   favorited/tagged in Immich are deleted from the frame(s) they are no longer selected
   for, so re-tagging a photo from one frame's tag to another's moves it.
6. **Push** (optional) – an Immich Workflow with the *Asset Tagged* trigger and the
   *Webhook* action can `POST` to `/webhook` so new photos land within seconds instead
   of waiting for the next poll. See [Instant sync](#instant-sync-with-immich-workflows).

> **Caveat:** Skylight has no public API. All Skylight protocol handling is delegated to
> [go-skylight](https://github.com/sebrandon1/go-skylight), a community client that
> tracks the private app API; when Skylight changes something, bump that dependency.
> Use against your own account only.

## Container image

Images are published to `ghcr.io/jfroy/immich-skylight` for `linux/amd64` and
`linux/arm64`, built from distroless `static:nonroot`, signed with cosign (keyless),
with SLSA provenance and SBOM attached.

```bash
cosign verify ghcr.io/jfroy/immich-skylight:latest \
  --certificate-identity-regexp='^https://github.com/jfroy/immich-skylight/' \
  --certificate-oidc-issuer=https://token.actions.githubusercontent.com
```

## Choosing a frame (optional)

**If your Skylight account has exactly one frame, skip this** — it is selected
automatically. With several frames you must tell the daemon which to target, either by
name (`SKYLIGHT_FRAME_NAMES=Kitchen`) or by ID (`SKYLIGHT_FRAME_IDS=1234567`). Frame
names are what you see in the Skylight app; to list names and IDs, run the `frames`
command once with just your Skylight credentials. It makes no changes to your account.

With a container runtime (Docker or Podman):

```bash
docker run --rm \
  -e SKYLIGHT_EMAIL=you@example.com -e SKYLIGHT_PASSWORD=... \
  -e STATE_FILE=/tmp/state.db \
  ghcr.io/jfroy/immich-skylight:latest frames
```

Or straight from a checkout with Go:

```bash
SKYLIGHT_EMAIL=you@example.com SKYLIGHT_PASSWORD=... STATE_FILE=/tmp/state.db \
  go run ./cmd/immich-skylight frames
```

```
ID           NAME
1234567      Kitchen
2345678      Office
```

## Deploying with Kubernetes

`deploy/kubernetes/immich-skylight.yaml` contains a Deployment, PVC and Service.
Configuration is plain environment variables; secrets come from a Secret via
`envFrom.secretRef`:

```bash
kubectl create secret generic immich-skylight \
  --from-literal=IMMICH_API_KEY=... \
  --from-literal=SKYLIGHT_EMAIL=... \
  --from-literal=SKYLIGHT_PASSWORD=...
kubectl apply -f deploy/kubernetes/immich-skylight.yaml
```

Edit the `env` block in the manifest for `IMMICH_URL`, selection (`IMMICH_FAVORITES`,
`IMMICH_TAGS`) and, if needed, `SKYLIGHT_FRAME_NAMES`/`SKYLIGHT_FRAME_IDS`.

The pod runs as non-root (uid 65532) with a read-only root filesystem, all capabilities
dropped, `RuntimeDefault` seccomp, no service-account token, and a `ReadWriteOnce` PVC at
`/data` for `state.db` — the only path the process ever writes. Use a block/filesystem
storage class rather than NFS (SQLite WAL over NFS is unreliable). If you run the
Prometheus Operator, add a `ServiceMonitor` on the `http` port at `/metrics`.

## Deploying with Docker Compose

```bash
cd deploy/compose
cp .env.example .env && $EDITOR .env          # IMMICH_URL, IMMICH_API_KEY, SKYLIGHT_EMAIL, SKYLIGHT_PASSWORD, frame selection
DRY_RUN=true docker compose run --rm immich-skylight sync   # preview what would be sent
docker compose up -d && docker compose logs -f
```

`deploy/compose/compose.yaml` applies the same hardening (non-root, read-only rootfs,
no capabilities, `no-new-privileges`) and keeps `state.db` in a named volume.

## Running the binary directly

```bash
cp .env.example .env && $EDITOR .env
make build
make run ARGS=sync      # one-shot, DRY_RUN=true to preview
make run                # daemon

make docker             # local single-arch image
make docker-multiarch   # linux/amd64 + linux/arm64 via buildx
```

## Configuration

All configuration is via environment variables.

| Variable | Default | Description |
|---|---|---|
| `IMMICH_URL` | — | Immich server URL. **Required.** |
| `IMMICH_API_KEY` | — | Immich API key (Account Settings → API Keys). **Required.** See [API key permissions](#immich-api-key-permissions). |
| `IMMICH_FAVORITES` | `true` | Sync favorited photos. |
| `IMMICH_TAGS` | — | Comma-separated tag names or full paths (`Skylight`, `Family/Skylight`). Case-insensitive. Any listed tag qualifies. |
| `IMMICH_IMAGE_SOURCE` | `preview` | `preview` (~1440px JPEG), `fullsize`, or `original`. Falls back down the chain if unavailable/unsupported. |
| `INCLUDE_VIDEOS` | `false` | Also send MP4/MOV originals. |
| `PRUNE_FRAME_TAGS` | `false` | Delete a frame's tag from Immich once the frame is no longer a sync target. Needs `tag.delete`. |
| `IMMICH_FRAME_TAG_TEMPLATE` | `Skylight/{{ .Name }}` | Go `text/template` rendered per target frame (`.Name`, `.ID`) giving an Immich tag path; assets with that tag go only to that frame. The tags are created at startup. Set empty to disable. |
| `SKYLIGHT_EMAIL` / `SKYLIGHT_PASSWORD` | — | Your Skylight account. **Required.** |
| `SKYLIGHT_FRAME_IDS` | — | Comma-separated frame IDs. Optional: with exactly one frame on the account it is selected automatically. See [Choosing a frame](#choosing-a-frame-optional). |
| `SKYLIGHT_FRAME_NAMES` | — | Alternative to IDs; case-insensitive match on the frame name as shown in the Skylight app. |
| `SKYLIGHT_CAPTION` | `true` | Use the Immich description as the photo caption. |
| `REMOVE_UNSELECTED` | `false` | Delete photos from the frame when they stop being selected in Immich. |
| `SYNC_INTERVAL` | `15m` | Poll interval for `run`. |
| `STATE_FILE` | `/data/state.db` | SQLite database holding tokens and sent-asset records. WAL sidecar files (`-wal`, `-shm`) are created alongside. Back it up if you care about `REMOVE_UNSELECTED`. |
| `DRY_RUN` | `false` | Log what would be uploaded/removed without touching Skylight. |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error`. |
| `HTTP_ADDR` | `:8080` | Listener for `/metrics`, `/healthz`, `/readyz`, `/webhook`. |
| `WEBHOOK_SECRET` | — | Enables `POST /webhook`; requests must send this value in `X-Webhook-Secret`. |
| `OTEL_SERVICE_NAME` | `immich-skylight` | Service name in telemetry. |
| `OTEL_EXPORTER_OTLP_*` | — | Standard OTel SDK variables. Setting an endpoint enables OTLP export of traces, metrics and logs. |

### Immich API key permissions

Create the key with only these permissions (Immich → Account Settings → API Keys):

| Permission | Used for |
|---|---|
| `user.read` | `GET /users/me` — connectivity/auth check at startup |
| `asset.read` | `POST /search/metadata` — finding favorites and tagged assets |
| `asset.view` | `GET /assets/{id}/thumbnail` — the default `preview`/`fullsize` renditions |
| `asset.download` | `GET /assets/{id}/original` — `IMMICH_IMAGE_SOURCE=original`, videos, and `fullsize` when Immich redirects to the original |
| `tag.read` | `GET /tags` — resolving `IMMICH_TAGS` |
| `tag.create` | `PUT /tags` — upserting the per-frame tags (`IMMICH_FRAME_TAG_TEMPLATE`). Not needed if that is disabled. |
| `tag.update` | `PUT /tags/{id}` — renaming a frame tag when its frame is renamed in Skylight. Optional; without it a frame rename fails at startup with a clear error. |
| `tag.delete` | `DELETE /tags/{id}` — only with `PRUNE_FRAME_TAGS=true`. |

The key never modifies assets; it only reads them and manages the tags it created.

#### Frame tag lifecycle

The tag created for each frame is recorded (frame ID → tag ID) so it survives changes:

- **Frame renamed in Skylight** — the tag is renamed in place (`Skylight/Kitchen` →
  `Skylight/Living Room`); its photos follow, nothing is re-uploaded or removed.
- **Template changed** so the tag's *parent* differs (`Skylight/…` → `Frames/…`) — a new
  tag is created; the old one is left untouched and a warning reports how many assets
  still carry it. Move them yourself, then delete the old tag.
- **Frame no longer a target** (removed from `SKYLIGHT_FRAME_*` or from the account) — the
  tag stays by default and a warning is logged each start. Set `PRUNE_FRAME_TAGS=true` to
  delete it (this also unlinks it from all its photos).

At least one of `IMMICH_FAVORITES=true`, `IMMICH_TAGS`, or `IMMICH_FRAME_TAG_TEMPLATE` must
be set (all three are on by default via favorites + the frame tag template).

### Instant sync with Immich Workflows

Polling (`SYNC_INTERVAL`) always runs, but Immich ≥ 3.2 can push. Set `WEBHOOK_SECRET`
to a random string, expose the `http` port to Immich (in-cluster Service / Compose
network is enough; no ingress needed), then in Immich → *Utilities → Workflows* create:

- **Trigger:** Asset Tagged
- **Filter (optional):** Asset Tag — any of your `Skylight/*` tags and `IMMICH_TAGS`
- **Action:** Trigger Webhook
  - URL: `http://immich-skylight:8080/webhook`
  - Method: `POST`
  - Header name: `X-Webhook-Secret`, Header value: your secret

Each hit schedules an immediate sync pass (coalesced if several arrive at once). The
payload is only used as a nudge — the pass re-reads Immich as the source of truth — so a
missed or duplicate webhook is harmless. Favoriting has no workflow trigger yet; it is
picked up on the next poll.

## Observability

- **Logs** — structured JSON on stdout via `log/slog`. Records emitted inside a span carry
  `trace_id`/`span_id`. When OTLP is configured, logs are also shipped through the OTel log
  bridge.
- **Metrics** — every measurement is recorded twice: to native `prometheus/client_golang`
  collectors served on `/metrics` (plus Go/process collectors), and to OTel instruments
  pushed via `otlpmetrichttp` when OTLP is configured. Key Prometheus series:

  | Metric | Notes |
  |---|---|
  | `immich_skylight_sync_runs_total{result}` | passes by success/failure |
  | `immich_skylight_sync_duration_seconds` | histogram |
  | `immich_skylight_sync_last_success_timestamp_seconds` | alert if stale |
  | `immich_skylight_selected_assets`, `immich_skylight_tracked_assets` | gauges |
  | `immich_skylight_uploads_total{result,rendition}` | success / fetch_failed / upload_failed / dry_run |
  | `immich_skylight_upload_size_bytes` | histogram |
  | `immich_skylight_removals_total{result}` | reverse-sync deletions |
  | `immich_skylight_skylight_auth_total{kind,result}` | login / refresh events |
  | `immich_skylight_http_client_requests_total{target,status}` | immich / skylight / storage |
  | `immich_skylight_http_client_request_duration_seconds{target,status}` | histogram |

- **Traces** — one root span per sync pass (`sync.Pass`) with child spans for search,
  each asset upload, downloads, Skylight API calls and the S3 PUT (via `otelhttp`).
  Exported with `otlptracehttp` when `OTEL_EXPORTER_OTLP_ENDPOINT` (or
  `..._TRACES_ENDPOINT`) is set; otherwise sampling is off and nothing leaves the process.

## Commands

| Command | Description |
|---|---|
| `run` | Daemon: sync every `SYNC_INTERVAL` until SIGINT/SIGTERM. |
| `sync` | Single pass, then exit (for cron). |
| `frames` | Log in and print frame IDs/names. Only needs `SKYLIGHT_EMAIL`/`SKYLIGHT_PASSWORD`; read-only. |
| `version` | Print version/commit/date. |

## Notes

- Photos are uploaded oldest-first so the frame's feed stays chronological.
- Assets already on the frame before you started using this tool are unknown to it and
  never touched.
- `state.db` contains your Skylight refresh token. Treat it as a secret.
- Schema changes are versioned SQL migrations in `migrations/sqlite/` (golang-migrate,
  embedded in the binary) and are applied automatically at startup. The version in
  effect is logged as `schema_version`.
- If Skylight changes its login flow, `frames`/`run` will fail at "skylight login";
  check for a newer [go-skylight](https://github.com/sebrandon1/go-skylight) and bump it
  (`go get github.com/sebrandon1/go-skylight@main`). Renovate is configured to propose this.
- Readiness (`/readyz`) turns green only after Immich and Skylight auth and frame
  resolution succeed, so a bad secret shows up as a never-ready pod rather than a crash loop.

## Development

```bash
make lint test                    # vet, gofmt, race tests against fake Immich + Skylight servers
make migration NAME=add_widgets   # scaffold migrations/sqlite/00000N_add_widgets.{up,down}.sql
```

Migrations use [golang-migrate](https://github.com/golang-migrate/migrate) with the
`iofs` source (files embedded via `migrations/embed.go`) and its cgo-free sqlite driver.
`Open` runs `Up` and refuses to start on a dirty version. Down files exist for authoring
and manual rollback with the `migrate` CLI; the daemon never runs them.

CI (`.github/workflows`): `ci.yaml` runs vet/test/build on PRs and main; `image.yaml`
builds multi-arch on every push to main (and PRs, without pushing), pushes to GHCR with
provenance + SBOM and signs with cosign; `release.yaml` does the same for `v*` tags with
semver tags.

## License

[Apache-2.0](LICENSE)
