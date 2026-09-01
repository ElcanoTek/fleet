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
| [0001](0001-one-governed-run-loop.md) | One governed agent run loop | Accepted |
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
| [0012](0012-unified-fleet-cli.md) | One `fleet` binary — `serve` plus the operator CLI (back-compat preserved) | Accepted |
| [0013](0013-team-rbac.md) | Team RBAC — roles + opt-in, team-scoped conversation reads | Accepted |
| [0014](0014-oidc-sso-in-nextjs.md) | OIDC / OAuth2 SSO lives in the Next.js layer, not the chat server | Accepted |
| [0015](0015-remote-mcp-tls-pinning-mtls.md) | TLS pinning and mTLS for remote MCP servers | Accepted |
| [0032](0032-host-side-ingress-guardrails.md) | Host-side untrusted-ingress guardrails | Accepted |
| [0033](0033-cross-provider-failover.md) | Cross-provider failover before stream commitment | Accepted |
| [0034](0034-audit-gate-commitment-binding.md) | Audit-gate commitment binding, payload-level failure, and create reconciliation | Accepted |
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
| [0049](0049-kubernetes-backend-split-control-plane.md) | Kubernetes as a first-class deployment — split control plane, pluggable sandbox backend | Accepted |
| [0050](0050-remote-mcp-seats.md) | Multiple logins per hosted MCP connection ("seats") | Accepted; amends ADR-0009 |
| [0051](0051-a2a-protocol-server.md) | An A2A protocol server as a translation over the governed task seam | Accepted |
| [0052](0052-operations-always-on-mcp-selection.md) | Operations MCP selections add optional servers without replacing always-on servers | Accepted |
| [0053](0053-public-api-through-the-tls-front.md) | The public HTTP API is served through the TLS front, with the header-trust channel stripped | Accepted; amends ADR-0004 |
