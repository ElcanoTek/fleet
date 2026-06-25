.PHONY: all build compile bins test test-race test-cover lint lint-go fmt tidy clean help

all: build

help:
	@echo "fleet — build/test/lint targets"
	@echo "  make build       compile-check ./... AND emit ./fleet + ./fleet-admin"
	@echo "  make bins        emit ./fleet + ./fleet-admin only (no full compile-check)"
	@echo "  make compile     go build ./...   (compile-check every package; no artifacts)"
	@echo "  make test        run the Go test suite"
	@echo "  make test-race   run the Go test suite with the race detector"
	@echo "  make lint        run golangci-lint"
	@echo "  make fmt         gofmt the tree"
	@echo "  make tidy        go mod tidy"

# build is the canonical target: it BOTH compile-checks every package (the CI
# gate AGENTS.md documents) AND emits the two deployable artifacts the README +
# deploy/update path install (./fleet, ./fleet-admin). `go build ./...` alone
# discards command binaries, so the `-o` lines are what actually leave artifacts
# on disk — without them scripts/update.sh would rebuild, report success, and
# restart the UNCHANGED old binary.
build: compile bins

# compile-check every package (no artifacts emitted — `go build ./...` discards
# the command binaries it produces).
compile:
	go build ./...

# emit just the two deployable artifacts (used by scripts/update.sh + bootstrap.sh).
bins:
	go build -o ./fleet ./cmd/fleet
	go build -o ./fleet-admin ./cmd/fleet-admin

# Tests run WITH the fleet_host_executor tag so the host-mode fixtures + MockMode
# tests compile. The release binary (`make build`/`bins`) is built WITHOUT it, so
# the unsandboxed host executor never ships (#159).
test:
	go test -p 1 -tags fleet_host_executor ./...

test-race:
	go test -race -p 1 -tags fleet_host_executor ./...

test-cover:
	go test -cover -p 1 -tags fleet_host_executor ./...

lint: lint-go

lint-go:
	golangci-lint run

fmt:
	gofmt -w .

tidy:
	go mod tidy

clean:
	go clean ./...
	rm -f coverage.out
