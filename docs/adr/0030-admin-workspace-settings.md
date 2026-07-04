# ADR-0030: Admin-managed workspace feature settings (DB override > env > default)

- **Status:** Accepted
- **Date:** 2026-07-04
- **Deciders:** fleet maintainers

## Context

An audit of every env-configured knob found ~20 optional product features
(PII redaction #450, tool disclosure #506, phone-a-friend #175, memory
auto-index #234, error analysis #317, …) that shipped governed and tested but
configurable **only** through env flags — several undocumented outside
`FEATURE-NOTES.md`, none administrable from the web UI. In practice these
features die: operators don't know they exist, and changing one means editing
an env file and restarting a box that other people are using. The web UI also
carried one actively *dishonest* control — a concurrency-cap card calling
`GET/PUT /concurrency`, endpoints fleet never served.

Two mechanisms already existed, neither fitting this need:

- **Env-file hot-reload (#286)** covers five ceiling/temperature vars,
  triggered by SIGUSR2/HTTP — still file-editing, not UI, and deliberately a
  tiny allowlist.
- **Admin-managed LLM providers (#637)** proved the per-feature pattern (DB
  table → admin API → hot-swap closure → admin panel) but is a row-collection
  with sealed secrets; replicating a full table per scalar toggle would not
  scale to a dozen settings.

## Decision

Add one generic slice for **scalar, secret-free, live-applicable** feature
settings:

- A `workspace_settings` key/value table (chat-DB migration 035) storing
  **overrides only**; a typed server-side registry (`internal/settings`)
  declaring key, kind (bool/int/enum), bounds, and env-var provenance;
  admin-gated CRUD (`GET/PUT/DELETE /admin/settings[/{key}]`); and one admin
  panel rendering the resolved registry.
- **Precedence: admin DB row > env var > built-in default.** Env vars remain
  the deployment defaults, so existing deployments and the fake-LLM test seam
  are byte-for-byte unchanged until an admin writes an override. Reset =
  delete the row.
- **Live-apply hooks are injected by `cmd/fleet`** (mirroring
  `WithLLMProvidersChanged`): the PII redactor hot-swaps via the existing
  `SetPIIRedactor` seam; the two agentcore knobs read atomic holders per
  turn/tool call; config-backed booleans get mutex-guarded `Live*`/`Set*`
  accessors on the shared `config.Config`, reusing the #286 reload lock. The
  #286 reloadable set and this registry are **disjoint by construction**, so
  the two runtime-mutation paths cannot fight over a field.
- **Registry admission rule:** a setting may join only if its consumer
  re-reads the value per use — the panel never has to render a "restart
  required" state, which keeps the UI honest by construction. Boot-bound
  settings (concurrency, sandbox sizing, TLS, search FTS, browser tool)
  stay env-only, and the dead concurrency-cap UI was removed rather than
  half-fixed.
- **No secrets in the registry.** Secret-bearing config (SMTP, webhook
  signing, VAPID) is excluded; making notifications admin-manageable requires
  the sealed-secretbox, write-only-key treatment providers got (a future
  slice), per the credentials-stay-host-side invariant.

## Consequences

- Features become discoverable and tunable where admins already are, with
  provenance (who set what, what it reverts to) and audit logging; env-file
  workflows and automation keep working unchanged.
- A new feature toggle costs one registry entry + one apply hook + one
  defaults entry (construction fails loudly if either is missing) + UI copy.
- Two sources of truth exist for a covered setting's effective value (env and
  DB); the panel surfaces provenance explicitly to keep that legible, and
  boot-time apply failures degrade to env-derived behavior with a loud log.
- Invariants untouched: settings values are feature toggles only — no secret
  material enters the table, the model context, or logs; governance stays in
  the one `agentcore.Run` loop.

See [`ADMIN-SETTINGS.md`](../ADMIN-SETTINGS.md) for the operator-facing
contract and the full audit inventory.
