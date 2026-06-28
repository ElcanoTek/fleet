.PHONY: all build compile bins test test-race test-cover lint lint-go fmt tidy clean help \
	govulncheck ci-go ci-web ci-e2e-mocked ci-local

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
	@echo ""
	@echo "CI-mirroring convenience targets (run the SAME commands CI runs — see docs/TESTING.md):"
	@echo "  make govulncheck   dependency-CVE scan (CI 'go' job)"
	@echo "  make ci-go         the full Go CI job: build + vet + lint + test + test-race + govulncheck"
	@echo "  make ci-web        the Web CI job: npm ci + lint + vitest + build (in web/)"
	@echo "  make ci-e2e-mocked the mocked Playwright CI job (in web/)"
	@echo "  make ci-local      the fast PR gates locally: ci-go + ci-web (no e2e)"

# build is the canonical target: it BOTH compile-checks every package (the CI
# gate AGENTS.md documents) AND emits the two deployable artifacts the README +
# deploy/update path install (./fleet, ./fleet-admin). `go build ./...` alone
# discards command binaries, so the `-o` lines are what actually leave artifacts
# on disk — without them scripts/update.sh would rebuild, report success, and
# restart the UNCHANGED old binary.
build: compile bins

# The release version, single-sourced from the top-level VERSION file, stamped
# into both binaries below via `-ldflags -X` (see internal/version). `compile`
# and the CI compile-check intentionally DON'T stamp it — a bare `go build`
# falls back to the "dev" sentinel + VCS revision, which is honest for an
# unstamped build. $(file <VERSION) reads the file without spawning a shell
# (GNU Make 4.x); the strip drops the trailing newline.
VERSION := $(strip $(file < VERSION))
VERSION_PKG := github.com/ElcanoTek/fleet/internal/version
VERSION_LDFLAGS := -X $(VERSION_PKG).version=$(VERSION)

# compile-check every package (no artifacts emitted — `go build ./...` discards
# the command binaries it produces).
compile:
	go build ./...

# emit just the two deployable artifacts (used by scripts/update.sh + bootstrap.sh).
bins:
	go build -ldflags "$(VERSION_LDFLAGS)" -o ./fleet ./cmd/fleet
	go build -ldflags "$(VERSION_LDFLAGS)" -o ./fleet-admin ./cmd/fleet-admin

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

# ---------------------------------------------------------------------------
# CI-mirroring convenience targets
#
# These delegate to the EXACT commands .github/workflows/ci.yml runs so a
# contributor can reproduce "the same thing CI runs" locally. docs/TESTING.md
# documents each CI lane and how these targets map onto it, including the two
# Postgres DSNs the Go suites require (FLEET_TEST_DATABASE_URL /
# CHAT_TEST_DATABASE_URL for the chat suites, DATABASE_URL for the sched suite)
# and FLEET_CLIENT_CONFIG_DIR=config/default. Set those in your environment
# before `make ci-go` / `make ci-local`; see docs/TESTING.md for the values.
# ---------------------------------------------------------------------------

# Dependency-CVE scan — the CI 'go' job's govulncheck step, verbatim. Pinned to
# @latest exactly as CI does (CI: `go run golang.org/x/vuln/cmd/govulncheck@latest ./...`).
govulncheck:
	go run golang.org/x/vuln/cmd/govulncheck@latest ./...

# The full Go CI job, in CI's order: build (release config, host executor NOT
# compiled in) → vet (tagged) → lint → test → test-race → govulncheck. Each step
# below reuses an existing target that already carries the exact CI flags
# (-p 1 -tags fleet_host_executor, etc.). `compile` is `go build ./...`, the same
# release-config compile-check CI runs as its "go build" step.
ci-go: compile
	go vet -tags fleet_host_executor ./...
	$(MAKE) lint
	$(MAKE) test
	$(MAKE) test-race
	$(MAKE) govulncheck

# The Web CI job, verbatim, run from web/: npm ci → lint → vitest → build.
ci-web:
	cd web && npm ci && npm run lint && npx vitest run && npm run build

# The mocked Playwright CI job, run from web/. Assumes browsers are installed
# (`cd web && npx playwright install --with-deps chromium`); CI installs them in
# a dedicated step. Uses the npm script that pins --project=mocked.
ci-e2e-mocked:
	cd web && npm run test:e2e:mocked

# The fast PR gates, locally: the Go job + the Web job. Excludes the Playwright
# and live/e2e lanes (they need browsers / a real sandbox); run those explicitly
# with ci-e2e-mocked or the e2e scripts documented in docs/TESTING.md.
ci-local: ci-go ci-web
