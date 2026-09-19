# syntax=docker/dockerfile:1.27

ARG GO_VERSION=1.27

# Cross-compile on the build host; Go handles TARGETARCH natively so no QEMU is needed.
FROM --platform=$BUILDPLATFORM golang:${GO_VERSION}-bookworm AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

ARG TARGETOS=linux
ARG TARGETARCH
ARG VERSION=dev
ARG COMMIT=unknown
ARG DATE=unknown

RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath \
      -ldflags="-s -w -X main.version=$VERSION -X main.commit=$COMMIT -X main.date=$DATE" \
      -o /out/immich-skylight ./cmd/immich-skylight

# Static, rootless, no shell. Runs fine with readOnlyRootFilesystem: the only
# write path is STATE_FILE (mount a volume at /data).
FROM gcr.io/distroless/static-debian12:nonroot AS runtime

COPY --from=build /out/immich-skylight /immich-skylight

USER nonroot:nonroot
ENV STATE_FILE=/data/state.db \
    HTTP_ADDR=:8080
EXPOSE 8080
ENTRYPOINT ["/immich-skylight"]
CMD ["run"]
