# Changelog

- Align the recommended model lineup across Fleet, Chat, and MOC: new work
  defaults to `z-ai/glm-5.2`, while advanced escalation and task fallback use
  OpenRouter's exact `openai/gpt-5.6-sol` slug.

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

- **Admin server status**: Settings now includes an admin-only **Server** tab
  with a lightweight, auto-refreshing view of CPU/load, memory, root-disk,
  uptime, and aggregate non-loopback network traffic. The read-only endpoint is
  role-gated and deliberately omits process, environment, address, and
  filesystem-detail data.
- **Hybrid prompt library** (`docs/PROMPT-LIBRARY.md`): Chat and Operations
  Center now share a searchable prompt picker that live-loads read-only
  `.yaml`/`.yml`/`.md`/`.txt` entries from the client bundle's Git-trackable
  `prompts/` directory and merges them with UI-authored private or
  workspace-shared prompts. Authors/admins can edit or delete database-backed
  entries, seed a new entry from the current draft, and download a versioned
  JSON backup suitable for ordinary cloud-drive storage. The generic bundle
  includes a neutral example prompt.
- **Resilient live Operations logs**: the task activity viewer now reconnects
  dropped fetch-based SSE streams and forwards `Last-Event-ID`, resuming at the
  next tool/message event instead of silently freezing or replaying the live
  buffer from the start. Tool details are now expandable rather than clipped at
  600 characters. Live cancellation continues to interrupt the governed run
  and records the stopping operator; task creators can now stop their own jobs
  without gaining permission to stop a teammate's job.

- **`fleet mcp test` (docs/MCP-TESTING.md)**: per-server smoke test for the
  bundle's MCP catalog — loads the bundle through the boot loader (same env
  interpolation, enable gates, TLS), spawns/handshakes each server exactly as
  the broker would (`initialize` + `tools/list`), and reports tools or the
  actionable failure per server. `--all` sweeps the catalog, `--json` for CI
  gates; exit 0/1/2. Boots nothing (no DB, no server, no sandbox) — run it
  where the deployment's env lives. `--deep` additionally calls each
  server's advertised auth-status tool (`auth_status` / `*_auth_status`) to
  verify the credentials against the upstream, failing the run on an
  `isError` result.

### Changed

- **Fleet application icons**: browser favicons, iOS/Android install icons, and
  the maskable launcher icon now derive from one checked-in Fleet mark. App
  Router file conventions eliminate duplicate icon metadata, and installed-app
  labels now follow `NEXT_PUBLIC_APP_NAME` instead of the old hardcoded name.
- **Coherent latest-Fedora sandbox policy**: the generic sandbox upgrades from
  `fedora-minimal:latest` and installs current Fedora packages on every rebuild,
  without hand-maintained pip overlays that can conflict with RPM ownership.
  Grype continues to publish every finding, while the merge gate blocks only on
  fixable CRITICAL Fedora RPM findings that the image can act on. Client bundles
  may still pin a base digest, package NEVRAs, or a prebuilt image when they need
  reproducibility.
- **Public-repo hygiene (#721)**: untracked the runtime `fleet.pid` file (now
  gitignored along with `/workspace/`), removed the dead
  `web/scripts/smoke-mcp-reporting.py` (bundle repos own their smoke tests),
  and replaced deployment-specific strings in generic content — private bundle
  paths/repo names, internal hostnames in handler tests, personal emails in
  webhook examples, and vendor credential names in `deploy/fleet.service` —
  with `example.com`-style placeholders. The out-of-repo bundle sanity test is
  now `internal/clientconfig/bundle_sanity_test.go`, opt-in via
  `FLEET_SANITY_BUNDLE_DIR` (skips cleanly when unset) and deployment-neutral.
  A README naming note clarifies that the v1 names (chat/moc/gig/cutlass)
  refer to the internal predecessor stack this project replaces.

### Added

- **Connector-directory onboarding** (`docs/CONNECTOR-ONBOARDING.md`): every
  hosted-directory entry is now either connectable in place or visibly
  documented. Per-user remote MCP gains an **API-key auth mode** (migration
  039: key sealed AES-256-GCM host-side like OAuth tokens, replayed as
  `Authorization: Bearer` or the entry's `api_key_header`; rotation via
  `PUT /remote-mcp-servers/{id}/key`) — the 60 `api_key` catalog entries
  previously had no working connect path. api_key and open adds (and key
  rotations) are **validated with a real MCP handshake** before anything is
  stored: a rejected key or unreachable URL fails the add with an actionable
  error and the guided form keeps the typed values, while a successful add
  confirms with the observed tool count. Directory cards grow **guided add
  forms**: per-`{placeholder}` inputs with a live URL preview for
  tenant-scoped endpoints, a write-only key field for api_key entries, and
  bring-your-own OAuth client fields for vendors without dynamic client
  registration (`client_registration: manual` — Google Workspace, Microsoft
  Work IQ, Slack, HubSpot). New catalog fields `setup_hint` (rendered visibly;
  CI-required for every tenant/api_key entry) and `setup_url` link the
  vendor's actual connect walkthrough; hints were researched and authored for
  all 97 such entries. Catalog refresh: new `google-people` (Contacts) entry
  plus community self-hosted `google-workspace-self-hosted`
  (Docs/Sheets-capable) and `microsoft-365-self-hosted` (no Copilot license
  needed) entries; auth corrections for aws-mcp and the key-in-URL vendors
  (Scrapfly, thirdweb, Smartlead). A curation audit removed 25 low-quality
  listings (docs-search-only servers for niche products, unworkable auth
  schemes, obscure duplicates) and added 15 newly-verified official hosted
  endpoints (Docusign, Adobe for Creativity, WordPress.com, Contentful,
  Sanity, Algolia, Cloudinary, Amazon Ads, DoorDash, ServiceNow, Microsoft
  Dataverse/Dynamics 365, Vimeo, Egnyte, Granola, Jotform) — net 267 → 282.
  A new **Featured shelf** (`featured: true`, capped 8–20 entries) surfaces
  the household names — the Google Workspace trio, GitHub, Notion, Slack,
  Linear, Atlassian, Asana, monday, Airtable, Stripe, PayPal, HubSpot,
  Canva, Figma, Adobe, Zapier, Hugging Face — ahead of the category listing.
  The "remote MCP isn't configured" notice is now admin-aware instead of
  showing env-var instructions to members.

- **v1 → fleet cutover runbook** (`docs/CUTOVER.md`, #718): the ordered
  operational sequence for a box already live on the legacy chat + moc stack —
  backups, stopping **and disabling** the legacy units (and the runner daemon
  on worker boxes), exporting with the correct env sourcing, bootstrapping
  with an explicit DB decision, importing, copying the non-DB state (moc task
  files and the legacy chat attachment dir), port/Caddy coexistence, minting
  fleet API keys for external intake callers, DNS, and post-cutover cleanup.
  Linked from `DEPLOYMENT.md` and `LEGACY-IMPORT.md`; `LEGACY-IMPORT.md`'s
  runbook now disables (not just stops) the legacy units, sources
  `/etc/fleet/fleet.env` on systemd deploys, and covers the chat data dir;
  `BACKUP_RESTORE.md` now names the on-disk state (attachments, task uploads,
  workspaces) that `fleet backup`'s DB dumps do not capture.

- `bootstrap.sh` DB flags (#718): `--chat-db-name`/`--chat-db-user`/
  `--sched-db-name`/`--sched-db-user` (fresh names that dodge a legacy
  collision) and `--adopt-existing-chat-db`/`--adopt-existing-sched-db` (skip
  provisioning for that pair — no CREATE, no ALTER, no password rotation — and
  take the operator-supplied DSN, validated with `SELECT 1`).

- **Per-task wall-clock timeout** (#724): `FLEET_TASK_WALL_TIMEOUT` (default
  4h, `0` disables) bounds a scheduled run's total elapsed time; on expiry the
  run is cancelled and the task fails with a clear, deterministic timeout error
  that never consumes the transient-retry budget. Documented in
  `docs/AGENT-RUNTIME.md` alongside the other enforced ceilings.
- **`fleet sched task list`** (#722): the daily-driver task listing (short id,
  name/prompt excerpt, status, priority, recurrence/scheduled time, model) with
  `--status`, `--limit`, and `--json`; plus `fleet start` alongside
  stop/restart, and top-level help for the shipped-but-unlisted verbs
  (sched trigger/dlq, apikey rotate, task memories).
- **Scoped keys can upload** (#719): `POST /v1/upload` accepts a scoped API key
  with create_task permission — external intake apps no longer need the
  full-access admin key to attach files to a create. Admin key / bearer /
  cookie paths unchanged; under-scoped typed keys get 403.
- **Task serialization** (#709): `TaskCreate` accepts an optional opaque
  `serialization_key`; fleet guarantees at most one task per key is active
  (leased/running) at a time, enforced by an advisory-locked re-check at the
  scheduler's claim gate plus a best-effort queue-visibility filter. A blocked
  task is skipped (stays queued), never failed. Recurring occurrences inherit
  the key. See `docs/TASK-SERIALIZATION.md`.
- The legacy migration bundle carries `serialization_key` on sched tasks
  (#712), and `fleet import` gained `--overwrite` for the restore-from-bundle
  use case.
- Scheduled task inputs support logical `file_names` paired with stored upload
  names. Dedicated MCP runs materialize them in `${FLEET_WORKSPACE}/inputs`,
  and bundles may use `${FLEET_TASK_ID}` for connector-side task attribution.

- **Start-of-run create reconciliation** (#717): a scheduled run whose resolved
  MCP workspace holds unresolved pre-POST markers in the bundle servers'
  cross-run create ledger (`creates.jsonl`) gets the v1-byte-compatible
  reconciliation block appended to its task prompt, so a retried/resumed task
  verifies whether creates landed before issuing any new create. Appended to
  the task (user) portion only — the cached system prefix stays byte-stable.
  See [ADR-0034](docs/adr/0034-audit-gate-commitment-binding.md).

### Fixed

- **`fleet update` no longer breaks sandbox starts after a bundle pull**: the
  updater runs as root, so files a client-bundle `git pull` created or rewrote
  came out root-owned — and the sandbox bind-mounts bundle dirs with an SELinux
  relabel (`:z`) that rootless podman may only apply to files the service user
  owns. One root-owned `protocols/*.yaml` was enough to dead-letter every task
  with `podman run: exit status 126` / `lsetxattr … operation not permitted`.
  The bundle-pull step now re-applies service-user ownership (mirroring
  `bootstrap.sh`), also healing checkouts a previous root-run pull broke.

- **Cookie users can mutate orchestrator state from the web UI again**:
  launching or editing a task while signed in with the elcano session cookie
  died with `403 Cross-origin request blocked`. The Next.js orchestrator proxy
  forwards the user's identity as `X-User-Email` + `X-Orchestrator-Server-Token`
  without the browser's `Origin` header (it enforces same-origin itself before
  forwarding), but the backend's `CSRFMiddleware` treated the missing Origin as
  cross-site. The shared-token header now joins `X-API-Key`/`Bearer` in the
  CSRF exemption list — browsers can't attach custom headers cross-site, and
  the auth middleware still fail-closed rejects a wrong token value. The
  in-handler auth of the routes registered outside the auth group
  (`POST /tasks`, `/tasks/batch`, `/tasks/estimate`, `/upload`) now honors the
  same header-trust identity via a shared fail-closed helper — previously it
  only accepted admin/API keys, bearer tokens, or the raw cookie, so the
  proxied cookie request would have cleared CSRF only to die with 401.

- **Sandbox Soup Sieve ReDoS (GHSA-836r-79rf-4m37)**: replaced Fedora's
  vulnerable `soupsieve` 2.8.3 RPM payload with upstream 2.8.4 and removed the
  old RPM metadata from the final image so Grype no longer reports the
  vulnerable package.

- **`bootstrap.sh` can no longer hijack a legacy database or truncate a
  foreign Caddyfile** (#718). Local-mode provisioning refuses when a
  role/database matching the configured names already exists on the cluster
  but is not recorded in fleet's env file by a previous run — previously the
  unconditional `ALTER ROLE … PASSWORD` silently rotated the legacy `chat`
  role's password (locking out the still-installed legacy server and any later
  `chat-admin export`) and fleet's migrations then ran on the legacy database.
  The password-converging `ALTER` now provably applies only to roles the
  script itself provisioned. Likewise `--enable-web --domain` refuses to
  overwrite an `/etc/caddy/Caddyfile` it did not write; `--force-caddy`
  overrides with a timestamped backup (`Caddyfile.fleet-backup.<ts>`) and a
  loud merge warning.

- `deploy/fleet.service` gained `Wants=postgresql.service` alongside the
  existing `After=` (#718): `After=` only orders units and never starts
  Postgres, so on a `--postgres=local` box whose cluster unit was not enabled,
  a reboot brought fleet up against a down database (`Restart=always` churn
  until someone started Postgres by hand). `Wants=` (not `Requires=`) is
  ignored when the unit is absent, so external-Postgres deploys are unchanged.
- **Audit-gate commitments bind to the full tool name, record ids, and values
  digest** (#715): a typed `confirm_audit` `critical_actions` entry now
  registers a commitment keyed by the full server-qualified MCP tool name plus
  its optional `deal_id`/`deal_ids` record binding and `values_digest`.
  Suffix-count matching had let one approval authorize — and be silently
  discharged by — a same-suffix tool on a different server, variant, or record;
  batches carried no id/digest binding at all. Typed audits now fail closed:
  zero resolved commitments authorizes nothing — refused outright when an
  entry named a critical suffix without its full server-qualified name, and
  accepted-but-empty (every critical call still blocked) for explicit no-op
  declarations. The bare-suffix path survives only as the legacy fallback for
  untyped audits and can never approve a server-side batch.
  See [ADR-0034](docs/adr/0034-audit-gate-commitment-binding.md).

- **Payload-level MCP failures no longer discharge critical-action
  commitments** (#716): a critical tool result whose payload reports failure
  (`{"success": false}` or a non-empty top-level `"error"`) over a clean
  transport now counts as a FAILED execution — it discharges nothing, counts
  against the retry budget, and keeps finish enforcement armed. This
  generalizes the previous `send_email`-only payload check to every critical
  tool; batch tools returning per-record `results[]` discharge per succeeded
  record, idempotently, and never for record ids the audit did not approve.

### Changed

- **Unknown personas are rejected at task create** (#720): a non-empty
  `persona` not present in the client bundle is now a 400 listing the valid
  names, instead of silently dispatching on the global default persona. Tasks
  whose persona disappears from the bundle after creation still fall back at
  dispatch. `docs/openapi.yaml` TaskCreate now documents the previously-missing
  `name`, `persona`, `credential_allowlist`, `tags`, `timezone`, `description`,
  `max_retries`, and `carry_context` fields, and `/upload`'s security note
  reflects the scoped-key change.
- **`fleet sched apikey create --rate-limit-per-minute`** (#722) names the
  rate-limit unit; `--rate-limit` remains a deprecated alias that warns (the
  v1 tooling's flags were per hour — porting numbers 1:1 set a 60× stricter
  cap). The dev fast lane (`dev-ci.yml`) now runs with a Postgres service so
  DB-gated suites — including the `fleet import` tests — run on every dev push
  (#723).

### Fixed

- **Batch task creation was broken on dev** (#710 regression): the `file_names`
  column was added to the task insert without bumping
  `taskInsertColumnsCount`, so every multi-row `POST /tasks/batch` INSERT
  failed with "INSERT has more target columns than expressions". Caught the
  moment the dev lane gained a Postgres service (#723); a DB-free test now
  pins the column/arg/count triple.
- **Chat usage tests no longer accumulate rows**: migration 038 dropped
  `turn_metrics`' FK to conversations (so usage survives conversation
  deletion), which silently removed it from the test-isolation
  `TRUNCATE ... CASCADE`; it is now truncated explicitly.
- **`fleet import` re-runs no longer revert live task state** (#713): sched
  tasks and run logs whose UUID already exists in fleet are skipped by default
  (previously upserted — a completed one-shot flipped back to pending and ran
  again, and fleet-side run history was replaced). Import validation and
  dry-run parity were tightened alongside (#714): warnings for MCP
  opt-ins/personas missing from the loaded bundle and for inert
  scheduled-without-schedule tasks, the same-database guard now covers
  single-section bundles on the shared `DATABASE_URL` fallback, dry-run
  memory/orphan-log checks match the real run, `--live-only` skips are no
  longer mislabeled "already present", flags may precede the bundle path, and
  a chat memory lookup failure no longer aborts the section mid-run.
- The task batch/tx insert paths bound one more argument than their
  placeholder count after the `file_names` column landed
  (`taskInsertColumnsCount` stayed at 60 for 61 columns); the constant is now
  pinned by a drift test.
- Test-suite portability: the chat store's test truncation now clears
  `turn_metrics` (its conversations FK was dropped by migration 038, so it
  stopped cascading and the usage-analytics tests accumulated rows across
  runs), `projects`, `user_connector_prefs`, and `user_skills`; the path
  traversal test no longer assumes the checkout lives outside `os.TempDir()`;
  and `update.sh` accepts a git-worktree checkout (`.git` pointer file).
- **Remote MCP OAuth callbacks use the deployed Fleet domain**: web-enabled
  bootstrap runs now write the same computed public origin to the backend's
  `FLEET_PUBLIC_BASE_URL` and generate the token-encryption key once; normal
  `fleet update` runs also reconcile existing installs from the deployed web
  origin before restarting. A
  `--domain` deployment therefore registers `https://<domain>/api/oauth/mcp/callback`
  instead of retaining a localhost callback.

- **Open-access hosted MCPs no longer get forced through OAuth discovery**:
  one-click catalog adds now preserve the entry's authentication mode. Servers
  such as AWS Knowledge are stored ready to use without a bearer header, so an
  intentional GET redirect to provider documentation no longer fails setup.
  Redirects remain disabled for remote MCP traffic and OAuth discovery.

- **The bundled MCP directory no longer advertises a nonexistent hosted X API
  server**: `https://api.x.com/mcp` returns 404 and X documents its API-capable
  XMCP as self-hosted. The valid open-access X Docs MCP remains available at
  `https://docs.x.com/mcp`, while operators can still add a separately hosted
  XMCP deployment as a bundle-authored connector.

- **Weekly task schedules support multiple days**: the Create New Task modal's
  simple repeat editor now accepts combinations such as Monday and Wednesday.
  Supported cron weekday lists also hydrate the same selected-day UI when
  switching back from Advanced cron.

- **Deleting chats no longer resets user usage totals**: chat cost and token
  metrics now survive conversation deletion and retention cleanup. Transcript
  content is still deleted; only the existing non-content accounting row is
  retained, with unavailable model/project attribution grouped as unknown.

- **Task scheduling controls fit cleanly on mobile**: the Create New Task modal
  now contains its date and time pickers with consistent labels and padding,
  and its Repeat tab defaults to a plain-language daily/weekday/weekly builder.
  Advanced cron remains available without an overlapping clock icon.

- **Chat labels remain editable with mobile keyboards**: viewport resizes caused
  by opening a software keyboard no longer dismiss the conversation menu while
  its label input has focus. Ordinary viewport resizes still close open menus.

- **Default core tier drops `:nitro` for cache locality**: the default
  `z-ai/glm-5.2:nitro` slug asked OpenRouter for throughput-priority
  routing, spraying requests across the ~26 providers serving GLM-5.2 —
  and prompt caches are per-upstream, so the implicit-cache discount
  (~80% off cached input) almost never hit. The default is now the plain
  `z-ai/glm-5.2`, soft-pinned to the Z.AI upstream via `canonicalUpstream`
  (Order + fallbacks allowed, same policy as the Anthropic/OpenAI/Moonshot
  pins). Verified live: the second pinned call served 3,968 of 4,018 prompt
  tokens from cache; with `:nitro` the same pair read 0. Trade-off: routing
  now prefers Z.AI's own endpoint over whichever provider is momentarily
  fastest; fallbacks still engage if Z.AI is down.

- **Prompt caching works on native LLM providers**: a model served by a
  native provider (#289) reports its provider-local slug (`claude-fable-5`,
  no `anthropic/` prefix), which failed `supportsExplicitBreakpoints`'s
  prefix match — so native-Anthropic runs got **no** `cache_control`
  breakpoints and paid full input price on every turn (~10× the cached rate
  on the strong tier). Bare `claude-`/`gemini-` slugs now match; fantasy's
  native Anthropic provider reads the same per-message marker shape the
  OpenRouter hook does, verified live against OpenRouter (99.8% cache-read
  on the second call).
- **Compaction summaries are cache boundaries now**: the shared loop enables
  `WithCompactionSummaryBreakpoint`, so the summary message a compaction
  pass inserts carries its own breakpoint (4 total — Anthropic's maximum)
  instead of the tail markers having to re-cover it. The option existed and
  `interactive.go` tagged summaries for it, but no production caller ever
  enabled it.
- **1M-context beta header follows the active model**: `anthropic_beta:
  context-1m` was attached whenever the *configured* primary or fallback was
  a long-context Claude, so it also rode along on requests actually served
  by the other (possibly non-Claude) model. It is now gated on the active
  slug, the same rule extended thinking already used.

- **Settings sections no longer jump in width while navigating**: the settings
  shell's scroll container now reserves a stable scrollbar gutter
  (`scrollbar-gutter: stable`). Sections differ in height — General and the
  Admin overview fit the viewport while Connections and Skills scroll — so on
  classic-scrollbar platforms (Windows/Linux) the scrollbar appeared and
  disappeared across navigations, resizing the centered layout and shifting
  the sub-nav and content sideways. Every section now renders at the same
  width, including while Connections/Skills catalogs are still loading.
- **Bootstrap re-runs no longer lock admins out of the Operations Center**:
  `scripts/bootstrap.sh` wrote `ORCHESTRATOR_SERVER_TOKEN=<ADMIN_API_KEY>` into
  `/etc/fleet/fleet-web.env`, but the orchestrator's header-trust path verifies
  `X-Orchestrator-Server-Token` against the chat shared secret
  (`FLEET_SERVER_TOKEN`), fail-closed — so any bootstrap (re-)run 403'd every
  cookie-authenticated Operations Center request until the env file was
  hand-repaired. Bootstrap now mirrors `FLEET_SERVER_TOKEN` into both
  `CHAT_SERVER_TOKEN` and `ORCHESTRATOR_SERVER_TOKEN`, matching the middleware
  and the web tier's documented fallback.
- **Operations Center model picker actually lists the catalog again**: the
  task form's picker fetched the OpenRouter catalog directly from the
  browser, which the app's Content-Security-Policy (`connect-src 'self'`,
  #590) silently blocks — stranding the picker on its two seed models. It now
  loads through the existing session-gated `/api/model-catalog` proxy, so the
  full text-model catalog (plus pricing for ranking) is searchable again.

### Docs

- **PROMPT-CACHE-CONTRACT.md**: records the day-granular Runtime Date
  Context as the sanctioned exception to the no-`time.Now()` rule (one
  cache miss per conversation per UTC-midnight rollover, deliberate), and
  documents the provider-local slug forms + the now-default compaction
  breakpoint.

### Added

- **MCP bundle env contract for the cutlass-family servers**
  (docs/MCP-BUNDLE-ENV.md): the reserved `${FLEET_WORKSPACE}` manifest-env
  token substitutes a fleet-provided writable workdir at MCP subprocess launch
  (per-run for a scheduled task's dedicated client, a stable per-deployment
  dir for shared spawns; token-bearing keys are dropped when no dir is
  offered), so bundles can wire `CUTLASS_RUN_WORKDIR`-style vars without
  hardcoding paths. Account-variant spawns now inject
  `MCP_VARIANT_CLIENT=<account>` (cutlass mcp_loader parity), and the new
  per-server `identity_env` manifest list refuses a partially-suffixed named
  account whose identity-routing vars would silently inherit the default
  seat. Bundle-declared `agent_policy.critical_tools` suffixes are now staged
  through the interactive approval-card UX too (previously scheduled-mode
  audit gating only), honoring session pre-approvals and the #225 per-tool
  timeout chain.
- **Host-side prompt-injection guardrails (#702)**: optional workspace-wide
  `off` / `observe` / `block` screening covers seed user/task messages and tool
  output before it enters model context. Operators bring a local HTTP detector,
  configure it live in Admin Features, and can test it with a synthetic sample;
  block mode fails closed while observe mode reports outages without blocking.
- **Cross-provider LLM failover (#703)**: client bundles can declare an ordered
  `fallback_providers` chain. Retryable failures can promote the same model to
  the next eligible backend before streaming begins; explicit provider pins stay
  isolated, model-level `fallback_model` remains distinct, and Fleet suppresses
  failover after any semantic event to prevent stream splicing or duplicate
  side effects.

- **Workspace-provider models in the chat picker**: models from
  admin-configured LLM providers (Settings → Admin → Model providers) now
  appear in the chat composer's model menu with a "workspace" pill — they
  previously surfaced only in the task form's picker. Labels and context
  windows resolve for the chip and the context ring too.
- **Catch-all providers get a browsable catalog (catwalk)**: an
  Anthropic/OpenAI provider saved with an empty models list (= serves any
  slug) previously contributed nothing to the pickers. The member-level
  `GET /llm-provider-models` read now also returns the enabled provider
  roster (name/type/catch_all — no secrets), and the web tier expands such
  rows into `<provider>/<model>` entries from the
  [catwalk](https://github.com/charmbracelet/catwalk) model database (the
  same no-auth catalog Charm's Crush uses) via a new session-gated
  `/api/catwalk-models` proxy with a 24 h cache. `CATWALK_URL` overrides the
  default `https://catwalk.charm.land` for air-gapped deployments; failures
  degrade to typed slugs. See `docs/LLM-PROVIDERS.md`.

### Changed

- **New Task modal redesign**: the Operations Center create-task form is now
  grouped into titled sections (Task · Schedule · Email · Tools & files ·
  Advanced) with consistent field styling — several controls (the advanced
  toggles, run_if row, tags/persona inputs) previously rendered with no CSS
  at all. Real toggle switches with full-row click targets, a two-column grid
  for schedule/model/limit fields, the custom cron input promoted next to the
  presets (click an active preset again to clear it), a Cancel button, and
  Estimate Cost anchored at the footer's start. Field IDs, validation, and
  submit behavior are unchanged.
- **Chat share UX**: creating (or revisiting) a share link now opens a
  proper dialog — the full URL in a selectable field, an explicit "Copy
  link" button with copied-state feedback, a read-only-access explainer,
  and "Stop sharing" — instead of only flipping a small sidebar icon and
  silently writing the clipboard (which browsers can block with no
  feedback). The shared-row menu item is now "Share link…".
- **schedule_task approval card is editable**: the "Make recurring task"
  proposal (and any agent-staged schedule_task) can be adjusted before
  approving — Edit… exposes the name, cron schedule (recurring tasks),
  and the FULL prompt (the card previously showed a truncated preview
  only); "Approve with changes" sends just the changed fields, and the
  server applies them to the staged args BEFORE the same validation and
  single task-create path the unedited flow uses. Nothing is created
  until approve, as before.

- The **Branch** button appears as soon as a turn finishes, not after a
  reload. The turn stream gains a `history.persisted` event: after the
  server persists the turn's history rows it tells the live stream which
  DB ids the messages became, and the client backfills them — the Branch
  affordance (which gates on a persisted id) enables immediately. Mocked
  turns emit the same event, so Playwright covers the flow.
- Model picker polish: the two pinned rows are named in the same
  "Lab: Model" style as every other row ("Z.AI: GLM 5.2 (nitro)",
  "Anthropic: Claude Fable 5"); Z.AI joins the per-lab "newest model"
  curation (listed first); and a lab's newest entry is excluded
  variant-insensitively when it IS the pinned model (a ":nitro" pin and
  its base catalog slug no longer render as duplicate rows).
- The login MOTD banner is now a rowing galley (oars out) instead of the
  small masted-ship mark.

- **Model lineup + no price ceiling + alias retirement.** The recommended
  everyday model is now `z-ai/glm-5.2:nitro` and the strong tier is
  `anthropic/claude-fable-5`, across chat (picker pins, new-conversation
  default, title/metadata fallbacks, lockdown-mode default allow-list) AND
  the Operations Center (task-create primary/fallback defaults + picker
  seeds). The user-facing "default"/"advanced" ALIASES are retired: the
  picker's two pinned rows now show real model names ("GLM 5.2 Nitro",
  "Claude Fable 5") with the existing "recommended" pill, and the
  escalation flow (`suggest_advanced_model`), the spreadsheet nudge, and
  the model chip all render the actual model name — the strong-tier ROLE
  survives (the agent can still suggest switching up), only the aliasing
  is gone. Both price ceilings on model selection are removed (the chat
  picker's $30/M-output cap and the Operations Center picker's $8/$30 per-M
  caps): any OpenRouter model is pickable, and spend stays governed by the
  per-run cost ceilings (`FLEET_MAX_COST_USD`), not by restricting
  selection. Claude Fable 5 is wired into the extended-thinking family
  gate; the 1M long-context beta is deliberately NOT enabled for it until
  verified upstream (falls back to the standard window).

- Chat keyboard shortcuts no longer collide with core browser shortcuts. The
  bindings that shadowed browser features are gone or moved: ⌘/Ctrl+F is
  released back to find-in-page (⌘K remains the search binding), "new
  conversation" moves from ⌘/Ctrl+N (reserved in Chrome — uninterceptable, it
  opened a browser window) to **⌘/Ctrl+Shift+O**, and "focus the composer"
  moves from ⌘/Ctrl+J (browser downloads) to **Shift+Esc** — both matching
  ChatGPT's chords, the prevailing muscle memory for AI-chat apps. The
  bare-key set (?, j/k, Enter, p, a, r, #, y, e) is unchanged; the "?" help
  overlay reflects the new chords.
- `fleet update` systemd-unit drift check is now functional-diff-based and can
  adopt the shipped unit in one step. It compares `deploy/*.service` against the
  installed unit with comments and blank lines stripped, so a reworded header no
  longer raises a spurious "a unit fix may not be live" warning. On a real
  (functional) change an interactive run prints the diff and offers to adopt the
  shipped unit — installing it, creating the `fleet-web` user + chowning `.next`
  when the pre-#654 non-root migration requires it, and running `daemon-reload`
  before the step-5 restart — so operators no longer hand-run `install`/`useradd`/
  `daemon-reload` themselves. A new `--adopt-units` flag
  (`FLEET_UPDATE_ADOPT_UNITS=1`) adopts unattended; `--yes` alone never adopts a
  unit (it only skips the commit-range confirm), so an automated update can't
  silently clobber an operator's hand-edited unit. Declined/non-interactive runs
  still print the actionable manual hint.
- Web UI/UX pass over four surfaces, matching the unified-shell design
  (fleet-unified reference): rail typography aligned to the design's type ramp
  (brand name 0.95rem/700, group headers 0.8rem, conversation/folder/label
  rows 0.875rem, menu items and account email 0.82rem, filter chips 0.78rem);
  a click-toggled ⓘ explainer next to the Recent group header stating the
  retention default (unpinned chats delete after 14 days of inactivity —
  `CONVERSATION_TTL_DAYS` — pin to keep; dismisses on outside click and
  Escape); a new app-wide `--gradient-bg` page-background token painted once
  on `<body>` (cover, fixed) beneath now-transparent chat/orchestrator shells,
  with `--sidebar-surface` moved to the design's rail gradient; and the chat
  composer toolbar rebuilt to the design — model chip opening a listbox
  popover (search field on top preserves type-to-search + free slug entry,
  with arrow-key navigation, Esc close, and focus return), a toolbar divider,
  icon tool-buttons (persona · attach · tools with a corner count badge ·
  context ring), sectioned tools popover with mini-switch toggles populated
  from the live optional-MCP catalog, and the circular gradient send button.
  Composer popovers now all close on Escape and return focus to their
  trigger. Fixes a latent styling bug where gradient tokens consumed via
  bare arbitrary-value background classes compiled to `background-color`
  and never painted (now emitted as `background-image`).

### Added

- **User management in the web UI.** Settings → Admin "Users & roles" now
  covers the full lifecycle — create accounts (typed or generated password,
  shown once), reassign role/team, reset passwords (generated, shown once),
  and delete — so admins no longer need CLI access to the box. New chat-server
  endpoints `POST /admin/users`, `DELETE /admin/users/{email}`, and
  `PUT /admin/users/{email}/password` (admin-gated like the rest of
  `/admin/*`; every write is audit-logged with the acting admin). Role writes
  carry `fleet admin add`'s two-plane semantics: promoting to admin also
  ensures the Operations Center admin row, demoting or deleting removes it —
  via a new injected `WithOpsAdmins` seam (httpapi stays sched-agnostic), with
  the users list annotating who holds it (`ops_center_admin`, the "ops" badge).
  Self-demote and self-delete are refused so an admin can't lock themselves
  out mid-session. Web proxy routes are Origin-checked (CSRF) like the other
  mutating admin routes; passwords travel one way and never appear in
  responses or logs.
- **One-command admin provisioning + interactive bootstrap credentials.** New
  `fleet admin add <email>` provisions a FULL admin across both user planes in
  one idempotent step — the chat web login (password prompted hidden with
  double-entry, or `--password -`), the chat-admin role, and the Operations
  Center admin row the session-cookie bridge (#458) resolves by email — with
  `fleet admin list` (two-plane view) and `fleet admin rm` (removes from both).
  New `fleet config set-openrouter-key` (hidden prompt or `--key -`; upserts
  `OPENROUTER_API_KEY` into the server env file) and `fleet config
  set-auth-pubkey [<key>|--from <file>]` (enables Elcano SSO: accepts the
  `auth pubkey` output line verbatim, validates standard-base64 → 32-byte
  Ed25519 fail-closed, writes `AUTH_SIGNING_PUBKEY` — plus optional
  `--login-url`/`--cookie-domain` — into the web env file). `scripts/bootstrap.sh`
  is now fully interactive on a terminal: a bare run walks through the
  deployment choices (systemd service? web UI? public TLS domain?) and then the
  credentials — the OpenRouter key (hidden prompt when absent), the Elcano SSO
  key under the web tier (`--auth-pubkey <value|@file>` skips the prompt and is
  validated up-front, before the npm build), and admin registration once the
  service is active (`--admin a@x,b@y` or an interactive prompt, passwords via
  `fleet admin add`). Previously-set `AUTH_*` keys now carry across re-runs
  (the web env rewrite used to silently drop them — SSO turned off on every
  re-bootstrap). Explicit flags always win; non-TTY runs skip every prompt and
  keep the old flag/default behavior, printing the equivalent `fleet admin add`
  / `fleet config` follow-up commands instead.
- One-time legacy migration ingest ([docs/LEGACY-IMPORT.md](docs/LEGACY-IMPORT.md)):
  `fleet import <bundle.json>` consumes the versioned `fleet-migration-bundle`
  JSON envelope produced by the deprecated chat repo (`chat-admin export`: users
  with bcrypt hashes, conversations + full message history with pins and
  timestamps preserved, memories) and the deprecated moc repo (`moc
  -export-fleet`: sched users, scheduled/recurring tasks with next-run
  recomputed in the task's timezone, run logs). All legacy-schema knowledge
  lives in the exporters; fleet only consumes the bundle. Idempotent re-runs
  (`--dry-run`, `--live-only` supported); imported history is FTS-indexed.

- Per-task extended-thinking override (#220): a nullable `thinking_budget_tokens`
  on tasks (sched migration 053) lets a scheduled task set its own Claude
  thinking budget — NULL/omitted inherits the global
  `FLEET_DEFAULT_THINKING_BUDGET_TOKENS` (unchanged behavior), `0` forces
  thinking off, `N` sets its budget (clamped to the provider range at run
  time). Exposed on the create/PATCH task API, the `schedule_task` tool, the
  admin CLI import, and a "Thinking budget" field in the Operations Center
  task-create form; carried through task export/import. Resolves override >
  global, mirroring the interactive per-conversation override.


### Added

- Usage report CSV export ([docs/USAGE-ANALYTICS.md](docs/USAGE-ANALYTICS.md)):
  `GET /admin/usage?format=csv` returns the report as a `text/csv` attachment
  (row per bucket + a TOTAL row), and the Operations Center Usage panel gains a
  **Download CSV** button using the current filters.
- Optional paused-task auto-expire ([docs/ASK-NOTIFY.md](docs/ASK-NOTIFY.md)):
  `FLEET_PAUSED_TASK_EXPIRY_MINUTES` (>0) makes the scheduler fail a task that
  has sat in `paused_awaiting_input` past the window (terminal `error`),
  mirroring the anti-starvation sweep (#230). Default 0 = OFF (wait forever),
  so existing behavior is unchanged.


### Changed

- Skill `allowed-tools` is now parsed and **surfaced** for review — the skills
  library UI and `GET /skills` show each skill's declared tool contract (both
  the YAML-list and comma-string frontmatter forms are accepted). It remains
  **advisory, not enforced**: skills are read on-demand mid-turn with no single
  "active skill" to gate a tool roster against, and a skill can never exceed
  the turn's sandbox/MCP/approval limits, so a self-declared list can't be an
  authorization boundary. The "Honesty in docs" invariant (AGENTS.md) and
  docs/SKILLS.md now spell out why, plus the naming-portability caveat for
  skills imported from Claude Code.


### Fixed

- A bare deployment (no client-config bundle) could never start: two fresh-box
  bugs found by running the full interactive bootstrap end-to-end in a clean
  Fedora systemd container. (1) `deploy/fleet.service` listed
  `/opt/fleet/client` in `ReadWritePaths=` unconditionally — on a box using the
  in-repo `config/default` bundle that path doesn't exist, and systemd fails
  namespace setup outright (`status=226/NAMESPACE`); it is now `-`-prefixed
  (ignore-if-missing). (2) bootstrap wrote the default bundle dir into the env
  file as the RELATIVE `config/default`, which the unit resolves under its
  `WorkingDirectory=/var/lib/fleet` — "client config bundle … no such file or
  directory" at boot; `CLIENT_CONFIG_DIR` is now absolutized (repo-anchored
  default, CWD-resolved `--client-config` paths) before it is persisted.
- Admin Health panel: the MCP catalog pills no longer render optional servers in
  the red **danger** tone, which read as "broken". The panel does not probe MCP
  servers — the flag is "enabled by default for new conversations" — so an
  off-by-default optional server is normal, not an error. On-by-default servers
  are now `success` (green) and optional servers `neutral` (grey), never danger;
  the header is relabeled "MCP catalog" with a tooltip clarifying it isn't a
  health probe, and each pill has an explanatory title.

- `fleet update` now pulls the client-config bundle even when the update changes
  `update.sh` itself. The self-update re-exec ran the new script with
  `--no-pull` to avoid re-fetching the already-fast-forwarded fleet checkout,
  but `--no-pull` also skips the client-bundle pull (step 2, which hadn't run
  yet) — so any release that touched `update.sh` left the bundle stale. The
  re-exec now uses an internal `FLEET_UPDATE_REEXEC` marker that skips only the
  SRC fetch + self-update detection (no loop) and otherwise runs a normal update,
  including the bundle pull and the Containerfile-hash sandbox-rebuild gate. Also
  made the unit-drift adopt hint accurate for `fleet-web.service`: it now shows
  the one-time `useradd fleet-web` + `.next` chown prerequisite (the unit runs as
  a non-root user since the deploy-hardening change) and a `systemctl restart`, so
  following it doesn't leave the web tier failing to start.

- Paused-task expiry no longer kills a recurring task's schedule. The
  `FLEET_PAUSED_TASK_EXPIRY_MINUTES` sweep failed an unattended ask-paused task
  with a bare bulk UPDATE that bypassed the terminal-transition path — so an
  expired occurrence of a recurring task went `error` AND permanently ended the
  schedule (no next occurrence). The sweep now uses `UPDATE ... RETURNING` and
  spawns the next occurrence for each expired recurring row (race-free against a
  concurrent resume, which is status-guarded). Opt-in feature, default off. The
  sweep still does not fire the runner's completion notification (the notifier
  lives in the runner) — documented as a known gap.

- The anti-starvation sweep (`FLEET_STARVATION_*`) measured wait from
  `created_at`, but a recurring occurrence's row is created at the previous
  occurrence's completion — so its `created_at` is ~one period old the moment it
  becomes pending, and enabling the window floor-promoted every recurring/retried
  task instantly, inverting the priority queue. It now keys on
  `GREATEST(created_at, scheduled_for)` (the eligibility time); a genuinely
  starving task is still promoted, a freshly-eligible one is not. Opt-in feature,
  default off; no migration.

- Recurring tasks created by an agent or through the chat approval card no
  longer drift to UTC. `EnqueueTask` (the `create_task` tool, the chat
  `schedule_task` approval, promote-to-task) evaluated the first cron fire in
  the server-clock zone and never persisted a per-task timezone, so
  `FLEET_DEFAULT_TIMEZONE` was ignored and every occurrence after the first
  fired in UTC (a "9am" task became 9am UTC). It now resolves and persists the
  per-task timezone exactly like the HTTP create path, so recurrence stays in
  the task's zone. HTTP-created tasks were already correct.

- `fleet update` / `scripts/fleet-upgrade.sh` now actually install the freshly
  built binaries on the standard `/opt/fleet` topology. Both scripts parsed
  `systemctl show -p ExecStart --value` with `awk '{print $1}'`, which grabs
  the literal `{` of systemd's exec-command struct rather than the binary
  path — `update.sh` then resolved the install dir to the source checkout and
  skipped the copy as "in place" (the restart re-ran the OLD binary while
  reporting success), and `fleet-upgrade.sh` resolved it to the operator's
  cwd, voiding the backup/rollback guarantee. The `path=` field is now
  extracted explicitly. `fleet update` also warns when the installed systemd
  units have drifted from the shipped `deploy/*.service` (bootstrap installs
  units only when absent, so shipped unit fixes never reached existing boxes
  and were previously invisible).

- Removed accidentally committed local agent-run artifacts (`hello.txt`,
  `audit_log.txt`, `data/audit/bash.log`, `data/task-run-*/session.json`) and
  added the server runtime `data/` directory to `.gitignore` so runtime output
  can no longer land in the repository.


### Security

- Host-side file tools (`view_file`/`write_file`/`edit_file`) are now confined to
  the workspace root the sandbox bind-mounts, closing an absolute-path bypass.
  Their allowlist (`AllowedBaseDirs`) fell back to the process working directory,
  which under systemd is the whole StateDirectory (`/var/lib/fleet`) — so an
  absolute path could read or write `data/` attachments/uploads and
  `api_keys.json` that the sandbox never mounts, defeating the "sandbox is
  mandatory" invariant for file I/O. `cmd/fleet` now registers the workspace root
  (`tools.SetWorkspaceRoot`, resolved exactly as the agent manager resolves it)
  as the authoritative base; the process cwd is no longer blessed. `ValidatePath`
  already resolves symlinks and re-checks the real path, so a symlink planted in
  the workspace pointing at `data/` is rejected too. `os.TempDir()` and
  operator-opted `FLEET_ALLOWED_DIRS` remain allowed; unregistered (tests/CLI)
  keeps the legacy cwd allowlist.

- Orchestrator admin auth now fails closed when `ADMIN_API_KEY` is unset.
  `verifyAdminKey` compared `sha256(header)` against `sha256(configured key)`
  in constant time, but with the key unset `sha256("") == sha256("")`, so a
  request sending no `X-API-Key` header authenticated as admin — silently
  opening the entire admin surface (`/keys`, `/users`, `/tasks/cleanup`,
  config/MCP reload, the principal `isAdmin` flag) on any deploy that left the
  key unset. Now returns false on an empty configured key, closing all call
  sites at once, and the process logs a startup warning when it's unset.
  `bootstrap.sh` always sets the key, so bootstrapped deploys are unaffected.

- The orchestrator listener now defaults to `127.0.0.1:8000` (loopback) instead
  of `:8000` (all interfaces), matching the chat listener's loopback default
  and what the deploy docs (Caddyfile, fleet.service, DEPLOYMENT.md) already
  promised. On a host without a firewall, the old default exposed the
  orchestrator/admin API directly to the network. Multi-host topologies that
  relied on the wide bind must now set `FLEET_ORCHESTRATOR_ADDR` explicitly
  (e.g. `0.0.0.0:8000`). The chat listener's last-resort fallback (used only
  when `FLEET_SERVER_ADDR` is set but empty) is loopback now too.

- The public-facing web tier (`deploy/fleet-web.service`) now runs as a
  dedicated unprivileged `fleet-web` system user instead of root; bootstrap
  creates the user and hands it `.next/` (Next's runtime cache — the unit's
  only writable path), and `fleet update` re-chowns it after each deploy.
  Existing installs keep their current unit (bootstrap never overwrites);
  `fleet update` now surfaces the drift with the exact adopt command.

- The TLS edge (`deploy/Caddyfile` and the bootstrap-generated variant) now
  sends `Strict-Transport-Security`, `X-Content-Type-Options: nosniff`, and
  `X-Frame-Options: DENY` on every response, mirroring the values the Go
  backends already set (`securityHeadersMiddleware`) so the whole origin
  carries one header policy.

- Interactive chat now honors the fleet-wide sandbox egress mode
  (`FLEET_DEFAULT_NETWORK_MODE`), closing a gap ADR-0012 deferred: a
  non-lockdown chat turn used to get open network egress even under
  `allowlisted` or `lockdown`. `takeTurnSandbox` now seals (`lockdown`) or
  proxy-filters (`allowlisted`) chat turns exactly as scheduled tasks do; a
  containing mode takes precedence over the persistent Python REPL. `open`
  (the default) is unchanged. ADR-0031.

### Added

- Rampart ML engine for PII redaction
  ([docs/PII-REDACTION.md](docs/PII-REDACTION.md)): the #450 "interface-ready
  follow-on" is now shipped — `pii_redaction_engine` picks `pattern` (the
  built-in regexes) or `rampart`, the 17-entity-type MiniLM ONNX classifier
  (names, addresses, government IDs, bank numbers, …) running behind an
  operator-deployed HTTP service (`pii_rampart_url`;
  [`scripts/rampart-service`](scripts/rampart-service/README.md) is the
  reference implementation over the official npm runtime, ~25 ms/call on
  CPU). Rampart redacts with stable numbered placeholders
  (`[GIVEN_NAME_1]`), the deterministic engine sweeps its output as a second
  pass (strict superset of the pattern floor — a missed formatted phone
  number is still caught), and a service outage falls back to the pattern
  engine, never fail-open. The Features panel gains the engine picker, the
  service URL field, a **Test detection** button
  (`POST /admin/pii-redaction/test`) that runs the live redactor over a
  synthetic sample, and a **one-click Install Rampart service** button
  (`/admin/pii-redaction/install`) — fleet builds the service container (model
  baked in, reference service embedded in the binary), runs it on loopback via
  the rootless podman it already uses for the sandbox, health-checks it, fills
  in the service URL, and re-starts it after a reboot. No bootstrap/update
  changes are needed to use it; operators who prefer their own systemd unit
  can instead run `scripts/rampart-service/install.sh`.
- Admin-managed task notifications
  ([docs/NOTIFICATIONS.md](docs/NOTIFICATIONS.md)): Settings → Admin gains a
  "Notifications" panel — configure the email (SMTP) and signed-webhook
  channels and the status filter at runtime, with a **Send test** button per
  channel (one real delivery of a synthetic event, key-free result). The
  config lives in the chat DB (`notify_settings`, migration 036) with the
  SMTP password and webhook signing secret sealed under the store's secretbox
  cipher and **write-only** semantics; a save hot-swaps the running
  notifier's config (`notify.SetConfig` — one shared pointer serves the
  runner, budget alerts, and email reply-back), so the next task completion
  uses it with no restart. Env vars remain the deployment default; the saved
  config replaces them wholesale and "Use env config" reverts. Email
  reply-back (#511) enablement is now consulted live, so enabling SMTP from
  the panel activates it without a restart. The store secret cipher now
  installs whenever `FLEET_MCP_OAUTH_ENCRYPTION_KEY` is set (previously only
  with the remote-MCP feature's public base URL also configured).
- Admin-managed workspace feature settings
  ([docs/ADMIN-SETTINGS.md](docs/ADMIN-SETTINGS.md)): Settings → Admin gains a
  "Feature settings" panel — the env-flag-only product toggles (PII redaction
  mode, tool disclosure threshold, tool-output ceiling, phone-a-friend,
  sub-agent delegation, memory auto-index, task error analysis, auto-title,
  connector recommendations, context handles) are now visible and editable
  from the web UI. Overrides live in the chat DB (`workspace_settings`,
  migration 035) with precedence **admin override > env var > built-in
  default**; every change is validated against a typed server-side registry,
  audit-logged, applied **live** (next turn/run/tool call — the registry only
  admits restart-free settings), and reversible per-setting via Reset. Env
  vars keep working unchanged as the deployment defaults. The registry holds
  no secrets by construction (SMTP/webhook notification config stays env-only
  pending sealed-secret treatment). ADR:
  [docs/adr/0030-admin-workspace-settings.md](docs/adr/0030-admin-workspace-settings.md).

- Admin-managed LLM providers ([docs/LLM-PROVIDERS.md](docs/LLM-PROVIDERS.md)):
  Settings → Admin gains a "Model providers" panel — add an OpenRouter,
  Anthropic, or OpenAI API key, or any OpenAI-compatible endpoint (Ollama,
  vLLM…), with several providers routing simultaneously. Rows live in the chat
  DB (`llm_providers`, migration 034) with keys sealed under the store's
  secretbox cipher and **write-only** semantics (no read path ever returns a
  key). Edits rebuild + hot-swap the model resolver — no restart — and the
  admin rows overlay the client bundle's `providers:` table (same-name rows
  replace, new names append). Enabled providers' listed models appear in the
  shared model picker as `provider/model` entries with a "Workspace" badge.
  With no rows configured, behavior is byte-identical to before (single
  catch-all OpenRouter from `OPENROUTER_API_KEY`), keeping the fake-LLM seam
  and existing deployments unchanged. Each row has a **Test** button — one
  host-side probe against the provider's real endpoint that verifies the
  key/base URL and cross-checks the listed models against the endpoint's
  catalog (absences warn, never fail); works on disabled rows so admins can
  verify before enabling.
- Skills builder (docs/SKILLS.md phases 2 + 3): Settings → Skills gains a
  "Your skills" section — create, edit, enable/disable, and delete personal
  skills (name + description + markdown instructions). User skills are
  DB-owned (`user_skills`, migration 033), strictly per-user, materialized
  into the conversation workspace before each chat turn
  (`user-skills/<name>/SKILL.md`) and listed in the prompt roster; `/name`
  invocation matches them after bundle/built-in names. Scheduled tasks load
  them too: the runner resolves the task owner and inlines their active
  skills into the run's prompt (24KB budget, loud truncation). The agent can
  draft skills from a run via the new `propose_skill` tool (both modes,
  intercepted at the policy boundary like `propose_note`); proposals stage as
  inert "Proposed by agent" entries the owner approves or deletes on the
  Skills page.
- Always-on bundled connectors now render as visible-but-locked rows on the
  connections page — nothing is invisibly enabled.
- New live-lane Playwright coverage drives the skills builder CRUD, the
  connector directory search, and the availability toggles against the real
  stack (real Postgres, real server, real prefs rows).
- Built-in Agent Skills pack + skills library: fleet embeds five
  generally-useful skills (data-profiler, web-research-brief,
  code-review-checklist, release-notes, executive-report) that every bundle
  inherits by default via a merged on-disk skills dir (bundle skills win name
  collisions; `skills_builtin: false` / `skills_hidden: [...]` manifest knobs
  opt out or trim). New Settings → Skills library page (account menu) with
  search, Workspace/Built-in provenance badges, and a full SKILL.md read view
  (`GET /skills/{name}`). Workspace symlinks now point at the registered
  bundle dirs (`tools.SetSupportingDocDirs`) so merged skills resolve in every
  sandboxed run. See `docs/SKILLS.md`.
- Unified connector enablement: Settings → Connections is now the per-user
  AVAILABILITY layer for every connector. Bundled (sandboxed) connectors get
  an "Enabled for me" toggle and a default credential-account seat picker;
  own and shared remote connections get personal On/Off ("Off for me" on a
  shared connection never affects the owner or other grantees). Chat pickers
  and turns honor the prefs (disabled connectors disappear; the default seat
  rides into the turn as the MCP account) while scheduled tasks deliberately
  keep their own pinned per-task `{server, account}` selection + credential
  allowlist — supervised chat follows your defaults, unsupervised automation
  never drifts with them. New `user_connector_prefs` (migration 032) +
  `GET/PUT/DELETE /connector-prefs`; absence of a pref row means the operator
  default, so nothing changes until a user opts. See `docs/CONNECTOR-PREFS.md`.
- Remote MCP connection sharing: the owner of a connected hosted MCP server
  can share it with named users or with everyone on the box (`remote_mcp_shares`,
  migration 031). Grantees' chats and scheduled tasks mount the server's tools;
  tool calls authenticate with the OWNER's OAuth token host-side (the token
  never leaves the host and the grantee never sees it), shared runs are logged
  with owner attribution, and revoking a grant (or deleting the server) takes
  effect on the next turn. New API: `shares` + `shared_with_me` on
  `GET /remote-mcp-servers`, `POST/DELETE /remote-mcp-servers/{id}/shares[/…]`;
  the connections page gains a Share panel per owned server and a "Shared with
  you" section. See `docs/CONNECTION-SHARING.md`.
- Built-in MCP connector directory: fleet now embeds a curated directory of
  ~290 vendor-hosted MCP servers (`internal/clientconfig/builtin_remote_catalog.yaml`)
  spanning 19 categories, inherited by every client bundle by default — the
  directory is a listing only; nothing connects until a user explicitly adds a
  server and completes the per-user OAuth flow (#443). Catalog entries gain
  `category`, `tags`, `provenance` (`official` | `third_party` | `community`),
  `repo_url`, and `auth` metadata; the built-in file requires all of them
  explicitly (CI-enforced) while bundle entries keep back-compat defaults. New
  manifest knobs: `remote_mcp_catalog_builtin: false` (opt out),
  `remote_mcp_catalog_community: true` (opt into community-hosted entries,
  off by default), and `remote_mcp_catalog_hidden: [...]` (tombstone
  individual entries — also the between-release kill switch for a dead or
  compromised endpoint). Bundle entries override built-ins by name; an
  `mcp_servers` name collision shadows the built-in entry with a loud log
  line instead of failing the load. Settings → Connections becomes a
  searchable, category-grouped connector directory with provenance badges,
  docs/source vet-links, auth hints, and an explicit operator-named consent
  step before adding any non-official endpoint. Every shipped URL was
  verified against the vendor's own documentation. See `docs/MCP-CATALOG.md`.

### Changed

- Unified settings UX: Connections, Skills, the new General settings page
  (concurrency cap + MCP credential accounts, formerly the Operations Center's
  Settings modal — now retired), and Admin all render inside one shared
  settings shell with a section nav, an Operations Center cross-link, and a
  "Back to chat" button. The account menu offers Settings · Connections ·
  Skills · Admin identically on both surfaces.
- Web UI polish — rail, composer, motion, responsive (design-handoff parity):
  the rail collapses to a 4.25rem icon strip (toggle in the brand row,
  preference persisted to `localStorage["rail-collapsed"]`, avatar-only
  account menu, collapse closes menus and exits select mode); at ≤900px it
  auto-collapses and expands as an overlay drawer with a scrim, while the
  <640px hamburger drawer is unchanged. Multi-select is redesigned per the
  handoff: a row kebab's "Select…" is the only way into select mode
  (checkboxes are no longer hover-revealed), with a compact icon bulk bar at
  the rail's foot (move-to-folder / pin / add-label / delete / cancel,
  tooltips, disabled at zero selected, popovers with inline folder/label
  creation, Esc exits). The composer container matches the design's geometry
  (53rem column, `--radius-xl`, `--shadow-md`, a focus-fading keyboard hint
  below the bar) — the toolbar keeps its existing control set — and gains a
  sealed variant (accent border + explainer strip + sandbox placeholder) on
  lockdown conversations. The icon-only "new sealed chat" button explains
  what a sealed chat is on hover and keyboard focus. The
  account-menu theme segment is now System | Light | Dark — System follows
  the OS live and clears the stored preference. All transitions/animations
  ride the shared `--motion-fast`/`--motion-base` tokens, every popover and
  modal enters with one fade+rise, reduced-motion handling is consolidated
  into the single global block, and small controls (kebab, chip removers,
  search clear, modal close) get ~2rem hit areas with no visual change.

### Fixed

- Removed the Settings → General concurrency-cap card: it called
  `GET/PUT /concurrency`, endpoints fleet's orchestrator never served (a
  moc-era leftover), so it could only ever surface an error toast. The cap
  remains `FLEET_MAX_CONCURRENT_AGENTS` in the env file (boot-bound — it
  sizes the admission semaphore and sandbox warm pool at startup); see
  [docs/ADMIN-SETTINGS.md](docs/ADMIN-SETTINGS.md) for which settings are
  live-tunable instead.
- `fleet update` now actually ships web changes: `scripts/update.sh` deploys
  the freshly built Next app into the `fleet-web` unit's WorkingDirectory
  (`/opt/fleet/web`) and restarts `fleet-web` after the backend restart —
  previously the build stayed in the source checkout and the live site kept
  serving the old bundle. The rebuild also re-applies the `NEXT_PUBLIC_*`
  origin/app-name stamps bootstrap baked in (grepped from
  `/etc/fleet/fleet-web.env`; the 0600 file is still never sourced).
- Runner terminal side effects gated on the DB write (#580): the success
  notification and email reply fired even when the terminal DB write failed,
  producing spurious "success" and duplicate emails on retry; all terminal
  side effects now wait for the write to land.
- Stale-lease fencing on runner cleanup (#581): the worker cleanup deleted the
  active-map entry by task ID without a token guard, letting a stale goroutine
  clobber a fresh re-claim; the delete is now token-guarded.
- Resumed task keeps the human's answer (#582): a paused-awaiting-human task
  that failed transiently and retried lost the answer because `ClearPendingQA`
  ran before the run; it now clears only at the terminal transition, so the
  answer survives a retry.
- Paused dataset rows resume instead of failing (#586): pausing a running
  dataset marked in-flight rows permanently `failed` and resume never retried
  them; interrupted rows are now requeued to `pending` with the attempt refunded.
- Rune-safe truncation (#595): byte-boundary truncation emitted invalid UTF-8
  (a dataset row could stick in `running`; prompt/log corruption). A shared
  `internal/truncate.Clamp` cuts on rune boundaries and is used by datasets,
  the prior-run handoff, and evals.
- Sub-agent budget over-grant (#588): a release-before-charge lock ordering let
  concurrent sub-agent spawns over-reserve against the parent budget; the
  reservation and charge now settle atomically under one lock.
- Cached-token accounting (#587): context-pressure and child-budget math
  double-counted cached tokens on OpenRouter, so the token ceiling could fire
  late and cache hit-rate could exceed 100%; all usage accumulators are
  normalized onto one include-cached convention.
- Runaway-compaction cap made reachable (#598): `maxConsecutiveCompactions`
  (and `ErrContextBudgetExhausted`) was dead code; the counter now resets only
  on a round that completes without force-compaction, so a genuine
  compaction storm is refused.
- BranchConversation range check + atomicity (#578, #597): an out-of-range
  `branch_point_message_id` silently copied the entire conversation (now errors),
  and the INSERT + history copy now run in one transaction instead of two
  statements.
- AdminStats excludes soft-deleted conversations (#579), and the conversation
  mutators (`SetModel`, `SetPinned`, title/rename, archive, optional-MCP,
  approval-timeout) now exclude soft-deleted rows (#596), matching the guarded
  `SetShareToken`/`SetThinkingConfig`.
- bridgeStdout data race (#583): the container/host `runPython` reader goroutine
  raced bridge teardown on context cancellation; the reader now snapshots the
  bridge handle under the mutex.
- ConcurrencyLimiter eviction (#594): the per-key map never evicted released
  keys (a slow unbounded leak on a long-lived process); a key is now removed the
  moment its in-flight count hits zero.
- Migration DDL linter (#593): the linter missed the COLUMN-less forms of
  `ADD ... NOT NULL` / `DROP ...` it claims to reject; both spellings are now
  caught, and the script gained its first test harness.
- Orchestrator live SSE parser (#589): the orchestrator parser diverged from the
  hardened chat parser (CRLF broke it, multi-line `data:` corrupted); it now
  reuses `parseSseChunk`, so both consumers share one hardened parser.
- OpenAPI + docs honesty (#591, #592, #599): the OpenAPI spec now matches what
  the handlers emit (`GET /api/me` body, `POST /users` 201), the README no
  longer claims the OpenAPI CI test skips body-schema gating (it doesn't), and
  the critical-action cap comment now describes its real failed-attempt-count
  semantics.
- Tool-output ceiling on direct MCP calls (#576): MCP tools registered directly
  (roster below the #506 tool-disclosure threshold — the common case) applied
  redaction and PII gating but not the #199 output ceiling, so one oversized
  connector result could enter the transcript untruncated and overflow the
  context window. `mcpTool.Run` now caps output exactly like the wrapped
  (deferred `tool_call`) path — identical truncation above and below the
  threshold, error results included — and the `tool_output_limit.go` comment no
  longer overstates `policyGuardedTool.Run` as the universal choke point.
- Scheduler liveness (#566): `ProcessScheduledTasks` no longer live-locks when
  a full batch of due tasks makes no forward progress — e.g. ≥1000 one-shot
  tasks all soft-held by a declining `run_if` gate. The old loop paged with a
  plain `LIMIT` and terminated only on a short batch, so a full batch of
  soft-held rows (which stay scheduled + due by design) was re-fetched
  identically forever, hanging the scheduler goroutine and with it lease
  recovery and starvation promotion for the whole box. The due set is now
  walked with a keyset cursor over the total order `(scheduled_for, id)` — each
  row is handled at most once per tick, a held prefix can't mask due work
  behind it (the `id` tiebreaker makes pages stable even when many rows share
  one `scheduled_for`), per-tick cost is linear, and a defensive 100k-rows valve
  bounds a pathological tick. Soft-hold semantics are unchanged: a held one-shot
  stays scheduled and is re-evaluated next tick.
- Web rate limiter (#561): `(*rateLimiter).wait` no longer double-unlocks its
  mutex when the context is cancelled mid-wait. Cancelling a `web_fetch` /
  `web_search` turn while it was blocked in the minimum-interval (or per-minute)
  wait triggered `fatal error: sync: unlock of unlocked mutex`, which `recover()`
  cannot catch and which crashed the whole `fleet` process — killing every
  user's in-flight session. Both waits are now context-aware and release the
  lock exactly once on every path.
- Recurring tasks (#565): the next occurrence of a recurring task now preserves
  every definition field. The recurrence path built each new occurrence from a
  hand-maintained `TaskCreate` literal that omitted many fields, so occurrence
  #2+ silently reset `allow_network`, `carry_context`, `output_schema`,
  `sandbox_limits`, the delegation / task-creation / event-trigger capability
  bits, `persona`, and the SLA config to their zero values — e.g. a daily
  `allow_network:true` task had its sandbox resealed `--network=none` from the
  second run on and failed silently. The path now delegates to the canonical
  `TaskToCreate` clone (also used by re-run/clone #270), and `TaskToCreate` was
  itself completed to carry `persona` and `carry_context`, which it had been
  dropping — so re-run/clone stops losing them too. A reflection guard
  (`TestTaskToCreateCarriesEveryDefinitionField`) now fails if a new `TaskCreate`
  definition field is added without being carried. Network posture is preserved,
  never widened.
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
- API keys minted by the CLI (`fleet sched apikey create`) now authenticate
  against an already-running server without a restart: on a lookup miss the
  key manager stats `api_keys.json` and reloads newly-appended keys (existing
  in-memory keys — and their runtime rate/budget state — are never clobbered,
  and a just-revoked key can't be resurrected). Found by exercising the
  documented mint-and-use flow live.

### Added

- Usage analytics (#601 part 1): admin-only `GET /admin/usage?group_by=&from=&to=`
  rolls the already-persisted metering (per-iteration `task_iterations` ⋈
  `tasks`, plus the chat `turn_metrics` session log) up by user, API key,
  project, model, or day/week time bucket over a requested window — a pure
  read model: no new accounting path, no new tables. Rendered in the
  Operations Center as an admin-only "Usage" tab (KPI tiles, single-hue
  bar/column charts coherent in light + dark, full table view). Honest scope
  (#289): native-provider runs accrue $0 unless a pricing override is
  configured, so the endpoint and panel always show token totals alongside
  dollars and say so. Per-principal budgets are part 2 of #601 and are not
  included here. See `docs/USAGE-ANALYTICS.md`.
- Per-principal rolling budgets with alerts (#601 part 2): a budget is
  `{scope: user|key|project, principal, window: day|week|month, soft/hard
  bounds in dollars AND tokens}` (one new sched table, migration 052),
  enforced at task-create by ONE shared gate across every create path —
  `POST /tasks`, `POST /tasks/batch`, and the chat `schedule_task` approval
  seam. Spend is recomputed per check from the part-1 usage read model (no
  second accounting path). At a soft bound exactly one notify alert fires per
  window crossing (persisted marker — restart-safe, concurrency-safe) through
  the existing email/webhook/Web Push pipeline; at a hard bound new task
  creation is refused with 402 + `Retry-After` until the window rolls over.
  Fail-safe composition with #286: effective hard bounds are clamped to the
  LIVE global `FLEET_MAX_COST_USD` / `FLEET_MAX_TOTAL_TOKENS` — budgets only
  narrow, never widen, and no budget configured is byte-for-byte today's
  behavior. Admin CRUD at `GET/POST /admin/budgets` +
  `DELETE /admin/budgets/{id}` (in `docs/openapi.yaml`, parity-tested); the
  Operations Center Usage panel lists configured budgets read-only with live
  window spend. Honest scope: `scope=project` is stored/reported but not yet
  enforced (no create path carries a project); admin-key submissions and
  in-process spawn/rerun paths are not gated. See `docs/USAGE-ANALYTICS.md`.
- `fleet task run <task.yaml>` — the local one-shot harness (run a single task
  to completion through the governed scheduled runtime, no server/DB) is now a
  verb of the unified CLI instead of the separate `cutlass` binary; the logic
  moved to `internal/taskrun` and `cmd/cutlass` remains as a warning shim for
  one deprecation release (the fleet-admin pattern). The example task file is
  now `docs/examples/local-task.yaml`, and the Charmbracelet acknowledgement
  credits the full charm stack behind the TUI (Bubble Tea, Bubbles, Lip Gloss,
  Glamour, vhs, freeze) alongside Fantasy.
- Tool-pipeline metrics endpoint (#543): admin-only
  `GET /admin/pipeline-metrics` derives per-run tool turns, total/distinct
  tool calls, tokens, cost, and wall clock from the session logs fleet already
  persists (no new columns — works retroactively on every retained run), plus
  a fleet-wide aggregate with a tool-turn histogram and the share of runs that
  were long multi-tool pipelines. This is the sensor behind #505's reopen
  criteria: optimization decisions become a threshold check, not a vibe.
- Demo GIFs for every surface (#540): the README now opens with three
  recordings telling one story — plan in chat (a REAL model + sandbox take),
  automate in the Operations Center (real scheduler), ride along in the TUI
  (deterministic mock). Recording + conversion pipelines are scripted
  (`docs/scripts/record-web-demos.mjs`, `generate-web-gifs.sh`,
  `generate-tui-gif.sh` with a verified-take retry loop) and documented in
  `docs/generating-demo-gif.md`.

- `fleet cleanup [--dry-run] [--deep]` — reclaim host-side build/deploy cruft:
  dangling podman image layers (each sandbox rebuild strands ~1.3 GB) and the
  Go build/test caches; `--deep` additionally prunes unused named images,
  stopped containers, and networks. Never touches databases, workspaces, or
  the client bundle. `scripts/update.sh` now prunes dangling layers
  automatically after a successful sandbox rebuild, so routine updates can't
  fill the disk. CLI usage strings now name the unified `fleet` binary
  instead of the deprecated `fleet-admin`.
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

### Security

This release closes a batch of security findings raised against the run loop,
the scheduler, the HTTP surface, and the native tools. Every fix is a strict
tightening — no previously-sealed posture was widened, and each restores or
reinforces a documented invariant.

- Lockdown network seal on approved bash (#562, P0): approved-bash execution
  ran in a network-enabled warm-pool container, silently dropping the
  `--network=none` hard seal for lockdown conversations. Staged bash now takes
  the sealed container whenever the conversation is locked down or
  `CHAT_LOCKDOWN_ONLY=true`, fail-closed, mirroring the turn path.
- Lockdown model allowlist bypass (#568, P1): the per-turn model override on the
  existing-conversation chat path skipped the lockdown allowlist gate the
  create/PATCH paths enforce; overrides are now checked and rejected before the
  turn runs.
- Persona tool-allowlist (Gate-4) bypass (#570, P1): once BM25 tool disclosure
  deferred MCP tools behind bridges, the persona allowlist stopped governing
  them. Persona policy is now applied to each logical MCP tool before the
  disclosure decision, so denied tools never enter the deferred registry.
- Secret scrubber gaps (#569, P1): the redactor missed
  `aws_secret_access_key`-style names and `Authorization: Basic` headers,
  leaking credentials into the model context and logs; both are now scrubbed.
- API-key rotation stale hash (#567, P1): a second rotation left the first
  rotated-out key hash valid indefinitely; the outgoing predecessor is now
  revoked so only the current key and the most recent predecessor (within grace)
  authenticate.
- DeleteUser credential inheritance (#563, P1): deleting a user orphaned their
  `remote_mcp_servers` rows and encrypted OAuth tokens, so re-using the email
  inherited them; deletion now purges them (and owned projects) in-transaction.
- download_url workspace escape (#564, P1): a relative `output_dir` containing
  `..` escaped the workspace to arbitrary host-side writes; traversal segments
  are now rejected before the path is joined.
- Cross-conversation workspace traversal (#575): file tools accepted `..` to
  reach other conversations' workspaces; per-conversation confinement is now
  enforced across the view/edit/write and upload paths.
- Email content_file arbitrary read (#573): outbound email could materialize
  arbitrary host files via absolute paths and `$VAR`/`~` expansion; content is
  now confined to the conversation workspace with no shell expansion.
- SSRF classifier gaps (#574): `web_fetch`/`web_search` did not block CGNAT
  `100.64.0.0/10` (Alibaba/Oracle cloud-metadata) or several reserved ranges.
  The private/reserved-IP classifier is now a single shared `internal/netguard`
  implementation (previously duplicated and drifted) covering CGNAT, the RFC
  test/benchmark/reserved nets, and multicast, applied at dial time.
- output_schema external `$ref` file read (#585): an untrusted `output_schema`
  with a `file://` (or `http(s)://`) `$ref` made the host open arbitrary files
  during schema compile; external ref resolution is now refused.
- Personal memory API scope escape (#577): personal `UpdateMemory`/`DeleteMemory`
  omitted `project_id IS NULL`, letting project-scoped memory be mutated via the
  personal API; the guard is now applied.
- Webhook signature timing oracle (#572): Slack and custom-HMAC-header trigger
  verification returned early on shape mismatches, leaking a slug-enumeration
  timing side channel; verification now performs the same body-HMAC work on
  every path and fails closed.
- Shared-link limiter DoS (#571): `/shared/{token}` created an unbounded rate
  bucket per unvalidated token; the token is now resolved before a bucket is
  created, bounding the map and making the abuse gate effective.
- Config-reload secret rotation (#584): reload silently rotated file-sourced
  webhook/Slack HMAC secrets, bypassing the non-reloadable contract; such
  secrets are now held at their boot value and reported under `Skipped`.
- HTTP-tool JSON body injection (#600): inline HTTP-tool JSON body templates
  interpolated model args raw, allowing JSON injection into the outbound
  request; values substituted into JSON bodies are now JSON-escaped (fail-closed).
- Content-Security-Policy for the public share iframe (#590): responses now
  carry a CSP (strictest on `/shared/*`) so assistant-authored HTML in the
  sandboxed preview iframe can no longer beacon out to attacker hosts.
- Sandbox image CVEs (#620): the sandbox image pins patched `pypdf`, `tornado`,
  `pip`, `idna`, and `pygments` over the lagging Fedora RPMs (removing the RPM
  copies so a single unambiguous version remains), clearing 41 Grype findings
  (verified 41 → 0 with the CI-pinned scanner).
- Static-analysis hardening: enabled CodeQL code scanning, secret scanning +
  push protection, Dependabot security updates, and private vulnerability
  reporting. Addressed the resulting CodeQL findings — path-injection sinks
  routed through recognized confinement barriers, unbounded allocations bounded,
  and a narrowing integer conversion guarded; findings that were provably safe
  (dial-time-guarded SSRF, high-entropy-token SHA-256 with constant-time
  compare, HTTP-boundary-confined image paths) were dismissed with written
  justification.
