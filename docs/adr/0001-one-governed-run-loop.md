# ADR-0001: One governed agent run loop

- **Status:** Accepted
- **Date:** 2026-06-28 (documents a decision that predates this record)
- **Deciders:** fleet maintainers

## Context

fleet serves two surfaces from one process: interactive chat and the
scheduling engine. The tempting shape is two code paths — a rich, permissive
interactive loop and a separate "headless" loop for scheduled runs. That shape
rots predictably: policy, cost/token ceilings, audit, and the end-of-run
verifier drift apart, and the scheduled path quietly becomes the weaker,
less-governed one — exactly the path that runs unattended.

## Decision

There is **one** governed run loop: `agentcore.Run`
(`internal/agentcore`). Interactive vs. scheduled behaviour is **data**, not a
second code path — it is expressed through the `Policy` seam
(`InteractivePolicy` vs `ScheduledPolicy`) and the `RunConfig`/`Deps` fields
that the loop reads. New entrypoints (HTTP, scheduler, a future channel
adapter) **adapt their I/O around this loop**; they do not fork it.

The end-of-run verifier is part of this single governed path, not a second
agent: it is a host-side fallback-model call
(`(*Agent).runEndOfRunVerifier`, `internal/agent/verifier.go`), wired in at the
scheduled policy's finish check (`internal/agent/scheduled.go`). It reviews and
can require one more enforcement round — it does **not** spawn an ungoverned
sub-agent.

## Enforcement

- `TestSeamPurity_NoModeBranchInTrunk`
  (`internal/agentcore/mode_parity_test.go:154`) fails the build if the trunk of
  the run loop branches on mode instead of going through the `Policy` seam.
- The package comments in `internal/agentcore` explain *why* each governance
  invariant holds; preserve that level of explanation when extending them
  (see [`../../AGENTS.md`](../../AGENTS.md), "Governance is one core").

## Consequences

- Any cost ceiling, audit hook, or safety gate added to the loop applies to
  **both** chat and scheduled runs automatically — there is no second place to
  forget.
- Features that feel like "a different kind of agent" (sub-agents, review
  agents, channel bots) must be expressed as configuration of, or adapters
  around, `agentcore.Run` — not as a parallel loop. This constrains how
  issues like sub-agent support (#175) may be built.
- Mode-specific behaviour costs a little more up front (it must be modelled as a
  `Policy`/config value) in exchange for never having a weaker governance path.

## Alternatives considered

- **Two loops (interactive + headless).** Rejected: guarantees governance
  drift, and the unattended path is the one that most needs the guarantees.
- **A shared library of helpers called by two loops.** Rejected: "shared
  helpers" still leaves two control-flow trunks where a gate can be added to one
  and not the other; the seam-purity test exists precisely to forbid that.
