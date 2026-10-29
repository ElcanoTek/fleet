# Architecture Decision Records

This directory records the **load-bearing decisions** behind fleet — the ones a
contributor (human or AI) must understand before changing the affected
subsystem, and the ones that are easy to violate by accident because the
*reasoning* would otherwise live only in scattered code comments.

These ADRs do not invent new policy. They write down decisions that are
**already made and already enforced in code**, so the rationale is diff-able,
reviewable, and citable. Each record names the file or test that enforces it.

## Convention

- One decision per file: `NNNN-short-kebab-title.md`, numbered sequentially.
- Start from [`0000-template.md`](0000-template.md).
- A record is immutable once `Accepted`. To change a decision, add a **new**
  ADR that supersedes the old one and flip the old one's status to
  `Superseded by ADR-NNNN`.
- **If your change adds, weakens, or reverses one of these invariants, it must
  add or supersede an ADR in the same PR.** A change that contradicts an
  `Accepted` ADR without superseding it is wrong even if the tests pass — see
  the "Honesty in docs" and "do NOT weaken these" invariants in
  [`../../AGENTS.md`](../../AGENTS.md).

## Index

| ADR | Title | Status |
| --- | --- | --- |
| [0001](0001-one-governed-run-loop.md) | One governed agent run loop | Accepted; amended by ADR-0072 |
| [0002](0002-mandatory-rootless-podman-sandbox.md) | Mandatory rootless-Podman sandbox; host executor never ships | Accepted |
| [0003](0003-host-side-mcp-credential-brokering.md) | Host-side MCP credential brokering | Accepted |
| [0004](0004-single-box-vm-native-deployment.md) | Single-box, VM-native deployment (no Kubernetes) | Accepted; amended by ADR-0049 |
| [0005](0005-separate-chat-and-sched-databases.md) | Separate Postgres databases for chat and sched | Accepted |
| [0006](0006-external-client-config-bundle.md) | Client content lives in an external config bundle | Accepted |
| [0007](0007-governed-sub-agents.md) | Governed sub-agents spawn only through the one run loop | Accepted |
| [0008](0008-persistent-python-repl-per-conversation.md) | Opt-in persistent Python REPL is scoped per-conversation | Accepted |
| [0009](0009-per-user-remote-mcp-oauth.md) | Per-user remote MCP servers via OAuth | Superseded by ADR-0040 |
| [0010](0010-microvm-sandbox-runtimes.md) | microVM sandbox runtimes (Kata / libkrun) via a fail-closed `--runtime` selector | Accepted |
| [0011](0011-remove-worker-node-registry.md) | Remove the worker-node registry; the in-process worker is the only runner | Accepted |
| [0012](0012-unified-fleet-cli.md) | One `fleet` binary — `serve` plus the operator CLI (back-compat preserved) | Accepted; completed by ADR-0060 |
| [0013](0013-team-rbac.md) | Team RBAC — roles + opt-in, team-scoped conversation reads | Accepted; amended by ADR-0057 |
| [0014](0014-oidc-sso-in-nextjs.md) | OIDC / OAuth2 SSO lives in the Next.js layer, not the chat server | Accepted; amended by ADR-0091 (the web tier, and so OIDC, is Go: `internal/webui`) |
| [0015](0015-remote-mcp-tls-pinning-mtls.md) | TLS pinning and mTLS for remote MCP servers | Accepted |
| [0032](0032-host-side-ingress-guardrails.md) | Host-side untrusted-ingress guardrails | Accepted |
| [0033](0033-cross-provider-failover.md) | Cross-provider failover before stream commitment | Accepted |
| [0034](0034-audit-gate-commitment-binding.md) | Audit-gate commitment binding, payload-level failure, and create reconciliation | Accepted; amended by ADR-0071, ADR-0081 |
| [0035](0035-side-effect-gated-stream-recovery.md) | Side-effect-gated recovery after stream commitment | Accepted |
| [0036](0036-sandboxed-file-tools-and-host-io-exceptions.md) | Sandboxed file tools, and the host-side I/O exception classes | Accepted |
| [0037](0037-agent-tool-panic-containment.md) | Contain panics at the AgentTool dispatch boundary | Accepted |
| [0038](0038-governed-lifecycle-hooks.md) | Governed lifecycle hooks (bundle-declared, sandbox-executed) | Accepted |
| [0039](0039-durable-turn-journal.md) | Durable turn journal gates interactive terminal success | Accepted |
| [0040](0040-child-owned-remote-mcp-runtime.md) | Child-owned per-user remote MCP runtime | Accepted |
| [0041](0041-mandatory-session-epoch-claim.md) | Session cookies carry a mandatory session-epoch claim | Accepted |
| [0042](0042-child-side-mcp-scope-authorization.md) | Child-side MCP scope authorization (the broker enforces, not just transports) | Accepted |
| [0043](0043-per-task-run-log-scoping.md) | Run-log transcripts are creator-scoped; fleet-wide reads need an explicit grant | Accepted |
| [0044](0044-remove-in-sandbox-browser-tool.md) | Remove the in-sandbox browser tool; browser automation is a connector | Accepted |
| [0045](0045-remove-node-name-scopes.md) | Remove node-name scopes; a principal's authority is its permission set | Accepted |
| [0046](0046-remove-per-key-spending-caps.md) | Remove per-API-key spending caps; rolling budgets are the one spend gate | Accepted |
| [0047](0047-self-serve-team-membership.md) | Self-serve team membership — create/leave is yours, joining is granted | Accepted |
| [0049](0049-kubernetes-backend-split-control-plane.md) | Kubernetes as a first-class deployment — split control plane, pluggable sandbox backend | Accepted; amended by ADR-0055 |
| [0050](0050-remote-mcp-seats.md) | Multiple logins per hosted MCP connection ("seats") | Accepted; amends ADR-0009 |
| [0051](0051-a2a-protocol-server.md) | An A2A protocol server as a translation over the governed task seam | Accepted |
| [0052](0052-operations-always-on-mcp-selection.md) | Operations MCP selections add optional servers without replacing always-on servers | Accepted |
| [0053](0053-public-api-through-the-tls-front.md) | The public HTTP API is served through the TLS front, with the header-trust channel stripped | Accepted; amends ADR-0004 |
| [0054](0054-agent-plugins.md) | Agent Plugins load as bundle content, translated onto the existing skills + MCP primitives | Accepted |
| [0055](0055-kubernetes-skills-staged-into-the-workspace-claim.md) | On the kubernetes backend the skills tree is staged into the workspace claim | Accepted; amends ADR-0049 |
| [0056](0056-a2a-outbound-delegation.md) | Outbound A2A delegation as bundle-declared tools over the MCP seam | Accepted |
| [0057](0057-team-shared-chats-live-in-team-shared-projects.md) | A team-shared chat lives inside a team-shared project | Accepted; amends ADR-0013; transcript-only rule superseded by ADR-0079 |
| [0058](0058-per-conversation-attachment-scoping.md) | Attachments are scoped per conversation; the uploads tree is mounted into no sandbox | Accepted |
| [0059](0059-date-based-rolling-releases.md) | Date-based rolling releases; every green push to `main` is tagged automatically | Accepted; re-anchors ADR-0012 |
| [0060](0060-remove-the-fleet-admin-shim.md) | Remove the `fleet-admin` shim, and evict it from the boxes that have it | Accepted; completes ADR-0012 |
| [0061](0061-retire-the-changelog.md) | Retire `CHANGELOG.md`; the PR is the record and the release notes are generated | Accepted; extends ADR-0059 |
| [0062](0062-trunk-based-development.md) | Trunk-based development: `main` is the only branch, every PR squash-merges into it, one CI lane | Accepted; amends ADR-0059 and ADR-0061 |
| [0063](0063-remove-the-rampart-pii-engine.md) | Remove the Rampart PII engine, its one-click installer and the `scripts/rampart-service` npm tree; PII redaction keeps the built-in pattern engine | Accepted; amends ADR-0028 and ADR-0036 |
| [0064](0064-application-session-lifetimes.md) | Fleet sessions live one day, idle out after twelve hours, and are re-minted on activity | Accepted; amends ADR-0041 |
| [0065](0065-completed-step-stream-recovery.md) | Resume provider failures from completed steps | Accepted; narrows ADR-0035 |
| [0066](0066-mcp-workspace-file-arguments.md) | Hash-bound sandbox workspace references for MCP binary arguments | Accepted; amended by ADR-0088 |
| [0067](0067-fallback-on-provider-rejection.md) | A per-request 4xx provider rejection promotes the configured fallback model from a safe checkpoint; the relayed upstream cause is named in logs and dead-letter reasons | Accepted; narrows ADR-0033 |
| [0068](0068-chat-scheduled-tasks-inherit-connectors.md) | A task scheduled from chat inherits the conversation's connector selection; the card names it and warns when there is none | Accepted; amends ADR-0052 |
| [0069](0069-per-job-scheduled-workspaces.md) | A non-worktree scheduled run works in `<workspace-root>/tasks/<lineage_id>/` — one directory per job, carried to occurrences, re-runs and clones — instead of the shared root | Accepted |
| [0070](0070-dead-lettered-recurrences-spawn-successor.md) | A dead-lettered recurring occurrence spawns its successor; two consecutive dead-letters park the chain | Accepted; amended by ADR-0073 and ADR-0077 |
| [0071](0071-critical-tool-aliases.md) | Bundle-declared critical tool aliases are one commitment (amends ADR-0034) | Accepted |
| [0072](0072-deterministic-completion-predicate.md) | A declared completion predicate may replace the end-of-run verifier; a verifier outage does not dead-letter an audited run (amends ADR-0001) | Accepted |
| [0073](0073-malformed-requirements-park-on-first-dead-letter.md) | A malformed EXECUTION REQUIREMENTS declaration is refused at save time and parks its chain on the first dead-letter (amends ADR-0070) | Accepted |
| [0074](0074-central-auth-owns-fleet-membership-state.md) | Central Auth owns Fleet membership desired state | Accepted; amended by ADR-0076 |
| [0075](0075-vendor-answers-cross-the-mcp-broker.md) | A hosted vendor's own answer to a failed tool call crosses the MCP broker, bounded and scrubbed (amends the error clause of ADR-0040) | Accepted |
| [0076](0076-fleet-publishes-account-events.md) | Fleet publishes signed account events; for centrally managed identities the most recent change wins (amends ADR-0074; amended for teams 2026-09-30) | Accepted; amended by ADR-0095 (one way, Auth to Fleet, where Fleet signs in through Auth) |
| [0077](0077-connector-outage-is-not-a-recurrence-strike.md) | A dead-letter caused by a declared connector that failed to connect transiently is re-run first and never counts toward the recurrence park breaker (amends ADR-0070) | Accepted |
| [0078](0078-track-the-latest-node-major.md) | Track the latest node major (retires "LTS, never Current"); install the signed nodejs.org build when the distro has no `nodejs<major>` stream | Accepted |
| [0079](0079-a-team-share-carries-the-chats-outputs.md) | A team share carries the chat's outputs (files the agent presented, minus the owner's exclusions), served through a per-request gated, no-symlink team-files route; a teammate's branch copies them; archive unshares (supersedes ADR-0057's transcript-only rule) | Accepted |
| [0080](0080-generative-ui-cards-are-declarative-data.md) | Generative-UI cards (`show_ui`) are declarative data over a fixed component catalog, validated server-side and rendered by fleet; a card never acts — its submit is an ordinary user turn, so every resulting action passes the governed loop; interactive chat only | Accepted; amended by ADR-0086 |
| [0081](0081-email-last-batches-settle-definitive-create-failures.md) | Bundle-declared email-last batches: a definitive create failure settles its commitment for the summary email (and for finish after it), an ambiguous one never does, and an abort still sends the single summary email (amends ADR-0034) | Accepted |
| [0082](0082-approval-card-describers-run-a-declared-read-at-staging.md) | Approval-card describers: at staging, fleet may call one bundle-declared parallel-safe, non-critical read on the same server through the turn's own scope, bounded and best-effort, and use its output only as validated display data on the approval card | Accepted |
| [0083](0083-a-settled-approval-card-may-start-a-turn.md) | A settled approval card of a bundle-opted-in critical tool may start one chat turn, through the input queue (same governance, at most once per card, hourly cap, never after a restart, its input labelled as fleet's); amends "turns start from user input" | Accepted |
| [0084](0084-a-settled-card-reads-its-outcome-from-the-calls-own-result.md) | A settled approval card of a bundle-opted-in critical tool may render a strictly validated `result_card` read from the approved call's own governed result (no second call); outcome/row/retry consistency enforced; served only on a recorded success; retry is a user turn | Accepted |
| [0085](0085-interactive-hidden-tools.md) | A bundle may hide MCP tools from interactive chat only (`agent_policy.interactive_hidden_tools`): driver-supplied data on the chat turn's `RunConfig`, a parent-side narrowing that drops the tool before disclosure and refuses it at the call gate, inherited by sub-agents; scheduled runs unchanged | Accepted |
| [0086](0086-show-ui-card-by-reference.md) | `show_ui` may show a card a tool built by reference (`from_result`) to a successful tool result the model can see in this conversation; the spec is validated exactly as an inline card and rides the `show_ui` result as display data (`card`), persisted for reload, never replayed to the model (amends ADR-0080) | Accepted |
| [0087](0087-interactive-mcp-scopes-carry-the-signed-in-user.md) | An interactive chat turn's MCP scope carries the authenticated sender to the credential owner as data (`ScopeSpec.User`), substituted for the reserved `${FLEET_USER_EMAIL}` bundle env token (there is no name token: `${FLEET_USER_NAME}` is reserved and refused, since sign-in carries no name and none is derived from the email); every other spawn path (scheduled, webhook, shared, probe) drops those keys; the identity authorizes nothing (ADR-0042 unchanged) | Accepted |
| [0088](0088-mcp-workspace-file-cap-and-windowed-reads.md) | MCP workspace-file references: 64 MiB whole file, 8 MiB per reference per call; Fleet hashes the whole file host-side once per reference and slices windows from verified bytes; one 384 MiB process-wide budget charges every byte held (in-flight reads, cached files, each window's base64 and dispatch copies) from allocation until the dispatch lets go, with one admission per call; the sandbox helper streams reads and runs in Python isolated mode | Accepted |
| [0089](0089-an-approval-may-narrow-a-declared-list-argument.md) | An approval of a bundle-opted-in critical tool may narrow its ONE declared list argument to a non-empty subset of the staged values (staged order, nothing else changed), recorded in the claim, re-checked before the run, told to the server in `_meta["fleet/narrowed"]` and to the model in a labelled line | Accepted |
| [0090](0090-a-person-may-stop-a-running-approved-call.md) | A person may stop a running approved call of a bundle-opted-in tool: the request is marked `fleet/cooperative_cancel`, Stop sends `notifications/cancelled` and fleet keeps waiting for the answer under the call's budget (a deliberate, announced deviation from the spec's ignore-the-answer); the stop is recorded on the row and told to the model | Accepted |
| [0091](0091-go-serves-the-browser-tier.md) | The fleet binary serves the browser tier: `internal/webui` serves every `/api/*` route in-process (auth gate, CSRF, OIDC, CSP, SSE, uploads) on `FLEET_WEB_ADDR` (127.0.0.1:3000), and the web UI is a Vite + React SPA embedded with `go:embed`; the Next.js tier (`fleet-web.service`, the Helm `web` Deployment) is retired, with `fleet update`/`fleet doctor` migrating its keys and removing its unit | Accepted |
| [0092](0092-blocked-is-a-notification-status.md) | A run that recorded a blocked outcome notifies as `blocked` (with its reason), and an On filter naming `failure` also fires for it; the third blocked run in a row is named in the message instead of being escalated to `failure` | Accepted |
| [0093](0093-team-shares-name-their-audience-by-team-id.md) | A team share names its audience by a surrogate team ID (`teams`, `conversations.team_shared_with_id`) as well as the label; every read gate requires the id; `RenameTeam` carries the id (a merge re-points it); joining a name nobody holds retires a leftover row so a new team under an old name inherits nothing; migration 084 backfills ids and unshares rename orphans (amends ADR-0057) | Accepted |
| [0094](0094-unattended-runs-host-fetch-follows-the-sandbox.md) | In scheduled runs `web_fetch` is registered only when the sandbox has egress (`allow_network`, not lockdown) and is held to the egress allowlist in allowlisted mode; `agent_policy.unattended_disabled_tools` may remove `create_task`, `web_fetch`, `download_url` from every scheduled run; chat unchanged (amends ADR-0036) | Accepted |
| [0095](0095-central-auth-fleets-have-no-passwords.md) | Configuring central Auth SSO (`FLEET_OIDC_*`) turns Fleet's own passwords off in the gateway, chat-server and the Operations Center, including cookies and bearers already issued from a password; Auth also manages accounts, roles and teams (Users and Team pages view-only, their writes refused, Auth team sync required; erasing data stays `fleet admin rm`; membership flows one way, Auth to Fleet, amending ADR-0076); decided once at startup from configuration, never from Auth's availability; `FLEET_OIDC_ALLOW_PASSWORD=1` is the break-glass for passwords only | Accepted |
