# syntax=docker/dockerfile:1
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/immich-skylight ./cmd/immich-skylight

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/immich-skylight /immich-skylight
VOLUME /data
ENV STATE_FILE=/data/state.json
USER nonroot
ENTRYPOINT ["/immich-skylight"]
CMD ["run"]
