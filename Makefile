.PHONY: build test docker run
build:
	CGO_ENABLED=0 go build -trimpath -o bin/immich-skylight ./cmd/immich-skylight
test:
	go test -race ./...
docker:
	docker build -t immich-skylight:latest .
run: build
	set -a; . ./.env; set +a; STATE_FILE=$${STATE_FILE:-./data/state.json} ./bin/immich-skylight $(ARGS)
