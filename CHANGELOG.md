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

### Fixed

- Structured output extraction (#244 hardening): a final answer carrying
  SEVERAL top-level JSON values (a narrated intermediate plus the restated
  final object — observed in a live run) now validates to the last conforming
  value instead of failing outright; extraction scans complete JSON values
  with a decoder, so braces inside strings can't derail it.
- `fleet chat` no longer races the terminal on its first markdown render:
  glamour's auto-style raw-queried the terminal (OSC 11) mid-session while
  bubbletea owned stdin, so the reply could wedge input or be typed into the
  composer as garbage on real terminals. The TUI now resolves dark/light via
  bubbletea's RequestBackgroundColor handshake and always hands glamour an
  explicit style. Also: typed letters no longer scroll the transcript (the
  viewport's default h/j/k/l keymap was receiving composer keystrokes).

### Added

- Demo GIFs for every surface (#540): the README now opens with three
  recordings telling one story — plan in chat (a REAL model + sandbox take),
  automate in the Operations Center (real scheduler), ride along in the TUI
  (deterministic mock). Recording + conversion pipelines are scripted
  (`docs/scripts/record-web-demos.mjs`, `generate-web-gifs.sh`,
  `generate-tui-gif.sh` with a verified-take retry loop) and documented in
  `docs/generating-demo-gif.md`.

- Browser push notifications via the Web Push API (#292): opt in per browser
  under Settings → Connections and get a low-detail alert — task complete or
  failed (`FLEET_PUSH_ON_TASK_COMPLETE`), approval needed
  (`FLEET_PUSH_ON_APPROVAL_REQUEST`), or a paused task waiting for an answer —
  even with the fleet tab backgrounded. Web Push rides the existing
  `internal/notify` fan-out as a per-user backend (a new `Event.Audience`
  routes to the task owner's subscriptions). Setup: `fleet generate-vapid-keys`
  → set `FLEET_VAPID_PUBLIC_KEY` / `FLEET_VAPID_PRIVATE_KEY` /
  `FLEET_VAPID_CONTACT` (private key stays host-side); unconfigured
  deployments are byte-for-byte unchanged (`/push/*` answers 501). Payloads
  carry only a title, state, and deep link — never message content, tool args,
  or secrets. See `docs/PUSH-NOTIFICATIONS.md`.
- Temporal knowledge-graph memory (#523, stage 3 of #515): derived
  entity/relation tables over the memories table (chat migration 030) with
  provenance links back to each source memory and NO time columns of their
  own — all temporal semantics derive through the memories join (ADR-0029).
  As-of queries over both bi-temporal axes (`GET /memories/graph` and
  `GET /memories` with `as_of_valid=`/`as_of_learned=`), LLM extraction of
  entities + triples when a memory becomes active (gated by
  `FLEET_MEMORY_GRAPH_ENABLED`, default OFF; model via
  `FLEET_MEMORY_GRAPH_MODEL`; async + best-effort, plus a manual
  `POST /memories/{id}/extract-graph`), and a "Graph" tab in the memory
  manager with "what was true on…" / "what did fleet know on…" time-travel
  inputs. Deterministic auto-conflict rules stay deferred per the issue's
  triage; conflict handling remains human-confirmed supersession.
- `fleet chat` TUI polish + an animated README demo (#540): speaker pills,
  ✓/✗ tool-outcome glyphs, a full-width header bar (model · conversation), a
  rounded composer box that dims while streaming, a right-aligned hint bar, a
  formatted `/help`, and Esc-to-cancel. The README now embeds a deterministic
  demo GIF recorded with charm's vhs against a canned mock server (no model,
  no keys) — `docs/generating-demo-gif.md` documents the pipeline. Also new:
  `docs/BUILDING-ON-FLEET.md` (the HTTP API as an automation substrate) and a
  README "Batteries included" tour.
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

### Changed

- Web UI aesthetic unity (#540): one token-driven design system across
  `/chat`, `/orchestrator`, `/admin`, `/settings`, and `/login`. Recurring
  hardcoded status colors (the chip/banner greens, ambers, and reds) became
  semantic theme-aware tokens (`--color-{success,warning,danger}-strong/-soft`,
  defined for dark *and* light with AA-contrast light values), and the
  hand-rolled status pills and notice banners were consolidated into shared
  primitives (`web/src/app/shared/ui/StatusChip.tsx`, `NoticeBanner.tsx`).
  Stray near-miss border radii moved onto the shared `--radius-*` scale, input
  focus states aligned on the accent-border treatment, and a few
  never-defined `var(--color-text)` references were fixed to
  `--color-text-primary`. Raw hex in `.tsx` is now the exception (meta
  theme-color + the approval-card HTML-preview inversion map only) — the
  system, token families, and the "no raw hex; add a semantic token" rule are
  documented in `web/src/app/DESIGN.md`. No redesign: same look, one source
  of truth.
