# fleet documentation index

Two lists. **By question** (first) is curated: the pages an agent or
contributor reaches for most, grouped by what they need to know, in the order
that used to live inside `AGENTS.md` before it moved here so that guide stays
short. **All pages** (second) is complete: every Markdown file in `docs/`,
alphabetically, with its title — a test (`scripts/check_docs_index_test.go`)
fails when a page is added without a row there. Every entry is a design note,
runbook or decision record about how fleet is shaped — the *why* a code comment
cannot carry. The historical one-paragraph feature notes are in
[`FEATURE-NOTES.md`](FEATURE-NOTES.md), and every decision that touches an
invariant is an ADR in [`adr/`](adr/), which keeps its own index.

Three other entry points sit outside this directory:
[`../AGENTS.md`](../AGENTS.md) (the operating guide, for humans and agents
alike), [`../CONTRIBUTING.md`](../CONTRIBUTING.md) (the contributor front
door) and [`../.agents/skills/steward/SKILL.md`](../.agents/skills/steward/SKILL.md)
(how a PR is driven to green after it is opened).

## By question

- **Versioning and releases** (date-based `vYYYY.MM.DD.N`, tagged automatically
  on every green push to `main`; there is no `VERSION` file, no semver, and no
  release ceremony — do not add a hand-authored version number anywhere):
  [`docs/VERSIONING.md`](VERSIONING.md) +
  [ADR-0059](adr/0059-date-based-rolling-releases.md)
- **Per-feature design notes** (shipped design, deviations from the issue, honest
  scope — one bullet per feature): [`docs/FEATURE-NOTES.md`](FEATURE-NOTES.md).
  Newer features each have a dedicated page in [`docs/`](./), and invariant
  changes have an ADR in [`docs/adr/`](adr/).
- **Agent runtime mechanics** (per-turn sandbox seal, cost/token ceilings,
  context compaction, MCP credential allowlist, the scheduled end-of-run
  verifier, the optional "phone a friend" super-LLM review, git-worktree
  isolation): [`docs/AGENT-RUNTIME.md`](AGENT-RUNTIME.md)
- **Architecture overview:** [`README.md`](../README.md) ("Architecture at a glance")
- **Why the invariants are the way they are:** [`docs/adr/`](adr/)
  (Architecture Decision Records)
- **Contributor workflow + CI gates:** [`CONTRIBUTING.md`](../CONTRIBUTING.md)
- **Driving a PR to green after it is opened** — the follow-up loop for any
  coding agent or human. Posture: *we can fix everything*; if you are driving
  the PR, every red check and every open thread on it is yours, whoever wrote
  the code. The procedure is the skill
  [`.agents/skills/steward/SKILL.md`](../.agents/skills/steward/SKILL.md)
  (`.claude/skills/steward` is a symlink to it); the reference behind it —
  the two CI lanes, promotion mechanics, reviewers, pinned tool versions, the
  local-reproduction traps — is
  [`docs/PR-STEWARDSHIP.md`](PR-STEWARDSHIP.md)
- **Testing strategy** (unit / fake-LLM / mocked + live Playwright / canary):
  [`docs/TESTING.md`](TESTING.md)
- **The scanning stack** (who checks what, why ruff owns Python lint, why
  Semgrep runs all four registry packs — `p/github-actions`, `p/golang`,
  `p/javascript`, `p/python` — and blocks with 7 false positives waived at the
  line, what blocks vs what only reports, and the known gaps, chief among them
  that the `dev` ruleset requires no status checks):
  [`docs/SCANNING.md`](SCANNING.md)
- **CodeQL** (why default setup was replaced by an advanced-setup workflow, how
  the Go toolchain is resolved, why it runs security queries only, why a
  `pull_request` run certifies a diff and not a tree, and the High-band threshold
  plus accepted-findings register): [`docs/CODEQL.md`](CODEQL.md) +
  [ADR-0048](adr/0048-codeql-severity-gating.md)
- **HTTP API versioning** (the `/v1` prefix + `X-Fleet-API-Version` + `/api-info`
  discovery + deprecation contract): [`docs/api-versioning.md`](api-versioning.md)
- **Machine clients** (what the TLS front routes to the Go listeners and why the
  bare paths 307 to `/login`, minting a key into the store the *service* reads,
  `X-API-Key` as the one public auth path, `/v1/tasks/estimate` as the free
  connection test): [`docs/API-CLIENTS.md`](API-CLIENTS.md) +
  [ADR-0053](adr/0053-public-api-through-the-tls-front.md)
- **Database migrations** (the two runners, safe-DDL patterns, the migration DDL
  linter, `fleet migrate status`, rollback scope): [`docs/MIGRATIONS.md`](MIGRATIONS.md)
- **Web-tier shutdown** (why `fleet-web` dumped core on nearly every restart:
  the npm wrapper's `uv_kill` segfault, Fedora's abort-on-timeout, the residual
  upstream teardown crash, and `LimitCORE=0` — plus the drain theory that
  measurement refuted): [`docs/WEB-TIER-SHUTDOWN.md`](WEB-TIER-SHUTDOWN.md)
- **Downloading a chat** (the three export formats and why the readable one is
  the default, the include-the-agent's-work scope, and the HTML renderer's
  escaping guarantees): [`docs/CHAT-EXPORT.md`](CHAT-EXPORT.md)
- **Chat stream recovery** (why a lost SSE socket reconciles against Postgres
  instead of stamping a terminal state — the walk-away-and-come-back case):
  [`docs/CHAT-STREAM-RECOVERY.md`](CHAT-STREAM-RECOVERY.md)
- **Shared files** (the native cross-chat file library: canonical bytes
  host-side, a read-only staged tree under the workspace root both sandbox
  backends mount, the reconciler, the size cap):
  [`docs/SHARED-FILES.md`](SHARED-FILES.md)
- **Attachment scoping** (why the uploads tree is mounted into no sandbox, how
  a turn's attachments reach one — staged into that conversation's own
  workspace on both backends — the per-owner upload segment that makes
  containment the ownership check, and the injected-context column that keeps
  server-added context out of the user's message text):
  [`docs/ATTACHMENT-SCOPING.md`](ATTACHMENT-SCOPING.md) +
  [ADR-0058](adr/0058-per-conversation-attachment-scoping.md)
- **Task titles** (the operator-facing display label, and why it is NOT the
  unique import/export `name` column): [`docs/TASK-TITLES.md`](TASK-TITLES.md)
- **Sharing work inside a project** (team-shared chats and the read-only view
  teammates branch from, "team learnings" as the user-facing name for a
  project's shared memory, the vocabulary, and why a team-shared chat can only
  live inside a team-shared project): [`docs/TEAM-SHARING.md`](TEAM-SHARING.md)
  + [ADR-0057](adr/0057-team-shared-chats-live-in-team-shared-projects.md)
- **Agent Plugins** (the portable `plugin.json` + `skills/` + `mcp.json`
  package format from agent-plugins.org, loaded from the bundle's `plugins/`
  dir and `plugin_roots:`; how it maps onto the skills tree + MCP catalog, the
  spec's failure boundaries, what is deliberately not read):
  [`docs/AGENT-PLUGINS.md`](AGENT-PLUGINS.md) +
  [ADR-0054](adr/0054-agent-plugins.md)
- **MCP server hot-reload** (add/remove/update MCP servers without a restart via
  `fleet mcp reload` / SIGHUP / admin endpoint): [`docs/MCP-RELOAD.md`](MCP-RELOAD.md)
- **Testing MCP servers** (`fleet mcp test` per-server smoke: handshake +
  tools/list with the boot loader's exact env/gates; plus the full testing
  ladder): [`docs/MCP-TESTING.md`](MCP-TESTING.md)
- **The connector directory** (trust classes, the built-in hosted-server
  catalog, provenance tiers) and its **guided onboarding** (setup hints,
  guided tenant/API-key/BYO-client add forms, the per-user api_key auth mode):
  [`docs/MCP-CATALOG.md`](MCP-CATALOG.md) +
  [`docs/CONNECTOR-ONBOARDING.md`](CONNECTOR-ONBOARDING.md)
- **Bundle-managed SES/S3 email-report infrastructure:** use the external
  canonical [new-client email-report runbook](https://github.com/ElcanoTek/ses-s3-setup/blob/main/docs/NEW-CLIENT-EMAIL-SETUP.md);
  keep client-specific inventory in the external client bundle.
- **White-labeling from a bundle** (`branding:` — strings, `logo`, the themable
  color tokens, what stays build-time env, and the trust class of the two brand
  asset routes): [`docs/BRANDING.md`](BRANDING.md)
- **Admin-managed workspace feature settings** (the Settings → Admin Features
  panel: DB override > env var > default, live apply, the registry admission
  rule, what stays env-only): [`docs/ADMIN-SETTINGS.md`](ADMIN-SETTINGS.md)
- **Task notifications** (email/webhook channels, the admin Notifications
  panel with sealed write-only secrets + test sends, env precedence):
  [`docs/NOTIFICATIONS.md`](NOTIFICATIONS.md)
- **Reclamation, disk backpressure & stuck-task backstops** (the one hourly
  maintenance loop, the daily `fleet-maintenance.timer`, the free-space floor
  that sheds scheduled work while chat keeps serving, and the terminal backstop
  for every way a task can stall): [`docs/MAINTENANCE.md`](MAINTENANCE.md)
- **Installing the backup/maintenance timers on an existing box**
  (`fleet timers install`, the `fleet update` offer + `--no-timers` opt-out,
  the non-systemd/Kubernetes posture): [`docs/TIMERS.md`](TIMERS.md)
- **Kubernetes as a first-class deployment** (the `deploy/helm/fleet` chart,
  the pluggable sandbox backend — `FLEET_SANDBOX_BACKEND=podman|kubernetes`,
  sandboxes as ephemeral pods, the fail-closed cluster preflight, and the
  honest deviations from the podman backend):
  [`docs/DEPLOYMENT-KUBERNETES.md`](DEPLOYMENT-KUBERNETES.md) +
  [ADR-0049](adr/0049-kubernetes-backend-split-control-plane.md). The
  bundle side of that path is
  [`ElcanoTek/example-kubernetes-config`](https://github.com/ElcanoTek/example-kubernetes-config),
  the Kubernetes peer of `example-config` — out-of-repo client content, per the
  coupling doctrine below, so fleet links to it rather than vendoring it.
- **Load testing & benchmarks** (`fleet-bench` HTTP chat load via the fake-LLM
  seam + subsystem throughput benchmarks): [`docs/LOAD-TESTING.md`](LOAD-TESTING.md)
- **Prompt-cache prefix-stability contract** (what must stay byte-stable in the
  cacheable prefix so the provider prompt cache keeps hitting):
  [`docs/PROMPT-CACHE-CONTRACT.md`](PROMPT-CACHE-CONTRACT.md)
- **Upstream routing floor** (why a soft provider pin needs a serving-precision
  allow-list under it, and the served-upstream attribution that tells a routing
  fallback apart from a bad model):
  [`docs/UPSTREAM-ROUTING-FLOOR.md`](UPSTREAM-ROUTING-FLOOR.md)
- **Evals & regression gating** (golden capture, the `evals/` bundle contract,
  scorers + LLM-judge, `fleet eval` CLI):
  [`docs/EVALS.md`](EVALS.md) + [`docs/adr/0018-self-hosted-eval-harness.md`](adr/0018-self-hosted-eval-harness.md)
- **Governed lifecycle hooks** (bundle-declared `hooks:` run in the sandbox at
  prompt-submit / pre+post-tool / turn-end; observe-or-narrow only, never widen):
  [`docs/HOOKS.md`](HOOKS.md) + [ADR-0038](adr/0038-governed-lifecycle-hooks.md)
- **Reporting a vulnerability:** [`SECURITY.md`](../SECURITY.md)

## All pages

Every page under `docs/`, A–Z. Add a row when you add a page; the test named
above fails otherwise.

- [`A2A.md`](A2A.md) — The A2A protocol server
- [`ADMIN-SETTINGS.md`](ADMIN-SETTINGS.md) — Admin-managed workspace feature settings
- [`AGENT-PLUGINS.md`](AGENT-PLUGINS.md) — Agent Plugins (#1166)
- [`AGENT-RUNTIME.md`](AGENT-RUNTIME.md) — The fleet agent runtime
- [`API-CLIENTS.md`](API-CLIENTS.md) — Machine clients: reaching the fleet API from another system
- [`api-versioning.md`](api-versioning.md) — HTTP API versioning (#321)
- [`APPROVAL-CARDS.md`](APPROVAL-CARDS.md) — Approval cards: the human-review surface in chat
- [`ASK-NOTIFY.md`](ASK-NOTIFY.md) — ask / notify + paused-awaiting-human run state
- [`ATTACHMENT-SCOPING.md`](ATTACHMENT-SCOPING.md) — Attachment scoping, and where a turn's injected context lives
- [`AUX-MODEL-CALL-METERING.md`](AUX-MODEL-CALL-METERING.md) — Auxiliary model-call metering (#1118)
- [`BACKUP_RESTORE.md`](BACKUP_RESTORE.md) — Backup & restore (disaster recovery)
- [`BENTO-PDF-EXPORT.md`](BENTO-PDF-EXPORT.md) — Bento PDF export — what shipped, and what it is not
- [`BRANDING.md`](BRANDING.md) — White-labeling fleet from a bundle
- [`BROWSERBASE.md`](BROWSERBASE.md) — Browserbase: hosted browser sessions with a human handoff (#987)
- [`BUILDING-ON-FLEET.md`](BUILDING-ON-FLEET.md) — Building on fleet: the API as your automation substrate
- [`CHAT-EXPORT.md`](CHAT-EXPORT.md) — Downloading a chat
- [`CHAT-STREAM-RECOVERY.md`](CHAT-STREAM-RECOVERY.md) — Chat stream recovery — losing the socket is not losing the turn
- [`CODEQL.md`](CODEQL.md) — CodeQL: advanced setup, and the Go analysis that had stopped working
- [`CONFIG-RELOAD.md`](CONFIG-RELOAD.md) — Config hot-reload (#286)
- [`CONNECTION-SHARING.md`](CONNECTION-SHARING.md) — Sharing a remote MCP connection
- [`CONNECTOR-ONBOARDING.md`](CONNECTOR-ONBOARDING.md) — Connector-directory onboarding — guided setup, API keys, BYO OAuth clients
- [`CONNECTOR-PREFS.md`](CONNECTOR-PREFS.md) — Unified connector enablement — availability, selection, binding
- [`CONTEXT-HANDLES.md`](CONTEXT-HANDLES.md) — Composer context handles (#517)
- [`CUTOVER.md`](CUTOVER.md) — v1 → fleet cutover runbook (a box already running the legacy chat + moc stack)
- [`DATASETS.md`](DATASETS.md) — Dataset / table agent
- [`DEPLOYMENT-KUBERNETES.md`](DEPLOYMENT-KUBERNETES.md) — Deploying fleet on Kubernetes
- [`DEPLOYMENT.md`](DEPLOYMENT.md) — Deploying fleet
- [`DISCUSS-RUN.md`](DISCUSS-RUN.md) — Discuss this run
- [`DOCTOR.md`](DOCTOR.md) — Doctor — box-level diagnose + repair (`fleet doctor` + Settings → Admin → Doctor)
- [`ENV-CLI.md`](ENV-CLI.md) — `fleet env` — inspect + edit the deployment env files
- [`EVALS.md`](EVALS.md) — Self-hosted evals & regression gating
- [`EVENT-TRIGGERS.md`](EVENT-TRIGGERS.md) — Event-driven triggers — email ingress (#511)
- [`FEATURE-NOTES.md`](FEATURE-NOTES.md) — Feature design notes
- [`FILE-EDIT-SAFETY.md`](FILE-EDIT-SAFETY.md) — File-edit safety (#787)
- [`generating-demo-gif.md`](generating-demo-gif.md) — Generating the demo GIFs (TUI + web)
- [`GUARDRAILS.md`](GUARDRAILS.md) — Prompt-injection guardrails (#702)
- [`HOOKS.md`](HOOKS.md) — Governed lifecycle hooks (#788)
- [`implementation-plans-enhancements.md`](implementation-plans-enhancements.md) — Implementation plan: #984 — Fleet ↔ Buzz bridge
- [`INPUT-QUEUE.md`](INPUT-QUEUE.md) — Input queue & mid-turn steering (#785)
- [`keyboard-shortcuts.md`](keyboard-shortcuts.md) — Keyboard shortcuts
- [`KUBERNETES-LIVE-TEST.md`](KUBERNETES-LIVE-TEST.md) — Real-cluster sandbox integration test
- [`LEGACY-IMPORT.md`](LEGACY-IMPORT.md) — Legacy import — migrating chat + moc data into fleet
- [`LIVE-RUNS.md`](LIVE-RUNS.md) — Live run visibility & take-over
- [`LLM-PROVIDERS.md`](LLM-PROVIDERS.md) — Admin-managed LLM providers
- [`LOAD-TESTING.md`](LOAD-TESTING.md) — Load testing & benchmarks (#296)
- [`MAINTENANCE.md`](MAINTENANCE.md) — Reclamation, disk backpressure, and stuck-task backstops
- [`MCP-BROKER-SCOPES.md`](MCP-BROKER-SCOPES.md) — MCP broker scoped sessions
- [`MCP-BUNDLE-ENV.md`](MCP-BUNDLE-ENV.md) — MCP bundle env contract: `${FLEET_WORKSPACE}`, `MCP_VARIANT_CLIENT`, `identity_env`, interactive critical-tool staging
- [`MCP-CATALOG.md`](MCP-CATALOG.md) — The MCP connector directory — trust classes, built-in catalog, provenance
- [`MCP-RELOAD.md`](MCP-RELOAD.md) — MCP server hot-reload (#218)
- [`MCP-TESTING.md`](MCP-TESTING.md) — Testing MCP servers
- [`MEMORY.md`](MEMORY.md) — User memory: typed, provenanced, reviewable
- [`MIGRATIONS.md`](MIGRATIONS.md) — Database migrations
- [`MODEL-COST-INDICATORS.md`](MODEL-COST-INDICATORS.md) — Model cost indicators ($ … $$$$)
- [`NODE-TOOLCHAIN-HANDOFF.md`](NODE-TOOLCHAIN-HANDOFF.md) — The node toolchain handoff (`fleet update` ⇄ `fleet doctor --node`)
- [`NOTIFICATIONS.md`](NOTIFICATIONS.md) — Task notifications (email + webhook) & admin management
- [`ONBOARDING.md`](ONBOARDING.md) — Onboarding: clone to your first sandbox session
- [`OPEN-REMOTE-MCP.md`](OPEN-REMOTE-MCP.md) — Open-access remote MCP connections
- [`OPERATORS.md`](OPERATORS.md) — Operating fleet
- [`OPS-CONNECTOR-DEFAULTS.md`](OPS-CONNECTOR-DEFAULTS.md) — Operations connector defaults
- [`PII-REDACTION.md`](PII-REDACTION.md) — Optional PII redaction (#450)
- [`PR-STEWARDSHIP.md`](PR-STEWARDSHIP.md) — PR stewardship — the reference
- [`PRIME-AGENT-COMPARISON.md`](PRIME-AGENT-COMPARISON.md) — Prime Agent comparison — what fleet borrowed, and what it deliberately didn't
- [`PROJECTS.md`](PROJECTS.md) — Projects / Spaces: shared team workspaces
- [`PROMPT-CACHE-CONTRACT.md`](PROMPT-CACHE-CONTRACT.md) — Prompt-cache prefix-stability contract (#507)
- [`PROMPT-LIBRARY.md`](PROMPT-LIBRARY.md) — Hybrid prompt library
- [`PROVIDERS.md`](PROVIDERS.md) — Multi-provider LLM configuration (#289)
- [`PUSH-NOTIFICATIONS.md`](PUSH-NOTIFICATIONS.md) — Browser push notifications (Web Push)
- [`RECURRENCE-END.md`](RECURRENCE-END.md) — Recurrence end conditions + horizon-based Upcoming projection
- [`RELIABILITY-REVIEW.md`](RELIABILITY-REVIEW.md) — Repository reliability review
- [`REMOTE-MCP-MULTI-LOGIN.md`](REMOTE-MCP-MULTI-LOGIN.md) — Multiple logins for hosted (official) MCP connections
- [`RUN-LOG-HISTORY.md`](RUN-LOG-HISTORY.md) — Per-attempt run log history
- [`RUNTIME-DATE.md`](RUNTIME-DATE.md) — Runtime date window (#1026)
- [`SANDBOX-IMAGE-FRESHNESS.md`](SANDBOX-IMAGE-FRESHNESS.md) — Sandbox image freshness — the max-age rebuild backstop
- [`SANDBOX-RUNTIMES.md`](SANDBOX-RUNTIMES.md) — Sandbox OCI runtimes — runc · Kata · libkrun
- [`SANDBOX-START-TIMEOUT.md`](SANDBOX-START-TIMEOUT.md) — Sandbox start timeout & the keep-id image pre-warm (#1358)
- [`SCANNING.md`](SCANNING.md) — The scanning stack: who checks what, and what actually gates
- [`SCHEDULER-UX.md`](SCHEDULER-UX.md) — Scheduler UX 2.0 — upcoming runs + recurring context carry (#504)
- [`SELF-IMPROVING-MEMORY.md`](SELF-IMPROVING-MEMORY.md) — Self-improving memory: feedback → learned instructions
- [`SELF-WAKE.md`](SELF-WAKE.md) — Self-wake: sleep / wake_on_event
- [`SERVER-STATS.md`](SERVER-STATS.md) — Admin server statistics
- [`SESSION-EPOCH.md`](SESSION-EPOCH.md) — Per-user session epoch (password reset ends outstanding sessions)
- [`SHARED-FILES.md`](SHARED-FILES.md) — Shared files: the cross-chat file library
- [`SKILLS.md`](SKILLS.md) — Skills (#513, phase 1)
- [`STRUCTURED-OUTPUT.md`](STRUCTURED-OUTPUT.md) — Structured output contracts
- [`SUBAGENTS.md`](SUBAGENTS.md) — Sub-agents: default-on, parent decides, typed children (#1043)
- [`TASK-SCHEDULE-UX.md`](TASK-SCHEDULE-UX.md) — Create Task schedule controls
- [`TASK-SERIALIZATION.md`](TASK-SERIALIZATION.md) — Task serialization — opaque `serialization_key` mutual exclusion (#709)
- [`TASK-TITLES.md`](TASK-TITLES.md) — Task titles
- [`TEAM-SHARING.md`](TEAM-SHARING.md) — Sharing work inside a project — team-shared chats and team learnings
- [`TESTING.md`](TESTING.md) — Testing fleet
- [`TIMERS.md`](TIMERS.md) — `fleet timers install` — one-command setup for the scheduled-maintenance timers
- [`TOOL-DISCLOSURE.md`](TOOL-DISCLOSURE.md) — BM25 progressive tool disclosure
- [`TOOL-OUTPUT-BOUNDARY.md`](TOOL-OUTPUT-BOUNDARY.md) — Bounded model-visible tool output
- [`TOOL-PANIC-CONTAINMENT.md`](TOOL-PANIC-CONTAINMENT.md) — Agent tool panic containment
- [`TURN-JOURNAL.md`](TURN-JOURNAL.md) — Durable turn journal & commit-gated terminal success (#798)
- [`UNIFIED-ADMIN-PERMISSION-UI.md`](UNIFIED-ADMIN-PERMISSION-UI.md) — Unified admin permission in the Users UI
- [`UPLOADS-AND-STORAGE.md`](UPLOADS-AND-STORAGE.md) — Uploads & storage management
- [`UPSTREAM-ROUTING-FLOOR.md`](UPSTREAM-ROUTING-FLOOR.md) — Upstream routing: precision floor + served-upstream attribution
- [`USAGE-ANALYTICS.md`](USAGE-ANALYTICS.md) — Usage analytics & budgets (#601)
- [`VERSIONING.md`](VERSIONING.md) — Versioning and releases
- [`WEB-TIER-SHUTDOWN.md`](WEB-TIER-SHUTDOWN.md) — Web-tier shutdown — why `systemctl restart fleet-web` was dumping core
- [`WEBHOOK-SIGNING.md`](WEBHOOK-SIGNING.md) — Webhook signing (verifying fleet's outbound webhooks)
- [`WEBHOOKS.md`](WEBHOOKS.md) — Webhook-triggered conversations (#268)
