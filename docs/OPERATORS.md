# Operating fleet

> Operator runbook: the env file, the client-config checkout, and every lifecycle verb in detail. Part of the [fleet README](../README.md).


The operator lifecycle is **bootstrap → update → status**, one box. The server
runs via `fleet serve` (bare `fleet` also serves, for back-compat); all other
verbs are the operator CLI. (`fleet-admin <verb>` still works but is deprecated
and will be removed.) Every verb is idempotent and exposed both as a shell script
(`scripts/`) and as a `fleet` subcommand that wraps it, so a re-run converges on
the same state rather than double-applying. None of them ever run application
migrations — each service self-migrates on start (chat's advisory-lock runner;
sched's golang-migrate). `make install` puts the `fleet` binary on PATH.

For a terminal chat with the agent, `fleet chat` opens a Bubble Tea TUI that
streams replies from the running server's chat API (or `fleet chat --message
"…"` for a one-shot, scriptable turn) — same governed run loop and sandbox as the
web chat, just a CLI front-end for power users. **On the box running fleet you
usually only need `fleet chat --email you@org`:** the shared `FLEET_SERVER_TOKEN`
is read automatically from the same env file the server uses (`$FLEET_ENV_FILE`,
else `.env.local`, else `/etc/fleet/fleet.env`), so an operator who can read that
0600 file logs in without copying the token anywhere. The token is still never
accepted on argv — override discovery with `$FLEET_SERVER_TOKEN`, `--token-file`,
or `--env-file <path>` when the file lives elsewhere.

```
fleet bootstrap   →   fleet update   →   fleet status
  (provision a box)   (roll a new version)   (health / doctor)
```

> **`bootstrap` and `update` operate on a fleet *source checkout*.** They run
> `make build` (and, on update, `git pull`) against the checkout and install the
> resulting `fleet` + `fleet-admin` binaries to `FLEET_INSTALL_DIR` (default
> `/opt/fleet`, the unit's `ExecStart` dir). Keep the repo cloned on the box (Go
> toolchain present); `status`, `restart`, `stop`, and `logs` work off the
> installed binary alone.

## The env file (the one source of credentials)

A single **0600** env file (`FLEET_ENV_FILE`, default `.env.local`; on a box
typically `/etc/fleet/fleet.env`) carries every secret and connection string.
`deploy/fleet.service` `EnvironmentFile`s it, `fleet` parses the same file via
`config.Load`, and the `fleet` operator CLI reads it for MCP account secrets — so process
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
(`fleet mcp account set <server> <account> --secret KEY=-`, value via
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

## The client-config checkout

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

## bootstrap — provision a box

```
fleet bootstrap --postgres=local                     # dnf+initdb+pg_hba+\gexec, sslmode=disable
fleet bootstrap --postgres=external                  # validate the DSNs with SELECT 1, sslmode=require
fleet bootstrap --client-config <git-url|path>       # check out / point at a client bundle
fleet bootstrap --enable-service                     # systemctl enable --now the fleet unit at the end
fleet bootstrap --enable-web [--domain <fqdn>]       # also build+enable the web tier (+ Caddy TLS with --domain); implies --enable-service
fleet bootstrap --dry-run                            # print the plan; touch nothing
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

## update — roll a new version in place

```
fleet update              # pull → build → conditional sandbox rebuild → restart
fleet update --no-pull    # rebuild the current checkout(s) only
fleet update --dry-run    # print the plan
fleet update --check      # read-only "commits behind" report; touch nothing
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
untouched; roll back with `git checkout <sha> && fleet update --no-pull`.

## upgrade — drain, swap, health-gate, auto-roll-back

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

## status (doctor) — is the box healthy?

```
fleet status                # ✓/✗ report; exits non-zero if unhealthy
fleet status --no-sandbox   # skip the podman run check
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
> image store), so the sandbox image lives in *that* user's store. `fleet
> status` run as **root** therefore reports the sandbox image as not runnable
> (root's Podman can't see it) even though the service runs it fine — a false
> negative. Verify the sandbox as the service user instead, e.g.
> `sudo -u fleet env XDG_RUNTIME_DIR=/run/fleet podman run --rm <ref> true`, or
> just confirm a chat turn executes a `run_python` tool call. Use `--no-sandbox`
> to skip the check when running `status` as root.

## diagnose — a redacted support bundle for issue reports

```
fleet diagnose                       # write fleet-diagnose-<UTC>.tar.gz to the cwd
fleet diagnose --output /tmp/bundle.tar.gz
fleet diagnose --no-sandbox          # skip the podman image inspection
```

`diagnose` collects a single gzipped tar you can attach to an issue. It bundles
four text sections: `status.txt` (the **exact** `fleet status` ✓/✗ report —
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

## service lifecycle — restart · stop · logs

Day-2 conveniences over the host systemd unit, so you never drop to raw
`systemctl`/`journalctl`:

```
fleet restart                 # systemctl restart the fleet unit
fleet stop                    # systemctl stop the fleet unit
fleet logs                    # tail the last 50 journal lines (a.k.a. `tail`)
fleet logs -n 200             # last 200 lines
fleet logs -f                 # follow (stream) until Ctrl-C
fleet restart --service foo   # target a non-default unit name
```

The unit is resolved from `--service`, else `$FLEET_SERVICE_NAME`, else `fleet`.
`restart`/`stop` manage a **system** unit, so — like the systemd unit itself —
they need root/sudo; systemctl's own permission error surfaces via the exit code.
`logs` reads the journal (usually permitted unprivileged) and exits non-zero if
the unit isn't installed.

## process logs — stderr by default, optional rotating file

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

### structured JSON format (#178)

As of #178 the `fleet serve` process log is emitted as **structured `log/slog`
JSON by default** — every line is a JSON object (`time`/`level`/`msg`), which
log aggregators (Loki, Datadog, CloudWatch, journald JSON mode) ingest and index
natively. This applies to both stderr and the rotating file above.

```
FLEET_LOG_FORMAT=json                      # default; structured JSON. Use "text" for the legacy plaintext lines.
FLEET_LOG_LEVEL=info                        # debug|info|warn|error (default info)
```

Two operator notes:

- **`FLEET_LOG_FORMAT=text`** restores the exact prior plaintext behavior if you
  prefer it (or have tooling that parses the old format).
- **`FLEET_LOG_LEVEL`** currently sets the threshold for *native* structured
  (`slog`) call sites. The bulk of today's lines are emitted through a
  compatibility bridge (the standard-`log`→`slog` conversion is ongoing) and
  **always emit at info regardless of the level** — so raising the level will not
  silently suppress existing diagnostics/error lines. Its reach grows as call
  sites are converted to native `slog` (#178).

CLI verbs (`fleet status`, `fleet migrate status`, …) are unaffected — they keep
human-readable plaintext output.

## backup · restore — disaster recovery

fleet keeps every conversation in the **chat** DB and every scheduled task in the
**sched** DB. Both are backed up and restored per-database with `pg_dump -Fc` /
`pg_restore` (one custom-format dump file each — the two DBs have independent
DSNs, so a single cluster-wide dump would not fit the credential model):

```
fleet backup                          # dump BOTH DBs into the cwd (fleet-<db>-<UTC>.dump)
fleet backup --db=chat --out /backups # dump just chat into /backups
fleet restore --db=sched FILE.dump    # restore one DB (--clean --if-exists; overwrites it)
```

`backup` prints each dump path on stdout (scriptable for a cron job). `restore`
is deliberately single-DB — it overwrites a live database, so the target is named
explicitly (no `--db=all`). Connection params, including the password, are passed
to the child processes through the environment, never argv. See
**[`docs/BACKUP_RESTORE.md`](BACKUP_RESTORE.md)** for the full recovery
runbook, a cron example, and the round-trip verification procedure.

## Where the sandbox build fits

The execution sandbox is a **per-client bundle artifact**: each bundle ships its
own `sandbox/Containerfile` (base tracks `fedora-minimal:latest`; pin a digest
for reproducibility). `bootstrap` builds it on the
box by default (auditable supply chain); `update` rebuilds it only when the
Containerfile changed; `status` verifies the resolved image runs. Registry
publish stays opt-in — set `sandbox.image` in the bundle manifest to a prebuilt
ref and all three steps consume that instead of building.
