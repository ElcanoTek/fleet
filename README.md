# fleet

[![codecov](https://codecov.io/gh/ElcanoTek/fleet/branch/main/graph/badge.svg)](https://codecov.io/gh/ElcanoTek/fleet)

**A general-purpose agent fleet you run yourself — any model, in a
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

- **Any model.** fleet runs its own native agent loop and lets you choose the
  **best model for each task** rather than hard-wiring one vendor.

- **Sandboxed by default.** The agent loop runs in the fleet process, but every
  tool call — bash, Python, file I/O, MCP — executes inside an ephemeral,
  rootless-Podman container over a persistent per-conversation workspace. There
  is **no fast path that skips the tool sandbox**. MCP credentials never enter
  the sandbox: they are isolated by the **out-of-process MCP broker**, which
  injects them only when it runs a delegated MCP call host-side (issue #167).

- **Cost-controlled.** Each turn runs against configurable per-task cost and
  token **ceilings**, with usage and cost accounting tracked as the agent works.
  A model that won't stop calling tools is bounded by the ceiling, the
  per-turn timeout, and an iteration cap — not by your invoice.

- **Resilient scheduling.** A scheduled task that fails on a *transient* infra
  blip can be re-queued with exponential backoff up to its `max_retries`
  (default 0 = off, opt-in per task); a deterministic failure is never retried.
  An optional per-task `retry_policy` tunes the backoff (exponential or fixed,
  custom initial/max delay) and which failure classes retry (e.g. allow
  `context_budget`, block `cost_ceiling`); unset = the default transient-only
  curve. Retries are bounded and the agent is told its attempt number so it can
  avoid repeating non-idempotent side-effects — fleet does not auto-dedupe those.
  A daily retention sweep prunes terminal task runs (and their JSONB logs) older
  than `FLEET_RUN_LOG_RETENTION_DAYS` (default **90**) so the scheduler DB can't
  grow without bound, while always keeping the most recent
  `FLEET_KEEP_RUNS_PER_TASK` (default **10**) runs of each task regardless of age
  (so a task's last-known state is never lost). Set the retention to `0` to
  disable pruning; the sweep runs at `FLEET_CLEANUP_HOUR` UTC (default 04:00).
  As a middle path that keeps the audit trail, an **optional** archival sweep
  (off by default) gzip-compresses the log payloads of terminal tasks older than
  `FLEET_LOG_ARCHIVE_AFTER_DAYS` **in place** in the scheduler DB — reads inflate
  them transparently, so retrieval is unchanged. Set a base64 32-byte
  `FLEET_LOG_ARCHIVE_ENCRYPTION_KEY` (held host-side, never logged) to also
  AES-256-GCM encrypt the archived payloads. It runs on the same daily
  `FLEET_CLEANUP_HOUR` timer; `0` (the default) leaves it off.

- **Connected to your data and tools, wherever they live.** fleet speaks
  [MCP](#standards) and ships a per-deployment **MCP catalog**. Tasks select
  which MCP servers they need, with **multi-account credentials** brokered
  host-side: the broker injects the right credentials when it runs a delegated
  MCP call, so secrets never travel into the sandbox or the model's context.

- **Reusable workflows and shared, preconfigured tools.** Personas, protocols
  (playbooks), skills (packaged capabilities), the MCP catalog, branding, and
  model defaults all come from a pluggable **client-config bundle** (see below).
  Standardize your team's agent setups once; roll your own as needed.

- **Standards-compliant.** fleet is built on open standards, all shipped and
  tested (see [Standards](#standards)): **MCP** (Model Context Protocol) for
  tools and data, and the open **Agent Skills** format for packaged, on-demand
  capabilities.

- **MIT-licensed and observable.** The whole platform is open source. The agent
  runtime emits structured observer events for every turn — tool calls, results,
  usage, enforcement nudges — so you can see exactly what an agent did and what
  it cost.

## Built for trust: governed, auditable delegation

Delegating real work to an agent raises three concerns: can it do the job, can
you trust it with this task, and are you comfortable handing over control. fleet
answers each with a concrete mechanism, organized below.

### Can it do the job — reproducibly?

A setup that worked once but can't be reproduced isn't something you can
delegate. fleet makes an agent's configuration a **versioned artifact**: the
system prompt, personas, protocols (playbooks), skills, connected MCP tools, and
model defaults all live in a versioned **client-config bundle** (a plain git repo — see
below). The setup that worked is the setup that runs again next time, for the
next person, on a schedule. And because every turn emits structured **observer
events** — each tool call, its result, token usage, cost, and any enforcement
nudge — streamed live over SSE in the chat UI, you judge the work from its actual
trace, not just a final answer.

### Should I trust it with this task?

Trust here means **bounded** and **inspectable** — known limits going in, a full
record coming out.

- **Hard limits that actually fire.** Each turn runs against a per-turn cost
  ceiling, a token ceiling, an iteration cap, and a timeout. They are enforced,
  not advisory: a model that won't stop calling tools is stopped by the ceiling.
  A runaway loop costs you a capped turn, not an open-ended invoice.
- **A record you can replay.** The observer events persist as a per-turn audit
  trail an operator can inspect after the fact. fleet ships no usage dashboard,
  but the trail is the substrate one would be built on — the per-turn data needed
  to answer "what did this agent do, and what did it cost?" is captured by
  default.
- **fleet owns execution end to end.** The agent loop runs in the fleet process
  and fleet owns tool execution, policy, and accounting for every turn — there is
  no self-executing agent whose work fleet can only observe. The session log
  records the real, executed tool calls, so you don't have to guess what the
  agent did; the trail says so.

### Am I comfortable handing over control?

The honest answer to "what if it does the wrong thing" is to ensure it **can't**
reach the things that would hurt, and to keep a human on the decisions that
matter.

- **The agent has no direct power.** Every tool call — bash, Python, file I/O,
  MCP — runs inside an ephemeral rootless-Podman sandbox over a persistent
  per-conversation workspace, with no fast path that skips it; the host enforces
  all policy. The agent loop runs in the fleet process but holds no privileged
  executor of its own — each tool call is handed to the sandbox under host
  policy, so the agent can only act through that governed seam.
- **Credentials stay out of reach.** MCP credentials are isolated by the
  out-of-process MCP broker: it injects them only when it runs a delegated MCP
  call host-side, so they never enter the sandbox, the model's context, or the
  logs. They live in a `0600` env file managed through `fleet-admin`, with
  per-MCP multi-account seats. The agent uses your connectors without ever
  holding their keys. This isolation is about the *sandbox*; the client-config
  bundle's own host-side MCP servers **do** receive these brokered credentials by
  design — so treat write access to the bundle repo as production access (see
  [`SECURITY.md`](SECURITY.md), "The client-config bundle is root-equivalent").
- **A human stays on the loop.** Sensitive actions raise an **allow / deny** card
  in the chat UI and block the turn until someone answers. It is **default-deny**
  on timeout, and there is **no "approve all"** — every request is decided on its
  own merits. Scheduled work, which has no human to ask, is **fail-closed**: its
  execution sandbox is network-sealed by default and an end-of-run verifier
  re-checks the run before it is allowed to finish (see
  [`docs/AGENT-RUNTIME.md`](docs/AGENT-RUNTIME.md)).

Together these make delegation something you can watch, cap, and stop:
reproducible setups, a live and replayable record, limits that fire, isolated
credentials, and human checkpoints on the actions that matter.

## Architecture at a glance

A single `fleet` process runs, on one box:

1. **Interactive real-time chat** sessions (streamed over SSE), and
2. A **scheduling engine** that runs recurring background agent tasks,

both executing their tool calls inside the **same** rootless-Podman sandbox, and
both driven by **one** unified agent runtime (`internal/agentcore`).

## Standards

fleet is built on open protocols. We list only what is actually implemented and
tested in this repository:

- **MCP — Model Context Protocol.** A merged Go MCP client (stdio + HTTP) drives
  the tools and data sources in the deployment's MCP catalog. See
  [`internal/mcp`](internal/mcp).
- **Agent Skills.** The client-config bundle's `skills/` directory holds packaged,
  on-demand agent capabilities in the open
  [Agent Skills format](https://github.com/anthropics/skills) — a `SKILL.md` per
  skill (`name` + `description` frontmatter) plus optional bundled scripts and
  reference files. fleet loads them with **progressive disclosure**: only each
  skill's name, description, and path enter the system prompt; the agent reads the
  full `SKILL.md` and runs any bundled scripts on demand, inside the same
  rootless sandbox every other tool call uses. See
  [`internal/clientconfig`](internal/clientconfig) (the loader + the `ReadSkills`
  parser) and the shipped [`config/default/skills`](config/default/skills)
  example. _(Design rationale: Anthropic's
  [Equipping agents for the real world with Agent Skills](https://www.anthropic.com/engineering/equipping-agents-for-the-real-world-with-agent-skills).)_

The orchestrator HTTP API is published as an OpenAPI 3.1 contract at
[`docs/openapi.yaml`](docs/openapi.yaml); a CI test
(`cmd/fleet/openapi_drift_test.go`) keeps its routes + auth schemes in lockstep
with the shipped router (it does not gate body schemas).

## Repository layout

```
cmd/
  fleet/          the fleet binary (chat HTTP/SSE + orchestrator HTTP + scheduler + worker pool)
  fleet-admin/    unified admin CLI (bootstrap, users, MCP credential accounts)
  cutlass/        optional local one-shot debug entrypoint (not the production scheduled path)
  sandbox-probe/  deploy-time sandbox smoke test
internal/
  agentcore/      the one unified run loop + shared agent primitives (cost ceilings, policy)
  agent/          input sources, observers, policies, finalize (interactive + scheduled)
  runner/         in-process capped worker pool (the old "gig", folded in)
  creds/          MCP credential-account store (host-side credential broker)
  clientconfig/   loads the pluggable CLIENT BUNDLE (branding, MCP catalog, prompts, skills, ...)
  mcp/            merged Go MCP client (stdio + HTTP)
  mcpbroker/      out-of-process MCP credential broker (keeps connector secrets out of the loop's address space)
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
docs/             architecture & operator docs; docs/adr/ records the load-bearing
                  Architecture Decision Records behind the invariants
```

## The client-config bundle

fleet ships **no** client-specific content. It loads a **client config bundle**
from `FLEET_CLIENT_CONFIG_DIR` (default `config/default`, a generic bundle with
neutral branding and no MCP connectors). A real deployment points the variable
at a checked-out client repo whose `manifest.yaml` supplies the branding, model
defaults, MCP-server catalog, empty-state cards, and agent tool policy, and
whose `system_prompts/`, `personas/`, `protocols/`, `skills/`, and `mcp/`
directories supply the prompts, personas, playbooks, Agent Skills, and Python
MCP servers. See
[`config/default/README.md`](config/default/README.md) and
[`internal/clientconfig/clientconfig.go`](internal/clientconfig/clientconfig.go)
for the bundle contract.

This is how you make fleet yours: package your team's reusable agent setups —
the personas, the playbooks, the skills, the connected MCP tools — into a bundle
and point a deployment at it.

**Choosing a bundle:**

- **Run bare** — point nothing; fleet uses the in-repo `config/default` (neutral
  branding, no connectors). Good for a first look.
- **Fork the public template** —
  [`ElcanoTek/example-config`](https://github.com/ElcanoTek/example-config) is a
  public, generic "fork-this-to-start" bundle (fictional branding, an example
  always-on MCP + a gated connector, three example personas). Clone it, rename,
  and add your own connectors.
- **Your own private bundle** — a private git repo with your branding, MCP
  catalog, personas, and protocols. Because it's private, the box needs **read
  access** when it clones the bundle: create a **read-only GitHub Personal Access
  Token** (fine-grained, `Contents: read` on just that repo) and either bake it
  into the clone URL or configure git's credential store on the box (see the
  quick start below). The token never needs write or any other scope.

`bootstrap --client-config <git-url[#<sha-or-tag>]|path>` clones (or points at)
the bundle. Without a pin it tracks the branch and `update` fast-forwards it;
with a `#<sha-or-tag>` pin, `update` advances the checkout only to that ref, so
a bundle change is a deliberate operator action rather than a silent pull — the
same digest-pinning discipline the registry-published `sandbox.image` already
supports. Because the bundle is built and run host-side under the fleet service
identity (see [`SECURITY.md`](SECURITY.md)), pin it in production. See **Deploy**
and **Operating fleet**.

## No lock-in: your agent IP is portable

Everything that defines how your agents behave lives in the **client-config
bundle** — a plain git repo or directory you own (`FLEET_CLIENT_CONFIG_DIR`), not
inside fleet's database or binary:

- **`system_prompts/`** — base prompts for chat and tasks
- **`personas/`** — reusable agent profiles
- **`protocols/`** — playbooks your agents follow
- **`skills/`** — packaged [Agent Skills](#standards) (`SKILL.md` + bundled scripts)
- **`mcp/`** — your MCP connectors (+ `requirements.txt`)
- **`manifest.yaml`** — MCP catalog, tool policy, model defaults, sandbox block
- **`sandbox/Containerfile`** — the exact image your tool calls run in

Those are versioned files you control, and fleet reaches tools and data over an
**open protocol** — [MCP](#standards). So your agent setup travels *with* you:
version it in git, fork it per team, share it across orgs, or point it at another
MCP-capable platform. Moving off fleet doesn't mean starting over — you keep
the bundle, and the wire protocol is not fleet-specific. The assets are yours
and the protocol is open, which keeps adoption low-risk: you can
start on real work without betting that you can never leave.

The public template
[`ElcanoTek/example-config`](https://github.com/ElcanoTek/example-config) shows
the full layout — fork it and the whole thing is yours from day one.

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

fleet runs as **one** `fleet` process on a **single host** (one well-sized
server or VM). The browser only ever talks to the Next.js web app; the web app
proxies, server-side over loopback, to the two Go backends the single process
boots (chat on `127.0.0.1:8080`, orchestrator on `:8000`). Caddy fronts the web
app with TLS; the backends stay loopback-only.

> **Single-host by design.** Scheduled-task crash recovery uses single-owner
> database leases and the worker-pool concurrency cap is a per-process semaphore —
> both assume one running process. fleet scales **vertically**: put it on a
> bigger box, not more replicas. One well-specced server goes a long way (see the
> sizing table below).

### Choosing a host (sizing)

The dominant cost is the **execution sandbox**: each concurrently-running agent
holds one rootless-Podman container (the ~1.3 GB Python/IPython image) doing the
agent's bash/`run_python` work. Model inference is **remote** (OpenRouter), so
you are sizing for sandbox CPU/RAM and image+workspace disk, not GPUs — which is
exactly why fleet goes so far on a single box: one well-specced server runs an
org's worth of concurrent agents.

`FLEET_MAX_CONCURRENT_AGENTS` (default **8**) is the **box-wide** cap on agent
turns in flight at once — interactive chat **and** scheduled tasks combined. It is
a true sizing knob: the host never runs more concurrent sandboxes than this. Chat
is prioritized — a slice of the cap (≈¼, derived automatically) is **reserved for
interactive turns**, so a backlog of scheduled tasks can never starve a person at
the keyboard; chat still bursts to the full cap when the scheduler is idle. When
the box is genuinely at capacity, a new chat turn waits briefly, then returns a
clean "at capacity — resend in a moment" instead of hanging or over-subscribing
the host. The sandbox warm pool scales with the cap (up to 8 pre-warmed) — pin it
explicitly with `FLEET_SANDBOX_WARM_SIZE`, and a background keeper reaps and
replaces a warm container that has sat idle past `FLEET_SANDBOX_WARM_TTL` (default
**300s**), bounding the age of any warm container a turn can receive (so a
long-idle container that may have been OOM-killed or cgroup-frozen is rotated out
rather than handed to a turn). By default the `run_python` IPython kernel is
**fresh per turn**; set `FLEET_PYTHON_REPL_MODE=persistent` to keep one kernel
alive **per conversation** so variables and DataFrames survive across turns (it
is never shared across conversations — see
[ADR-0008](docs/adr/0008-persistent-python-repl-per-conversation.md) and the
[agent runtime guide](docs/AGENT-RUNTIME.md)). Size the host to the cap:

| Concurrent agents | vCPU | RAM    | Disk   | Who it's for                              |
| ----------------- | ---- | ------ | ------ | ----------------------------------------- |
| 2                 | 2    | 8 GB   | 40 GB  | trial / a couple of users                 |
| 8 (default)       | 8    | 32 GB  | 120 GB | a team / steady scheduled load            |
| 16                | 16   | 64 GB  | 200 GB | a busy team, heavy concurrent + scheduled |
| 32                | 32   | 128 GB | 400 GB | a department running agents all day       |
| 64                | 64   | 256 GB | 800 GB | a large org on one big box                |

Rule of thumb: a **~2 vCPU / 6 GB base** (the Go process + web app + local
Postgres) plus **~1 vCPU and ~1.5–3 GB RAM per concurrent agent**, and disk for
the sandbox image (~1.5 GB) + the Podman overlay store + your persistent
per-conversation workspaces. Heavy `pandas`/`matplotlib` workloads push RAM per
agent up. A single large server (**32–64 cores, 128–256 GB RAM** — a few thousand
dollars of dedicated hardware) comfortably runs an org's worth of agents; raise
`FLEET_MAX_CONCURRENT_AGENTS` and the host together. External managed Postgres
lowers the host's base footprint.

> **Per-container cap.** Each sandbox runs under a cgroup cap that defaults to
> **512 MiB / 1.0 CPU / 128 pids**. For the heavy `pandas`/`matplotlib`
> workloads above, raise it to match the per-agent RAM you provisioned via
> `FLEET_SANDBOX_MEMORY` (e.g. `2g`), `FLEET_SANDBOX_CPUS`, and
> `FLEET_SANDBOX_PIDS` — otherwise those workloads are OOM-killed against the
> 512 MiB default, not your host's free RAM.

> **Per-task resource telemetry.** To help right-size those caps, fleet samples
> `podman stats` read-only over each sandbox container's lifetime and records the
> run's peak/average CPU and memory plus cumulative I/O and peak PID count. This
> is **observability only** — it never changes the container's isolation or
> limits. The peaks of the most recently finished run are exported on `/metrics`
> as `fleet_sandbox_cpu_usage_percent`, `fleet_sandbox_memory_usage_bytes`,
> `fleet_sandbox_memory_limit_bytes`, `fleet_sandbox_io_bytes{direction=…}`, and
> `fleet_sandbox_pids_peak` (last-write-wins gauges, deliberately **without** a
> per-task label to avoid unbounded time-series cardinality). When a run's memory
> crosses 90% of its limit, a one-shot warning is logged so an OOM-prone task is
> visible. Sampling cadence is `FLEET_SANDBOX_STATS_INTERVAL_SECONDS` (default
> **10s**, floor **5s**); set it to a negative value to disable collection. When
> `podman stats` is unavailable the feature degrades silently — it never fails a
> turn.

### Quick start (one host)

The topology (Caddy → web app → loopback backends):

```
browser ──TLS──▶ Caddy ──▶ Next web app (:3000) ──▶ fleet: chat :8080 + orchestrator :8000
```

On a bare Fedora/RHEL box this is **four steps** — the bootstrap script installs
the toolchain (Go, Node, podman, python3), provisions Postgres, builds + installs
the binary, and installs + enables the systemd units:

```sh
# 1. Git, and (for a PRIVATE config bundle) cache a read-only token so the box
#    can clone it. Skip the credential line if your bundle is public or you pass
#    a token in the --client-config URL.
sudo dnf install -y git
git config --global credential.helper store   # then `git clone` your private bundle once to cache the PAT

# 2. Clone fleet.
sudo git clone https://github.com/ElcanoTek/fleet.git /opt/fleet/src

# 3. Bootstrap. Point --client-config at your bundle (a git URL or a path);
#    omit it to run bare on config/default, or use the public template
#    https://github.com/ElcanoTek/example-config to start from.
#    Under --enable-service the script writes credentials to /etc/fleet/fleet.env
#    (the path the systemd unit reads) by default.
sudo bash /opt/fleet/src/scripts/bootstrap.sh \
  --postgres=local --enable-service \
  --client-config https://github.com/ElcanoTek/example-config.git

#    …or stand up the full browser-facing stack (Next.js web UI + Caddy TLS) in
#    ONE command — swap --enable-service for --enable-web --domain <your-domain>:
# sudo bash /opt/fleet/src/scripts/bootstrap.sh \
#   --postgres=local --enable-web --domain fleet.example.com \
#   --client-config https://github.com/ElcanoTek/example-config.git

# 4. Add your OpenRouter key + connector secrets to the env file, then restart.
sudo "$EDITOR" /etc/fleet/fleet.env       # set OPENROUTER_API_KEY=… (+ MCP creds)
#    If the bundle's default persona isn't "assistant", also set
#    PERSONA_DEFAULT=<persona> here (e.g. PERSONA_DEFAULT=victoria).
sudo fleet-admin restart
#    With --enable-web, also (re)start the web unit: it BindsTo fleet.service, so
#    it stays down until the backend is healthy (i.e. until the key is set).
# sudo systemctl restart fleet-web
```

> **The read-only token.** A private bundle repo needs read access at clone
> time. Create a **fine-grained GitHub PAT** scoped to *just that repo* with
> **`Contents: read`** (no write, no other scope). Cache it via
> `git config --global credential.helper store` (then one manual `git clone` to
> seed it) or embed it in the `--client-config` URL
> (`https://<token>@github.com/ORG/your-config.git`). `update` reuses the same
> cached credential to fast-forward the bundle.

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
   # each client ships its own flavor. Build the bundle's sandbox:
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
   process startup — this deploy step (or the client's registry push) does. Each
   bundle's Containerfile owns its base image: the shipped defaults track
   `fedora-minimal:latest` so on-box rebuilds pick up current patches — pin a
   digest there if you need byte-for-byte reproducible builds.

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

   > **One command for the web tier + TLS.** `bootstrap.sh --enable-web
   > [--domain <fqdn>]` automates everything in the rest of this section: it
   > builds the Next app into `/opt/fleet/web`, writes the 0600
   > `/etc/fleet/fleet-web.env` (generating `APP_SESSION_SECRET` and mirroring
   > `CHAT_SERVER_TOKEN`/`ORCHESTRATOR_SERVER_TOKEN` from the backend env), enables
   > `fleet-web`, and with `--domain` installs Caddy + opens 80/443 for automatic
   > TLS. The manual steps below are the by-hand equivalent.
   >
   > **Login model.** The web app authenticates two ways: a **self-contained
   > email + password** path (`POST /api/auth/login` → backend `/auth/verify` →
   > bcrypt against the chat user store; HMAC session signed with
   > `APP_SESSION_SECRET`) — add users via `fleet-admin chat user add` — and an
   > optional Elcano **SSO** cookie path that is **disabled unless
   > `AUTH_SIGNING_PUBKEY` is set**. A stand-alone deploy needs no external auth
   > service; users just log in with email + password.
   >
   > **`fleet-web` BindsTo `fleet`.** It stays down until the backend is healthy
   > (i.e. until `OPENROUTER_API_KEY` is set), so after a first `--enable-web`
   > bootstrap: set the key, `fleet-admin restart`, then `systemctl start fleet-web`.

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
   and `caddy run --config deploy/Caddyfile`. This is the recommended path: the
   Next.js app is the only public entrypoint, so Caddy (or Tailscale Serve, whose
   `tsnet` CA provides HTTPS with no public port) terminates TLS in front of it
   and the Go backends stay loopback.

   For deployments that terminate TLS **directly at the Fleet chat process**
   instead of a fronting proxy, the chat server can serve HTTPS itself via
   `FLEET_TLS_MODE` (default `off`, no change):
   - `manual` — `FLEET_TLS_CERT_FILE` + `FLEET_TLS_KEY_FILE` (TLS 1.2+); a port-80
     listener 301-redirects to HTTPS.
   - `auto` — Let's Encrypt via `golang.org/x/crypto/acme/autocert`:
     `FLEET_TLS_DOMAIN` (required), `FLEET_TLS_ACME_DIR` (cert cache, default
     `/var/lib/fleet/acme-cache`), `FLEET_TLS_ACME_EMAIL`. Ports 443 + 80 must be
     publicly reachable for the HTTP-01 challenge; a private/loopback DNS result
     is warned about at startup.

   When TLS is active the chat responses carry HSTS +
   `X-Content-Type-Options`/`X-Frame-Options`. The orchestrator stays loopback
   HTTP — it is impersonation-load-bearing and must remain on 127.0.0.1.

5. **IP access control (optional defense-in-depth)** — the chat server can
   restrict access at the network level, in front of the shared-token auth, so an
   operator can express "only our office + VPN ranges" in fleet config instead of
   host firewall rules. All three knobs are **empty by default**, which is fully
   backward compatible — no list means every source IP is allowed, exactly as
   before:
   - `FLEET_IP_ALLOWLIST` — comma-separated IPs/CIDRs (e.g.
     `192.168.1.0/24,10.0.0.0/8,203.0.113.7`). When set, **only** matching
     addresses may connect; a bare host is treated as `/32` (IPv4) or `/128`
     (IPv6).
   - `FLEET_IP_DENYLIST` — comma-separated IPs/CIDRs that are **always** blocked.
     **Deny overrides allow** — an address in both lists is denied.
   - `FLEET_TRUSTED_PROXIES` — comma-separated IPs of trusted reverse proxies
     (e.g. the fronting Caddy: `127.0.0.1,::1`). Only when the immediate peer is
     one of these does fleet read the real client IP from `X-Forwarded-For`.
     **Without this set, `X-Forwarded-For` is never consulted**, so an untrusted
     client cannot spoof an allowlisted address via the header — you must
     explicitly opt in by naming your proxy IPs.

   Blocked requests get a uniform `403 Access denied` (plain text, no reason
   leaked); `/healthz` is exempt so load-balancer probes keep working; a
   malformed CIDR/IP entry is a **fatal startup error** (a silently-dropped
   allowlist entry could leave the box more open than intended); and the active
   filter state is logged at startup and surfaced in `GET /admin/health-summary`.

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
stdin — never on argv). Account names are **canonicalized**: hyphens and spaces
fold to underscore and case is ignored, so `client-a`, `client_a`, and
`Client_A` all resolve to one credential seat (`<VAR>_CLIENT_A`). Use distinct
base words — not separator tricks — to keep seats apart.

Optional tuning knobs live in the same env file. `FLEET_DISABLE_PROMPT_CACHE=true`
turns off Anthropic prompt-cache breakpoints; leave it unset to keep caching on
(it serves repeated system-prompt tokens from cache at ~10% of input cost). The
breakpoints are only ever emitted for `anthropic/`- and `google/`-prefixed model
slugs — other providers are unaffected by the setting. Cache efficiency is
visible per user in `/admin/stats` (`total_cached_tokens`,
`total_cache_creation_tokens`, `cache_hit_rate_pct`). The legacy
`CHAT_DISABLE_PROMPT_CACHE` / `CUTLASS_DISABLE_PROMPT_CACHE` aliases still work.

### The client-config checkout

fleet ships **no** client content; it loads a **client config bundle** from
`FLEET_CLIENT_CONFIG_DIR` (default `config/default`, the generic bundle). A real
deployment checks out a client repo and points the variable at it. `bootstrap
--client-config <git-url[#<sha-or-tag>]|path>` automates this: a **git URL** is
cloned to a stable location (`/opt/fleet/client`, or `./.fleet-client` when
`/opt` is not writable); a **path** is pointed at directly. Either way the
resolved dir is written to `FLEET_CLIENT_CONFIG_DIR` in the env file. An
unpinned URL tracks the remote default branch and `update` fast-forwards it; a
`#<sha-or-tag>` pin (recorded under the state dir, so `update` re-applies it
without sourcing the env file) makes `update` advance only to that exact ref —
or repin at update time with `update --pin <ref>`. Set
`FLEET_CLIENT_CONFIG_VERIFY=1` to additionally `git verify-tag`/`verify-commit`
the pinned ref (fail-closed) when a signing key / allowed-signers is configured.
The bundle also owns the **sandbox** — see below.

### bootstrap — provision a box

```
fleet-admin bootstrap --postgres=local                     # dnf+initdb+pg_hba+\gexec, sslmode=disable
fleet-admin bootstrap --postgres=external                  # validate the DSNs with SELECT 1, sslmode=require
fleet-admin bootstrap --client-config <git-url|path>       # check out / point at a client bundle
fleet-admin bootstrap --enable-service                     # systemctl enable --now the fleet unit at the end
fleet-admin bootstrap --enable-web [--domain <fqdn>]       # also build+enable the web tier (+ Caddy TLS with --domain); implies --enable-service
fleet-admin bootstrap --dry-run                            # print the plan; touch nothing
```

Under `--enable-service` (and `--enable-web`, which implies it) the credential env
file defaults to `/etc/fleet/fleet.env` — the path `deploy/fleet.service` reads —
so the one-command deploy writes secrets where the unit picks them up. Set
`FLEET_ENV_FILE` to override; plain local/dev runs still default to `.env.local`.

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

### upgrade — drain, swap, health-gate, auto-roll-back

```
git pull && scripts/fleet-upgrade.sh            # build → backup → swap → restart → /readyz gate
scripts/fleet-upgrade.sh --no-build             # swap the already-built source binaries
scripts/fleet-upgrade.sh --dry-run              # print the plan; change nothing
```

`scripts/fleet-upgrade.sh` is a safer companion to `update.sh` for production
boxes. It does not pull (run `git pull` first); it `make build`s, **backs up the
live `fleet`/`fleet-admin` binaries**, installs the new ones, `systemctl
restart`s, then **gates on the new process's `/readyz` probe** before declaring
success — and if `/readyz` does not come green within `--health-timeout` (default
90s) it **reinstalls the backup binaries and restarts**, so a bad build
self-heals to the last-known-good version instead of crash-looping.

The **drain is the binary's, not the script's**: `systemctl restart` sends
`SIGTERM`, and `cmd/fleet` already handles it gracefully — it flips `/healthz`
and `/readyz` to `503` (a load balancer stops routing to it), lets in-flight chat
turns **and** running scheduled tasks finish within
`FLEET_SHUTDOWN_GRACE_SECONDS` (default 30s, bounded by the unit's
`TimeoutStopSec`), then force-cancels stragglers and exits 0. The script's value
is the **backup/rollback + readiness gate around** that built-in drain; it adds
no Go code and runs no migrations.

> **Honest about "zero-downtime."** This is *zero-downtime-ish* / brief-blip, not
> truly zero-downtime. fleet is a **single process on one box** (the deployment
> posture — no rolling replicas behind a proxy), so there is an unavoidable window
> from when the old process finishes draining and exits until the new one binds
> its listeners and passes `/readyz`, during which new requests get a `503` (while
> draining) or a connection refusal (during the swap). What *is* graceful:
> **in-flight work is drained, not killed.** True zero-downtime would need a
> second instance plus a front proxy that fails over — out of scope for the
> single-big-box deployment.

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

> **Sandbox check + the dedicated service user.** The systemd unit runs `fleet`
> as a dedicated **`fleet`** user with **rootless Podman** (its own subuid range +
> image store), so the sandbox image lives in *that* user's store. `fleet-admin
> status` run as **root** therefore reports the sandbox image as not runnable
> (root's Podman can't see it) even though the service runs it fine — a false
> negative. Verify the sandbox as the service user instead, e.g.
> `sudo -u fleet env XDG_RUNTIME_DIR=/run/fleet podman run --rm <ref> true`, or
> just confirm a chat turn executes a `run_python` tool call. Use `--no-sandbox`
> to skip the check when running `status` as root.

### diagnose — a redacted support bundle for issue reports

```
fleet-admin diagnose                       # write fleet-diagnose-<UTC>.tar.gz to the cwd
fleet-admin diagnose --output /tmp/bundle.tar.gz
fleet-admin diagnose --no-sandbox          # skip the podman image inspection
```

`diagnose` collects a single gzipped tar you can attach to an issue. It bundles
four text sections: `status.txt` (the **exact** `fleet-admin status` ✓/✗ report —
the same checks, not a copy), `config.txt` (the **names** of the set
`FLEET_*`/`CHAT_*`/`DATABASE_URL`/`OPENROUTER_API_KEY` env vars — never their
values — plus the loaded bundle's app name, model hints, and MCP server names),
`db.txt` (the migration version of **both** databases via read-only SQL — no
migrations run), and `sandbox.txt` (the resolved sandbox image ref and, when
podman is present, that image's id/size).

It **never uploads anything** — it only writes a local file — and it **never
writes a secret value**: every section is run through fleet's centralized
scrubber (`internal/redact`, seeded with the values of secret-named env vars) and
DSN passwords are stripped before anything is added to the archive. A section that
can't be collected (e.g. a DB is unreachable) becomes an `ERROR …` line; the rest
of the bundle is still written. Review the tarball before sharing it.

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

### process logs — stderr by default, optional rotating file

fleet writes its process log (startup diagnostics + operational lines) to
**stderr**. Under the shipped systemd unit that goes to **journald**, which
already rotates it — so the default needs no configuration and is unchanged.

For a **container / non-systemd** deployment where nothing else rotates the log,
set `FLEET_LOG_FILE` to **also** tee those lines to a rotating file (the file
sink is OFF until you set it):

```
FLEET_LOG_FILE=/var/log/fleet/fleet.log   # opt in; empty (default) = stderr only
FLEET_LOG_MAX_SIZE_MB=100                 # rotate when the file reaches this size (default 100)
FLEET_LOG_MAX_BACKUPS=7                   # keep this many rotated files (default 7)
FLEET_LOG_MAX_AGE_DAYS=0                  # delete rotated files older than this; 0 = no age limit (default)
FLEET_LOG_COMPRESS=true                   # gzip rotated files (default true)
```

With the file sink on, lines still go to stderr **as well** — it tees, it does
not replace — so journald/Docker log drivers keep working alongside the file. The
file directory must be writable by the service user (the systemd unit's
`StateDirectory`/`ReadWritePaths` model); a bad path fails loudly at startup.

This rotates the **existing** log lines as-is. It does **not** convert the log to
structured JSON — the std-`log`-to-`slog` migration is tracked separately
([#178](https://github.com/ElcanoTek/fleet/issues/178)).

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
own `sandbox/Containerfile` (base tracks `fedora-minimal:latest`; pin a digest
for reproducibility). `bootstrap` builds it on the
box by default (auditable supply chain); `update` rebuilds it only when the
Containerfile changed; `status` verifies the resolved image runs. Registry
publish stays opt-in — set `sandbox.image` in the bundle manifest to a prebuilt
ref and all three steps consume that instead of building.

## Built by Elcano (commercial support)

fleet is built by **ElcanoTek**. The platform itself is MIT-licensed,
pre-1.0, and yours to run — the open-source project ships no support contract or
SLA. Separately, the same team takes on **commercial engagements** for
organizations that want to move faster than a self-serve deployment allows.

An agent is only as useful as the data connectors it can reach, the workflows
it's allowed to run, and the guardrails that keep it honest — which is exactly
what fleet encodes and what we build:

- **Custom agents** tuned to your domain.
- **Fleets** deployed and operated on your infrastructure.
- **Bespoke MCP servers and data connectors** that wire fleet into the systems
  your work actually lives in.

The platform stays open and self-hostable; an engagement is for when you'd rather
have the people who wrote it design the connectors, protocols, and ceilings with
you.

Learn more at [elcanotek.com](https://elcanotek.com) or reach out directly:
[brad@elcanotek.com](mailto:brad@elcanotek.com).

## Contributing

Contributions are welcome — see [`CONTRIBUTING.md`](CONTRIBUTING.md) for the
build/test workflow, branch/PR conventions, and CI gates. Please also read the
[`CODE_OF_CONDUCT.md`](CODE_OF_CONDUCT.md). To report a security issue privately,
see [`SECURITY.md`](SECURITY.md).

## Acknowledgements

fleet stands on the shoulders of excellent open-source projects and open
standards. Our thanks to the teams and communities behind them:

- **[Podman](https://github.com/containers/podman)** — rootless, daemonless
  containers. Every agent tool call (`bash`, `run_python`, MCP) executes inside a
  rootless-Podman sandbox; there is no trusted fast path that skips it.
- **[Fedora](https://fedoraproject.org)** — `fedora-minimal`
  (`registry.fedoraproject.org/fedora-minimal`) is the slim base image for the
  default sandbox: a small attack surface and current security patches on every
  on-box rebuild, with RPM-sourced Python rather than runtime `pip`.
- **[Model Context Protocol](https://modelcontextprotocol.io)** and its SDKs —
  the open standard fleet speaks (stdio + HTTP) to reach tools and data through a
  credential-brokered MCP catalog.
- **[Agent Skills](https://github.com/anthropics/skills)** — the open skill
  format fleet loads from the client-config bundle (`SKILL.md` + bundled scripts,
  with progressive disclosure).
- **[Fantasy](https://github.com/charmbracelet/fantasy)** by
  [Charmbracelet](https://github.com/charmbracelet) — the Go framework underneath
  fleet's multi-provider, multi-model agent run loop.
- **[OpenRouter](https://openrouter.ai)** — unified, provider-agnostic model
  routing that backs fleet's "any model, the right one per task" design.

## License

fleet is released under the [MIT License](LICENSE).
