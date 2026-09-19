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
                                      │ state.db (SQLite): tokens + asset → frame → message IDs
```

1. **Select** – queries Immich for favorites and/or assets carrying configured tags
   (`POST /api/search/metadata`), unioned and de-duplicated.
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
   favorited/tagged in Immich are deleted from the frame.

> **Caveat:** Skylight has no public API. All Skylight protocol handling is delegated to
> [go-skylight](https://github.com/sebrandon1/go-skylight), a community client that
> tracks the private app API; when Skylight changes something, bump that dependency.
> Use against your own account only.

## Quick start (Kubernetes)

Images are published to `ghcr.io/jfroy/immich-skylight` for `linux/amd64` and
`linux/arm64`, built from distroless `static:nonroot`, signed with cosign (keyless),
with SLSA provenance and SBOM attached.

```bash
cosign verify ghcr.io/jfroy/immich-skylight:latest \
  --certificate-identity-regexp='^https://github.com/jfroy/immich-skylight/' \
  --certificate-oidc-issuer=https://token.actions.githubusercontent.com
```

Two deployment flavours are included:

- `deploy/flatops/` — Flux `HelmRelease` on bjw-s `app-template` + `ExternalSecret`,
  matching the [flatops](https://github.com/jfroy/flatops) layout. Drop
  `deploy/flatops/immich/skylight/` into `kubernetes/apps/default/immich/` and append
  `deploy/flatops/immich/ks.yaml` to the existing `ks.yaml`. Populate the `immich-skylight`
  secret in OpenBao with `immich_api_key`, `skylight_email`, `skylight_password`.
- `deploy/kubernetes.yaml` — plain Deployment/PVC/Service for any cluster.

Both run with `runAsNonRoot` (uid 65532), `readOnlyRootFilesystem`, all capabilities
dropped, `RuntimeDefault` seccomp, no service-account token, and a 1 Gi PVC at `/data`
for `state.db` — the only path the process ever writes. Secrets are plain env vars via
`envFrom.secretRef`.

Find your frame ID once with a one-off pod (or locally) using `frames`:

```bash
kubectl -n default run -it --rm skylight-frames --restart=Never \
  --image=ghcr.io/jfroy/immich-skylight:latest \
  --env STATE_FILE=/tmp/state.db \
  --overrides='{"spec":{"containers":[{"name":"skylight-frames","image":"ghcr.io/jfroy/immich-skylight:latest","args":["frames"],"envFrom":[{"secretRef":{"name":"immich-skylight"}}],"env":[{"name":"STATE_FILE","value":"/tmp/state.db"}]}]}}'
```

## Quick start (local binary / Docker)

```bash
cp .env.example .env && $EDITOR .env
make build
make run ARGS=frames    # list frames
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
| `STATE_FILE` | `/data/state.db` | SQLite database holding tokens and sent-asset records. WAL sidecar files (`-wal`, `-shm`) are created alongside. Back it up if you care about `REMOVE_UNSELECTED`. |
| `DRY_RUN` | `false` | Log what would be uploaded/removed without touching Skylight. |
| `LOG_LEVEL` | `info` | `debug`, `info`, `warn`, `error`. |
| `HTTP_ADDR` | `:8080` | Listener for `/metrics`, `/healthz`, `/readyz`. |
| `OTEL_SERVICE_NAME` | `immich-skylight` | Service name in telemetry. |
| `OTEL_EXPORTER_OTLP_*` | — | Standard OTel SDK variables. Setting an endpoint enables OTLP export of traces, metrics and logs. |

Either `IMMICH_FAVORITES=true` or at least one `IMMICH_TAGS` entry must be set.

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
| `frames` | Log in and print frame IDs/names. Only needs `SKYLIGHT_EMAIL`/`SKYLIGHT_PASSWORD`. |
| `version` | Print version/commit/date. |

## Notes

- Photos are uploaded oldest-first so the frame's feed stays chronological.
- Assets already on the frame before you started using this tool are unknown to it and
  never touched.
- `state.db` contains your Skylight refresh token. Treat it as a secret.
- Upgrading from the JSON state file: on first start with an empty database, a legacy
  `state.json` next to it (or `STATE_FILE` with `.json` swapped in) is imported and
  renamed `*.imported`.
- If Skylight changes its login flow, `frames`/`run` will fail at "skylight login";
  check for a newer [go-skylight](https://github.com/sebrandon1/go-skylight) and bump it
  (`go get github.com/sebrandon1/go-skylight@main`). Renovate is configured to propose this.
- Readiness (`/readyz`) turns green only after Immich and Skylight auth and frame
  resolution succeed, so a bad secret shows up as a never-ready pod rather than a crash loop.

## Development

```bash
make lint test          # vet, gofmt, race tests against fake Immich + Skylight servers
```

CI (`.github/workflows`): `ci.yaml` runs vet/test/build on PRs and main; `image.yaml`
builds multi-arch on every push to main (and PRs, without pushing), pushes to GHCR with
provenance + SBOM and signs with cosign; `release.yaml` does the same for `v*` tags with
semver tags.
