# ADR-0003: Host-side MCP credential brokering

- **Status:** Accepted
- **Date:** 2026-06-28 (documents a decision that predates this record)
- **Deciders:** fleet maintainers

## Context

MCP servers and connectors need credentials (API keys, OAuth tokens). The agent
loop and its tools run model-influenced code; the sandbox is treated as
potentially hostile (ADR-0002). If a credential were placed in the sandbox, the
agent container, the model context, or a log line, it would be one prompt
injection or one stray `print` away from exfiltration.

## Decision

Credentials **stay host-side** and **never** enter the sandbox, the agent
container, the model context, or logs. MCP calls that need a credential are
**brokered out-of-process**: the broker (`internal/mcpbroker`, reached through
the broker seam in `internal/agentcore/mcp_broker.go`) injects the credential
only when *it* runs a delegated MCP call on the host. The model sees tool
results, never the secret used to obtain them. Which credentials a given run may
use is itself constrained by an allowlist
(`internal/agentcore/credential_allowlist.go`).

The real `OPENROUTER_API_KEY` and other secrets live **outside the repo**; tests
use the fake-LLM seam and obvious placeholders.

## Enforcement

- `gitleaks` gates CI on every push and PR (`.github/workflows/ci.yml`); no
  secret may land in the tree.
- The broker boundary is exercised by `internal/mcpbroker/*_test.go`.
- The credential allowlist gates which secrets a run may reach.

## Consequences

- A compromised or injected agent can request a *brokered action* but cannot
  read the credential behind it, and cannot reach a credential outside its
  allowlist.
- Every new connector must route its secret through the broker; "just pass the
  token into the tool" is not an option.
- Brokering adds an out-of-process hop and some plumbing per connector — the
  accepted cost of keeping secrets off the model path.

## Alternatives considered

- **Inject credentials into the sandbox as env vars.** Rejected: directly
  contradicts ADR-0002; one prompt injection exfiltrates them.
- **Pass credentials through the model context to the tool.** Rejected: the
  model context is logged, summarised, and sent to a third-party provider —
  the worst possible place for a secret.
