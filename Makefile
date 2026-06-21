.PHONY: all build test test-race test-cover lint lint-go fmt tidy clean help

all: build

help:
	@echo "fleet — the Elcano Mega Box monorepo"
	@echo "  make build       build all Go packages and commands"
	@echo "  make test        run the Go test suite"
	@echo "  make test-race   run the Go test suite with the race detector"
	@echo "  make lint        run golangci-lint"
	@echo "  make fmt         gofmt the tree"
	@echo "  make tidy        go mod tidy"

build:
	go build ./...

test:
	go test -p 1 ./...

test-race:
	go test -race -p 1 ./...

test-cover:
	go test -cover -p 1 ./...

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
