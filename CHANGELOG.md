# Changelog

All notable changes to fleet are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project aims to adhere to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

The current release number lives in the top-level [`VERSION`](VERSION) file — the
single source of truth that the build stamps into both binaries (run
`./fleet version` or `./fleet-admin version` to read it back). fleet has not cut
a tagged release yet, so the history below starts at the Unreleased section; no
prior versions are listed because none have shipped.

## [Unreleased]

### Added

- Top-level `VERSION` file as the single source of truth for the release number,
  stamped into the `fleet` and `fleet-admin` binaries at build time via
  `-ldflags -X` (`internal/version`). `fleet version` / `fleet-admin version`
  (also `--version`) print the version plus the VCS revision; the chat health
  summary and the orchestrator `/health` + `/api/config` endpoints report the
  same string. Builds without the ldflag (a bare `go build`) fall back to a
  `dev` sentinel and the VCS revision recovered from the Go build info.
- This `CHANGELOG.md`, in Keep a Changelog format.
