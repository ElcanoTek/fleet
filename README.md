# fleet

**A general-purpose agent fleet you run yourself — any agent, any model, in a
sandbox, on a budget, connected to your data.**

fleet is an open-source platform for running AI agents — both one-shot scheduled
tasks and interactive real-time chat — on infrastructure you control. One
`fleet` process boots a unified agent runtime, an execution sandbox, a
scheduler, and a worker pool, and serves both a chat UI and an orchestrator UI.
Every tool call an agent makes runs inside a rootless-Podman sandbox; every turn
is metered against a cost ceiling; and the tools and data an agent can reach are
brokered host-side so credentials never enter the sandbox.

If your team keeps reaching for the same agent recipes — the same prompts, the
same connected tools, the same guardrails — fleet is the place to standardize
them.

> **Status:** early, active development. fleet is pre-1.0 — the architecture is
> in place and exercised by an extensive test suite (Go + web + live e2e), but
> APIs and config shapes can still change. Expect rough edges.

## Why fleet

- **Any agent, any model.** fleet is an [ACP](#standards) client: alongside its
  own native agent loop it can drive **other coding agents** (Claude Code,
  Goose, …) as selectable, sandboxed "flavors" you pick per chat or per
  scheduled task. Models are routed OpenRouter-style, so you choose the right
  model per task rather than hard-wiring one vendor. (The
  ["best model for the job" idea](https://www.notdiamond.ai/) is a good mental
  model for why this matters.)

- **Sandboxed by default.** Tool calls — bash, Python, file I/O, MCP calls —
  execute inside an ephemeral, rootless-Podman container over a persistent
  per-conversation workspace. **Even the native agent runs in the sandbox**: its
  loop runs inside the container and delegates every execution back to the host,
  so it has no privileged local executor. There is no "trusted" fast path that
  skips the sandbox.

- **Cost-controlled.** Each turn runs against configurable per-task cost and
  token **ceilings**, with usage and cost accounting tracked as the agent works.
  A model that won't stop calling tools is bounded by the ceiling, the
  per-turn timeout, and an iteration cap — not by your invoice.

- **Resilient scheduling.** A scheduled task that fails on a *transient* infra
  blip can be re-queued with exponential backoff up to its `max_retries`
  (default 0 = off, opt-in per task); a deterministic failure is never retried.
  Retries are bounded and the agent is told its attempt number so it can avoid
  repeating non-idempotent side-effects — fleet does not auto-dedupe those.

- **Connected to your data and tools, wherever they live.** fleet speaks
  [MCP](#standards) and ships a per-deployment **MCP catalog**. Tasks select
  which MCP servers they need, with **multi-account credentials** brokered
  host-side: the broker injects the right credentials when it runs a delegated
  MCP call, so secrets never travel into the sandbox or the model's context.

- **Reusable workflows and shared, preconfigured tools.** Personas, protocols
  (playbooks), the MCP catalog, branding, and model defaults all come from a
  pluggable **client-config bundle** (see below). Standardize your team's agent
  setups once; roll your own as needed.

- **Standards-compliant.** fleet implements two open protocols, both shipped and
  tested (see [Standards](#standards)): **ACP** (Agent Client Protocol) to drive
  the native and external agents, and **MCP** (Model Context Protocol) for
  tools and data.

- **MIT-licensed and observable.** The whole platform is open source. The agent
  runtime emits structured observer events for every turn — tool calls, results,
  usage, enforcement nudges — so you can see exactly what an agent did and what
  it cost.

## Architecture at a glance

A single `fleet` process runs, on one box:

1. **Interactive real-time chat** sessions (streamed over SSE), and
2. A **scheduling engine** that runs recurring background agent tasks,

both executing their tool calls inside the **same** rootless-Podman sandbox, and
both driven by **one** unified agent runtime (`internal/agentcore`).

This repository consolidates what used to live across five repositories
(`chat`, `moc`, `cutlass`, `gig`, `sandbox`). `lifeline` remains an external
per-developer coding MCP and is not vendored here.

## Standards

fleet is built on open protocols. We list only what is actually implemented and
tested in this repository:

- **ACP — Agent Client Protocol.** fleet is an ACP client that spawns each agent
  flavor as a sandboxed subprocess and owns the host-side governance seam (tool
  execution, MCP calls, policy/audit, observer events). The native flavor
  (`cmd/fleet-native-agent`) wraps fleet's own run loop as an ACP agent; external
  agents (Claude Code, Goose, …) plug in the same way. See
  [`internal/acpruntime`](internal/acpruntime) and
  [`docs/USING-AGENTS.md`](docs/USING-AGENTS.md). _(Advanced/optional: fleet can
  also run **as** an ACP agent so an editor drives it — see
  [`docs/USING-AGENTS.md`](docs/USING-AGENTS.md). Not needed for the web app.)_
- **MCP — Model Context Protocol.** A merged Go MCP client (stdio + HTTP) drives
  the tools and data sources in the deployment's MCP catalog. See
  [`internal/mcp`](internal/mcp).

The orchestrator HTTP API is published as an OpenAPI 3.1 contract at
[`docs/openapi.yaml`](docs/openapi.yaml); a CI test
(`cmd/fleet/openapi_drift_test.go`) keeps its routes + auth schemes in lockstep
with the shipped router (it does not gate body schemas).

### Roadmap (not yet shipped)

Items here are aspirational and **not** current capabilities — do not rely on
them:

- **Skills** — packaged, shareable agent capabilities beyond the current
  persona/protocol bundle.

## Repository layout

```
cmd/
  fleet/          the Mega Box binary (chat HTTP/SSE + orchestrator HTTP + scheduler + worker pool); `fleet acp` runs it as an ACP agent over stdio (editor ingress)
  fleet-admin/    unified admin CLI (bootstrap, users, MCP credential accounts)
  fleet-native-agent/  the native ACP agent (wraps agentcore.Run inside the sandbox)
  cutlass/        optional local one-shot debug entrypoint (not the production scheduled path)
  sandbox-probe/  deploy-time sandbox smoke test
internal/
  agentcore/      the one unified run loop + shared agent primitives (cost ceilings, policy)
  acpruntime/     the ACP client + agent-side runner (drives native + external agents)
  acpingress/     fleet AS an ACP agent over stdio (`fleet acp`) — editor ingress on the governed turn
  agent/          input sources, observers, policies, finalize (interactive + scheduled)
  runner/         in-process capped worker pool (the old "gig", folded in)
  creds/          MCP credential-account store (host-side credential broker)
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

See [`docs/MIGRATION_PLAN_V2.md`](docs/MIGRATION_PLAN_V2.md) for the architecture
and the phased migration plan.

## The client-config bundle

fleet ships **no** client-specific content. It loads a **client config bundle**
from `FLEET_CLIENT_CONFIG_DIR` (default `config/default`, a generic bundle with
neutral branding and no MCP connectors). A real deployment points the variable
at a checked-out client repo whose `manifest.yaml` supplies the branding, model
defaults, MCP-server catalog, empty-state cards, and agent tool policy, and
whose `system_prompts/`, `personas/`, `protocols/`, and `mcp/` directories
supply the prompts, personas, playbooks, and Python MCP servers. See
[`config/default/README.md`](config/default/README.md) and
[`internal/clientconfig/clientconfig.go`](internal/clientconfig/clientconfig.go)
for the bundle contract.

This is how you make fleet yours: package your team's reusable agent setups —
the personas, the playbooks, the connected MCP tools — into a bundle and point a
deployment at it.

## Using other agents

fleet is an ACP client: besides its own loop it can drive **other coding agents**
(Claude Code, Goose, …) as selectable, sandboxed flavors you pick per chat or per
scheduled task. See **[`docs/USING-AGENTS.md`](docs/USING-AGENTS.md)** for the
flavor model, how to add an external agent to a client bundle, the permission UI,
the governance tiers (stated honestly), and a worked example. Read the governance
and data-residency sections before enabling an external agent.

> **Advanced/optional — driving fleet *from* an editor (ACP ingress).** fleet can
> also expose *itself* as an ACP agent (`fleet acp`) so an editor (Zed, Neovim)
> drives its governed pipeline over stdio. This is a developer convenience, **not**
> part of the web chat/orchestrator product — most deployments never use it. The
> setup lives at the end of [`docs/USING-AGENTS.md`](docs/USING-AGENTS.md).

## Development

```
make build      # go build ./...
make test       # go test ./...
make lint       # golangci-lint run
```

For the full build/test workflow (including the Postgres-backed Go suites, the
web app, and the Playwright e2e suites), see
[`CONTRIBUTING.md`](CONTRIBUTING.md).

### Running one task locally (cutlass)

`cmd/cutlass` runs a **single task YAML** to completion locally — no orchestrator,
no HTTP server, no database — through the **same governed scheduled runtime** the
production scheduler uses (`agentcore.Run`, `Mode=Scheduled`; tool calls still run
in the sandbox, MCP credentials still brokered host-side). It is the local
debug/iteration entrypoint, not a second execution path.

```
scripts/run_workflow_live.sh docs/examples/cutlass-task.yaml   # builds the sandbox image, isolates a workspace, tails a log
go run ./cmd/cutlass --log out.json path/to/task.yaml          # or invoke it directly
```

See [`docs/examples/cutlass-task.yaml`](docs/examples/cutlass-task.yaml) for the
task schema (a thin mirror of the scheduled-task create shape).

## Deploy

The Mega Box is **one** `fleet` process. The browser only ever talks to the
Next.js web app; the web app proxies, server-side over loopback, to the two Go
backends the single process boots (chat on `127.0.0.1:8080`, orchestrator on
`:8000`). Caddy fronts the web app with TLS; the backends stay loopback-only.

```
browser ──TLS──▶ Caddy ──▶ Next web app (:3000) ──▶ fleet: chat :8080 + orchestrator :8000
```

**Quick start (bare Fedora/RHEL box).** Clone the repo and run the bootstrap
script — it installs the toolchain (Go, Node, podman, python3), provisions
Postgres, builds + installs the binary, and installs the systemd units:

```
sudo dnf install -y git
git clone https://github.com/ElcanoTek/fleet.git /opt/fleet/src
sudo bash /opt/fleet/src/scripts/bootstrap.sh --postgres=local --enable-service
```

The first run is always the **shell script** — `fleet-admin` doesn't exist until
it's built. Once installed, `fleet-admin bootstrap`/`update`/`status` wrap the
same scripts for day-2 ops. The numbered steps below break down what bootstrap
does (and the manual path if you'd rather run each piece yourself):

1. **Bootstrap** the databases + the 0600 credential env file (one cluster, two
   DBs; never runs app migrations — each service self-migrates on first start):

   ```
   scripts/bootstrap.sh --postgres=local      # or --postgres=external
   ```

   bootstrap installs the build/runtime/sandbox toolchain (Go, Node, podman,
   python3 — skipped on non-dnf hosts), then writes the two
   `FLEET_*_DATABASE_URL`s and `FLEET_CLIENT_CONFIG_DIR` into the env file for
   you; you then add `OPENROUTER_API_KEY`, the bundle's MCP connector
   credentials, and any MCP account secrets (`fleet-admin mcp account set ...`).
   See **Operating fleet** below for the full bootstrap → update → status
   lifecycle (`fleet-admin bootstrap` wraps this).

2. **Build** the binary, the sandbox image, and the web app:

   ```
   make build                              # → ./fleet AND ./fleet-admin
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
   install -D -m 0755 fleet-admin      /opt/fleet/fleet-admin
   git clone <client-config-repo>      /opt/fleet/client   # set FLEET_CLIENT_CONFIG_DIR=/opt/fleet/client
   install -D -m 0644 deploy/fleet.service /etc/systemd/system/fleet.service
   install -D -m 0600 <your-env-file>  /etc/fleet/fleet.env
   systemctl daemon-reload && systemctl enable --now fleet
   ```

   (`fleet-admin bootstrap --enable-service` automates this build → install →
   unit-install → enable from a source checkout — see **Operating fleet** below.)

   Run the Next web app as its own supervised unit (`deploy/fleet-web.service` —
   it `npm run start`s the built app on port 3000), wiring
   `CHAT_SERVER_URL`/`ORCHESTRATOR_SERVER_URL` to the loopback backends and
   `CHAT_SERVER_TOKEN` to the binary's `FLEET_SERVER_TOKEN` in its 0600
   `/etc/fleet/fleet-web.env`:

   ```
   cd web && npm ci && npm run build        # build the Next app
   install -d /opt/fleet/web && cp -a web/. /opt/fleet/web/
   install -D -m 0644 deploy/fleet-web.service /etc/systemd/system/fleet-web.service
   install -D -m 0600 <your-web-env-file> /etc/fleet/fleet-web.env
   systemctl daemon-reload && systemctl enable --now fleet-web
   ```

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

> **`bootstrap` and `update` operate on a fleet *source checkout*.** They run
> `make build` (and, on update, `git pull`) against the checkout and install the
> resulting `fleet` + `fleet-admin` binaries to `FLEET_INSTALL_DIR` (default
> `/opt/fleet`, the unit's `ExecStart` dir). Keep the repo cloned on the box (Go
> toolchain present); `status`, `restart`, `stop`, and `logs` work off the
> installed binary alone.

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

### service lifecycle — restart · stop · logs

Day-2 conveniences over the host systemd unit, so you never drop to raw
`systemctl`/`journalctl`:

```
fleet-admin restart                 # systemctl restart the fleet unit
fleet-admin stop                    # systemctl stop the fleet unit
fleet-admin logs                    # tail the last 50 journal lines (a.k.a. `tail`)
fleet-admin logs -n 200             # last 200 lines
fleet-admin logs -f                 # follow (stream) until Ctrl-C
fleet-admin restart --service foo   # target a non-default unit name
```

The unit is resolved from `--service`, else `$FLEET_SERVICE_NAME`, else `fleet`.
`restart`/`stop` manage a **system** unit, so — like the systemd unit itself —
they need root/sudo; systemctl's own permission error surfaces via the exit code.
`logs` reads the journal (usually permitted unprivileged) and exits non-zero if
the unit isn't installed.

### backup · restore — disaster recovery

fleet keeps every conversation in the **chat** DB and every scheduled task in the
**sched** DB. Both are backed up and restored per-database with `pg_dump -Fc` /
`pg_restore` (one custom-format dump file each — the two DBs have independent
DSNs, so a single cluster-wide dump would not fit the credential model):

```
fleet-admin backup                          # dump BOTH DBs into the cwd (fleet-<db>-<UTC>.dump)
fleet-admin backup --db=chat --out /backups # dump just chat into /backups
fleet-admin restore --db=sched FILE.dump    # restore one DB (--clean --if-exists; overwrites it)
```

`backup` prints each dump path on stdout (scriptable for a cron job). `restore`
is deliberately single-DB — it overwrites a live database, so the target is named
explicitly (no `--db=all`). Connection params, including the password, are passed
to the child processes through the environment, never argv. See
**[`docs/BACKUP_RESTORE.md`](docs/BACKUP_RESTORE.md)** for the full recovery
runbook, a cron example, and the round-trip verification procedure.

### Where the sandbox build fits

The execution sandbox is a **per-client bundle artifact**: each bundle ships its
own `sandbox/Containerfile` (digest-pinned base). `bootstrap` builds it on the
box by default (auditable supply chain); `update` rebuilds it only when the
Containerfile changed; `status` verifies the resolved image runs. Registry
publish stays opt-in — set `sandbox.image` in the bundle manifest to a prebuilt
ref and all three steps consume that instead of building.

## Contributing

Contributions are welcome — see [`CONTRIBUTING.md`](CONTRIBUTING.md) for the
build/test workflow, branch/PR conventions, and CI gates. Please also read the
[`CODE_OF_CONDUCT.md`](CODE_OF_CONDUCT.md). To report a security issue privately,
see [`SECURITY.md`](SECURITY.md).

## License

fleet is released under the [MIT License](LICENSE).
