# Deploying fleet

> Deep deployment reference: host sizing, the one-command web+TLS stack, and every option. Part of the [fleet README](../README.md).


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

## Choosing a host (sizing)

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
[ADR-0008](adr/0008-persistent-python-repl-per-conversation.md) and the
[agent runtime guide](AGENT-RUNTIME.md)). Size the host to the cap:

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
> 512 MiB default, not your host's free RAM. A scheduled **task can override**
> these per run with `sandbox_limits: {memory_mb, cpus, pids}` (#205) — a heavy
> task gets 4 GiB while the common case keeps the lean default — bounded by the
> operator ceilings `FLEET_SANDBOX_{MEMORY_MAX_MB,CPUS_MAX,PIDS_MAX}` (defaults
> 8192 / 16 / 1024). A per-task override always cold-starts that run's container.

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

> **Hypervisor isolation (optional).** By default each sandbox is a rootless
> container sharing the host kernel. For untrusted prompts or sensitive data,
> set the bundle manifest's `sandbox.runtime` (or `FLEET_SANDBOX_RUNTIME`) to
> `kata` or `libkrun` to run every tool call in a dedicated **KVM microVM** with
> its own guest kernel — an escape then needs a hypervisor CVE, not just a
> container-escape. Requires `/dev/kvm`; fleet fail-closed preflights it at boot.
> See [`docs/SANDBOX-RUNTIMES.md`](SANDBOX-RUNTIMES.md) and
> [ADR-0010](adr/0010-microvm-sandbox-runtimes.md).

## Quick start (one host)

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
sudo fleet restart
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

The first run is always the **shell script** — the `fleet` binary doesn't exist
until it's built. Once installed, `fleet bootstrap`/`update`/`status` wrap the
same scripts for day-2 ops. The server runs via `fleet serve` (bare `fleet` also
serves, for back-compat); all other verbs are the operator CLI. (`fleet-admin
<verb>` still works but is deprecated and will be removed.) The numbered steps
below break down what bootstrap does (and the manual path if you'd rather run
each piece yourself):

1. **Bootstrap** the databases + the 0600 credential env file (one cluster, two
   DBs; never runs app migrations — each service self-migrates on first start):

   ```
   scripts/bootstrap.sh --postgres=local      # or --postgres=external
   ```

   bootstrap installs the build/runtime/sandbox toolchain (Go, Node, podman,
   python3 — skipped on non-dnf hosts), then writes the two
   `FLEET_*_DATABASE_URL`s and `FLEET_CLIENT_CONFIG_DIR` into the env file for
   you; you then add `OPENROUTER_API_KEY`, the bundle's MCP connector
   credentials, and any MCP account secrets (`fleet mcp account set ...`).
   See **Operating fleet** below for the full bootstrap → update → status
   lifecycle (`fleet bootstrap` wraps this).

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

   To automate that publish path (#195), fleet ships a **reusable GitHub Actions
   workflow** — `.github/workflows/publish-sandbox-image.yml` (`workflow_call`).
   A client config repo adds a small caller workflow (the contract is documented
   at the top of the reusable file) that fires on `sandbox/**` changes: it
   builds the bundle's image with the same `scripts/build-sandbox-image.sh`,
   pushes an immutable `{git-sha}` tag plus `:latest` to GHCR with the caller's
   `GITHUB_TOKEN`, and opens a PR in the client repo pinning
   `sandbox.image` to the sha tag. Fleet core CI deliberately does NOT publish
   the generic bundle's image (the coupling removed in 24ce69f stays removed);
   a manual `workflow_dispatch` exists for ad-hoc publishes.

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

   (`fleet bootstrap --enable-service` automates this build → install →
   unit-install → enable from a source checkout — see **Operating fleet** below.)

   > **One command for the web tier + TLS.** `bootstrap.sh --enable-web
   > [--domain <fqdn>]` automates everything in the rest of this section: it
   > builds the Next app into `/opt/fleet/web`, writes the 0600
   > `/etc/fleet/fleet-web.env` (generating `APP_SESSION_SECRET` and mirroring
   > `CHAT_SERVER_TOKEN`/`ORCHESTRATOR_SERVER_TOKEN` from the backend env), enables
   > `fleet-web`, and with `--domain` installs Caddy + opens 80/443 for automatic
   > TLS. The manual steps below are the by-hand equivalent.
   >
   > **Login model.** The web app authenticates three ways, all minting the same
   > HMAC session cookie (signed with `APP_SESSION_SECRET`) so everything
   > downstream is identical: (1) a **self-contained email + password** path
   > (`POST /api/auth/login` → backend `/auth/verify` → bcrypt against the chat
   > user store) — add users via `fleet chat user add`; (2) an optional Elcano
   > **magic-link** cookie path, **disabled unless `AUTH_SIGNING_PUBKEY` is set**;
   > and (3) an optional **OIDC / OAuth2 SSO** path (Authorization Code + PKCE),
   > **disabled unless `FLEET_OIDC_ISSUER` + `FLEET_OIDC_CLIENT_ID` +
   > `FLEET_OIDC_CLIENT_SECRET` are set** (optional: `FLEET_OIDC_SCOPES`,
   > `FLEET_OIDC_ALLOWED_DOMAINS`, `FLEET_OIDC_BUTTON_LABEL`,
   > `FLEET_OIDC_REDIRECT_URI`). SSO lives entirely in the Next.js layer — the Go
   > chat server never speaks OIDC. In every case the chat user-list still gates
   > **membership** (an authenticated email that isn't provisioned lands on the
   > no-access page), so SSO/magic-link prove *who you are* while the user-list
   > decides *who may use chat*. A stand-alone deploy needs none of this; users
   > just log in with email + password.
   >
   > **`fleet-web` BindsTo `fleet`.** It stays down until the backend is healthy
   > (i.e. until `OPENROUTER_API_KEY` is set), so after a first `--enable-web`
   > bootstrap: set the key, `fleet restart`, then `systemctl start fleet-web`.

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
list (listener addresses, admin token, bootstrap admins, data dir, timezone).
