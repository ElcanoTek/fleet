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
  clientconfig/   loads the pluggable CLIENT BUNDLE (branding, MCP catalog, prompts, ...)
  mcp/            merged Go MCP client (stdio + HTTP)
  sandbox/        the single execution backend (ephemeral container over a persistent workspace)
  tools/          native agent tools (bash, python, ...)
  store/          interactive (chat) Postgres layer + migrations
  sched/          orchestrator/scheduler (was moc) + its migrations
  httpapi/        chat HTTP/SSE/auth layer
  config/         unified configuration (env loading; the MCP catalog comes from the bundle)
web/              one Next.js app: /chat and /orchestrator
config/default/   the GENERIC client bundle baked into the repo (runs bare),
                  including config/default/sandbox/Containerfile — the sandbox
                  image is a per-client bundle artifact (build-on-box default)
```

fleet ships **no** client-specific content. It loads a **client config bundle**
from `FLEET_CLIENT_CONFIG_DIR` (default `config/default`, a generic bundle with
neutral branding and no MCP connectors). A real deployment points the variable
at a checked-out client repo whose `manifest.yaml` supplies the branding, model
defaults, MCP-server catalog, empty-state cards, and agent tool policy, and
whose `system_prompts/`, `personas/`, `protocols/`, and `mcp/` directories
supply the prompts, personas, playbooks, and Python MCP servers. See
`config/default/README.md` and `internal/clientconfig/clientconfig.go` for the
bundle contract.

See `docs/MIGRATION_PLAN_V2.md` for the architecture and the phased migration plan.

## Using other agents

fleet is an ACP client: besides its own loop it can drive **other coding agents**
(Claude Code, Goose, …) as selectable, sandboxed flavors you pick per chat or per
scheduled task. See **[`docs/USING-AGENTS.md`](docs/USING-AGENTS.md)** for the
flavor model, how to add an external agent to a client bundle, the permission UI,
the governance tiers (stated honestly), and a worked example. Read the governance
and data-residency sections before enabling an external agent.

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

   bootstrap writes the two `FLEET_*_DATABASE_URL`s and `FLEET_CLIENT_CONFIG_DIR`
   into the env file for you; you then add `OPENROUTER_API_KEY`, the bundle's MCP
   connector credentials, and any MCP account secrets
   (`fleet-admin mcp account set ...`). See **Operating fleet** below for the full
   bootstrap → update → status lifecycle (`fleet-admin bootstrap` wraps this).

2. **Build** the binary, the sandbox image, and the web app:

   ```
   make build                              # → ./fleet
   # The sandbox image is a per-client BUNDLE artifact (build-on-box by default):
   # the Containerfile lives in the bundle at <bundle>/sandbox/Containerfile and
   # each client ships + digest-pins its own flavor. Build the bundle's sandbox:
   FLEET_CLIENT_CONFIG_DIR=<bundle> scripts/build-sandbox-image.sh   # → the manifest's tag (podman)
   #   (defaults to config/default → localhost/fleet-sandbox:latest)
   cd web && npm ci && npm run build       # Next production build
   ```

   Registry publish is **opt-in per client**: instead of building on the box, a
   client may set `sandbox.image` in its `manifest.yaml` to a prebuilt ref it
   pushed (e.g. `ghcr.io/<org>/sandbox@sha256:...`); fleet then pulls/uses that
   and skips the build. fleet resolves the ref from the bundle
   (`clientconfig.Sandbox().ResolvedImageRef()` — `image` if set, else `tag`); an
   explicit `FLEET_SANDBOX_IMAGE` env var still overrides. fleet never builds at
   process startup — this deploy step (or the client's registry push) does.
   Reproducibility comes from the digest-pinned base each bundle's Containerfile
   owns.

3. **systemd** — run the single binary under `deploy/fleet.service` (it
   `EnvironmentFile`s the 0600 env file, `Restart=always`, drains the worker
   pool on `SIGTERM`). Check out the client config bundle and point
   `FLEET_CLIENT_CONFIG_DIR` at it (fleet itself ships only the generic
   `config/default` bundle):

   ```
   install -D -m 0755 fleet            /opt/fleet/fleet
   git clone <client-config-repo>      /opt/fleet/client   # set FLEET_CLIENT_CONFIG_DIR=/opt/fleet/client
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

## Operating fleet

The operator lifecycle is **bootstrap → update → status**, one box. Every verb is
idempotent and exposed both as a shell script (`scripts/`) and as a `fleet-admin`
subcommand that wraps it, so a re-run converges on the same state rather than
double-applying. None of them ever run application migrations — each service
self-migrates on start (chat's advisory-lock runner; sched's golang-migrate).

```
fleet-admin bootstrap   →   fleet-admin update   →   fleet-admin status
  (provision a box)         (roll a new version)      (health / doctor)
```

### The env file (the one source of credentials)

A single **0600** env file (`FLEET_ENV_FILE`, default `.env.local`; on a box
typically `/etc/fleet/fleet.env`) carries every secret and connection string.
`deploy/fleet.service` `EnvironmentFile`s it, `fleet` parses the same file via
`config.Load`, and `fleet-admin` reads it for MCP account secrets — so process
env and config-loaded values stay in sync. `bootstrap` writes/refreshes the
machine-managed keys in place (preserving your hand-edited lines and comments):

```
FLEET_CHAT_DATABASE_URL=postgres://chat:…@127.0.0.1:5432/chat?sslmode=disable
FLEET_SCHED_DATABASE_URL=postgres://sched:…@127.0.0.1:5432/sched?sslmode=disable
FLEET_CLIENT_CONFIG_DIR=/opt/fleet/client      # the client bundle checkout
FLEET_ENV_FILE=/etc/fleet/fleet.env            # so config.Load reads this same file
```

You then add `OPENROUTER_API_KEY`, any listener/admin tokens, the client
bundle's MCP connector credentials, and per-account MCP secrets
(`fleet-admin mcp account set <server> <account> --secret KEY=-`, value via
stdin — never on argv).

### The client-config checkout

fleet ships **no** client content; it loads a **client config bundle** from
`FLEET_CLIENT_CONFIG_DIR` (default `config/default`, the generic bundle). A real
deployment checks out a client repo and points the variable at it. `bootstrap
--client-config <git-url|path>` automates this: a **git URL** is cloned to a
stable location (`/opt/fleet/client`, or `./.fleet-client` when `/opt` is not
writable) and `update` keeps it fast-forwarded; a **path** is pointed at
directly. Either way the resolved dir is written to `FLEET_CLIENT_CONFIG_DIR` in
the env file. The bundle also owns the **sandbox** — see below.

### bootstrap — provision a box

```
fleet-admin bootstrap --postgres=local                     # dnf+initdb+pg_hba+\gexec, sslmode=disable
fleet-admin bootstrap --postgres=external                  # validate the DSNs with SELECT 1, sslmode=require
fleet-admin bootstrap --client-config <git-url|path>       # check out / point at a client bundle
fleet-admin bootstrap --enable-service                     # systemctl enable --now the fleet unit at the end
fleet-admin bootstrap --dry-run                            # print the plan; touch nothing
```

End to end, every run: ensure the 0600 env file → resolve the client bundle
(`--client-config`) → **build the sandbox image from the bundle** (calls
`scripts/build-sandbox-image.sh` with `FLEET_CLIENT_CONFIG_DIR`; skipped when the
manifest pins a prebuilt `sandbox.image`) → provision both `chat`+`sched`
roles/databases idempotently via `\gexec` (local) or validate the DSNs (external)
→ write the resolved DSNs + `FLEET_CLIENT_CONFIG_DIR` into the env file →
optionally `enable --now` the systemd unit. Local-mode role passwords are
generated when unset; set `CHAT_DB_PASSWORD`/`SCHED_DB_PASSWORD` to pin them.

### update — roll a new version in place

```
fleet-admin update              # pull → build → conditional sandbox rebuild → restart
fleet-admin update --no-pull    # rebuild the current checkout(s) only
fleet-admin update --dry-run    # print the plan
```

`update` (ported from the `moc`/`gig` pattern) `git pull`s **both** the fleet
checkout and the client-config checkout, runs `make build` (fleet binary) and
`cd web && npm ci && npm run build`, then **rebuilds the sandbox image only when
the bundle's `sandbox/Containerfile` changed** — it stores a SHA-256 of the
Containerfile under `.fleet-state/` and compares, skipping the ~2-3 min image
build when unchanged. Services self-migrate on restart, so `update` runs no
migrations; it finishes with `systemctl restart fleet` and a unit health check.
If the pull changed `update.sh` itself, the script **re-execs the fresh copy** in
rebuild-only mode (bash holds the pre-pull inode open, so the fix would otherwise
only land on the *next* update). On a build failure the live binary/image is left
untouched; roll back with `git checkout <sha> && fleet-admin update --no-pull`.

### status (doctor) — is the box healthy?

```
fleet-admin status                # ✓/✗ report; exits non-zero if unhealthy
fleet-admin status --no-sandbox   # skip the podman run check
```

`status` runs read-only checks and prints a ✓/✗ line per check, exiting non-zero
(6) if any required check fails: the client bundle loads + validates, required
env vars are set, **both** databases answer `SELECT 1` (a lightweight ping — no
migrations), the **sandbox image is present + runnable** (a throwaway
`podman run --rm <ref> true`, where `<ref>` is resolved exactly as the running
process resolves it — `FLEET_SANDBOX_IMAGE` env wins, else the bundle's
`ResolvedImageRef()`), and the systemd unit state when a unit is installed.
DSN passwords are redacted in the output.

### Where the sandbox build fits

The execution sandbox is a **per-client bundle artifact**: each bundle ships its
own `sandbox/Containerfile` (digest-pinned base). `bootstrap` builds it on the
box by default (auditable supply chain); `update` rebuilds it only when the
Containerfile changed; `status` verifies the resolved image runs. Registry
publish stays opt-in — set `sandbox.image` in the bundle manifest to a prebuilt
ref and all three steps consume that instead of building.
