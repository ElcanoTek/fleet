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
| [0004](0004-single-box-vm-native-deployment.md) | Single-box, VM-native deployment (no Kubernetes) | Accepted |
| [0005](0005-separate-chat-and-sched-databases.md) | Separate Postgres databases for chat and sched | Accepted |
| [0006](0006-external-client-config-bundle.md) | Client content lives in an external config bundle | Accepted |
| [0007](0007-governed-sub-agents.md) | Governed sub-agents spawn only through the one run loop | Accepted |
