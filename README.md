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

## Deploy

The Mega Box is **one** `fleet` process. The browser only ever talks to the
Next.js web app; the web app proxies, server-side over loopback, to the two Go
backends the single process boots (chat on `127.0.0.1:8080`, orchestrator on
`:8000`). Caddy fronts the web app with TLS; the backends stay loopback-only.

```
browser ──TLS──▶ Caddy ──▶ Next web app (:3000) ──▶ fleet: chat :8080 + orchestrator :8000
```

1. **Bootstrap** the databases + the 0600 credential env file (one cluster, two
   DBs; never runs app migrations — each service self-migrates on first start):

   ```
   scripts/bootstrap.sh --postgres=local      # or --postgres=external
   ```

   Then fill in `OPENROUTER_API_KEY`, the two `FLEET_*_DATABASE_URL`s, and any
   MCP account secrets (`fleet-admin mcp account set ...`) in the env file.

2. **Build** the binary and the web app:

   ```
   make build                          # → ./fleet
   cd web && npm ci && npm run build    # Next production build
   ```

3. **systemd** — run the single binary under `deploy/fleet.service` (it
   `EnvironmentFile`s the 0600 env file, `Restart=always`, drains the worker
   pool on `SIGTERM`):

   ```
   install -D -m 0755 fleet            /opt/fleet/fleet
   cp -r personas protocols system_prompts /opt/fleet/
   install -D -m 0644 deploy/fleet.service /etc/systemd/system/fleet.service
   install -D -m 0600 <your-env-file>  /etc/fleet/fleet.env
   systemctl daemon-reload && systemctl enable --now fleet
   ```

   Run the Next web app alongside (`cd web && npm run start`, port 3000), wiring
   `CHAT_SERVER_URL`/`ORCHESTRATOR_SERVER_URL` to the loopback backends and
   `CHAT_SERVER_TOKEN` to the binary's `FLEET_SERVER_TOKEN`.

4. **TLS** — `deploy/Caddyfile` reverse-proxies the public domain to the web app
   (SSE-aware: `flush_interval -1`, long read timeout). Point it at your domain
   and `caddy run --config deploy/Caddyfile`.

See `deploy/fleet.service` and `deploy/Caddyfile` for the full annotated knob
list (listener addresses, admin/registration tokens, data dir, timezone).
