.PHONY: build
build:
	go build -o bin/flux4swarm ./cmd/flux4swarm

include test/e2e/e2e.mk
