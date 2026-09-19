VERSION ?= dev
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS  = -s -w -X main.version=$(VERSION) -X main.commit=$(COMMIT) -X main.date=$(DATE)

.PHONY: build test lint docker docker-multiarch run
build:
	CGO_ENABLED=0 go build -trimpath -ldflags="$(LDFLAGS)" -o bin/immich-skylight ./cmd/immich-skylight
test:
	go test -race ./...
lint:
	go vet ./... && test -z "$$(gofmt -l .)"
docker:
	docker buildx build --load -t immich-skylight:$(VERSION) \
	  --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --build-arg DATE=$(DATE) .
docker-multiarch:
	docker buildx build --platform linux/amd64,linux/arm64 -t immich-skylight:$(VERSION) \
	  --build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --build-arg DATE=$(DATE) .
run: build
	set -a; . ./.env; set +a; STATE_FILE=$${STATE_FILE:-./data/state.json} ./bin/immich-skylight $(ARGS)
