.PHONY: all build compile bins fleet-bench install test test-race test-cover lint lint-go lint-python lint-migrations lint-actions fmt tidy clean help \
	govulncheck ci-go ci-web ci-e2e-mocked ci-local

# GOTOOLCHAIN=auto — the operator does NOT have to hand-install the pinned Go.
#
# go.mod pins an exact patch release, and that pin moves every time a Go
# security release lands, because govulncheck gates `main` and reports stdlib
# CVEs against whatever toolchain built the code. Distro Go packages lag that by
# days-to-weeks, and Fedora's `golang` additionally ships `GOTOOLCHAIN=local` in
# its go.env — which turns the lag into a hard build failure ("go.mod requires
# go >= 1.27.0 (running go 1.26.2; GOTOOLCHAIN=local)") instead of the download
# Go would otherwise do on its own.
#
# The env var takes precedence over that go.env default, so setting it here is
# what makes `fleet update` work on a stock Fedora box. It belongs in the
# Makefile rather than in each caller because every build path funnels through
# here — scripts/update.sh, scripts/fleet-upgrade.sh, bootstrap.sh, and CI all
# shell out to `make build`. (bootstrap.sh already passed GOTOOLCHAIN=auto by
# hand for exactly this reason; `fleet update` did not, so a box would install
# fine and then fail every upgrade.)
#
# `?=` so an operator who has deliberately chosen a value — an air-gapped host
# pinned to `local`, or a specific `go1.27.0` — keeps it.
GOTOOLCHAIN ?= auto
export GOTOOLCHAIN

all: build

help:
	@echo "fleet — build/test/lint targets"
	@echo "  make build       compile-check ./... AND emit ./fleet + ./fleet-admin"
	@echo "  make bins        emit ./fleet + ./fleet-admin only (no full compile-check)"
	@echo "  make install     build + install fleet (and the fleet-admin shim) to PREFIX/bin (default /usr/local)"
	@echo "  make compile     go build ./...   (compile-check every package; no artifacts)"
	@echo "  make test        run the Go test suite"
	@echo "  make test-race   run the Go test suite with the race detector"
	@echo "  make test-cover  run the Go test suite with coverage (writes coverage.out)"
	@echo "  make lint        run golangci-lint + ruff (check & format) + the migration DDL linter"
	@echo "  make lint-go     golangci-lint only"
	@echo "  make lint-python ruff check + ruff format --check (skips loudly if ruff is absent)"
	@echo "  make lint-migrations  reject dangerous DDL in changed migration files (#256)"
	@echo "  make fleet-bench build the load-testing tool (cmd/fleet-bench, #296)"
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
# fleet is the ONE unified binary (#461): `fleet serve` (or bare `fleet`) runs the
# server, every other verb is the operator CLI. fleet-admin is a thin deprecation
# shim that forwards to the same admin dispatch; it stays until the first release
# after 1.0.0 (ADR-0012) — scripts/update.sh and fleet-upgrade.sh hard-fail if it
# is not emitted, so do not drop the line below without updating them.
bins:
	go build -ldflags "$(VERSION_LDFLAGS)" -o ./fleet ./cmd/fleet
	go build -ldflags "$(VERSION_LDFLAGS)" -o ./fleet-admin ./cmd/fleet-admin

# fleet-bench: the load-testing tool (#296). A dev/ops utility, NOT part of the
# deployed runtime — `make build`/`make install` deliberately do not emit it (so
# a benchmarking tool isn't installed on every box). `make compile` still
# compile-checks it via `go build ./...`. Build it on demand for a load run.
fleet-bench:
	go build -ldflags "$(VERSION_LDFLAGS)" -o ./fleet-bench ./cmd/fleet-bench

# install puts the binaries on PATH so `fleet` and `fleet <verb>` (e.g.
# `fleet update`, `fleet status`, `fleet chat`) work without cd-ing into the
# checkout — the fix for "fleet isn't installed" on a dev box (#461). The
# systemd unit can keep ExecStart=$(BINDIR)/fleet (bare fleet still serves) or
# migrate to `fleet serve` on its own schedule; both work. The fleet-admin shim
# is installed alongside until the first release after 1.0.0 (ADR-0012; the
# scripts' upgrade path still expects it). Override the location with PREFIX (or
# BINDIR) and DESTDIR for packaging.
PREFIX ?= /usr/local
BINDIR ?= $(PREFIX)/bin
install: bins
	install -d "$(DESTDIR)$(BINDIR)"
	install -m 0755 ./fleet "$(DESTDIR)$(BINDIR)/fleet"
	install -m 0755 ./fleet-admin "$(DESTDIR)$(BINDIR)/fleet-admin"
	@echo "installed: $(DESTDIR)$(BINDIR)/fleet (+ fleet-admin shim)"

# Tests run WITH the fleet_host_executor tag so the host-mode fixtures + MockMode
# tests compile. The release binary (`make build`/`bins`) is built WITHOUT it, so
# the unsandboxed host executor never ships (#159).
test:
	go test -p 1 -tags fleet_host_executor ./...

test-race:
	go test -race -p 1 -tags fleet_host_executor ./...

# test-cover mirrors the CI 'go test' step's coverage instrumentation (issue
# #249): -coverprofile=coverage.out -covermode=atomic on the SAME tagged test
# run. CI adds -count=1 to defeat the cache; the local target leaves it off so
# repeated runs reuse the cache. Run `go tool cover -func=coverage.out` for a
# per-package table or `go tool cover -html=coverage.out` for a browsable view.
test-cover:
	go test -coverprofile=coverage.out -covermode=atomic -p 1 -tags fleet_host_executor ./...
	@go tool cover -func=coverage.out | tail -1

lint: lint-go lint-python lint-migrations lint-actions

lint-go:
	golangci-lint run

# lint-python: ruff over the 13 Python files (sandbox FileOp helper, python
# bridge, skill scripts, MCP test servers). Rule selection lives in ruff.toml so
# this and the CI job cannot disagree.
#
# Skips LOUDLY when ruff is absent rather than failing: not every contributor has
# it, and CI enforces the gate regardless (ci.yml + dev-ci.yml `python` job). The
# message names the install command so a local skip is a choice, not a surprise —
# a check that quietly does nothing is the failure mode this repo keeps writing
# post-mortems about.
lint-python:
	@if command -v ruff >/dev/null 2>&1; then \
		ruff check . && ruff format --check . ; \
	else \
		echo "ruff not installed — SKIPPING the Python lint (CI still enforces it)."; \
		echo "  install: python3 -m pip install --user 'ruff==0.15.8'"; \
	fi

# lint-migrations: reject dangerous DDL in NEW/CHANGED migration files (#256).
# Diff-scoped (vs the merge-base with origin/main), so the existing corpus is
# untouched; a no-op when no migration files changed or no base ref resolves.
lint-migrations:
	scripts/check-migrations.sh

# lint-actions: actionlint over .github/workflows/*.yml — the ~3.1k lines of
# workflow YAML that decide what every other gate on this list even runs.
#
# It is the one checker here whose subject is CI itself, and it covers ground
# no other lane does: expression syntax and type errors inside ${{ }}, unknown
# contexts, invalid `needs:`/`runs-on:`/cron, deprecated action syntax, and —
# via shellcheck — the bash inside every `run:` block. Semgrep's
# p/github-actions pack overlaps on ONE axis only (mutable action tags); it
# does not parse expressions and does not shellcheck run blocks.
#
# Skips LOUDLY when actionlint is absent, same contract as lint-python: CI
# enforces the gate regardless (ci.yml + dev-ci.yml `actions` job), so a local
# skip is a choice, not a surprise.
lint-actions:
	@if command -v actionlint >/dev/null 2>&1; then \
		actionlint; \
	else \
		echo "actionlint not installed — SKIPPING the workflow lint (CI still enforces it)."; \
		echo "  install: go install github.com/rhysd/actionlint/cmd/actionlint@v1.7.7"; \
	fi
	@if command -v shellcheck >/dev/null 2>&1; then \
		git ls-files '*.sh' | xargs shellcheck -S warning; \
	else \
		echo "shellcheck not installed — SKIPPING the shell lint (CI still enforces it)."; \
		echo "  install: dnf install ShellCheck   # or: apt-get install shellcheck"; \
	fi

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

# GO_PINNED is go.mod's exact toolchain (e.g. "go1.27.0"), derived from the pin
# rather than copied, so it cannot drift from it.
GO_PINNED := go$(shell awk '/^go /{print $$2; exit}' go.mod)

# GOVULN_TOOLCHAIN — which toolchain BUILDS the scanner (not the code).
#
# `go run <tool>@latest` resolves its toolchain from the TOOL's go.mod, and
# GOTOOLCHAIN=auto only upgrades past the local Go, never past what the tool
# asks for. govulncheck's module tracks latest-1, so on the configuration this
# repo explicitly supports — a distro Go that lags go.mod, with GOTOOLCHAIN
# fetching the pin — the scanner gets BUILT with the older Go and then refuses
# the tree it was pointed at:
#
#   package requires newer Go version go1.27 (application built with go1.26)
#
# Naming go.mod's toolchain fixes that, and is a no-op in CI, where setup-go
# installs `go-version-file: go.mod` so the local Go already IS the pin. This
# is the same coupling that forces the golangci-lint pin to move with go.mod
# (see .golangci.yml): a Go-analysing tool must be built with a Go at least as
# new as the code it reads.
#
# `$(origin ...)` keeps the deliberate-operator escape hatch that GOTOOLCHAIN's
# `?=` above promises: "file" means our own default won, so we may substitute;
# anything else (environment, command line) is a choice — an air-gapped host
# pinned to `local` — and is passed through untouched rather than turned into a
# download.
GOVULN_TOOLCHAIN := $(if $(filter file,$(origin GOTOOLCHAIN)),$(GO_PINNED),$(GOTOOLCHAIN))

# Dependency-CVE scan — the CI 'go' job's govulncheck step. Tracks @latest
# exactly as CI does, deliberately: both the scanner and its advisory database
# are meant to float, so this can fail on an unchanged tree when a new advisory
# lands. See docs/TESTING.md ("govulncheck") for why neither is pinned.
govulncheck:
	GOTOOLCHAIN=$(GOVULN_TOOLCHAIN) go run golang.org/x/vuln/cmd/govulncheck@latest ./...

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

# The Web CI job, verbatim: both npm audits → the override canary → npm ci →
# lint → typecheck → vitest → build. It said "verbatim" while running four of
# those eight, which is the wrong half of a CI==local promise: the two
# `npm audit` CVE gates, scripts/check-npm-overrides.sh, and the explicit
# `npm run typecheck` were all missing, so a contributor could go green locally
# and red on the PR. The typecheck matters most of the four — `next build`
# type-checks too, but it runs LAST, so dropping the explicit gate is what turns
# a one-line type error into a multi-minute discovery.
ci-web:
	cd web && npm audit --audit-level=low
	cd scripts/rampart-service && npm audit --audit-level=low
	scripts/check-npm-overrides.sh
	cd web && npm ci && npm run lint && npm run typecheck && npx vitest run && npm run build

# The mocked Playwright CI job, run from web/. Assumes browsers are installed
# (`cd web && npx playwright install --with-deps chromium`); CI installs them in
# a dedicated step. Uses the npm script that pins --project=mocked.
ci-e2e-mocked:
	cd web && npm run test:e2e:mocked

# The fast PR gates, locally: the Go job + the Web job. Excludes the Playwright
# and live/e2e lanes (they need browsers / a real sandbox); run those explicitly
# with ci-e2e-mocked or the e2e scripts documented in docs/TESTING.md.
ci-local: ci-go ci-web
