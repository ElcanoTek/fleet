# fleet

Elcano's fleet of agents — one-shot and interactive — consolidated into a single
"Mega Box" deployment.

A single `fleet` process runs, on one box:

1. **Interactive real-time chat** sessions (streamed over SSE), and
2. A **scheduling engine** that runs recurring background agent tasks,

both executing their tool calls inside the **same** rootless-Podman sandbox, and
both driven by **one** unified agent runtime (`internal/agentcore`).

This repository consolidates what used to live across five repositories
(`chat`, `moc`, `cutlass`, `gig`, `sandbox`). `lifeline` remains an external
per-developer coding MCP and is not vendored here.

## Layout

```
cmd/
  fleet/          the Mega Box binary (chat HTTP/SSE + orchestrator HTTP + scheduler + worker pool)
  fleet-admin/    unified admin CLI (bootstrap, users, MCP credential accounts)
  cutlass/        optional local one-shot debug entrypoint (not the production scheduled path)
  sandbox-probe/  deploy-time sandbox smoke test
internal/
  agentcore/      the one unified run loop + shared agent primitives
  agent/          input sources, observers, policies, finalize (interactive + scheduled)
  runner/         in-process capped worker pool (the old "gig", folded in)
  creds/          MCP credential-account store
  mcp/            merged Go MCP client (stdio + HTTP)
  sandbox/        the single execution backend (ephemeral container over a persistent workspace)
  tools/          native agent tools (bash, python, ...)
  store/          interactive (chat) Postgres layer + migrations
  sched/          orchestrator/scheduler (was moc) + its migrations
  httpapi/        chat HTTP/SSE/auth layer
  config/         unified configuration
web/              one Next.js app: /chat and /orchestrator
images/sandbox/   the one sandbox container image
mcp/              the deduped Python MCP servers
```

See `docs/MIGRATION_PLAN_V2.md` for the architecture and the phased migration plan.

## Development

```
make build      # go build ./...
make test       # go test ./...
make lint       # golangci-lint run
```
