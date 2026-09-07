# fleet documentation index

Where to look, by question. This index used to live inside `AGENTS.md`, which
is the operating guide agents read on every task; it moved here so that guide
stays short and this list can be as long as it needs to be. Every entry is a
design note, runbook or decision record about how fleet is shaped — the *why*
a code comment cannot carry. Newer features each have a dedicated page here;
the historical one-paragraph notes are in [`FEATURE-NOTES.md`](FEATURE-NOTES.md),
and every decision that touches an invariant is an ADR in [`adr/`](adr/).

Three other entry points sit outside this directory:
[`../AGENTS.md`](../AGENTS.md) (the operating guide, for humans and agents
alike), [`../CONTRIBUTING.md`](../CONTRIBUTING.md) (the contributor front
door) and [`../.agents/skills/steward/SKILL.md`](../.agents/skills/steward/SKILL.md)
(how a PR is driven to green after it is opened).

- **Versioning and releases** (date-based `vYYYY.MM.DD.N`, tagged automatically
  on every green push to `main`; there is no `VERSION` file, no semver, and no
  release ceremony — do not add a hand-authored version number anywhere):
  [`docs/VERSIONING.md`](VERSIONING.md) +
  [ADR-0059](adr/0059-date-based-rolling-releases.md)
- **Per-feature design notes** (shipped design, deviations from the issue, honest
  scope — one bullet per feature): [`docs/FEATURE-NOTES.md`](FEATURE-NOTES.md).
  Newer features each have a dedicated page in [`docs/`](), and invariant
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
