# ADR-0038: Governed lifecycle hooks

Status: accepted

## Context

Fleet had hard-coded policy gates and rich audit events but no operator-defined
lifecycle-hook seam. Deployments could not enforce organization-specific
checks, run a formatter/validator after an edit, or observe lifecycle events
without patching fleet or wrapping every tool — exactly the coupling the
bundle/engine doctrine forbids (issue #788).

## Decision

A client bundle may declare `hooks:` — commands run at four lifecycle points
(`user_prompt_submit`, `pre_tool_use`, `post_tool_use`, `turn_end`). A hook is
**governed**, not a general extension point:

- **Bundle data, not repo/repo-content code.** Hooks are configured only by the
  trusted operator bundle (`FLEET_CLIENT_CONFIG_DIR`), never by arbitrary
  repository content, and installed once at startup (like `agent_policy`; the
  MCP hot-reload path does not re-read them).
- **Executed inside the sandbox.** A hook command runs through the same
  per-turn `Executor` (sandbox `RunBash`) as bash/run_python — never a host
  `exec.Command`, which would recreate the #784 hole. It has no credentials, no
  host filesystem, and the container's network posture; the payload it receives
  on stdin is bounded, redacted, and carries no env/credentials/transcript.
- **Can only observe or narrow.** A hook outcome is continue, block-with-reason,
  or a bounded additional-context fragment. There is no argument rewriting (the
  issue defers it). Fleet's existing policy/approval/credential/audit gates
  evaluate **after** hooks on the same unmodified input, so a hook can never
  widen authority, add a tool, or grant network/budget/approval.
- **One governed core.** Hooks live inside `agentcore.Run`/`buildFantasyTools`,
  so interactive, scheduled, and spawned-sub-agent runs inherit them with zero
  driver changes — no second loop.
- **Bounded and fail-safe.** Each hook has a timeout (default 30s, ≤120s) with
  an in-container `timeout` prefix (the #796 leak-window mitigation), output and
  context caps, a per-run context budget, and panic containment. An enforcing
  hook (`enforce: true`) fails **closed** (a failure blocks); an advisory hook
  fails **observable** (audited, operation proceeds). Every invocation emits a
  `hook.decision` audit event.

## Consequences

Operators get a supported extension seam that cannot become an authority-
escalation path. The generic bundle ships zero hooks (byte-for-byte unchanged
default; the engine is not even constructed when none are configured). Adopting
`hooks:` requires a fleet release that understands the schema (strict YAML
decoding rejects it otherwise — the additive-first doctrine). Deferred:
`pre_compact`/`post_compact` and `subagent_start`/`subagent_end` events,
argument rewriting / an "ask" outcome, hook config hot-reload, `turn_end` on
cancelled/budget-stopped runs, and multi-source hook discovery — each recorded
in docs/HOOKS.md.
