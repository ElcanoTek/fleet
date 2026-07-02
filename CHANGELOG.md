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

- Reusable sandbox-image publish workflow (#195):
  `.github/workflows/publish-sandbox-image.yml` (`workflow_call`) lets a client
  config repo build its bundle's sandbox image in CI with the canonical
  `scripts/build-sandbox-image.sh`, push immutable `{git-sha}` + `:latest` tags
  to GHCR, and auto-open a PR pinning `sandbox.image` in its manifest — so
  deploys `podman pull` a prebuilt image instead of rebuilding ~1.3 GB on-box.
  Core CI still builds the sandbox locally (the 24ce69f decoupling stands).
- Trust-labeled MCP connector catalog (#538): a bundle can curate a directory
  of official third-party hosted MCP servers (`remote_mcp_catalog:` in
  `manifest.yaml`, validated at load), served alongside the bundled
  Optional-server catalog by the new `GET /mcp-catalog` and rendered on
  Settings → Connections with explicit "Bundled" vs "Third-party" trust badges
  and one-click add into the per-user remote-MCP OAuth flow (#443). The generic
  bundle ships a starter directory of official vendor endpoints. See
  `docs/MCP-CATALOG.md`.
- Skills, phase 1 of first-class skills (#513): `GET /skills` returns the
  client bundle's Agent Skill roster (name + description per skill), and a chat
  message starting with `/<skill-name>` (exact, case-sensitive match — unknown
  `/tokens` like paths are silently ignored) explicitly invokes that skill by
  appending an instruction block to the persisted user message, so the
  transcript records which skill was loaded. The web composer gains a `/`
  autocomplete popover over the roster (prefix filter, ↑/↓ + Enter/Tab to
  complete, Esc to dismiss) backed by a new `/api/skills` proxy. In-app
  authoring, save-from-run (DB-staged proposals with approval + operator export
  to a bundle), and project scoping are deferred to phases 2/3 — see
  `docs/SKILLS.md`.
- `fleet chat` — a terminal UI for chatting with the fleet agent (Bubble Tea /
  Lipgloss, glamour-rendered Markdown, streaming replies, tool-call + reasoning
  display, `/new` `/retry` `/model` `/reasoning` `/clear` `/quit`, Ctrl+C to
  cancel a turn). It is a thin SSE client of the running server's `POST /chat`,
  so every turn still flows through the one governed run loop, the sandbox, and
  host-side credential brokering — the TUI only renders. `fleet chat --message
  "<text>"` (or `--no-tui`) runs one turn non-interactively to stdout for
  scripts/pipes. Connection identity resolves from `--email`/`--server`/`--token-file`
  → `$FLEET_USER_EMAIL` / `$FLEET_SERVER_TOKEN`; the shared token is never logged.
- Unified the operator CLI into one `fleet` binary (`fleet serve` runs the
  server — bare `fleet` also serves, for back-compat — and `fleet <verb>` is every
  operator command); a new `make install` puts `fleet` on PATH; `fleet update
  --check` is a read-only "commits behind" report; bootstrap installs a login
  MOTD banner. The old `fleet-admin <verb>` still works for one deprecation
  release (it prints a warning and forwards to the same dispatch) and is then
  removed.
- Top-level `VERSION` file as the single source of truth for the release number,
  stamped into the `fleet` and `fleet-admin` binaries at build time via
  `-ldflags -X` (`internal/version`). `fleet version` / `fleet-admin version`
  (also `--version`) print the version plus the VCS revision; the chat health
  summary and the orchestrator `/health` + `/api/config` endpoints report the
  same string. Builds without the ldflag (a bare `go build`) fall back to a
  `dev` sentinel and the VCS revision recovered from the Go build info.
- This `CHANGELOG.md`, in Keep a Changelog format.
