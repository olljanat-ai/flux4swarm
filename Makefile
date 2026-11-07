BIN ?= bin/flux4swarm

.PHONY: build test vet lint

build:
	CGO_ENABLED=0 go build -trimpath -o $(BIN) ./cmd/flux4swarm

test:
	go test ./...

vet:
	go vet ./...

lint: vet
	@test -z "$$(gofmt -l .)" || (gofmt -l . && exit 1)

-include test/e2e/e2e.mk
