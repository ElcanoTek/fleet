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

### Removed

- **The in-sandbox `browser` tool (#503) and `FLEET_BROWSER_ENABLED`.**
  **Breaking for deployments that set that flag** — the tool is gone and the
  variable is no longer read (it is also out of the `.env` allowlist, so a
  leftover line is ignored rather than silently inert). The Playwright/Chromium
  reference layer is dropped from `config/default/sandbox/Containerfile`.
  It was never installable on the image fleet ships, its real Chromium path was
  never exercised in CI, and its shipped scope was DOM-only with no credentials
  and no login-walled sites. Browser automation now arrives as an MCP connector
  (e.g. Browserbase) under the existing host-side credential broker.
  See [ADR-0044](docs/adr/0044-remove-in-sandbox-browser-tool.md), which
  supersedes ADR-0022; `docs/BROWSER.md` is removed.
- **Operations Center username/password form.** Cookie/OIDC is the operator
  path. `fleet admin add` mints an unusable random password, so that form
  could not admit a real operator. Backend `POST /auth/login` and the bearer
  proxy stay for API clients. A chat-signed-in visitor who is not provisioned
  here sees a dead-end "ask an admin" card.
- **Budget `scope=project` is rejected on create.** A project budget was
  accepted and listed but never enforced (tasks have no project dimension).
  `POST /admin/budgets` now returns 400 for `scope=project`. Leftover rows
  still list. `user` and `key` scopes are unchanged.
- **Dead `GET /concurrency` Playwright stubs.** The moc-heritage concurrency
  card was already gone; the mocked e2e still answered an endpoint fleet never
  served.

### Added

- **Budget create/delete in the Usage panel** and `fleet sched budget
  list|create|delete`. The panel was read-only; CRUD is no longer API-only.
- **Per-task sandbox limits on the task form** (Advanced: memory / CPUs /
  PIDs) and `fleet sched task set-limits`. Create and edit persist
  `sandbox_limits`; `--clear` reverts to the global defaults.
- **Fire event + sleeping-task list.** A `paused_awaiting_wake` task waiting
  on a named event can be woken from the log viewer. A thin Sleeping list on
  the tasks tab surfaces parked work. Status filters include both pause
  states.

### Fixed

- **Sub-agents finish, and you can see what they are doing.** A spawned child
  ran the ROOT run's finish enforcement: it was refused a finish until it read
  `protocols/self-audit.md` and called `confirm_audit(...)`, and the host-side
  end-of-run verifier re-checked its deliverables. Neither is a child's gate.
  Against a real model, a child asked for a haiku spent 85 s and 31k prompt
  tokens flailing at the ritual and returned it narrated into the answer; a
  child that ran out of enforcement rounds mid-ritual returned nothing, which
  the parent surfaced as `[sub-agent produced no final answer]` — the reported
  "it spawns a sub-agent but never does anything with it". A child now runs
  `agentcore.NewDelegatedPolicy`: the same `ScheduledPolicy`, same gate chain,
  same ceilings and critical-tool audit gating, with **only** the self-audit
  ritual skipped at finish (task-tracker items, pending critical actions, and
  undischarged commitments still gate it, and the parent still audits the
  delegated work). The verifier no longer wraps a child either, and every child
  gets a short "you are a sub-agent" prompt section. The same live delegation
  now takes 13 s and comes back clean. See
  [ADR-0007](docs/adr/0007-governed-sub-agents.md) and
  [docs/SUBAGENTS.md](docs/SUBAGENTS.md).
- **A running sub-agent is no longer a black box.** A delegation showed the
  spawn arguments and then nothing at all — often for minutes — so a working
  child was indistinguishable from a stuck one. The child's own run events are
  now relabeled onto the parent's stream as `subagent.progress` (started / tool
  / tool_result / text / thinking / finished) carrying the parent's tool-call
  id, so: the chat spawn chip opens by default with a live activity panel and
  step count, the thinking indicator names what the CHILD is doing, and a
  scheduled run's child card shows its current action. Related: a scheduled
  child's raw events used to leak unattributed into the parent task's live feed
  (they arrive relabeled now), and the spawn result gained `{steps, tools_used}`
  so a reloaded transcript still shows a child's work trail.
- **Email approval card no longer dumps provider JSON after Send.** A
  successful send resolved the card to "Email sent ✓" and then printed the
  provider's raw JSON payload (status code, message id, HTML-lint warnings)
  under it, which read as an error to non-technical users. The card now reuses
  the transcript chip's humanized outcome — a status badge ("Queued for
  delivery" / "Not sent" / "Already sent") with the full payload behind a
  collapsed "Delivery details" disclosure. Non-JSON results (network errors,
  cancel notes) keep the raw view.
- **Themed deployments no longer flash fleet's default palette on refresh.**
  The brand palette's `html:root[data-theme=…]` rules need the `data-theme`
  attribute, but the script that set it ran via `next/script`
  `beforeInteractive`, which the App Router queues through the framework
  bootstrap — after first paint. Every hard refresh on a white-labeled
  deployment (reported on Reklaim) showed fleet's own colors for a beat. The
  bootstrap is now an inline synchronous `<head>` script, which executes
  during parse, before anything paints; `/scripts/theme.js` is gone.
- **Task SLA config is now validated and editable (#274).** `ValidateSLA`
  existed but no create path called it, so a task with a fail threshold at or
  below the warn threshold (or a non-positive expected duration) was accepted
  and misfired alerts at runtime; every create path (create, edit, import,
  estimate) now rejects it with a 400. Editing a task also actually persists
  `expected_duration_minutes` — the edit form sent it, the backend silently
  dropped it — and an edit that omits it clears the SLA. The web edit modal
  echoes API-set `sla_warn_multiplier`/`sla_fail_multiplier` so a UI edit no
  longer resets them to the defaults.

### Changed

- **`analyzing` is no longer a worker-reportable status.** Fleet never wrote
  it (error analysis is a post-terminal annotation). Leftover imported rows
  still decode, recover, and filter; workers can no longer report it. The
  Operations Center status filter now lists `leased` (a real in-flight status)
  instead of the leftover moc `assigned` value, and the live-log viewer
  attaches to `leased`/`running`.
- **Renamed `web/src/app/lib/mocServer.ts` → `orchestratorServer.ts`.** Same
  helpers; the filename was leftover moc vocabulary.

- **Sub-agents are now ON by default, and the parent agent decides whether to
  use them (#1043, amending ADR-0007).** The enablement compose inverts from
  `FLEET_SUBAGENTS_ENABLED || allow_delegation` (both default false) to
  `FLEET_SUBAGENTS_ENABLED && allow_delegation` (both default **true**), and
  migration 061 **backfills existing task rows to `allow_delegation = true`** —
  existing scheduled tasks start seeing the `spawn_subagent` tool and may
  delegate, bounded by the unchanged walls (depth 1, fan-out 5, ≤10% of the
  parent's remaining budget per child, refuse over-cap, spend charged back to
  the parent ceiling). Fan-out is never forced: registering the tool is the
  feature, and a run that never spawns is a successful use of it. Opt out per
  task with the new "Allow sub-agent delegation" toggle (Task form → Advanced,
  on by default) / `allow_delegation: false`, or fleet-wide via Admin →
  Features `subagents_enabled` / `FLEET_SUBAGENTS_ENABLED=false`. New in the
  same change: **interactive chat** registers the tool when the fleet flag is
  on (child spend shows in the chat cost chip); **typed children** —
  `role=explore` (default) is a read-only research child with write-capable
  native tools stripped, `role=worker` keeps the full roster; **per-child
  isolated workdirs** (`<workspace>/subagents/<child-session-id>/`, returned as
  `workdir` in the tool's JSON result); **child cards** on the task page
  (stored + live) and in the chat transcript (id, role, status, spend) instead
  of raw JSON, each with a **Transcript** disclosure backed by new
  child-transcript endpoints (`GET /logs/{task}/subagents/{child}` on the
  orchestrator, `GET /conversations/{id}/subagents/{child}` on chat — existing
  transcript/ownership gates plus a linkage check and strict id validation);
  and a **best-effort MCP write-tool name denylist** for explore children on
  top of the native strip. Design note:
  [docs/SUBAGENTS.md](docs/SUBAGENTS.md).

- The strong/escalation tier is now `x-ai/grok-4.6` (was `openai/gpt-5.6-sol`).
  This is what `suggest_advanced_model`, the spreadsheet nudge, and the task
  fallback resolve to. Same text+image+file modality with tool and reasoning
  support, so escalation keeps every input type it had; roughly 2.5x cheaper on
  input and 5x on output. **Its context window is smaller — 500,000 vs the
  1,050,000 the previous strong tier carried** — which matters because this is
  the tier a user escalates to for the hardest, usually largest, problems.
  Updated alongside `ADVANCED_MODEL` / `ADVANCED_MODEL_LABEL`, the picker seed
  list, the Operations Center's `DEFAULT_FALLBACK_MODEL`, and the lockdown
  allow-list. No `canonicalUpstream` entry: OpenRouter serves it from xAI alone,
  so there is no provider spread to collapse and the prompt cache is already
  single-upstream. The static context table gained
  `x-ai/grok-4.6` -> 500,000 ahead of the generic `x-ai/grok` -> 131,072 row.
- The recommended everyday model is now `deepseek/deepseek-v4-flash-0731`
  (was `z-ai/glm-5.2`); the strong tier is unchanged (`openai/gpt-5.6-sol`).
  Updated in every place the slug is mirrored: the frontend `DEFAULT_MODEL` /
  `DEFAULT_MODEL_LABEL` and the picker seed list, the Operations Center's
  `DEFAULT_PRIMARY_MODEL` for new tasks, `agentcore.DefaultCoreModel`,
  `config.DefaultTitleModel`, and the lockdown allow-list default. Same
  text-only modality as the model it replaces, with tool and reasoning support,
  so no capability is lost; roughly 4.5x cheaper on input and 7x on output.
- **DeepSeek models are now pinned to the first-party DeepSeek upstream**
  (`canonicalUpstream`, non-strict `Order` + fallbacks). OpenRouter serves this
  family from 28 endpoints whose context lengths span 131K-1M and whose
  quantization spans fp4-fp8, so an unpinned route was neither reproducible in
  quality nor safely sized - on top of losing the per-upstream prompt cache,
  which is the same reason `z-ai/` pins to Z.AI.
- The static context-window table resolves `deepseek/deepseek-v4*` to
  1,048,576, ahead of the `deepseek/` -> 128,000 entry that still covers the V3
  line (the table returns its FIRST prefix match, so ordering is what makes it
  longest-first). Cold-start/offline path only - a running fleet reads the live
  OpenRouter catalog - but without it a cold boot would compact a 1M-window
  default at 128K.
- **A serving-precision floor now backs the DeepSeek pin, and the upstream that
  actually served each step is recorded.** Pinning the family fixed cache
  locality but left quality unguarded: a non-strict pin is `Order` +
  `AllowFallbacks=true`, i.e. a *preference*, so every request was one busy
  first-party endpoint away from being served by anything else in a pool that
  spans **fp4-fp8** — and `provider.quantizations` was set nowhere in the repo.
  Since this family is the recommended everyday default, that fallback path is
  the hot path for ordinary chat turns, and an fp4 serving of a flash-tier model
  degrades in a way that reads as the model being broken (token-level
  misspellings, topic drift, runaway output) rather than as a routing event.
  `upstreamPinFor` now attaches an fp8-and-above allow-list
  (`fp8|fp16|bf16|fp32`, excluding `unknown`, which cannot be shown to clear the
  floor) to the DeepSeek pin; DeepSeek's own endpoint is fp8, so the preferred
  route is unchanged. Families whose pool does not mix precisions carry no
  filter and their requests stay byte-identical. Separately, `updateUsage` read
  only `.Usage.Cost` off OpenRouter's provider metadata and discarded
  `.Provider`, so a turn degraded by a fallback route was indistinguishable from
  a bad model; the run now records `LastServedUpstream`, latches a
  `ServedFallback` flag, and logs one line per upstream transition. Diagnosis
  only — it does not change routing or fail a run.

### Security

- **Un-inverted the event-trigger connector boundary: `allow_event_triggers=false`
  now grants zero connector seats instead of all of them** (#979). The opted-out
  spawn left its `credential_allowlist` unset, and an unset allowlist means
  *inherit global* — every seat — not *none*, so the secure default produced the
  most permissive run in the system, the exact inverse of what the code's own
  comment promised. On an email trigger whose policy approves a bare domain, any
  sender in that domain drove an agent holding every bundle connector, credential
  gating disabled. The opted-out run now persists an explicit deny-all allowlist
  (non-nil, empty), and the scheduled runner honors that value by wiring **no**
  connector rather than wiring them all and rejecting each call: an empty MCP
  selection (which otherwise means *the deployment default set*), a dedicated
  empty MCP client instead of the shared process-wide one, no
  `mcp_list_servers`/`mcp_load_servers` loader tools (calling `mcp_load_servers`
  spawns credentialed subprocesses as a side effect), and no per-user hosted
  remote-MCP overlay — the last of which was attached from the task *owner*, so a
  template that looked connector-less in an audit still reached everything that
  human had personally OAuth-connected. Tests now assert the **negative** (a
  `false` spawn reaches zero seats, checked through the same Gate-3 predicate the
  broker enforces); asserting only that the run "inherited nothing" is what let
  the code and its documentation drift apart, since `len(allowlist) == 0` is true
  for both deny-all and inherit-global. The `#177` webhook path is unchanged — it
  is HMAC-authenticated against a per-trigger secret. See
  [`docs/EVENT-TRIGGERS.md`](docs/EVENT-TRIGGERS.md).
- **Run-log transcripts are no longer readable fleet-wide by every `view_logs`
  holder** (#980). `GET /logs/{task_id}`, its per-attempt `/history` siblings,
  and the live `GET /tasks/{task_id}/stream` all authorized on the `view_logs`
  permission alone, with no scoping to the caller's own tasks — so any principal
  holding it (including a `fleet_task_*` key minted for one automation, and every
  `fleet_readonly_*` key) could read the transcript of every task on the box,
  with task ids enumerable through `GET /tasks`. A transcript carries the run's
  verbatim tool traffic — connector query results, whatever PII the agent
  handled, and cost data — so on a multi-client deployment one leaked or
  over-issued key was a cross-tenant read. The scope check these routes did carry
  was a no-op: `taskVisibleToScopes` returns true unconditionally, and unscoped
  principals skipped it entirely. All four routes now share one gate
  (`internal/sched/handlers/log_authz.go`): `view_logs` plus **ownership** of the
  task (creating user or creating API key — the same predicate that already made
  workspace files creator-private), or the new explicit fleet-wide
  **`view_all_logs`** permission, which `PermissionAdmin` implies and no role
  grants. `POST /keys` accepts an explicit `permissions` array on the legacy path
  so an operator can mint a fleet-wide log auditor without handing out an admin
  key; it 400s rather than silently losing to a `role` or `type` in the same
  request. Workspace files stay stricter (admin-or-creator) and are deliberately
  *not* widened by `view_all_logs`. Because ownership is now load-bearing, the
  one create path that dropped it was fixed too: a task scheduled from chat
  (`schedule_task`) is attributed to the approving user, so its own author can
  read its transcript — and, as a side effect, finally gets the completion push
  notification that path never fired. See
  [ADR-0043](docs/adr/0043-per-task-run-log-scoping.md) for the decision, the
  operational breaks (a readonly key reads no transcripts; a non-admin user keeps
  only its own), and the residual it does not close (the task *row* itself,
  prompt and result included, is still fleet-wide under `view_tasks`).

- **The MCP broker now authorizes, not just transports (#167, ADR-0042).** The
  credential-owning `fleet mcp-broker` child used to bind whatever
  `{server, account}` selection it was sent and run whatever tool a call frame
  named: every gate lived parent-side, inside the address space the boundary
  exists to distrust, so a parent-side gating bug was a total bypass. That was
  not hypothetical — activating the production boundary scrubbed the very
  `cfg.MCPServers` the scheduled agent read its Gate-2 allowlist from, and every
  scheduled run silently lost its tool allowlist (#960) with nothing on the
  credential side looking. The child now re-derives the per-server tool
  allowlist and the enabled-server set from **its own** bundle (refreshed inside
  `Reload`, under the lock that publishes new bases) and treats the parent's
  effective sets, which now cross as `ScopeSpec.Policy`, as narrowing only. A
  selection naming a server the child does not have enabled is refused before
  anything spawns; a call outside the scope's bound set is refused at dispatch;
  the scope's advertised catalog is filtered through the same gate that judges
  its calls; and the per-task credential allowlist (#184) is enforced child-side
  through the same `agentcore.GateMCPBrokerWithAllowlist` the in-process loop
  uses, preserving the nil ("inherit global") versus empty ("deny all")
  distinction. Refusals are value-free tool-level results carrying a stable
  `mcp_broker_policy_denied` marker. The unscoped shared client is restricted
  rather than removed: no production agent path reaches it any more, and what
  does is still bounded by the bundle allowlist. `docs/MCP-BROKER-SCOPES.md` no
  longer says "no authorization boundary is added here", because there is one.
- **Recorded the remote-MCP OAuth control-plane boundary as accepted, in
  writing (#167).** Per-user hosted MCP *runs* resolve tokens in the child
  (ADR-0040), but connect / callback / connection CRUD stay parent-side, so the
  parent installs the at-rest cipher and can decrypt any user's stored
  remote-MCP tokens. `SECURITY.md` now states the threat model plainly —
  compromise of the fleet parent process implies compromise of stored
  remote-MCP tokens — instead of leaving the gap implied by omission.
- **Cleared the three high-severity npm advisories and all seven govulncheck
  findings.** `npm audit` in `web/` reported 3 high-severity vulnerabilities and
  the Go CVE gate reported 7 reachable standard-library vulnerabilities; both
  now report clean. On the web side: `undici` 7.28.0 → 7.29.0 (five advisories —
  response desynchronization via the retry interceptor, cross-user disclosure
  and a parse-time crash via degenerate private cache directives, CRLF injection
  via a blob-like body `type`, cross-user disclosure via whitespace around `=`
  in `Cache-Control`, and cookie attribute injection via unsanitized `domain`),
  `nanoid` 3.3.16 → 3.3.18 (`GHSA-2v37-7h3g-55p8`, a custom generator can loop
  forever at size zero — pulled up by bumping its parent `postcss` 8.5.25 →
  8.5.26, since `npm audit fix` alone would not reach a transitive two levels
  down), and `js-yaml` 4.3.0 → 4.3.1 (`GHSA-5p4m-2wfm-xmqj`, quadratic CPU in
  `!!omap` resolution), which arrives on dev by merging main. All three are
  lockfile-only: no `package.json` range moved.
- **Bumped the pinned Go toolchain 1.26.5 → 1.26.6**, which is what actually
  fixes the seven standard-library CVEs govulncheck found reachable from fleet's
  own call graph — `GO-2026-6218` (`net/url`), `GO-2026-6091` (`html/template`),
  `GO-2026-6090` (`crypto/tls`), `GO-2026-6089` and `GO-2026-5026` (`net/http`),
  `GO-2026-6088` (`encoding/xml`), and `GO-2026-5972` (`encoding/asn1`). The pin
  is bumped in every place it is written down so the gate and a local run agree:
  `go.mod`, `web/go.mod`, the `run.go` target in `.golangci.yml`, the four
  workflows that name a `go-version` explicitly (`ci.yml`, `benchmark.yml`,
  `e2e-canary.yml`, `screenshots.yml` — `dev-ci.yml` reads `go.mod`), and the
  two docs that quote the version (`ONBOARDING.md`, `docs/TESTING.md`). No fleet
  source changed; this is a toolchain bump, not a language-version migration.

### Added

- **A server clock on the Operations Center dashboard**, beside the agent-status
  cards. A cron recurrence fires in the *server's* timezone, not the operator's,
  so `0 9 * * *` ran at 9am somewhere the UI never named — and
  `orchestratorApi.config()`, which knew the zone, had no callers at all. The
  clock now reads the orchestrator's own now from `GET /api/config` (which gains
  `server_time`, formatted in the server's location so the offset travels with
  it, plus `default_task_timezone`, the zone a task with no explicit one lands
  in). The browser measures its skew against that once and ticks locally, so a
  wrong laptop clock can't make the readout lie, a long-lived dashboard survives
  a DST change (hourly resync), and a browser whose zone database has never
  heard of the configured zone still shows the right wall time via the embedded
  offset. Hovering names the zone — and names the task-scheduling default too
  when a deployment has set it differently. An orchestrator that reports no
  `server_time` renders no clock rather than local time under a server label.

### Fixed

- **Multi-day chats no longer stay anchored to yesterday's mailbox
  dates (#1026).** The engine already injected a day-granular Runtime Date
  Context in the system prompt; the model still reused the previous turn's
  `date_from`/`date_to`, so a "check again" on August 13 searched only
  August 12 and missed mail that had already arrived. Every run now also
  gets a structured `runtime_today` + 3-day `freshness_window` in the
  message tail (interactive and scheduled), and mailbox/search MCP results
  whose `date_to` (or `on_or_before`) is before today — or that return
  `matches_found=0` on an exact sender/subject query — are annotated so an
  empty exact hit cannot be treated as proof of absence. OpenX-specific
  discovery, coverage-date parsing, and the pre-send ledger stay in the
  client bundle; this is the engine-side date/search gate. See
  [`docs/RUNTIME-DATE.md`](docs/RUNTIME-DATE.md).
- **The chat composer toolbar now fits on one row on a phone.** The model chip
  carried the full vendor-prefixed catalog name (`Z.AI: GLM 5.2`) plus the four
  cost glyphs, and the toolbar row wrapped — the tools button and the context
  ring dropped onto a second line under the chip. The row no longer wraps: the
  icon buttons keep their fixed size and the chip is the single elastic item,
  so it truncates with an ellipsis instead of pushing anything down. Below the
  `sm` breakpoint the chip also drops the vendor prefix (`GLM 5.2` — the full
  name stays the button's accessible name) and hides the cost glyphs, which are
  still shown on every row of the model picker. Desktop and tablet are
  unchanged: full name, capped at 11rem with an ellipsis, cost tier visible.
- **An approved MCP call runs on the account its turn was using, not the
  connector's default seat (#167).** An approval card outlives the turn scope
  that staged it, so execution reached for the unscoped broker at the default
  bundle seat — a multi-account footgun: a turn on a named account could have
  its approved send go out as a different client. Staging now records the public
  `{server, account}` seat (`approvals.mcp_server` / `mcp_account`, migration
  048) and approve/execute opens a fresh short-lived scope carrying that same
  selection; a seat that no longer resolves fails the approval with the seat
  named rather than silently downgrading. The card shows which account a
  named-seat send will run as. Rows staged before the migration carry no seat
  and execute on the shared broker exactly as before.
- **Approval staging looks at the turn's catalog instead of the process-wide
  default-seat one (#167).** `RunTurn` now hands the stager the turn scope's
  broker, catalog, and selection before any tool can stage a card. Previously,
  during a named-account turn, the staged tool identity
  (`mcp_<server>_<account>_<tool>`) did not appear in the stager's catalog at
  all, so email pre-validation ran against the wrong seat and the staged-call
  resolution could not find its own tool.
- **`fleet update` no longer fails on a box whose distro Go lags `go.mod`.**
  The Makefile now exports `GOTOOLCHAIN=auto`, so every build path fetches the
  pinned toolchain instead of demanding it be pre-installed. `go.mod` pins an
  exact patch release and that pin moves on every Go security release (the
  govulncheck gate reports stdlib CVEs against whatever toolchain built the
  code), which distro packages trail by days to weeks — and Fedora's `golang`
  additionally ships `GOTOOLCHAIN=local` in its `go.env`, turning that lag into
  a hard stop: `go: go.mod requires go >= 1.26.6 (running go 1.24.7;
  GOTOOLCHAIN=local)`. An environment variable outranks that `go.env` default,
  which is what makes the build work on a stock Fedora host. `bootstrap.sh`
  already passed `GOTOOLCHAIN=auto` by hand for exactly this reason, so a box
  would **install** cleanly and then fail **every** subsequent `fleet update` —
  setting it in the Makefile covers `update.sh`, `fleet-upgrade.sh`,
  `bootstrap.sh`, and CI at once, since all of them shell out to `make build`.
  `?=` leaves a deliberately-set `GOTOOLCHAIN` (an air-gapped host pinned to
  `local`) alone. Both upgrade scripts also gained a preflight that names the
  one case the fetch cannot cover — a Go older than 1.21, which predates
  `GOTOOLCHAIN` — rather than letting `make build` fail on a raw version
  mismatch. The operator-facing upshot, now documented in `ONBOARDING.md` and
  `docs/TESTING.md`: you need Go 1.21+, not the pinned patch release.
- **`fleet-upgrade.sh` and `update.sh` no longer abort two steps in when
  systemd is not reachable.** Both resolve `INSTALL_DIR` by probing the unit's
  `ExecStart` with `systemctl show … | sed … | head -n1`, and both run under
  `set -euo pipefail` — so the pipeline's non-zero exit killed the script
  outright instead of falling through to the `/opt/fleet` default that exists
  for exactly this case. The `command -v systemctl` guard did not help: the
  failing condition is not a *missing* `systemctl` but an unreachable systemd,
  which is every container carrying the binary without systemd as PID 1 (and
  `head -n1` can SIGPIPE the probe even where systemd is running). Both call
  sites now end in `|| true`, since the probe is best-effort and the fallback
  already covers "no path found". This was visible as
  `TestFleetUpgradeDryRunSmoke` failing on any container-based dev box while
  passing in CI, where the runners do have systemd — the test now passes in
  both.
- **The task prompt field is adjustable, and no longer opens a long prompt into
  a three-row box.** Editing an existing task showed its prompt through a ~78px
  window: auto-grow only ever ran from the textarea's `onChange`, and a prefill
  fires no change event — so the field stayed at `rows={3}` no matter how long
  the prompt was, which made editing one section of a long protocol a scrolling
  exercise. The prefill (and a template or prompt-library insert) is now sized
  like typed text, and the operator can take the height over two ways: **drag**
  the native grip (the field was `resize: none`), or hit the new **Expand**
  toggle beside the Prompt label for a tall editing pane, remembered per
  browser. Auto-grow yields to either — a chosen height is no longer snapped
  away by the next keystroke — and Expand/Collapse doubles as the reset. The
  auto-grow ceiling stays modest so the fields below stay reachable; a
  deliberate drag goes much further.

### Added

- **Tasks have titles** (`docs/TASK-TITLES.md`). The Operations Center
  identified a task by the first ~80 characters of its prompt, so operators were
  writing a title line at the head of the prompt to tell jobs apart — display
  text smuggled into the model's input. A task now carries an optional short
  title, shown in Recent Tasks (leading the row, prompt demoted to a muted
  second line), the Upcoming timeline and week board, the task-detail header,
  and the SLA report; the task search matches it alongside prompt and id. It is
  **not** the existing `name` column, which is the unique import/export identity
  key *and* is cleared on every recurrence occurrence — a title is non-unique
  and is carried onto every occurrence, re-run and clone, so all the runs of one
  job list under the same label. Optional, single-line, ≤120 characters, never
  injected into the agent's prompt; untitled tasks render exactly as before. A
  bundle template may set `task.title`, and the create form otherwise seeds it
  from the template's name. Inserting a **prompt-library** entry seeds an empty
  title from the entry's name too — for a bundle prompt that is the file's own
  `name:` header, the very line operators were reading off the top of the prompt
  to identify the job — without ever overwriting a title already typed.

- **Run now, for a job that has not run yet.** The Operations Center could only
  *Resubmit*, and only a task that had already finished — so a scheduled job had
  no on-demand kick-off at all. A daily task created in the afternoon could not
  be exercised until the next morning, which made the authoring loop for a new
  job a day long. Recent Tasks rows (and phone cards) now carry a **Run now**
  bolt beside the edit pencil, and the task-detail modal offers the same action
  for pending/scheduled tasks. It posts the existing
  `POST /tasks/{id}/rerun`: a fresh one-off copy that starts immediately and
  leaves the source **and its cron untouched** — the confirm dialog says so, so
  nobody has to guess whether "run now" also cancelled tonight's run. In-flight
  tasks are excluded (a second concurrent copy of a running job is a footgun),
  and the wording splits by state: a finished task is *Resubmitted*, one that
  has never run is *Run now*.

### Removed

- The fast.io native tools (`fastio_find`, `fastio_upload_file`) and the
  dedicated Fast.io system-prompt section. They encoded a vendor integration
  **and** one client's `ELCxxxxx` account-code convention — including a
  `regexp.MustCompile("(?i)\\bELC\\d{3,6}\\b")` — inside the client-agnostic
  engine, against this repo's own rule that client content lives in the bundle.
  They also could not express the shape the data actually has: a date-partitioned
  folder tree, where `fastio_find`'s 25-result cap and opaque parent ids silently
  reduce a month of daily partitions to whichever file was touched last.
  Replaced by `fastio_helpers` in the client bundle (`resolve_path`,
  `list_partitions`, `find`, `upload_file`), which adds path resolution and
  partition enumeration and states truncation instead of hiding it.

  Both native tools were **already dead code**: neither `NewFastIOFindTool` nor
  `NewFastIOUploadFileTool` had a single call site, so nothing registered them —
  while the system prompt instructed every agent with fast.io enabled to use
  them. Agents fell back to raw `mcp_fast_io_storage action=search` with none of
  the mitigations the prompt promised. Deleting them changes no runtime
  behaviour; the prompt no longer advertises tools that do not exist. The
  inline-base64 upload guard now defaults to the blob-flow hint alone and lets a
  bundle name its own path-taking upload via `RemediationHints.NativeUploadTool`.
  (#1016)

### Fixed

- Re-running or cloning a **named** task returned a 500. The copy inherited the
  source's `name`, which carries a partial unique index (it is the import/export
  identity key), so the insert collided with the source row still in the table.
  Both copy paths now clear it — the rule `scheduleNextRecurrence` already
  applied when minting the next occurrence of a recurring task.
- Scheduled tasks created from chat carried no model and dead-lettered on their
  first dispatch, before running anything, with `no model configured for
  scheduled task`. `schedule_task`'s `model` is optional and the agent was told
  it "defaults to the orchestrator's configured model" — a promise nothing kept:
  the chat seam copied the empty value through, `ParentModel` inheritance existed
  only task→task, and a deployment with no `*_TASK_MODEL` had nothing behind it.
  A chat-created task now **inherits the model of the conversation it was
  scheduled from**, which also makes the run reproducible instead of drifting
  with a server env var. Two create-time gates (the `POST /tasks` validator and
  the chat seam, which bypasses it) now refuse a task that has neither its own
  model nor a deployment default, so an unrunnable task fails immediately and
  visibly rather than up to a cron period later in the dead-letter queue. The
  dispatcher's error text and a new boot warning name every accepted spelling.
  (#1014)
- `FLEET_TASK_MODEL` and `FLEET_TASK_FALLBACK_MODEL` were silently ignored.
  These were the only two model knobs in `config.Load` read with a bare
  `os.Getenv("CUTLASS_…")`, so they skipped the `FLEET_`/`CHAT_`/`CUTLASS_`
  alias family that `EnvAliases` advertises for them and `TestEnvAliases`
  pins — an operator following the documented convention got no task model at
  all, and every scheduled task on that deployment dead-lettered. Both now
  resolve the whole family in precedence order; existing `CUTLASS_`-spelled
  deployments are unaffected. (#1015)

- Four chat empty-state cards rendered an empty icon box because the
  `core-icons` sprite had no symbol for the name they asked for: `file-text`
  (the `config/default` and example-config "Summarize a document" card, plus
  `DEFAULT_PILLS` — the hardcoded fallback the web renders whenever the
  `/config` fetch fails, so this one hit a bare install and every degraded
  one), `book-open` (example-config), and `globe` + `mail` (a client bundle's
  browse-a-site and mailbox cards, blank beside the `search` and `bar-chart`
  cards that worked). The four Lucide-derived glyphs are now in the sprite.
  An `<svg><use>` pointing at an undefined symbol id renders nothing and
  reports nothing — no console error, no failed request, no build or test
  failure — so two guards now close that silent-failure hole:
  `web/src/app/spriteCoverage.test.ts` checks every icon name this repo
  references (source literals, the `*_ICONS` lookup maps, and the built-in
  bundle's cards) against the sprite, and `TestRealBundleSanity` gained the
  same check for an out-of-repo bundle's cards when run with
  `FLEET_SANITY_BUNDLE_DIR`. Bundles were not changed: a bundle names an icon
  and the engine owes the glyph, so the fix belongs in the sprite.
- **Every Python error was invisible to the agent.** IPython colours its
  tracebacks; `python_bridge.py` passed `content["traceback"]` through verbatim;
  JSON encoding turned each ESC into `\u001b`; and
  `containsEscapedJSONControl` treated any escaped control below `0x20` as
  smuggled binary. `boundModelVisibleToolResponse` therefore suppressed the
  whole result at `shown_bytes: 0`, staged **no** artifact (the staging branch
  is skipped for binary), and told the agent only that "binary previews are
  intentionally unavailable" — so `run_python` failures came back with the
  exception erased. A production session took two blind `KeyError`s and a failed
  `view_file` before abandoning `run_python` for `bash`, where errors arrive as
  plain text. The same trap ate any `bash` output from a CLI that colours
  (`git diff --color`, `pytest --color=yes`, `ls --color=always`). ESC is now
  excluded from the encoded-binary signal — it is the one sub-`0x20` control that
  ordinary text is full of — and the bridge strips ANSI at the source, so the
  escapes no longer burn model tokens either. Genuine signals (escaped/literal
  NUL, `\b`, `\f`, data URIs, base64-named keys, long encoded runs) still
  suppress.
- `checkCommandSafety` rejected any bash command containing `":-"`, `":+"`,
  `":?"`, `"##"` or `"%%"` once a `${` appeared anywhere in it — and the test was
  against the **whole command**, not the inside of the expansion, so one
  unrelated `:-` on the line poisoned every expansion. In production
  `echo "${CUTLASS_INPUT_DIR:-not set}"` was refused, which is the ordinary way
  to read a variable with a default and what every `set -u` script needs. The
  rejection bought no safety: those forms only substitute and trim text, and the
  execution vectors inside an expansion (command substitution, backticks) are
  matched independently in the same loop — `${` falls through with `continue`, so
  `${VAR:-$(id)}` is still caught. `${!VAR}` stays blocked as genuine name-level
  indirection. The guard had no test coverage at all; it does now.
- The generic sandbox image was missing `file` and `which`. Two agent runs lost
  a chained command each to `exit 127` reaching for them (`curl … && file x`,
  `which node && node --check`). Neither was withheld on purpose.
- The interactive critical-tool gate staged one approval card per call with no
  awareness of the cards already waiting, so a turn could park two competing
  writes on the same MCP server. Because a card freezes its tool's arguments
  until the user clicks Approve, the second write is computed against state the
  first is about to change. A production session lost a Pages deploy this way:
  `mcp_pages_patch_page` was staged, the model read "Do NOT retry" as a rule
  about that tool name, rebuilt the same change as a full-file upload, and
  staged `mcp_pages_deploy_page_upload` for the same page carrying the
  pre-patch `expected_version`; the user approved both, the patch landed, and
  the upload was rejected as stale after two CSV ingests, a 14-row
  reconciliation, and 78 KB already transferred.
  `checkCriticalToolApproval` now refuses to stage a *different* critical tool
  on a server that already has a card pending, naming the pending action and
  its approval id. Keyed on `sameToolServer`, so a repeated tool name still
  stages (the batch case — N independent records — has no such coupling) and
  unrelated servers are unaffected; the pre-approval and pre-denial sentinels
  leave no card pending and so reserve nothing. The `APPROVAL_REQUIRED` text
  also now forbids re-routing the same change through a different tool, which
  is the loophole the model actually used.
- `fleet update` installed the new binaries and web build BEFORE the
  sandbox-image gate, so the gate's fail-closed die aborted with new code
  already on disk while the old service kept running — a silent, deferred
  inconsistency (the next restart, crash, or reboot would run code no update
  ever finished deploying) that contradicted the die message's implication
  that nothing had changed. The sandbox step now runs before the
  build/install step, so the abort leaves the box coherent: old binaries on
  disk, old service running. Separately, update.sh resolved the bundle's
  `${FLEET_SANDBOX_IMAGE:-}` image reference against the update shell's
  environment, while the service resolves it from
  `EnvironmentFile=/etc/fleet/fleet.env` (which update.sh deliberately never
  sources): a value set only in the env file forced a pointless multi-GB
  on-box build on every update and could trip the new die spuriously, while a
  value exported only in the operator's shell silently skipped the whole
  sandbox step — absence probe included — recreating the
  boots-clean-breaks-on-first-tool-call failure the gate exists to prevent.
  The reference is now read from the service's env file using doctor.sh's
  `env_get` idiom, matching what the restarted service will actually see.
- The CSRF canonical-origin hardening locked operators out of a box reached
  over an SSH tunnel. `verifyOrigin` compared the Origin host strictly against
  the configured `NEXT_PUBLIC_PUBLIC_ORIGIN` (which bootstrap writes on every
  deploy), and it guards every mutating route including the login form — so a
  browser at `http://localhost:3000` tunneled to a `--domain` box got 403 on
  every POST, presenting as a total auth outage. A loopback Origin
  (`localhost` / `127.0.0.1` / `[::1]`, any port — exact hostnames only, so a
  `localhost.evil.example` lookalike does not qualify) is now also accepted
  when it exactly matches the connection's own Host header: that pair is only
  producible by a browser genuinely connected to the box's loopback (the
  tunnel), never by a victim's browser talking to the real deployment, and
  x-forwarded-host stays ignored so a forwarded-header attack from a remote
  origin still fails. A mismatched local port (a different origin) is still
  rejected, and the server-side 403 log now names the expected vs. actual
  host so the failure is self-diagnosable.
- An async run_if gate's settle write could clobber a concurrent reschedule or
  edit. Both settle paths conditioned only on the task still being `scheduled`
  — a status an edit legitimately keeps — and the bounded async pool stretched
  the dispatch-to-settle window from milliseconds to the gate's full runtime
  (up to 300s). Concretely: a task postponed by an operator while its slow
  gate evaluated would run tonight anyway on the stale pass verdict, and a
  stale decline overwrote the operator's new `scheduled_for` with its retry
  backoff. Every gate settle write (promote to pending, skip record, and the
  recurrence-ended cancel) is now a compare-and-swap that also requires the
  row to still carry the `scheduled_for` the evaluation was dispatched
  against, so any interleaved edit or reschedule wins, the stale verdict is
  discarded (and logged as such, without counting a skip that never
  happened), and the next due tick re-evaluates the task's current
  definition.
- `Scheduler.Stop`'s run_if gate drain could panic the process during graceful
  shutdown. Stop closed the stop channel and immediately waited on the gate
  WaitGroup, but nothing waited for the scheduler's run loop to exit — a tick
  already inside the loop body kept running and could dispatch another gate
  evaluation (`WaitGroup.Add`) concurrently with that wait, which
  sync.WaitGroup forbids. The resulting misuse panic fired in Stop's drain
  goroutine, which has no recover (the per-tick recover covers only the tick),
  so it killed the process mid-shutdown and skipped every remaining deferred
  cleanup in main. The run loop now signals completion on return and Stop
  waits for it to exit before draining the gates — inside the same 5-second
  bound as before — so no new dispatch can ever race the drain.
- The MCP stdio transport read each response line from a server subprocess
  with no ceiling. In broker mode those servers run host-side, so a single
  data-driven oversized response — a connector returning a giant query result
  or a fetched page — was buffered whole in the fleet process's memory before
  any downstream truncation applied: one response could OOM the process and
  take down every user. This is the same risk class already capped on the
  sandbox bridge's response read, and the stdio transport now follows that
  pattern at the same 64 MiB ceiling: a line past the cap is drained to its
  delimiter — the stream stays framed and the healthy subprocess is not
  restarted — and the call fails with an explicit over-cap error rather than
  a silently truncated, plausible-but-wrong result.
- The storage-quota probe's inconclusive path re-probed on every container
  creation with no cap or backoff. Right direction — the earlier fix stopped a
  cancelled first probe from latching "unsupported" for the process lifetime —
  but on a degraded host (podman hanging) every creation then paid the
  serialized 30s probe, added straight to turn-start and scheduled-run
  latency, for as long as the host stayed degraded. Consecutive probe
  *timeouts* (inconclusive with a live caller context: the host being slow,
  not a user cancelling a turn) now cap at three, after which the pool treats
  the driver as unsupported for a 5-minute cooldown before re-probing.
  Containers created inside the window omit only the writable-layer quota —
  the per-file ulimit still applies — and a cancelled turn still never counts
  toward the latch, so a genuinely inconclusive answer still never latches
  permanently.
- The Operations Center rendered its username/password login card whenever the
  initial `/me` probe failed for any reason other than 403 — including the 500
  the orchestrator deliberately answers when its fail-closed session-epoch
  lookup can't reach the chat DB, and thrown network failures. During a chat-DB
  blip every user loading the page was therefore shown a login form instead of
  an error. No session was destroyed — the cookie and the stored bearer
  survived, and a reload after recovery restored the dashboard — but the page
  invited people to type credentials mid-incident: the same conflation of
  "unauthenticated" with "backend down" already fixed for the chat plane. The
  probe now applies that plane's verdict rule (`bootstrapFailure.ts`: only
  401/403 are auth verdicts); a 5xx or a network failure surfaces as a distinct
  "can't reach the server" retry notice and leaves the stored bearer untouched.
- One slow `run_if` gate degraded scheduling for the whole box. The scheduler
  tick ran task promotion, lease recovery, starvation promotion, paused-task
  expiry, and the wake sweep sequentially on one goroutine, and gates were
  evaluated inline during promotion — so a single admin-authored gate pointing
  at a hung dependency (`timeout_seconds` up to 300) delayed ALL scheduling
  and crashed-worker lease recovery by its full runtime, while the 30s ticker
  silently dropped ticks. Gates are now evaluated on a small bounded pool of
  goroutines and settled (promoted or skipped) asynchronously — the tick never
  waits on a gate, a task whose gate is still running is not re-dispatched,
  and one whose slot isn't free simply stays due for a later tick. The cheap
  recovery sweeps also run before promotion now, so a promotion regression
  can never starve lease recovery. Separately, a declined one-shot gate used
  to re-run its host command every 30s tick forever; a decline with no next
  cron tick now backs off exponentially on the already-tracked `skip_count`
  (30s doubling to a 30m cap), so a permanently-false condition costs ~2
  commands an hour instead of 120. Shutdown no longer races those async
  settles: `Scheduler.Stop` now waits up to 5s for in-flight gate evaluations
  to land their promote/skip writes; a gate still running at the bound is
  abandoned to finish on its own (its settle write is conditional, and a task
  left `scheduled` is simply re-evaluated after restart), so a slow gate can
  never extend shutdown by its full runtime.
- Editing a webhook trigger template turned it into a one-shot run. The task
  edit's status recompute derived only from `scheduled_for`, so saving any
  edit to a template (`scheduled`, nil `scheduled_for` — inert by
  construction) flipped it to `pending` and the worker pool executed the
  template itself once, outside any trigger firing. Same root cause and same
  fix as the gated-task edit bypass under Security below: the edit path now
  recomputes dispatch state with the shared `models.DeriveDispatchState`
  rule, which keeps a webhook template parked inert.
- Task export silently dropped `run_if`. The portable definition record had no
  field for the pre-run gate, so a box migration or backup-restore converted
  every gated task into an unconditional one with nothing flagging it — tasks
  whose gate suppressed runs under bad conditions started running
  unconditionally on the new deployment. The gate is now part of the export
  record and survives the round-trip (including `conflict=replace`, which
  overlays it like every other definition field). Because a `run_if` command
  executes on the host as the fleet user, importing a record that carries one
  requires admin permission — the same boundary as authoring one, checked
  up front before any record is written, so import cannot be a path around
  the create/edit admin gate.
- The pre-submission cost forecast (`POST /tasks/estimate`) was dead for every
  cookie-path Operations Center user — "Estimate failed: Unauthorized" — even
  though the sibling comment claimed the endpoint honored the Next-proxy
  header-trust identity. Its auth was a hand-rolled copy of the task-create
  check that predated header trust (#157) and never learned it; it failed
  closed, so nothing was exposed, just broken. The copy is gone: the endpoint
  now shares `authorizeTaskCreator` with `POST /tasks` and `/tasks/batch`, so
  the header-trust semantics and the session-epoch revocation gate apply
  identically to all three creation-shaped endpoints and cannot drift again.
- Every masked MCP-broker failure was undiagnosable. The credential owner
  replaces any operational error with a fixed `mcpbroker: credential-owner …`
  sentence before it crosses back to the parent — correct, because the real error
  can embed connector stderr, resolved URLs, or Authorization headers — but that
  replacement was the *only* thing that happened to it, so the detail existed
  nowhere: not in the parent, not in the broker process, not for the operator.
  A connector answering `Unknown tool: x` and a genuinely revoked credential both
  reached the agent as the same sentence, and the difference was unrecoverable
  without the upstream's own logs. In one observed case an agent retried four
  times and concluded "ops-side credential fix" for what was a version skew
  between a cached `tools/list` and the process serving the call. The broker now
  logs each masked failure host-side with its server and tool, scrubbed through
  `internal/redact` with the connector env registered as literals — so a bare
  high-entropy token quoted back by a connector is replaced by value, not merely
  when its shape matches a vendor pattern. The reply to the parent is byte-for-byte
  unchanged, and credentials-never-in-logs still holds
  (`internal/mcpbroker/redact.go`).
- Five sandbox lifecycle robustness gaps, all host-side and none affecting the
  container's isolation posture:
  - **One cancelled turn could permanently disable the `--storage-opt` disk
    quota.** The pool latched the boot probe's answer with a `sync.Once`, so a
    first probe running under an already-cancelled context cached
    "unsupported" for the process lifetime and every later container ran with
    no writable-layer quota. A probe cut short by its context is now
    inconclusive: that container safely omits the layer quota and the next
    creation re-probes; only a completed determination is memoized.
  - **The run_python bridge response was read into host memory with no
    ceiling.** The bridge caps its own stdout/stderr/result captures, but
    `vars` extraction is not size-bounded, so a cell returning a huge variable
    inflated host RSS without limit — unlike bash output, which
    `BashOutputCaptureCap` bounds. The response line is now capped at the same
    ceiling; overflow drains to the delimiter (the session stays framed and is
    kept), and the call fails with an explicit truncation error instead of a
    bare JSON parse error.
  - **A wedged bridge exec could block every operation on its container
    forever.** `terminateBridgeLocked` runs under the bridge mutex and waited
    on `cmd.Wait()` unboundedly — anything holding the exec client's pipes
    (the hazard `BashWaitDelay` documents) stalled bash, run_python and file
    ops behind it indefinitely. The bridge exec now carries a WaitDelay and
    the post-SIGKILL reap wait is bounded; past the bound the state is cleared
    and the abandoned wait finishes on its own.
  - **Routine teardown of an already-exited container logged a spurious
    error.** The "already gone" suppression in `close()`/`killContainerNow`
    predates podman's actual state error — `can only kill running containers.
    <id> is in state exited: container state improper` (verified verbatim on
    podman 5.8.2) — so the normal exit-vs-`--rm`-removal race logged "kill
    unconfirmed" on every occurrence. The matcher now recognizes the dead
    states (exited/stopped) while a paused container — frozen, not gone —
    still counts as unconfirmed.
  - **Crash-orphaned bridge/seccomp temp files leaked permanently.** Only the
    graceful close path removes the bridge-script and seccomp files written
    into BridgeDir, so every non-graceful exit leaked them forever. Boot now
    sweeps files matching the two CreateTemp patterns, age-bounded (1h) so a
    start still in flight — here or in a sibling instance sharing the
    directory — is never raced, alongside the existing container orphan prune.
- With the Go backend down, the chat client read the resulting 502 as an
  expired session and redirected to `/login`, which sent it straight back to
  `/chat` — an endless bounce instead of an error. Unauthenticated (401/403)
  and unreachable (502/503/504, network failure) are now distinguished, and the
  latter renders a retry state naming the real problem.

- A login attempt refused by the `/auth/verify` rate limiter surfaced as "the
  chat server isn't reachable" rather than the throttle message the backend
  actually returned.
- An env-file override of a `${VAR:-default}` MCP env/header key was silently
  ignored on the standalone paths (`fleet serve`, `task run`, `mcp test`,
  `validate-config`): the bundle manifest interpolates BEFORE `config.Load`
  applies the `.env` file, so the literal default was baked in at load — and
  the override's var name did not even survive the `.env` allowlist, because
  `EnvVarNames` scanned the post-interpolation values where the token no
  longer existed (the fleet#706 residual). Connector env/header values (and
  inline `http_tools` headers) now keep their raw `${...}` text through the
  load and resolve against the live process env at catalog-build time, after
  the `.env` file is applied, so the env-file value wins over the default.
  The same single-pass resolution repairs the `$${` escape for those fields,
  which the load pass used to consume so the spawn pass expanded — and
  blanked — the author's escaped literal. `${VAR:?...}` still validates at
  load, against the pre-`.env` process env.

- `${FLEET_WORKSPACE:-...}` / `${FLEET_WORKSPACE:?...}` — and any other
  colon-suffixed spelling of a reserved runtime token — bypassed the
  reserved-token guard, which matched only the bare form: the token was
  resolved from the process env (an exported `FLEET_WORKSPACE` could hijack
  it) or its default was baked into the manifest, silently defeating the
  launch-time workspace substitution. Non-bare reserved-token spellings now
  fail the bundle load with an error naming the contract
  (docs/MCP-BUNDLE-ENV.md), and the reserved names are no longer registered
  into the `.env` allowlist as if they were ordinary env vars.
- `sudo fleet update` rebuilt the sandbox image into root's podman store, which
  the `User=fleet` unit can never read — root's rootful store is a separate
  namespace from the service user's rootless one, which is why `bootstrap.sh`
  builds through `runuser`. Every rebuild an operator triggered through
  `fleet update`, or by following doctor's own "build it: sudo fleet update"
  hint, was therefore a no-op for the running service. Root-run builds now
  land in the service user's store, and the fix sits in
  `scripts/build-sandbox-image.sh` so every root caller inherits it.

- The sandbox-image rebuild gate compared only the Containerfile hash, so a
  bundle that renames `sandbox.tag` with byte-identical build instructions
  skipped the build. fleet does not verify the image at boot, so the box came
  up healthy and then failed every sandboxed tool call against a tag that was
  never built. The gate now also tracks the resolved tag and rebuilds when the
  expected image is absent from the service user's store.

- `fleet update` restarted the service even when the sandbox build it had just
  attempted failed and the resolved image existed nowhere in the service
  user's store — a `sandbox.tag` rename plus one transient build failure left
  the box reporting healthy while every sandboxed tool call failed, until a
  human noticed. The update now refuses to restart in that state, spelling out
  the recovery commands; a failed build only warns past when the
  currently-resolved tag still exists (Containerfile changed under the same
  tag — the previous image is stale but serviceable).

- The update's sandbox-store probe ran rootless podman as the service user
  without pre-creating `/run/<user>` — tmpfs, present only while the unit's
  `RuntimeDirectory=` keeps it alive — so during a stopped-unit maintenance
  window `podman image exists` failed environmentally and the gate read that
  as "image missing", burning a spurious multi-GB rebuild. The probe now
  pre-creates the runtime dir the way the build path already did, and only
  podman's positive "not found" (exit 1) counts as absent; any other podman
  failure leaves the image as-is and says so.

- The sandbox rebuild gate keyed on the manifest's `sandbox.tag` even when the
  bundle resolves `sandbox.image` to a prebuilt ref the service pulls directly
  (image wins over tag), so the first update after a client switches to
  registry-published images performed one pointless multi-GB on-box build that
  nothing reads. `update.sh` now mirrors `bootstrap.sh`'s
  `resolve_sandbox_image` skip — one rule, same interpolation.

- The update's unit-adoption loop covered only `fleet.service` and
  `fleet-web.service`, so a fix to the shipped backup units reached
  provisioned boxes only via doctor, never via the update path operators
  actually run on release. The loop now also adopts `fleet-backup.service`
  and `fleet-backup.timer` when they are installed on the box — without any
  restart, matching doctor's rule: the daemon-reload alone re-arms a rewritten
  timer's schedule, and restarting the oneshot would run a backup immediately.

- The production MCP-broker boundary refused to boot any bundle that wires the
  documented cutlass-family connector contract: the connector/parent env
  overlap guard treated the whole `CUTLASS_` prefix as parent-owned, but fleet
  itself designates `CUTLASS_RUN_WORKDIR` / `CUTLASS_MOC_TASK_ID` /
  `CUTLASS_REPORT_DIR` / `CUTLASS_INPUT_DIR` as bundle-to-connector wire keys
  (`internal/agentcore/mcp_workspace.go`), and operator passthroughs such as
  `CUTLASS_ALLOWED_DIRS` / `CUTLASS_USER_AGENT` are never resolved by the
  parent. Startup failed with "connector environment overlaps parent-owned
  configuration" on exactly the manifest shape the contract tells bundles to
  write. The guard still fails closed on overlap with parent runtime settings,
  provider keys, webhook signing secrets, and the `CUTLASS_*` names the parent
  itself resolves — those are now enumerated explicitly instead of claimed by
  prefix.

- The `--storage-opt size` support probe assumed `/usr/bin/true` exists in the
  sandbox image — a **client-bundle artifact** free to change its base, where a
  busybox-based bundle has `/bin/true`. Such a bundle failed the probe on every
  host, so the writable-**layer** disk quota was silently dropped with only a log
  line. Scope of that loss, stated precisely: the per-file `--ulimit fsize` cap
  still applied, total *workspace* bytes are unbounded either way (a bind mount
  is outside any storage-driver quota), and under `--read-only` plus size-bounded
  tmpfs the writable layer is nearly unwritable anyway — so this restores
  defense-in-depth, not a cap that was holding back live disk exhaustion. Podman
  validates `--storage-opt` at
  container-create time and only then execs the command — verified on an ext4
  host, where a nonexistent entrypoint still reports the *quota* error — so a
  failure to exec now counts as quota-accepted. The classification is
  deliberately narrow in the fail-closed direction: misreading a real quota
  rejection as an exec failure would pass `--storage-opt` to every container and
  break every start, which is worse than losing the layer quota.

- **`FLEET_DEFAULT_NETWORK_MODE=allowlisted` was entirely non-functional on a
  stock modern host.** Allowlisted egress emits
  `--network=slirp4netns:allow_host_loopback=true` to reach the host-bound
  proxy, but Podman ≥ 5.0 defaults to `pasta` and a stock modern box (verified on
  Fedora 44) ships
  pasta *without* `slirp4netns` — so every container start failed. It failed
  closed (a container that will not start cannot leak), but late and
  repeatedly: boot succeeded, the proxy bound, the log announced "egress
  filtered to […]", and then every interactive turn, scheduled task, and
  approved-bash call errored. `PreflightAllowlistedNetwork` now asks Podman at
  boot whether it has the helper (`podman info`, image-independent — a container
  probe would abort boot on any bundle whose rootfs lacked the probe command,
  since the netns is configured before the exec) and fails boot closed if it
  does not, naming the helper and the fix; `fleet validate-config` runs the same
  check; `scripts/bootstrap.sh`
  installs `slirp4netns` so a bootstrapped box supports the mode. Teaching
  `networkArgs` to use pasta's `--map-host-loopback` instead is deliberately
  deferred — pasta exposes the host at a different gateway address, so it also
  changes the proxy URL and `NO_PROXY`, and that belongs in its own reviewed PR
  with rootless-host verification (recorded in ADR-0012).

- **`validate-config` failed the persona knob that only degrades, ignored the one
  that is fatal, and `FLEET_PERSONA` did nothing.** The manifest check resolved
  `PERSONA` against the bundle ROOT and made a miss a blocking failure, so any
  bundle not shipping `personas/assistant.yaml` — the loader's built-in fallback
  — got a red `✗ manifest: default persona personas/assistant.yaml missing`
  unless `PERSONA` happened to be exported in the shell running the check. Our
  own production bundle, which ships `personas/victoria.yaml`, failed that way;
  a preflight that reports a bundle as broken when it boots fine teaches
  operators to ignore the whole report. Meanwhile `PERSONA_DEFAULT` — the
  interactive default, and the one a miss actually breaks — was never checked at
  all. Both are now resolved the way their readers resolve them (by basename
  inside the bundle's `personas/` dir, so `victoria`, `victoria.yaml` and
  `personas/victoria.yaml` all find the file), each at the severity its runtime
  earns: a missing `FLEET_PERSONA_DEFAULT` is a blocking `✗`, because
  `agent.Manager` returns an error when it cannot read the persona and the turn
  fails, and every new conversation starts on that name; a missing
  `FLEET_PERSONA` is a non-blocking `⚠`, because the scheduled driver ignores the
  read error and only loses the persona's expertise block. Either way the report
  names the knob to set and lists the personas the bundle *does* offer, in the
  shape that knob takes. Separately, both knobs were read with a bare
  `os.Getenv`, outside the prefix alias machinery: an operator following the
  documented `FLEET_` convention got silence and the built-in default, while the
  broker already treated the names as parent-owned. `FLEET_PERSONA` /
  `FLEET_PERSONA_DEFAULT` (and their `CHAT_`/`CUTLASS_` spellings) are now read
  first, with the unprefixed names kept as the back-compat fallback and the
  canonical spellings added to the env-file allowlist so they survive a
  `FLEET_ENV_FILE` load. The two shapes are now written down where personas are
  documented: `FLEET_PERSONA_DEFAULT` takes a persona *name*, `FLEET_PERSONA` a
  bundle-relative *path*.

- Empty MCP tool-call arguments crossed the broker wire as nil
  (`args,omitempty` drops empty maps) and were then marshaled onward as
  `"arguments": null`. Strict MCP servers reject null arguments with `-32602
  Invalid params` — pages.elcanotek.com refused every no-arg tool
  (`mcp_pages_list_templates` and friends) — and because the credential owner
  masks operational errors, the refusal reached the agent as `mcpbroker:
  credential-owner call failed` and read like a credential problem. A nil
  arguments map is now normalized to an empty object at both the broker
  boundary and in `callTool`, pinned by regression tests at each layer. (#958)

- `bootstrap.sh`'s no-`--domain` hint told only an operator proxying from
  ANOTHER host to set `NEXT_PUBLIC_PUBLIC_ORIGIN`. A same-host TLS proxy
  operator following it verbatim shipped a web tier that redirects every login
  to `http://localhost:3000` and mints session cookies without `Secure` — the
  configured origin drives both decisions (`web/src/app/lib/auth.ts`), same
  host or not. Diagnosis was non-obvious because Next inlines `NEXT_PUBLIC_*`
  at build time: editing the env file changes nothing until web/ is rebuilt.
  The hint now tells EVERY custom-proxy front to set the origin in
  `fleet-web.env` and rebuild + restart (`scripts/update.sh` does both),
  keeping `FLEET_WEB_HOST=0.0.0.0` as the extra step only for a proxy on a
  different host.

- A hand-quoted value in `fleet.env` (`FLEET_BACKUP_DIR="/mnt/x"` — legal in a
  systemd `EnvironmentFile=`, which strips the quotes) made every later
  bootstrap re-run die with the misleading `FLEET_BACKUP_DIR must be an
  absolute path (got '"/mnt/x"')`: the re-run read the value back verbatim,
  quotes included. Re-running bootstrap is the documented idempotent upgrade
  path, so a legal env file blocked provisioning until someone spotted the
  quotes. The backup read-backs now go through the same quote-stripping
  `env_get` that `doctor.sh` already uses.

- `validate-config`'s persona inventory accepted `.yml` files that no persona
  roster can load — `agent.ListPersonas`, the task-create catalog and the
  interactive loader are all `.yaml`-only (the loader force-appends the
  suffix) — so a `.yml`-only bundle looked persona-equipped and the report
  offered a remediation that loops: setting the knob to the suggested name
  still resolves to a `.yaml` file that does not exist, while chat's roster is
  empty. The inventory is now `.yaml`-only to match the readers, and such a
  bundle reports as shipping no personas.

- Run as root on a provisioned box (the unit installed), the live-sandbox
  harnesses (`scripts/e2e-boot-server.sh`, `scripts/run_workflow_live.sh`)
  built the sandbox image through `build-sandbox-image.sh`, whose root path
  delegates the build to the fleet unit's `User=` — correct for `sudo fleet
  update`, wrong here, because the harnesses probe and run fleet as the
  INVOKING user, whose image store is a separate namespace. Every root run
  burned a multi-minute rebuild into a store it never reads and then failed
  anyway — an invitation to "fix" it by weakening the delegation that protects
  the real deployment. Both harnesses now force a local build by pointing
  `FLEET_SERVICE_NAME` at a unit that does not exist, with a comment saying
  why the delegation must stay.

### Documentation

- Correct the sandbox documentation claims that outran the code — across the
  ADRs, `README.md`, `ONBOARDING.md`, `DEPLOYMENT.md`, the default bundle, and
  the Go comments that state the same guarantees — each verified against current
  `dev` rather than assumed. The load-bearing ones:
  `docs/SANDBOX-RUNTIMES.md` opened by listing **MCP** among the tool calls that
  run inside the sandbox — the exact opposite of the host-side credential
  brokering invariant (ADR-0003/ADR-0040); ADR-0002's Enforcement section
  credited `sandbox_hardened_test.go` with pinning the hardening flags, which it
  never did (and which no CI lane even runs — it is opt-in behind
  `FLEET_SANDBOX_HARDENED_TEST`), while the test that now genuinely pins them is
  `podman_args_test.go`; and ADR-0012/ADR-0031/FEATURE-NOTES claimed
  `allowlisted` is "strictly more restrictive than open", which is false in the
  host-loopback dimension — `allowlisted` requests
  `allow_host_loopback=true` and exempts the `10.0.2.2` gateway from `NO_PROXY`,
  so it can reach loopback-bound services that `open` cannot. That is now
  threat-modelled rather than denied. Also: the KVM-VM boundary is per
  container, not "per tool call"; `FLEET_PYTHON_REPL_MAX` is a soft cap checked
  only at create time; `--network=none` does have a loopback device; and the
  empty-allowlist boot warning named only scheduled tasks.

- Two more claims that outran the code, each verified against the current
  behaviour: `docs/MCP-BROKER-SCOPES.md` still said manifest interpolation
  replaces `${VAR}` with its value in the parsed bundle for connector values —
  connector env/header values now keep their raw text through the load and
  resolve lazily at catalog-build/spawn time, after the `.env` file is applied
  (the paragraph now also states why the name inventory exists at all: the
  names must survive independent of when values resolve) — and ADR-0041
  counted "three callers outside the auth middleware" in the sched epoch test,
  when `headerTrustUser` has three callers *total*: the middleware plus the
  two routes outside it (task create, upload).

### Security

- Two origin checks still derived their trusted host from client-suppliable
  `x-forwarded-*` headers after their siblings moved to the configured
  canonical origin: the OIDC `redirect_uri` a login sends to the IdP
  (`lib/oidc.ts` `buildRedirectUri`) and the host a mutating request's `Origin`
  header is compared against (`lib/csrf.ts` `verifyOrigin`). Behind the shipped
  Caddy config those headers are proxy-set and correct; behind a proxy that
  forwards a client's own values — the deployment shape the bootstrap no-domain
  path leaves to the operator's proxy — the SSO start route could hand the IdP
  an attacker-picked callback host (contained by an exact-match redirect-URI
  registration at the IdP, which is why this is defense-in-depth rather than an
  open hole), and the CSRF comparison could be aimed at an attacker-chosen
  expected host. Both now prefer `NEXT_PUBLIC_PUBLIC_ORIGIN` through the same
  `lib/auth.ts` helper the redirect and Secure-cookie decisions already use
  (now exported), keeping the header derivation only as the fallback when no
  origin is configured, so plain-http local dev is unchanged. **A deployment
  whose `NEXT_PUBLIC_PUBLIC_ORIGIN` differs from the host users actually hit —
  in practice a no-domain box fronted by an operator proxy without the
  `NEXT_PUBLIC_PUBLIC_ORIGIN` line bootstrap already instructs for that shape —
  will emit a different `redirect_uri` (needs re-registration at the IdP unless
  `FLEET_OIDC_REDIRECT_URI` pins it; the pin still outranks everything) and
  will refuse mutating requests until the origin is corrected.**
- A `run_if` pre-run gate was only enforced on one of the paths that could
  dispatch the task it guards. The gate — an admin-authored host-side shell
  condition, which is why authoring one requires admin permission — is
  evaluated solely at the scheduler's scheduled→pending promotion, but a task
  can reach dispatch without ever passing through it: `POST /tasks/{id}/rerun`
  copies the source's gate and mints an immediate *pending* run (so any
  principal with mere `create_task` permission could execute the gated work
  with the condition unchecked), an immediate create with a gate did the same,
  a webhook/email trigger spawn dropped the template's gate outright, and
  `PUT /tasks/{id}` re-derived the edited task's status from `scheduled_for`
  alone (so that same `create_task` principal could echo a gated scheduled
  task unchanged, omit `scheduled_for`, and flip it to `pending` with the
  gate intact and never evaluated). The contract is now enforced
  structurally: `models.NewTask` and the edit recompute in
  `storage.UpdateEditableTask` derive the dispatch state through one shared
  rule (`models.DeriveDispatchState`) that parks every gated cron task on the
  scheduler path (`scheduled`, with a nil `scheduled_for` defaulted to now),
  so whatever minted or last edited it — create, batch, rerun/clone, trigger
  spawn, import, recurrence, edit — the gate is evaluated before every
  dispatch, at the cost of up to one 30-second tick of latency for an
  "immediate" gated run. Trigger spawns now inherit the template's gate, and
  since a *pending* task is already past its evaluation point, the edit path
  refuses to attach or change (but still allows removing) a gate on one
  rather than silently re-parking an imminent dispatch. The full contract
  lives on `models.RunIf`.
- A password reset did not end the account's existing sessions, so the standard
  response to a compromised account did not evict the attacker. The web session
  cookie is a stateless HMAC over `{email, exp}` that the Next.js tier verifies
  by itself, and the only server-side gate — the user-list check in
  `membershipMiddleware` — admits any request whose email still exists. Nothing
  in the token or the `users` table recorded *when* the session was issued, so a
  stolen cookie stayed valid for its full remaining lifetime, up to 14 days,
  no matter how many times the password was changed underneath it. The only
  working levers were deleting the account outright or rotating
  `APP_SESSION_SECRET`, which logs out everybody. Sessions now carry a per-user
  **session epoch** derived from the stored bcrypt hash, forwarded alongside
  `X-User-Email`, and compared against the account's live value by **both**
  backends the one cookie authenticates: chat, inside the user lookup
  `membershipMiddleware` already performs, and the Operations Center, whose
  header-trust path resolves the chat-plane epoch through an injected lookup seam
  (the two planes keep separate databases, ADR-0005). A mismatch is a 401
  `session_revoked` that also drops the stale cookie, so a reset takes effect on
  the next request to either view. Deriving the epoch
  rather than storing a counter means every password write moves it, including
  `fleet chat user passwd`, `fleet admin add` on an existing address and the
  legacy importer, and a reset to the *same* password still rotates it because
  bcrypt re-salts. Magic-link (`elcano_auth`) sessions are unaffected and stay
  revocable at the auth service, which mints that cookie, and an Operations
  Center bearer login is its own credential; the design lives in
  [`docs/SESSION-EPOCH.md`](docs/SESSION-EPOCH.md) with the invariant in
  [ADR-0041](docs/adr/0041-mandatory-session-epoch-claim.md), and the revocation
  levers, these carve-outs and their blast radius in
  [`docs/DEPLOYMENT.md`](docs/DEPLOYMENT.md), including `APP_SESSION_SECRET`
  rotation as the deliberate break-glass global logout.
  **At deploy time every logged-in user must sign in once more:** cookies minted
  before this change carry no epoch claim, and the web tier refuses them rather
  than grandfathering a token that no backend check could evaluate.
- The `run_if` host-side gate — a scheduled task's shell command, run by the
  scheduler as the fleet user — was authored by the wrong set of people in both
  directions. `taskCreator.isAdmin` was set for **any** typed API key carrying
  `create_task`, so a scoped CI key could attach an arbitrary `sh -c` gate; and
  it was never set on the web/header-trust or cookie paths, so a user whose role
  really is admin was refused. Both create and edit now gate on one definition —
  possession of `models.PermissionAdmin` — which admits admin keys and role-admin
  users and refuses scoped keys.
- A bundle's per-server `tool_allowlist` (Gate-2) was silently ignored in two
  situations, so agents were offered every tool of every selected connector
  regardless of what the operator allowlisted. First, activating the production
  MCP-broker boundary scrubs `cfg.MCPServers` after the broker child boots, and
  the scheduled agent derived its gates from exactly that field — every
  scheduled and sub-agent run therefore ran ungated. Second, the gate looked up
  the allowlist by the *registered* server name, which for a named-account seat
  is `<server>_<account>`, so selecting a seat dropped the filter for that
  server. The allowlist now travels with the broker's public inventory, and
  registered names resolve back to their manifest entry the way the credential
  and persona filters already did.
- Replacing the blanket `CUTLASS_` parent-owned prefix with a hand-written list
  of names left real gaps: `CUTLASS_SERVER_TOKEN` (a supported legacy spelling
  of the shared server token, whose `FLEET_`/`CHAT_` spellings *were* listed)
  and six knobs the parent resolves lazily after broker boot —
  `CUTLASS_OPENROUTER_BASE_URL`, `CUTLASS_MODEL_CACHE_TTL_MINUTES`,
  `CUTLASS_CONTEXT_PRESSURE_WARN_THRESHOLD`,
  `CUTLASS_CONTEXT_COMPACTION_THRESHOLD`, `CUTLASS_SCHEDULED_AUTO_COMPACT` and
  `CUTLASS_MAX_ITERATIONS` — could be claimed by a bundle connector, which then
  unset them in the parent and dropped them from the reload surface. The legacy
  spellings are now derived from the `FLEET_`-spelled names and the configured
  legacy prefixes rather than hand-listed, so the set cannot drift again. A
  bundle that claims one of these names now fails broker boot validation
  instead of silently mutating parent runtime behavior; the cutlass-family
  connector wire contract stays claimable as before.
- `fleet-web` bound `0.0.0.0:3000` while the Caddyfile, `bootstrap.sh` and the
  deployment docs all described it as loopback-only. On a host without
  firewalld nothing stopped a client reaching the web tier directly, bypassing
  Caddy — which matters because the tier trusts `x-forwarded-proto` (a direct
  plain-HTTP login minted a session cookie without `Secure`) and
  `x-forwarded-host` (login/logout/OIDC redirects). The unit now binds
  loopback, and both helpers prefer the configured canonical origin
  (`NEXT_PUBLIC_PUBLIC_ORIGIN`) over client-supplied forwarding headers.

- Stop the boot-time orphan sweep from being starved by PID reuse, and from
  eating this process's own warm containers. Two independent bugs in the same
  sweep:
  - The `fleet.instance` label records `<pid>@<start>` and `prune.go` claimed the
    start time "disambiguates pid reuse" — but nothing read it. Once a crashed
    run's pid was recycled by any unrelated live process, its orphaned container
    was skipped forever, so the leak the sweep exists to reclaim became
    permanent. The labeled start time is now compared against the live process's
    actual `/proc` start time, failing safe in both unknown directions (an
    unparseable label or an unreadable `/proc` means "assume alive" — leaking a
    container is recoverable, force-removing a live sibling's sandbox mid-turn is
    not).
  - The sweep force-removed any `created`/`exited` container **regardless of
    label**, and it runs *after* the agent manager is built — which starts the
    warm pool filling from a goroutine. So this process's own warm containers,
    caught mid-creation, were removable by their own boot. Containers carrying
    this process's label are now skipped in every state, and the call-site
    comment no longer claims the sweep runs "before building the pool".
- Bound the sandbox telemetry poller so it can never block teardown. `podman
  stats` had no `WaitDelay`, so `Output()` could wait on pipes that context
  cancellation does not close (the hazard `BashWaitDelay` already exists for),
  and `close()` waited on the poller with a bare receive — making sandbox
  teardown, the turn's sandbox release, and the pool slot hostage to a telemetry
  goroutine. The poll is now `WaitDelay`-bounded at 2s (a stats sample that slow
  is discarded anyway) and the teardown wait at 5s, past which the rollup is
  abandoned with a log line rather than stalling.

- Deny `AF_VSOCK` sockets in the sandbox seccomp profile. Under the Kata/libkrun
  microVM runtimes (ADR-0010) `AF_VSOCK` is the guest↔host channel, and podman's
  own default profile denies it while ours allowed it. Closed with a single
  non-overlapping rule (allow every family except `AF_VSOCK`, letting the
  default-deny do the work) rather than by copying podman's five socket rules —
  which was tried and measurably does NOT work, because one of podman's broad
  `SCMP_CMP_NE` allows also matches `AF_VSOCK` and absorbs the narrow deny.
  Verified against the real image: `AF_VSOCK` returns EPERM while `AF_UNIX`,
  `AF_INET`, `AF_INET6`, datagram and `AF_NETLINK` sockets all still open, and
  DNS, HTTPS and `pip install` all work. **Scope, precisely:** this stops the
  ordinary calling convention, not a hostile payload — seccomp compares the full
  64-bit register while the kernel truncates `domain` to `int`, so
  `socket(0x100000028, …)` still succeeds. **Podman's own default has the
  identical bypass**, so this is parity with the platform default rather than a
  fleet-specific weakness, and two obvious hardenings were measured and rejected
  (ANDing two comparisons on one argument fails *open*; a masked-equality deny is
  absorbed by the broad allow). Also still open and documented in `seccomp.go`:
  `AF_NETLINK`+`NETLINK_AUDIT`, which podman denies, and `socketcall(2)` on the
  32-bit architectures the profile still lists.
- Deny `vmsplice` in the sandbox seccomp profile. Podman's own default profile
  denies it; ours allowed it, which made fleet's custom profile **weaker than
  shipping no profile at all** in that one dimension — the opposite of what it
  exists to do. `vmsplice` lets a process splice pages of its own address space
  into a pipe, a primitive that has featured in kernel-memory-disclosure and
  container-escape exploits. Verified nothing in the sandbox needs it: bash
  pipes, `cp`, 20 MB pipe/copy workloads, `shutil.copyfile`, `os.sendfile`
  zero-copy, pandas and numpy all work with it denied, and the profile test now
  pins the denial.

- Resolve the sandbox OCI runtime through Podman in the fail-closed preflight
  instead of guessing the binary from its name. `podman --runtime=<r> info`
  resolves the name through `containers.conf` and errors on an unregistered
  one, so the preflight now probes the binary Podman will actually exec: a
  `containers.conf` remap can no longer make it validate a *different* binary
  than the one running every tool call, and a runtime installed on `PATH` but
  never registered now fails at boot rather than at every container creation.
  Resolution applies to any non-empty runtime (only Podman's own default is a
  no-op); `kata`/`krun` keep their additional KVM and `+LIBKRUN` gates. This
  also removes a false FAIL in `fleet validate-config`, which reported a valid
  `containers.conf` mapping to an off-`PATH` binary as a missing runtime.
- Always emit the per-file `--ulimit fsize` sandbox disk quota, adding
  `--storage-opt size` on top where the driver supports it, instead of
  choosing one or the other. The either/or left the workspace **uncapped on
  exactly the hosts with the better storage driver**: `--storage-opt` bounds
  the writable layer, which is essentially unwritable under `--read-only`, and
  does not apply to bind mounts — so `dd if=/dev/zero of=big` in the default
  workdir could fill the host disk, the scenario `FLEET_SANDBOX_DISK_GB`
  exists to prevent. The boot log and the `DiskLimitGB` docs now state plainly
  that total workspace bytes remain unbounded. **Operator-visible:** hosts with
  XFS-pquota / btrfs / zfs now enforce the per-file ceiling they previously had
  none of — raise `FLEET_SANDBOX_DISK_GB` if a workload legitimately writes
  single files above the 5 GiB default (a `bash`+`curl` download of a very
  large file is the likeliest case). Hosts on other drivers are unaffected;
  they already had this cap.
- Apply the fleet-wide `FLEET_DEFAULT_NETWORK_MODE` to the approved-bash take.
  An approval executes out-of-band of the turn loop and honored only the
  per-conversation lockdown seal, so on a `lockdown` deployment an approved
  command from a non-lockdown conversation still ran with open egress, and on
  an `allowlisted` one it bypassed the proxy — despite ADR-0012/ADR-0031
  claiming the setting applies fleet-wide. It now mirrors the interactive turn
  take exactly; the per-conversation seal keeps its stricter no-degrade rule.
- Extend the #796 straggler guard to `run_python`: a cancelled or timed-out
  cell now synchronously kills the container and poisons the sandbox — parity
  with bash and the sandboxed file tools — instead of killing only the
  host-side bridge client while the cell kept executing (and, in persistent
  REPL mode, could be lent to later turns). Bridge write/read errors now reset
  the session so the next `run_python` boots a fresh bridge instead of wedging
  for the rest of the turn, and a failed close-time container kill is logged
  instead of silently swallowed.
- Cap host-side `run_if` stderr capture at 8 KiB while continuing to drain the
  command, preventing a noisy admin-authored gate from exhausting Fleet's heap;
  document the admin-only gate in the host-I/O exception inventory.
- Remove the unused arbitrary trailing Podman-argument seam from sandbox
  configuration, so internal callers cannot override mandatory hardening flags;
  correct stale package docs that claimed a production host fallback.
- Replace credential-owner MCP call, discovery, scope-open, scope-close, and
  reload error details with stable value-free broker errors; failed calls also
  discard partial output before it can cross IPC.
- Pin Next.js transitive `postcss` and `sharp` dependencies above their patched
  floors and refresh both vulnerable `brace-expansion` lines in the web lockfile.

### Added

- **EKS deployment guide (`docs/EKS-DEPLOYMENT.md`):** an operator recipe for
  running fleet on Amazon EKS as **one pod on one large, dedicated node** —
  keeping the single-process/single-writer model of
  ADR-0004 intact rather than sharding sandboxes across worker nodes (no such
  seam exists; ADR-0011 removed the worker registry). Covers the rootless
  Podman-in-a-pod requirements that fail *silently* if missed (subuid ranges,
  `newuidmap` file caps vs. privilege escalation, an overlay driver instead of
  vfs, `/dev/fuse` + `/dev/net/tun`, and the writable cgroup subtree without
  which `--memory`/`--cpus` are ignored and every per-sandbox cap becomes
  fiction), Containerfiles for the two images the repo does not ship, RDS
  two-database setup, an xfs+prjquota StorageClass so the writable layer gets a
  hard **total** cap on top of the per-file `ulimit` that applies regardless
  (both helpers installed in the image, since pasta is Podman ≥ 5.0's default for
  normal turns while the allowlisted-egress posture requires slirp4netns
  specifically), pod sizing that accounts
  for sandbox cgroups nesting under the pod limit, exec health probes (kubelet
  dials the pod IP and cannot reach the loopback-only listeners), the ALB idle
  timeout that otherwise severs SSE turns, and a verification checklist. Also
  answers the objections a Kubernetes-native reviewer raises, with the cluster
  integration each one needs: Pod Security Standards / Kyverno-Gatekeeper
  exceptions for the privileged pod, `fsGroup` so uid 1000 can write the EBS
  volume, IRSA or Pod Identity with **no** RBAC (fleet makes zero Kubernetes API
  calls), a NetworkPolicy that does govern agent egress (sandbox traffic NATs
  through the pod's netns), single-AZ pinning and the honest RTO/RPO of a
  single-writer workload, Kustomize/Argo packaging including the
  `volumeClaimTemplates`-immutability and PVC-prune traps, a loopback-preserving
  metrics scrape sidecar, and the defaults that silently break this pod
  (NodeLocal DNSCache vs. `slirp4netns`, namespace `ResourceQuota`, VPA `Auto`,
  runtime-security signatures firing on normal nested-container operation).
  Closes with an appendix carrying the whole manifest set assembled in apply
  order with a placeholder table, so it transcribes in one paste rather than
  nine. Flags the deployment gap the new `fleet-backup.timer` leaves on
  Kubernetes — the units and their doctor coverage are systemd-only, so a cluster
  install silently has no scheduled dumps unless RDS backups or a `CronJob` are
  chosen deliberately (the #966 failure mode), while noting the in-process Doctor
  panel does still work and reports those checks as `skip` rather than inventing
  advisories. No Helm chart or in-tree manifests: ADR-0004's enforcement clause stands,
  and an unmaintained chart would imply support this path does not have. States
  its own scope honestly: hand-verified, **not** exercised by CI, no chart or
  manifest shipped in-tree, and explicit about the weaker outer trust boundary a
  privileged pod implies.

- **Scheduled database backups are part of the deployment now, not a doc the
  operator had to find.** `docs/BACKUP_RESTORE.md` described a
  `fleet-backup.service` + `fleet-backup.timer` pair and told the operator to
  install a daily timer, but `scripts/bootstrap.sh` never installed it and
  `fleet doctor` never looked for it — so a box could report `38 ok, 0
  advisories` while holding no backups at all, which is what a production
  deployment did for five days with live client data (#966). Three changes
  close the gap. The two units now ship in `deploy/`, version-controlled and
  covered by doctor's unit-drift check the way `fleet.service` is — as
  optional-if-absent, since not every deployment installs them.
  `scripts/bootstrap.sh --enable-service` installs and enables the timer by
  default: it creates the backup directory `0700` root-owned (a dump holds
  every conversation, task and user row), writes `FLEET_BACKUP_DIR` and
  `FLEET_BACKUP_RETENTION_DAYS` into `fleet.env` alongside the DSNs, and
  converges rather than duplicating on a re-run — an installed unit is left
  alone, and both settings resolve process env > the value already in the env
  file > the default, so a re-run keeps a backup directory you relocated
  instead of resetting it; `--no-backup-timer` is the opt-out. And both halves
  of doctor — `scripts/doctor.sh` and the in-process `internal/boxdoctor`
  behind Settings → Admin → Doctor — report the timer's state, in agreement. A
  missing timer is an **advisory** and doctor never installs one: an operator
  who backs up at the volume or hypervisor layer is not misconfigured. So is a
  timer that is enabled but not **active** — `is-enabled` reads only the
  install symlink, so a stopped timer fires nothing while its service still
  reports its last `Result` as `success`. A timer whose **last run failed** is
  a **failure**, because the `oneshot` already exits non-zero when a dump fails
  its integrity check, and a box that looks covered but is not is worse than
  one that is visibly bare. Reinstalling a drifted backup unit does not trigger
  doctor's post-fix restart, so repairing one never costs chat downtime. The
  doc now states plainly what the timer buys: a same-host `pg_dump` survives a
  bad migration or an accidental delete, not the loss of this host or volume,
  and it still does not capture attachment/upload files.
- **Restaurant-style model cost indicators ($ … $$$$):** both model pickers —
  the chat composer's listbox (and its collapsed model chip) and the operations
  center task form's primary/fallback pickers — now show a four-glyph price tier
  per model, derived from the OpenRouter catalog's per-token prices blended
  3 prompt : 1 completion (the ratio that matches fleet's transcript-heavy agent
  loops). Hover/screen-reader text gives the blended `$/M tokens`. Models with no
  published pricing (workspace providers, half-typed custom slugs) show no
  indicator rather than a guessed one, and the tier is a comparison aid only —
  spend is still governed solely by the per-run cost ceilings, with no price
  ceiling on model selection. No new network calls: `/api/model-catalog` gained
  `price_prompt` and `/api/model-rankings` gained both prices, additively, from
  the already-cached catalog. Catalog prices are also joined back onto the two
  pinned "recommended" rows, which dedup had been leaving price-less. See
  [`docs/MODEL-COST-INDICATORS.md`](docs/MODEL-COST-INDICATORS.md).

- **Production child-owned remote MCP scopes:** interactive and scheduled run
  drivers now send only user identity and public server-name filters to
  `fleet mcp-broker`; the child owns token lookup/refresh, SSRF-guarded HTTP MCP
  clients, scoped calls, and cleanup. Explicit OAuth/connectors HTTP endpoints
  remain parent-side control-plane operations (ADR-0040).

- **Child-owned remote MCP scopes (#167 prerequisite):** `fleet mcp-broker`
  now opens an encrypted chat-store connection when remote MCP is configured,
  performs per-user connection lookup, token/API-key decryption and refresh,
  SSRF-guarded handshake, tool discovery, calls, and cleanup inside the child,
  and returns only public tools and skipped names. The dedicated DB pool is
  capped at 8 open/2 idle connections, scope-open resolver and per-server
  token/handshake failure values do not cross the pipe or enter logs, and
  disabled operation fails explicitly. Production consumes these scopes through
  the activation above. See
  [`docs/MCP-BROKER-SCOPES.md`](docs/MCP-BROKER-SCOPES.md).

- **Remote MCP scope protocol (#167 prerequisite):** broker scope-open requests
  can now carry a user email plus public enabled/shadowed remote-server names,
  with an explicit filter bit preserving interactive “none selected” versus
  scheduled “all connected.” Scope responses expose only public tools and
  skipped-server names. Mixed bundle/remote fields and ambiguous filters fail
  before backend dispatch; the production child consumes this selector as
  described above. See
  [`docs/MCP-BROKER-SCOPES.md`](docs/MCP-BROKER-SCOPES.md).

- **Remote MCP broker overlay seam (#167 prerequisite):** interactive and
  scheduled runs can now receive a per-user remote-MCP overlay as an injected
  broker, public tool catalog, routing-name set, and bounded close function.
  The injected opener takes precedence over the existing resolver, while the
  in-process OAuth client remains a compatibility path for tests and embedders.
  Production uses the child-owned remote scope described above. See
  [`docs/MCP-BROKER-SCOPES.md`](docs/MCP-BROKER-SCOPES.md).

- **Production bundle MCP process boundary (#167):** `fleet serve` now starts
  the credential-owning broker subprocess before serving and routes interactive
  turns, scheduled tasks, approvals, and MCP reload through it. The parent keeps
  public catalog/account/gating metadata only; after liveness and discovery
  succeed it unsets connector env keys, overwrites/drops resolved MCP and inline
  HTTP definitions, and prevents config hot-reload from rehydrating exact or
  account-suffixed keys. Startup fails on a connector/parent env-name collision
  or broker preflight error, without scrubbing on failure. Per-user run-time MCP
  clients are child-owned by the activation above. See
  [`docs/MCP-BROKER-SCOPES.md`](docs/MCP-BROKER-SCOPES.md).

- **Per-run MCP broker scope protocol (#167 prerequisite):** the internal broker
  can open an isolated session from non-secret server/account selections, task
  ID, and workspace path; return its opaque ID and public tool catalog; and route
  the existing `agentcore.MCPBroker` call seam through that scope. Scope calls
  retain request correlation and cancellation, close is idempotent after success
  and retryable after failure, legacy backends fail explicitly, and backend
  panics remain value-free and incident-correlated. This protocol groundwork is
  consumed by the production bundle and remote-MCP boundaries above. See
  [`docs/MCP-BROKER-SCOPES.md`](docs/MCP-BROKER-SCOPES.md).

- **Credential-owning MCP scope backend (#167 prerequisite):** `fleet
  mcp-broker` now constructs each requested account/task/workspace scope inside
  the child, serializes close against admitted calls, and reaps all remaining
  scoped subprocesses when its protocol loop exits. The shared spawn-definition
  builder now excludes disabled servers, so a stale task selection cannot launch
  one. A cancelled open also closes a late-created scope instead of losing its
  opaque handle. Production now consumes this backend as described above.

- **Interactive MCP broker seam (#167 prerequisite):** `TurnConfig` can now
  inject an out-of-process broker plus its public catalog into the unchanged
  `agentcore.Run` path. Per-user remote overlays compose with that injected base
  without shadowing bundle servers. The local-client default remains for
  compatibility callers and tests; production injects the subprocess broker.

- **Approval MCP broker seam (#167 prerequisite):** interactive email
  pre-validation and post-approval MCP execution now depend on the common
  broker/catalog seam instead of a concrete credentialed client. Prefixed tool
  routing remains server-qualified (including underscore-bearing names), and a
  broker tool-level error now resolves the approval as failed. Production uses
  the child broker's unscoped default-seat path for long-lived approvals.

- **Connector env inventory (#167 prerequisite):** client bundles now retain a
  names-only inventory of MCP and inline-HTTP-tool environment references from
  the raw manifest, before interpolation can erase already-exported variable
  names. The inventory also expands account-suffixed stdio env keys against a
  supplied environment, enabling the production parent to scrub exactly the
  connector keys after a broker child inherits them. The production boundary
  above now consumes this inventory.

- **Interactive Manager MCP scope seam (#167 prerequisite):** Manager can now
  consume an injected public broker/catalog/account inventory without creating
  a credentialed local MCP client, and can open one isolated broker scope per
  chat turn. Scope selection always includes mandatory bundle servers, includes
  only opted-in optional bundle servers, threads public account names and the
  bound conversation workspace, fails closed on open errors, and closes with a
  cancellation-independent timeout. Production now injects this seam.

- **Scheduled MCP broker seam (#167 prerequisite):** the scheduled `Agent` and
  its governed sub-agents can now call and discover bundle tools through an
  injected broker/catalog, including composition with the existing per-user
  remote overlay. Broker mode suppresses the in-process MCP loader tools instead
  of advertising an unavailable mutation path. Production uses this broker path.

- **Scheduled Runner MCP scopes (#167 prerequisite):** `scheduledrun.Runner`
  can now open and close an isolated broker scope per task, preserving explicit
  server/account choices and mapping an empty selection to all enabled bundle
  servers. Task ID and per-run workspace metadata cross the value-free seam;
  scope failures do not fall back locally, cancellation cannot suppress close,
  and remote-MCP shadowing uses the returned public catalog. Production injects
  this opener for every scheduled run.

- **Credential-owner MCP reload (#167 prerequisite):** the broker protocol can
  now ask the child to re-read its own bundle against its boot credential
  snapshot, apply the minimum server diff, and return only the public change summary and tool
  catalog, provisioned account-seat names, and enabled servers' public gating
  metadata. Resolved connector definitions never cross the pipe; reload retains
  request cancellation and value-free panic containment, existing scopes keep
  their snapshot, and future scope opens serialize onto a coherent old or new
  catalog.

- **Interactive broker reload seam (#167 prerequisite):** Manager can now apply
  a credential-owner reload result to its public catalog, account names,
  allowlists, optional gates, prompt roster, and picker metadata under the same
  serialized reload path as its local client. Broker-mode reload takes its
  server metadata from the child result instead of re-reading connector config
  in the parent, and publishes that entire public view atomically. An injected
  broker without a reload adapter fails explicitly rather than returning a false
  empty success. Production injects the credential-owner adapter.

- **Scheduled public MCP inventory (#167 prerequisite):** broker-scoped task
  runs can expand an empty selection and decide whether to mint a per-run
  workspace from a live names/`uses_workspace` snapshot, without retaining the
  credential-bearing `cfg.MCPServers` map. Updating the provider changes the
  next run while the local compatibility binder keeps its existing config path.

- **Query-parameter API keys for hosted connectors (Browserbase)**: some
  vendors authenticate their hosted MCP server with the key in a URL query
  parameter rather than a header. api_key connectors now support
  `api_key_query`: the key stays sealed at rest and is attached per-request
  by the HTTP transport, so the stored URL, logs, and error strings never
  carry it. The built-in Browserbase directory entry — previously marked
  OAuth, which its endpoint doesn't serve, so every connect failed at
  discovery — now uses `api_key_query: browserbaseApiKey` with a setup hint.
- **Self-wake** (`docs/SELF-WAKE.md`): a scheduled run can suspend itself
  and schedule its own resumption — new `sleep` (park until a deadline) and
  `wake_on_event` (park until `POST /tasks/{id}/wake` fires the matching
  key, or a timeout) tools with the exact ask-pause lifecycle: the run ends,
  sandbox/lease released, task parks in new status `paused_awaiting_wake`,
  and the scheduler's tick re-queues it as a fresh run carrying the agent's
  required note-to-self plus the wake reason. Every wake has a deadline
  (event waits default to 7 days), parks are capped at 100 per task, and
  cost accumulates on the task across cycles.

- **Discuss this run** (`docs/DISCUSS-RUN.md`): a finished scheduled run's
  log modal gains "Discuss in chat" — a one-way BFF bridge (inverse of
  promote-to-task) that reads the transcript through the caller's
  orchestrator credential, creates a chat conversation seeded with a clamped
  digest (new optional `seed` on `POST /conversations` — one user message,
  no turn), and deep-links to it via the new `/chat?c=<id>` boot param. The
  chat server never reads the sched store; ADR-0005's database split stands.

- **Per-attempt run log history** (`docs/RUN-LOG-HISTORY.md`): re-running the
  same task id — a retry, an ask-pause resume, a self-wake cycle — no longer
  destroys the prior attempt's transcript. The row the `logs` upsert would
  clobber is copied into `run_logs` in the same transaction (archived payloads
  travel verbatim), capped at 20 per task, pruned with the task by both
  retention paths. New `GET /logs/{task_id}/history[/{entry_id}]` behind the
  exact `GET /logs/{task_id}` gate, and an attempt picker in the task log
  modal that renders only when history exists.

- **A bundle can now brand the shell, not just tint it** (`docs/BRANDING.md`):
  new `branding.logo` — a bundle-relative image path served from the bundle by
  `/brand/logo` (proxied as `/api/brand/logo`), so the navigation rail's mark is
  a bundle change plus a restart, never a web rebuild or a file copied into
  `web/public`. Previously `NavRail` hardcoded `elcano-mark-primary.svg`, so
  every deployment wore one client's mark beside its own `app_name`; that asset
  is deleted from fleet and the fallback is fleet's own mark. The path is
  resolved at load — lexically local, still inside the bundle after symlink
  resolution, a regular file, a known image extension — so a bad value fails at
  startup instead of rendering a broken image on every page; the route caps it at
  2 MiB and pins delivery with `nosniff` + `default-src 'none'; sandbox` so an
  SVG carrying `<script>` executes nothing. `/client-config` advertises
  `logo_url` only when a file actually backed the field.
  **Seven more themable color tokens** — `text_disabled`, `border_strong`,
  `border_subtle`, `overlay_soft`, `overlay_strong`, `rail_hover`, `rail_active`
  — because `globals.css` hand-tints those from fleet's own primary hue, so a
  bundle overriding `primary` alone kept fleet-purple emphasis borders and rail
  rows beside its palette. The last two close the follow-up `globals.css` records
  inline. Semantic status colors (success/danger/warning) stay fleet's: they
  encode meaning, not brand.

### Fixed

- **Live Playwright no longer trips the password-brute-force limiter it is
  meant to preserve.** The real-stack suite submitted the same test account's
  password before nearly every spec, deterministically exhausting
  `/auth/verify`'s five-attempts-per-email window and turning unrelated tests
  into `/login?e=server` failures. It now creates one real password-authenticated
  worker session and copies that cookie into each isolated test context; the
  dedicated auth specs still exercise valid and invalid password submissions,
  and the production limiter remains unchanged.

- **Root Go gates no longer traverse npm dependencies.** An explicit module
  boundary at `web/` keeps `go build/test/vet ./...` scoped to Fleet-owned Go
  packages even after `npm ci` installs packages containing incidental Go code.

- **`fleet chat` now honors the server's terminal turn outcome.** The TUI and
  one-shot mode surface `turn.error`, model-selection failures, cancellation,
  premature stream EOF, and non-success health checks instead of treating them
  as successful empty or partial answers.

- **Chat migrations no longer deadlock with a one-connection pool.** The
  advisory lock and every migration query/transaction now share one dedicated
  connection, so `CHAT_DB_MAX_CONNS=1` remains a valid operator setting.

- **A recovered MCP-broker backend panic no longer strands its caller until the
  request timeout.** Call, tool-discovery, and account-discovery goroutines now
  complete the matching IPC request with a generic, incident-correlated error
  and keep the broker serving subsequent requests. The recovered panic value is
  classified only and never crosses the broker pipe, so connector material in a
  panic cannot be reflected into the agent-loop process. Runtime/README claims
  distinguish the now-active bundle process boundary from the still-in-process
  per-user remote MCP OAuth path tracked by #167.

- **Sandbox acquisition now fails closed at the pool shutdown boundary.** Every
  take path (warm, cold, persistent, lockdown, and allowlisted) returns
  `ErrClosed` after `Pool.Close` marks the pool closed; a concurrent cold start is
  immediately reaped instead of escaping the shutdown boundary. The pool is
  marked closed before its allowlist proxy is stopped, preventing a racing turn
  from receiving a container configured with a dead proxy. A nil pool now
  returns `ErrContainerUnavailable` instead of panicking, and the existing
  keeper/close stress test now exercises the advertised race rather than
  stopping all takers before close.

- **Chat input queues no longer grow forever or drain tied positions
  nondeterministically** (#835). Completed/cancelled rows are now purged after
  `FLEET_INPUT_QUEUE_RETENTION_DAYS` (30 days by default; `0` disables), at boot
  and after turns; pending/running/injected work is never retention-eligible.
  Enqueue and send-now position allocation now serialize per conversation, a
  partial unique index makes non-terminal positions structurally unique,
  migration 046 deterministically normalizes legacy ties (including rows that
  recovery may re-queue), and drain/list queries carry `created_at, id`
  tie-breakers. Fresh enqueue acknowledgements also return the database-assigned
  position instead of an incorrect zero value.

- **Every deployment's shared links unfurled with Elcano's logo** (#893). The
  `og:image` / `twitter:image` was a checked-in `web/public/share.png` containing
  Elcano's logo and wordmark, fleet's purple gradient, and fleet's marketing
  headline — served from the *client's own domain*, so nothing looked amiss to the
  unfurler. Pasting a link to a white-labeled instance into Slack, iMessage,
  Discord, Teams or LinkedIn showed another company's brand. The alt text was that
  same headline, hardcoded, so it leaked even to scrapers that only read
  `og:image:alt`, and to screen readers.

  New `branding.share_image` (bundle-relative, validated at load through the same
  `resolveBrandImage` containment checks as `logo` — lexically local, re-checked
  after `EvalSymlinks`, regular file, known extension), served by
  `/brand/share-image` and proxied as `/api/brand/share-image`. That proxy is in
  the middleware's public-path set out of necessity rather than convenience:
  unfurl scrapers are anonymous, so an `og:image` behind the session gate renders
  no preview at all. `/brand/meta` advertises `share_image_url` only when a file
  actually backed the field, mirroring `logo_url`, so the web never points a
  scraper at a 404. The cap is 5 MiB rather than the logo's 2 MiB, because the
  asset genuinely is larger — but still capped, since scrapers abandon slow
  fetches.

  `og:image:width` / `height` are now declared **only** for fleet's own card,
  whose size fleet knows. fleet does not decode a bundle's image, and a wrongly
  declared size renders a distorted preview; with the tags absent, scrapers fetch
  and measure. The committed default `share.png` is replaced with a **fleet-only**
  card — no Elcano logo or wordmark — so a bundle-less deployment doesn't ship
  another company's branding either, which also removes client-specific content
  from the engine repo per the `AGENTS.md` boundary doctrine.

- **A white-labeled deployment still introduced itself as "Fleet"** (#891, #892,
  #894, #895) — in its browser tab, its tab icon, its login headline, its PWA
  name and splash, its browser-chrome tint, and every shared-link unfurl. Four
  defects with one root cause: the surfaces that most define a deployment's
  identity all render **without a session**, and `/client-config`, where branding
  is served, is member-gated. So each had quietly settled for a fleet default:
  - `branding.login_title` / `login_tagline` were parsed, defaulted, API-served,
    and typed in the web client — then hardcoded in `login-card.tsx` (#892).
  - `branding.share_title` / `share_description` were read by **zero**
    components; `layout.tsx` built its own OG strings from a build-time env var
    nothing in the deploy path set from the bundle (#894).
  - `<meta name="theme-color">` was a pair of fleet-purple literals, and it
    overrides the PWA manifest's `theme_color` in Chrome — so it silently defeated
    `manifest.ts`'s bundle lookup (#895).
  - `app/favicon.ico` was still emitted alongside the bundle-driven
    `metadata.icons` and, being the only candidate carrying `sizes` and `type`,
    won the tab strip. `metadata.icons` overrides `icon.svg` and `apple-icon.png`
    but cannot suppress `favicon.ico`, which Next special-cases (#891).

  All four now resolve from one place: a new token-gated, identity-less
  `/brand/meta` (same trust class as `/theme.css` and `/brand/logo`, which exist
  for exactly this problem), read server-side through
  `web/src/app/lib/serverBranding.ts` — memoized 60s in-process, 2s fetch
  timeout, and it never throws, so branding can never fail a page render.
  `NEXT_PUBLIC_APP_NAME` is demoted to a backend-unreachable fallback.
  `favicon.ico` is deleted (unbranded deployments still get fleet's mark via
  `/api/brand/logo`'s 307), and the PWA's `any`-purpose icon now points at the
  bundle mark while the maskable one stays fleet's correctly-padded asset.

  Two **build-time traps** were found and closed in the process. `manifest.ts`
  was written to read the palette per request and never did once — its route was
  statically prerendered, so the fetch ran during `next build` in a staging dir
  with the backend down, and every deployment silently served the fallback
  (`theme_color: "#1a0b1e"` on a host whose `/api/theme` correctly served
  `#0A0908`). And a root-layout `generateMetadata` is not sufficient alone:
  `/settings/*`, `/no-access` and `/` were prerendered with `<title>Fleet</title>`
  **baked into the HTML artifact**, so only *some* tabs would have been wrong.
  Both are fixed with `force-dynamic` and asserted in tests, since the failure
  mode is silent.

- **Exports downloaded as a file named `export` with no extension** (#896). Both
  proxy funnels — `proxyToOrchestrator` (all 37 `/api/orchestrator/*` routes) and
  `chatServerPassthrough` — re-emitted only `Content-Type` and dropped every other
  upstream header, so the `Content-Disposition` filename four Go handlers set on
  purpose (dataset CSV, prompts, adoption CSV, project export) never reached the
  browser; a bare `download` attribute then names the file after the URL's last
  path segment. Header forwarding now lives once in `web/src/app/lib/proxyHeaders.ts`,
  generalizing the fix `api/conversations/[id]/export` already had by hand.
  `Content-Length` is deliberately not forwarded — `fetch()` decodes a compressed
  upstream body, so a copied length can truncate the response. Both funnels also
  stream `upstream.body` instead of buffering it, which kept whole CSV exports in
  memory and defeated the streaming writers upstream. The project export's filename
  moved into its Go handler (the only one that had none) so it saves as
  `Q3-Planning-a1b2c3d4.json` rather than `project-<uuid>.json`, and
  `exportFilename` now takes its fallback noun as a parameter instead of
  hardcoding `"chat"`.

- **White text was hardcoded on 16 token-driven fills, so a light-primary bundle
  rendered invisible labels** (#890). `on_primary` landed in #889 but only the two
  `--gradient-action-primary` consumers were converted; every flat fill still said
  `text-white`. On the Reklaim deployment (`primary: #FFDF03`) that measured
  **1.33:1** against WCAG AA's 4.5:1 — on the login page's Sign in button, all four
  tool-approval **Approve** buttons, every primary button in Settings, the user
  avatar, and the sidebar's selection tick. Primary fills now use
  `var(--color-on-primary)` (14.87:1 on that palette). Two further fills were
  failing for **fleet's own** stock palette, not just white-labeled ones — white on
  the dark theme's `--color-accent` was 2.28:1 and on `--color-danger` 2.77:1 — and
  now use `var(--color-surface-1)` (5.9–7.2:1), which was already the codebase's
  idiom at three other sites. The bulk-delete button's countdown state takes the
  muted disabled treatment instead of a high-contrast label on a half-alpha fill.
  Root cause was the rule's wording: `DESIGN.md` banned raw *hex* colors, and every
  offender spelled its color as a Tailwind utility, so nothing caught it. The rule
  now bans color utilities too, and `web/src/app/designTokens.test.ts` pins it —
  including a positive check that a primary fill declares `--color-on-primary`,
  which found a 14th site that grepping for `text-white` had missed.

- **A themed deployment still rendered fleet's colors on its most visible
  surfaces.** `branding.colors` themed the flat tokens, but the gradients did not
  read them: `--gradient-bg` (painted on `<body>`), `--sidebar-surface` (the rail),
  the surface/panel/card/composer gradients, and `--gradient-action-primary` were
  hardcoded fleet-purple, as were light-mode agent-link colors and the light
  usage-bar hue. A bundle could set all 18 tokens and the app still read as
  fleet-purple with a client-colored trim. All of them now derive from the palette
  via `color-mix()`; the percentages were fitted against the previous literals for
  fleet's own palette, so the stock appearance is unchanged (every stop within
  ΔRGB ≤ 6). See `docs/BRANDING.md`.
- **A bundle with a light primary had unreadable buttons.** Every rule painting
  `--color-primary` or `--gradient-action-primary` hardcoded white text, so a
  yellow-primary bundle rendered white-on-yellow at 1.33:1 — including the send
  button and the empty-state CTA. New themable `on_primary` token carries the
  foreground for those surfaces (14.87:1 with near-black on yellow), and light
  mode's action gradient now deepens via `--color-primary-hover` rather than
  mixing the brand color toward black.
- **The tab icon and the installed-app splash color ignored the bundle.**
  `icon.svg`/`apple-icon.png` are build-time file-convention assets a bundle cannot
  reach, so every deployment wore fleet's mark in the tab beside its own name; the
  PWA manifest hardcoded a splash color. The favicon now resolves to
  `branding.logo` via `/api/brand/logo` (which redirects to fleet's own mark when a
  bundle declares none, so the link is never dead), and the manifest reads the
  bundle's dark `--color-bg`.
- **An SVG bundle logo rendered as a broken image on every page.** `next/image`
  skips Next's optimizer only when the `src` path literally ends in `.svg`; a
  bundle mark arrives as `/api/brand/logo`, which has no extension, so it was
  rewritten to `/_next/image?url=…` — and that endpoint rejects `image/svg+xml`
  unless `images.dangerouslyAllowSVG` is set, which `next.config.ts` deliberately
  does not set. Every bundle whose `branding.logo` was an SVG therefore wore a
  broken mark in the rail, including the format `docs/BRANDING.md` uses in its own
  example. The rail's `<Image>` is now `unoptimized` (a 28px mark gains nothing
  from the optimizer anyway), and `NavRail.test.tsx` pins it by asserting the
  rendered `src` is the raw path with no generated `srcset`.
- **Bundle branding never actually reached the login page.** The root layout
  links `/api/theme` as a render-blocking stylesheet on every page, and both
  `theme.go` and the proxy route documented it as public — but the path was
  missing from the middleware's public-path set, so a pre-session request 401'd
  and the login page silently fell back to fleet's built-in palette. That is the
  one page every user sees before anything else, and the surface a white-labeled
  deployment cares most about. `/api/theme` and `/api/brand/logo` are now both
  public (deployment-wide, non-secret, and each degrades quietly to empty
  CSS / a 404), pinned by a regression test.

- **Catalog refresh: `gamma`** (`design-media`, provenance `official`, `auth:
  oauth`) — Gamma's hosted MCP at `https://mcp.gamma.app/mcp`: generate decks /
  docs / webpages from a prompt, template, or multi-page input, browse, read and
  export existing gammas (PPTX/PDF), read comments, and pull per-deck engagement
  analytics. Verified before shipping: `initialize` + `tools/list` answer 15
  tools, and the full OAuth chain resolves — RFC 9728 protected-resource
  metadata names `https://auth.gamma.app`, whose RFC 8414 metadata advertises an
  RFC 7591 `registration_endpoint` and PKCE `S256`, so dynamic client
  registration needs no operator-supplied client (no `client_registration:
  manual`). Gamma serves that metadata at the **origin root** only — the
  path-aware location (`/.well-known/oauth-protected-resource/mcp`) 404s — so
  this entry depends on the origin-root fallback added in #878. Scopes are
  `generate` and `gamma:read`; the MCP server is available on all Gamma plans
  and generations charge credits.

- **Bigger uploads with honest failures, plus admin storage visibility and
  cleanup** (`docs/UPLOADS-AND-STORAGE.md`): the per-file upload cap is now
  configurable (`FLEET_UPLOAD_MAX_BYTES`) and defaults to 1 GiB (was a
  hardcoded 256 MiB on chat attachments / 250 MB on task uploads); the chat
  composer learns the limit from `/server-config` and refuses oversize files
  at pick time with a visible explanation instead of a silent post-upload
  413, warns when a queued upload is large, and explains the disabled Send
  button; oversize errors from the server are human-readable 413s in every
  case (including the previous opaque `400 unexpected EOF`). Settings →
  Admin → Server gains a Storage panel — bytes for attachment uploads, task
  uploads, and workspaces, the largest conversation workspaces with owner/
  pinned context, and a "clean up now" action that deletes old unpinned
  chats (pinned/archived/shared/project chats are never touched) and sweeps
  aged files. The orchestrator's `temp_uploads` cleanup (previously dead
  code) and the attachment TTL sweep now also run on an hourly timer, so an
  idle server reclaims disk without waiting for a chat turn.

- **Recurrence end conditions + horizon-based Upcoming**
  (`docs/RECURRENCE-END.md`): recurring tasks can end on a date
  (`recurrence_until`) or after a total number of runs
  (`recurrence_remaining`), exposed in the task modal as "End repeat"; and
  `GET /tasks/upcoming?until=` projects every occurrence inside the window
  instead of capping recurring tasks at their next 5, so the week board no
  longer implies a schedule "ends" five occurrences out.
- **`fleet doctor` — box-level diagnose AND repair** (patterned on chat's
  `chat doctor`; `docs/DOCTOR.md`): a root pass over every box prerequisite —
  toolchain floors, fleet-critical package currency with broken-dnf-repo
  quarantine, the service user's rootless-podman prerequisites (subuid/subgid,
  dir ownership, containers.conf, stale pause namespaces), systemd unit drift
  vs `deploy/`, env-file shape/permissions, service health + `/healthz` +
  `/readyz`, and a sandbox smoke run **as the `fleet` user** — fixing what it
  can in place. `--check` diagnoses only; `--no-restart`; `--dry-run` prints
  the checklist. `doctor` is no longer a bare alias of `status` (the read-only
  contract lives on as `doctor --check`). Admins get a read-only version in
  **Settings → Admin → Doctor** (`GET /admin/doctor`, `internal/boxdoctor`):
  DB pings, disk headroom, podman prerequisites, sandbox image, unit states —
  each failing check annotated with the on-box repair command, plus an
  explicit "Run deep checks" sandbox smoke.

### Changed

- **Icon-button hover polish across chat + orchestrator**: the sidebar's
  green "New chat" hover treatment (colored pill background + tooltip +
  subtle motion-safe scale) is now applied consistently. Add/create actions
  hover green (sealed-chat lock — now the same size and brighter, collapsed-
  rail new chat, orchestrator New task, queue send-now); destructive actions
  hover red on the `--color-status-error-*` tokens (archived trash, bulk
  delete, queue remove, dataset column remove); section chevron rows and the
  log-modal close buttons gained missing hover feedback; and previously
  unlabeled icon buttons (run feedback thumbs, column remove, week nav,
  jump-to-latest) gained design tooltips.
- **Sidebar polish follow-ups (owner review)**: the sealed-chat lock hovers
  neutral instead of green (green stays reserved for the plain "+" adds), the
  conversation and project kebab buttons show a real design tooltip on hover
  instead of the browser-native title, and "Open project" uses the standard
  open/external glyph instead of a bare arrow. Tooltips now shrink-wrap to
  their text instead of always being 14rem wide.
- **Sealed-chat row badge is muted and right-aligned**: the lock marking a
  sealed conversation moved from a leading accent glyph to a quiet muted
  lock at the row's right edge (the retention-note treatment). The
  new-sealed-chat lock button stays in the Chats header next to "+".
- **Pinned and Temporary read as groups, not chats**: both headers are now
  small-caps eyebrow captions with a leading accent icon (pin / clock) — 
  visually distinct from the chat rows beneath them, but one level below
  the Projects/Chats/Labels section headers, matching their hierarchy.
- **One close button everywhere**: every dialog/drawer dismiss "×" (keyboard
  shortcuts — previously a tiny text glyph —, prompt library, save-prompt,
  projects, memories, mobile nav drawer, task create/log/dataset modals) now
  uses one shared CloseButton: same 2rem box, rounding, glyph, and a slight
  red-tint hover. The orphaned `.modal-close` CSS was removed.

### Added

- **"Sign out" on OAuth connectors**: Settings → Connections now separates
  ending an authorization from deleting the connection. Sign out revokes
  (best effort) and drops the stored tokens but keeps the registration —
  including a manually-registered OAuth client's ID/secret — so Connect
  signs back in without re-entering anything. Remove stays the full delete.

### Fixed

- **Remote-MCP OAuth discovery finds path-aware metadata (Google Workspace
  MCP)**: connectors whose resource URL has a path (e.g.
  `gmailmcp.googleapis.com/mcp/v1`) publish RFC 9728 §3.1 metadata at
  `/.well-known/oauth-protected-resource/<path>` and 404 the origin-root
  form fleet probed, so every Google Workspace connect failed at discovery.
  Discovery now tries the path-inserted location first, then the root.
- **OAuth connect no longer strands the browser on localhost**: after a
  successful remote-MCP authorization behind a reverse proxy, the callback
  route redirected to the internal origin (`localhost:3000`) because it built
  the URL from `request.url`; it now uses the forwarded host like every auth
  route (`getRedirectUrl`).
- **Manual OAuth client forms now show the callback URL**: connector setup
  hints (GitHub, Google Workspace, …) told users to register their OAuth app
  with "the callback URL shown in your Fleet instance's OAuth settings", but
  nothing in the UI displayed it. `/mcp-catalog` now returns
  `oauth_redirect_uri` and the guided add form renders it with a copy button.
- **A selection-less scheduled run no longer replays another task's create
  markers**: `bindTaskMCP` returned the **shared per-deployment** MCP workspace
  dir as the #717 reconciliation workdir for any task without an
  `mcp_selection`. That directory's `creates.jsonl` accumulates the markers of
  every task *and* every chat conversation on the box, keyed only by
  `(ssp, deal_name)` with no run attribution — so an unrelated task's
  half-finished create was injected into this task's prompt as "the prior
  process stopped after submitting these creates", and since an abandoned
  `submitted` marker is only ever cleared by a matching resolution in the same
  file, it was replayed into every future run forever. The selection-less path
  now returns no reconciliation workdir (an unattributable ledger is not a
  resume signal); a task that wants real per-run resume semantics declares an
  `mcp_selection` and gets the dedicated client, whose workdir and
  `${FLEET_TASK_ID}` identify exactly one run. When the catalog references
  `${FLEET_TASK_ID}` the run now logs that its per-task ledgers are inert, so a
  missing selection is visible instead of silent. New
  `agentcore.EnvReferencesTaskID`. Same root confusion — a shared directory
  treated as a run identity — as the SendGrid send-once bug fixed in
  elcano-config PR #48, where it made scheduled tasks report emails as sent
  that were never delivered; `docs/MCP-BUNDLE-ENV.md` now states the rule for
  both sides.
- **A sent email now reads as an outcome, not a JSON dump**: `ToolResultView`
  had purpose-built renderers only for `bash` and `task_tracker`, so a
  `send_email` result (built-in or any bundle's `mcp_*_send_email`) fell through
  to the raw `<pre>`. Because the approval gate resolves the call *after* the
  model's turn has ended, the last thing a user saw after clicking **Send** was
  the provider payload — status code, message id, and a paragraph of HTML-lint
  prose about Outlook table borders. It now renders one outcome line (`Queued
  for delivery` / `Already sent` / `Not sent`) with the message id, any
  formatting notes, and the full payload behind a collapsed "Delivery details"
  disclosure. A rejected send is read off the payload's `error`/non-2xx
  `status_code` rather than the tool-level error flag — the server returns a
  failure as a normal result, so trusting `is_err` alone printed "queued" over
  a send that never happened. The pre-approval `APPROVAL_REQUIRED` placeholder
  now says it is waiting for approval instead of showing the raw sentinel.
- **Env-file writes now survive a read round-trip (#834)**: `SetEnvKey` wrote
  values verbatim while both parsers (creds and the server's `loadEnvFile`)
  strip whitespace, surrounding quotes, and ` #`-style inline comments — so a
  secret containing any of those authenticated when saved and came back
  mangled after the next restart. The writer now wraps such values in one
  layer of quotes (which both parsers strip without touching the interior;
  plain API keys stay byte-verbatim on disk), and refuses values containing
  line breaks instead of physically corrupting the file.
- **`turn.retry` events are now actually emitted (#833)**: journal recovery
  and the web client both consumed the event (recovery drops the abandoned
  pre-retry partial text; the client shows an inline "retrying" badge) but no
  code ever produced it — so a crash after a mid-turn provider retry could
  project a garbled mix of pre- and post-retry text into the recovered
  history, and users never saw retries happen. The run loop now emits it
  (RetryEventPayload shape) from fantasy's inner-retry backoff and from every
  resilience re-drive (stream blip, fallback swap, compaction rollback).
- **Chat submit guard reads now fail closed (#832)**: a store error during
  the `input_id` idempotent-replay lookup fell through to start a fresh
  billed turn for an input that may already have been accepted, and on the
  queue path a `CountPendingInputs` error silently waived the
  unattended-spend depth cap (with a lookup error at the cap masquerading as
  a 429 "queue full"). All three reads now surface the error as a 500 so the
  client can retry the same `input_id` safely.
- **Shared MCP spawns no longer leak the literal `${FLEET_TASK_ID}`
  placeholder to connectors (#831)**: only the scheduled per-run path called
  `ExpandTaskIDEnv`, so boot-time shared spawns, hot-reload diffs,
  `BindMCPSelection`, and `fleet mcp test` probes handed a bundle env value
  mapping to the reserved token through as the raw string
  `"${FLEET_TASK_ID}"`. All four paths now drop token-bearing keys (the
  intended no-task-identity fail-safe); the scheduled path, which
  pre-substitutes the real task ID, is unaffected.
- **MCP HTTP clients no longer forward resolved credential headers across
  cross-origin redirects (#830)**: inline HTTP tools and the HTTP MCP
  transport (including the TLS-hardened variant) used Go's default redirect
  policy, which only strips `Authorization`/`Cookie` — a 30x naming a
  different host received every custom credential header (`X-Api-Key`,
  vendor auth headers) verbatim. A shared `CheckRedirect` policy now follows
  redirects but drops all originally-set headers when a hop leaves the
  original host:port origin, mirroring the guarantee
  `mcpoauth.SafeHTTPClient` already gave user-supplied remote servers.
- **Chat web: a stale busy flag no longer turns a `mode:"queue"` submission
  into an invisible billed turn (#824)**: the composer picks the queue path
  from client-side streaming state, but the server only honors queueing while
  a turn is actually running — if the turn finished in the race window, the
  POST started a direct SSE turn that the client mistook for a queue ack:
  no user bubble, no stream consumer, nothing in the queue until a page
  refresh. The queue branch now classifies the response by `Content-Type`;
  a `text/event-stream` answer cancels the unread body (the turn outlives
  its originating request by design) and hands off to `reattachToConv`,
  which attaches to the running turn's buffer and replays from event 0 —
  the `user.message` echo renders the submission's bubble and tokens stream
  normally.

- **Crash recovery preserves steered user messages (#826)**: an interrupted
  turn's recovery rebuilt assistant text, reasoning, and tool calls from the
  event ledger but never projected steered `user.message` events — the
  mid-turn instruction vanished from canonical history while recovery's
  `history_committed_at` stamp made boot queue-recovery mark the steer's row
  completed ("durably in history"). `buildRecoveredEntries` now projects
  steered user messages in stream order, exactly once per `input_id` (a
  resilience re-drive can re-emit the event; the turn-start non-steered
  `user.message` stays excluded — it is committed separately at turn_seq=1).

- **Injected steers are at-most-once, never re-executed after committed side
  effects (#823)**: an injected steer whose turn ended without a history
  commit was blindly re-queued and re-run as the next turn — but the model
  may already have ACTED on it (its committed tool side effects survive the
  failed turn per #820), so "send that email" could execute twice.
  `MarkInputInjected` now stamps the turn journal's max seq as an injection
  watermark (`injected_seq`, migration 044); turn-end settlement and boot
  recovery re-queue the row only when no tool intent was journaled after the
  watermark (the model provably never dispatched with the steer in context)
  and otherwise cancel it with a logged reason — dropping a resendable
  message instead of duplicating a side effect. Rows injected before the
  migration degrade to the coarse gate (any tool intent blocks the requeue).

- **Input-queue hardening (#785 follow-up)**: a per-conversation pending
  depth cap (20, HTTP 429 above it; idempotent replays still answer 200) so a
  retrying client can't bank unbounded unattended LLM turns; a transient
  launch failure of a drained row now schedules a re-kick instead of leaving
  the 202-acknowledged input stalled until the next submission; the Stop
  scope=all epoch gate is strictly-before, so a fresh submission accepted in
  the Stop's own second is no longer silently cancelled; an ambiguous-commit
  replay (`ErrTurnHistoryCommitted`) now settles the turn's injected steer
  rows instead of leaving them listed until the next reboot; queue
  `message_preview` truncates on a rune boundary; stale stop epochs are
  pruned; and turn-scoped queue lookups gained an index (migration 043).

- **Committed tool side effects survive a mid-round provider failure in
  canonical history**: when ADR-0035 suppresses recovery after a tool ran,
  the run's partial transcript was discarded with the error — the executed
  call existed only in the turn journal, was never projected into `messages`
  (the turn seals non-`running`, so recovery skips it), and the retried turn
  could re-execute the side effect blind. `streamErrorResult` now returns the
  partial transcript alongside `ErrCommittedSideEffects`, and the interactive
  manager commits it terminally before surfacing the failure.
- **Data race on a server's tool list during a mid-call stdio restart**:
  `initialize` reassigns `Server.tools` under `Server.mu` while catalog
  readers (`GetAllTools` and friends) hold only `Client.mu` — a torn read
  under concurrent conversations. The slice now has a dedicated RWMutex
  (readers deliberately do not take `Server.mu`, which is held for the whole
  restart including the network round-trip).

- **A lifecycle hook's reason-only trailer no longer downgrades its block**:
  `parseHookDecision` let any later contract-key line replace the verdict, so
  `{"decision":"block",…}` followed by `{"status":"ok","reason":"scan
  complete"}` executed the tool despite `enforce: true` — the fail-open class
  the parser exists to prevent, one key narrower. A line without an explicit
  `decision` key can no longer replace a seen verdict.
- **Turns that fail with `turn.model_required` are sealed as errors, not
  completed**: the engine deliberately emits it instead of `turn.error` (the
  user can fix it by switching models), but `inferTerminalStatus` didn't know
  it — the failed turn was sealed `completed` with `history_committed_at`
  NULL, breaking the turn journal's "gates terminal success" contract and
  hiding it from `RecoverStrandedTurns`.
- **A stale schema-invalid `output_json` candidate no longer blocks every
  lease-checked status write**: the structured-output gate re-validated the
  row's existing value on all updates (lease renewals, error/interrupt
  transitions included), stranding such tasks `running` until lease expiry
  requeued them — a duplicate side-effect window. Validation now applies only
  to output the current update supplies; the success gate still refuses to
  promote a stale candidate.

- **Env-file keys boot actually reads are now allowlisted**: `loadEnvFile`
  silently dropped non-allowlisted keys, and the list was missing ~16 names
  boot reads via `os.Getenv` — including `FLEET_CHAT_DATABASE_URL` /
  `FLEET_SCHED_DATABASE_URL` (documented in `deploy/fleet.service` as env-file
  keys; boot failed with an empty-DSN error despite a correct file) and
  `ADMIN_API_KEY` (admin API rejected everything while the startup warning
  claimed the key was unset).
- **`fleet mcp test` now reads the env file like a real boot**: it loaded the
  bundle without registering connector env-var names or calling
  `config.Load`, so `.env`-only credentials were invisible — credential-gated
  servers were reported as "enable gate is off" or probed with empty creds,
  the exact failure class the verb diagnoses.
- **MCP hot reload is bounded (60s, like boot)**: a stdio server that starts
  but never answers `initialize` blocked SIGHUP reload forever under the
  reload mutexes — wedging all future reloads and leaving the already-published
  new tool gates running against the old catalog indefinitely.
- **`export KEY=…` lines load from the env file** for parity with
  `internal/creds/envfile.go`, whose docs promise the two parsers agree.
- **Spawn-time `${ VAR }` interpolation trims the name** like the load-time
  pass, so a whitespace-padded reference resolves instead of going blank.
- **Editing a task's `carry_context` toggle now persists**: the column was
  missing from `UpdateTaskTx`'s SET list (the `PUT /tasks/{id}` edit path), so
  the toggle was acknowledged with 200 and silently reverted on the next read —
  every later occurrence of the recurring task ran with the stale setting.
- **ask-pause / progress notification emails now carry the message**: the
  `Event.Message` field (for a paused task, the agent's question — the reason
  the notification exists) was documented as "rendered into the email body"
  but never referenced by the text or HTML builders; operators got a
  status-only email with no question. Rendered escaped in both parts.
- **Notification task names clamp at a rune boundary**: the 60-char display
  label was a byte slice, so a multi-byte prompt could put invalid UTF-8 into
  email subjects and the webhook JSON payload (`truncate.Clamp`, the #595
  class).
- **Context-too-large recovery no longer re-executes committed tool side
  effects.** The forced-compact-and-re-drive (and its fallback-model swap) now
  ride the same ADR-0035 side-effect gate as the blip/retry-exhausted paths:
  once a tool ran in the failing attempt — the typical case, since a large
  tool result is what balloons the next request — the round surfaces
  `ErrCommittedSideEffects` so the whole-task RetryPolicy (the operator's
  explicit opt-in) owns the re-run instead of the loop silently repeating a
  send-email-class call in-run. The no-tool-events compaction retry also rolls
  back the failed attempt's partial output first, so the re-driven round
  replaces — not duplicates — it in the transcript.
- **The interactive leaked-tool-call finalize retry now streams under the
  run's ceilings**: the budget guard (pre-completion cost/token check) and the
  `CHAT_MAX_ITERATIONS` step cap are threaded into the retry via
  `FinalizeInput.GuardStep`/`StopWhen` — previously it could keep buying paid
  completions unboundedly, with tool calls only soft-blocked by policy.
- **`edit_file`/`write_file` no longer abort when the destination's ownership
  can't be preserved**: the sandbox executor's `fchown` on overwrite is now
  best-effort — an unprivileged executor overwriting a foreign-owned (e.g.
  host-seeded) workspace file keeps the executor-owned replacement instead of
  failing the whole edit with "cannot preserve destination ownership".
- **`view_file` with `offset` exactly at the file size returns a clean empty
  read** instead of an "offset is beyond file size" error — `offset += limit`
  paging that lands on the size no longer trips a spurious failure.

### Added

- **`fleet mcp test --deep` runs manifest-declared canary probes**
  (`docs/MCP-TESTING.md`): a catalog server may declare `probe:` — ONE
  read-only tool call (tool/args + optional `contains:` substring) the bundle
  author vetted for side effects — and `--deep` executes it after the
  auth-status checks, proving the upstream returns real data rather than just
  accepting the credentials. Fails the run on a not-advertised tool, a call
  error, an `isError` result, or a `contains:` mismatch; servers without a
  probe are noted, never failed. Load-time validation rejects a blank
  `probe.tool` or one outside the server's `tools:` allowlist. The runner
  only ever calls declared probes — it never auto-discovers tools to call.
- **Adoption view — exec per-user AI-usage audit** (`docs/USAGE-ANALYTICS.md`
  part 3): a new admin-only **Adoption** tab in the Operations Center backed
  by `GET /admin/usage/adoption` — per-user token/spend leaderboard with
  daily sparklines, active-day counts and engagement tiers, previous-period
  trend deltas, daily tokens/active-users trends, the provisioned-seat
  roster with a "not yet active" list, and CSV export. Strictly a read model
  over the existing metering (task_iterations ⋈ tasks + chat turn_metrics
  via two new seams); leaderboard sorts token volume first because tokens
  are the pricing-coverage-independent meter (#289), and the report carries
  the caveat that token volume is an adoption signal, not a performance
  grade. The Operations Center tabs now accept a `?tab=` deep link, and the
  Settings → Admin → Users/Overview pages label their numbers as all-time
  chat-only ("Chat spend") and cross-link to the windowed Usage/Adoption
  views instead of appearing to duplicate them.
- **Input queue + mid-turn steering** (#785, `docs/INPUT-QUEUE.md`): a
  submission during an active turn now QUEUES durably (stable ids, idempotent
  `input_id` replay, FIFO drain as separate turns) instead of implicitly
  cancelling the running turn — explicit Stop is the only cancellation, and
  it now covers queued follow-ups too (`scope=all` default). Optional
  `mode=steer` injects the message at the next safe step boundary inside the
  same governed loop: budget-accounted, cache-friendly append, durable
  queued→injected flip before model visibility, persisted exactly once via
  the #798 terminal commit, with a queued-turn fallback when the turn ends
  first. Queue chips under the composer (remove / send-now) ride the new
  `queue.updated` SSE snapshots. Migration 042.

- **Durable turn journal + commit-gated terminal success** (#798,
  `docs/TURN-JOURNAL.md`, ADR-0039): interactive turns now preserve the causal
  chain user input → tool intent → governed tool outcome → conclusion
  durably, BEFORE terminal success is advertised. The user message commits to
  canonical history before the first provider call; every tool route journals
  its call intent before dispatch (fail closed — no side effect without a
  durable record) and its exact governed model-visible result before the next
  provider step (the 4 KB SSE preview is never the persistence limit);
  `turn.completed`/`turn.cancelled` gate on the transactional history commit,
  so a failed write is a visible turn error instead of a completed answer
  that disappears on reload. Startup recovery projects crashed turns into one
  explicit interrupted turn with provider-valid call/result pairing —
  unmatched calls get a synthesized unknown-outcome error marked for
  reconciliation, so the next model verifies instead of silently repeating a
  possibly side-effectful call. Migration 041.

- **Governed lifecycle hooks** (#788, `docs/HOOKS.md`, ADR-0038): a client
  bundle can declare `hooks:` — commands run at fixed run lifecycle points
  (`user_prompt_submit`, `pre_tool_use`, `post_tool_use`, `turn_end`) **inside
  the per-turn sandbox**. A hook receives a bounded, redacted, credential-free
  JSON payload on stdin and prints a JSON decision (continue / block-with-reason
  / bounded additional-context). Hooks can only observe or NARROW — fleet's
  existing policy/approval/audit gates evaluate after them on the same
  unmodified input, so a hook can never widen authority, add tools, or grant
  network/budget/approval. Enforcing hooks fail closed; advisory hooks fail
  observable; every invocation emits a `hook.decision` audit event. The generic
  bundle ships none (zero-overhead default). Both interactive and scheduled runs
  invoke hooks through the one governed core.

- **Save a chat to the prompt library**: a per-conversation kebab action
  ("Save to prompt library…") distills a conversation into a reusable
  prompt-library draft. `POST /conversations/{id}/suggest-prompt` (chat server)
  renders the user/assistant transcript and calls the host-side synthesizer
  `SuggestLibraryPrompt` (`FLEET_LIBRARY_PROMPT_MODEL`, defaulting to the
  metadata/title model) to write a clean, self-contained prompt plus a short
  name and description. Nothing is persisted server-side: the client opens the
  draft in an editable review dialog, and saving goes through the orchestrator's
  existing `POST /prompts` (private by default, optionally workspace-shared), so
  library permissions apply unchanged.
- **Safer `edit_file`** (#787, `docs/FILE-EDIT-SAFETY.md`): `old_text` must now
  match exactly one location unless `replace_all` is set (an ambiguous match is
  rejected with the count instead of silently editing the first occurrence);
  no-op edits are rejected; an optional `expected_hash` (SHA-256 from
  `view_file`) fails the edit safely if the file changed since it was read; and
  a successful edit returns a bounded unified diff plus old/new content hashes.
  `view_file` and `write_file` now report the content SHA-256 so the model can
  round-trip it as `expected_hash`.
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

- **File tools (`view_file`/`write_file`/`edit_file`) now execute inside the
  sandbox** (#784, ADR-0036). They previously ran `os.ReadFile`/`os.WriteFile`
  directly in the Fleet host process, contradicting the ADR-0002 "every tool
  call runs in the sandbox" invariant — a configured Kata/libkrun runtime,
  seccomp, dropped caps, cgroups, and disk/PID limits did not apply to them.
  The three tools now dispatch read/write/edit through a new sandbox FileOp
  seam (`Sandbox.RunFileOp`) that runs a one-shot `python3` in the same
  per-turn container as bash/run_python, inheriting the full isolation posture;
  the executor also confines each call to a narrow conversation/worktree root
  with a boot-bound root identity and dirfd-relative no-follow traversal,
  defeating both interior symlink swaps and whole conversation-directory
  exchanges against the shared workspace mount. Atomic replacements preserve
  existing modes and fsync the parent; cancellation synchronously kills and
  retires the container so a helper cannot land a late rename. Host-side path
  validation stays as defense-in-depth input, and there is no host-execution
  fallback (the tools fail closed without a sandbox). Scheduled non-worktree
  turns now resolve relative tools against that same workspace root. The
  existing host-temp truncation spill remains an honestly documented temporary
  exception pending #793's governed artifact lifecycle. ADR-0036 also documents
  the
  remaining host-side control-plane/broker tools (network fetch, brokered
  credentials, governed datastore writes) as explicit, threat-modelled
  exceptions rather than a silent contradiction of the invariant.

- **Bounded model-visible tool output**
  ([docs/TOOL-OUTPUT-BOUNDARY.md](docs/TOOL-OUTPUT-BOUNDARY.md)): every native,
  loader, direct/deferred MCP, and media result now crosses a
  non-disableable 128 KiB hard boundary with valid structured envelopes,
  binary suppression, and dedicated metrics. Governed full-output recovery now
  writes only through the confined sandbox FileOp seam into 16 immutable,
  8-MiB workspace slots for private conversations and isolated worktree tasks;
  shared-root non-worktree tasks remain cap-only because they do not own the
  root. The old host `/tmp` bash/Python/web-fetch spills and 6 MiB
  overflow-file pass are gone. Fantasy's inner PrepareStep now budgets
  accumulated tool results and call inputs against the active model window
  while reserving the system prompt, tool schemas, completion, and provider
  framing.

- **Removed the stale Projects button from the collapsed sidebar**: the ≥sm
  icon strip carried a standalone briefcase button that opened the old
  Projects modal — leftover from before the rail's own Projects section
  (#509) became the entry point. Dropped the button and its `onOpenProjects`
  wiring; creating, opening, and pinning projects from the expanded rail is
  unchanged.


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

### Changed

- **Ops Center loads without the markdown pipeline**: the /orchestrator
  bundle statically shipped react-markdown (~43 KiB transfer) for the task-log
  modal only. `LogMarkdown` (renderer + workspace img/a rewrites, moved
  verbatim from LogViewer) is now lazy-loaded when a log modal actually opens,
  with a raw-text fallback until the chunk arrives — the same split #757 gave
  chat. Note: the ~13 KiB of legacy polyfills Lighthouse flags in the main
  chunk is Next's own baseline polyfill module, injected regardless of
  browserslist targets — verified by building with modern targets — so there
  is no supported way to drop it and no config was added for it.
- **Faster first paint on mobile (Lighthouse)**: the initial /chat bundle no
  longer ships the syntax highlighter (~75 KiB transfer; now lazy-loaded on
  first tool-chip expand with a plain `<pre>` fallback) or the ReactMarkdown
  pipeline (~43 KiB; lazy-loaded when a transcript message renders, showing
  raw text until the chunk arrives), and cold boot skips the serial
  `/api/session` round-trip by reusing the session the /chat server component
  already resolved. Together: ~40% fewer JS bytes and one less blocking RTT
  before the largest contentful paint.

### Fixed

- **A panic in any tool no longer crashes the whole fleet process**
  ([ADR-0037](docs/adr/0037-agent-tool-panic-containment.md), #795).
  fantasy runs streamed tool calls in unsupervised goroutines with no
  `recover`, so a panic in a tool (native/loader/MCP), a policy gate,
  an output guardrail, or an Observer callback would take the entire
  single-host process down — beyond the reach of the runner/httpapi
  goroutine-local recovers. Every tool fleet registers is now wrapped in an
  outermost panic-containment layer: a panic becomes exactly one in-band tool
  error result (paired to the call so the run continues), is recorded to
  PanicCounts/Sentry/`panic_events` with tool + boundary attribution, and is
  surfaced to the model as a stable incident id with a "possibly executed"
  warning. Raw recovered values and stacks are discarded before telemetry;
  only an opaque incident and value-free class are retained. Before-call,
  execution, and output panics receive exactly one failed logical-tool policy record, including
  deferred MCP. Observer panics are recorded and disabled, then surface as an
  ordinary run error only after Fantasy's tool goroutines settle.
  Tool-supplied Go errors are now flattened under the same boundary, screened
  and bounded before Fantasy sees them, so credential-bearing or panicking
  `Error()` methods cannot leak or break transcript pairing.
  The unused arbitrary `PreGatedTools` bypass was removed because a black-box
  tool that owned policy accounting internally made exactly-once recovery
  impossible to prove. Fleet now owns policy accounting for every externally
  supplied tool route.

- **Cancelled or timed-out bash no longer keeps running inside the sandbox**
  (#796). Cancellation used to kill only the host-side `podman exec` client;
  the in-container process tree survived — durable in persistent-REPL mode,
  where the next turn could share the container with a stopped turn's
  stragglers still mutating files or completing external side effects. On
  cancel/timeout the container is now SIGKILLed synchronously — tearing down
  its PID namespace kills every descendant, including a `setsid` daemon or
  backgrounded grandchild that escaped the process group — and the sandbox is
  poisoned so the persistent pool retires it and the next turn gets a fresh
  container. The tool result distinguishes timeout from cancellation and says
  when the sandbox was reset.
- **Declared structured output now fails closed.** A scheduled task with
  `output_schema` can reach success only when schema-valid `output_json` is
  committed in the same lease-checked transaction. The active OpenRouter model
  uses strict native schema mode; other providers use a forced terminal schema
  tool with two correction attempts and no ordinary tools. Format and
  persistence failures have distinct retry/DLQ classes, and success
  notifications/recurrence happen only after the output is durable.
- **Enforcement rounds no longer restart the task from scratch.** When the
  policy blocked a finish (audit not confirmed, critical actions pending),
  the next round's input carried only the original prompt plus the nudge —
  the blocked round's assistant/tool transcript was dropped, so the model
  re-ran its entire analysis on every nudge (observed as a 31-minute
  scheduled run doing its full pipeline twice). The round transcript is now
  carried forward (reasoning stripped, tool call/result pairing preserved)
  so a nudged round continues the work instead of repeating it.
- **Orchestrator (scheduled) runs no longer offer the interactive
  staging-card tools** (`preview_email`, `schedule_task`,
  `suggest_advanced_model`, `propose_memory`). Only the interactive chat
  orchestration guard gives them behavior; headless, three of them are
  mis-wiring tripwires whose non-nil error is fatal to the run — a scheduled
  agent that finished its report and called `preview_email` to present it
  dead-lettered the entire task. The docs always said these were
  interactive-only; the scheduled roster now actually excludes them.
- **`publish_artifact` works for non-worktree scheduled tasks.** It resolved
  paths only via the worktree-isolation working dir, so every task without
  worktree isolation got "no workspace is configured for this run" on every
  publish attempt. The tool now falls back to the run's effective workspace
  root (the same directory the workspace file browser serves); worktree runs
  keep their narrower scoping.
- **A transient mid-stream provider failure no longer dead-letters a whole
  run once any text has streamed** (ADR-0035). The ADR-0033 commitment guard
  suppressed the in-place stream-blip retry and the fallback chain after any
  semantic event, so a single 504 while the model composed its answer killed
  the task as `non_retryable`. Recovery is now gated on tool side effects:
  a text/reasoning-only attempt rolls its partial output back and re-drives
  (in-place retry, then fallback); an attempt that already executed a tool
  still suppresses in-run recovery but surfaces the transient
  `ErrCommittedSideEffects` sentinel so the task's RetryPolicy — not a
  deterministic-failure dead-letter — decides the re-run.
- Deferred MCP tools now advertise `arguments` as a JSON object instead of a
  byte array, preventing models from repeatedly sending array-wrapped arguments
  that every nested tool rejects. Live scheduled-run activity now keeps the
  task plan in a compact expandable progress panel, groups repeated failures,
  and persists structured tool calls/results so terminal logs retain the real
  cause of an incomplete run.
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
