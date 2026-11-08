# Operating fleet

> Operator runbook: the env file, the client-config checkout, and every lifecycle verb in detail. Part of the [fleet README](../README.md).


The operator lifecycle is **bootstrap → update → status**, one box. The server
runs via `fleet serve` (bare `fleet` also serves, for back-compat); all other
verbs are the operator CLI. (The `fleet-admin` shim was removed —
[ADR-0060](adr/0060-remove-the-fleet-admin-shim.md); `fleet update` and
`fleet doctor` delete a copy left on a box by an earlier install.)
Every verb is idempotent and exposed both as a shell script
(`scripts/`) and as a `fleet` subcommand that wraps it, so a re-run converges on
the same state rather than double-applying. None of them ever run application
migrations — the service self-migrates on start (the one migration runner,
[MIGRATIONS.md](MIGRATIONS.md)). `make install` puts the `fleet` binary on PATH.

For a terminal chat with the agent, `fleet chat` opens a Bubble Tea TUI that
streams replies from the running server's chat API (or `fleet chat --message
"…"` for a one-shot, scriptable turn) — same governed run loop and sandbox as the
web chat, just a CLI front-end for power users. **On the box running fleet you
usually only need `fleet chat --email you@org`:** the shared `FLEET_SERVER_TOKEN`
is read automatically from the same env file the server uses (`$FLEET_ENV_FILE`,
else `.env.local`, else `/etc/fleet/fleet.env`), so an operator who can read that
0600 file logs in without copying the token anywhere. The token is still never
accepted on argv — override discovery with `$FLEET_SERVER_TOKEN`, `--token-file`,
or `--env-file <path>` when the file lives elsewhere. With no `--model`, a new
conversation starts on the workspace default the server advertises (the same
slug the web picker starts on); resuming with `--conversation <id>` keeps the
model stored on that thread, and `/model <slug>` switches mid-session.
Both interactive and one-shot modes require a terminal SSE lifecycle event:
server-reported turn/model errors and an interrupted stream are failures, and
one-shot mode exits nonzero so automation cannot mistake a partial answer for
success. One-shot mode prints the conversation id to stderr (`conversation:
<id>`) so scripts can resume the thread — stdout carries only the agent's
prose.

`fleet acp` is the same client with a protocol in front of it: an Agent Client
Protocol agent on stdin/stdout, for ACP clients such as Zed or JetBrains to
launch. It resolves the token and `--email` exactly like `fleet chat`, and
each prompt is one turn on the running server. Give it a dedicated bot user,
because every ACP turn runs as that identity. Buzz's `buzz-acp` expects an
agent to post its own replies, so it launches `fleet acp` through
[`fleet-buzz-bridge`](https://github.com/ElcanoTek/fleet-buzz-bridge), a
separate adapter that is not part of fleet (see "Buzz" in
[`docs/ACP.md`](ACP.md)). Details: [`docs/ACP.md`](ACP.md).

Critical tools (send an email, schedule a task, a bundle-declared deploy) stage
an approval card, and the TUI is a full participant in that flow: the card's
summary renders in the transcript with the pending count in the status bar,
`/approve` runs the oldest staged action and `/deny` refuses it (the staged
tool's own outcome — task id, send confirmation — prints on approve), and a
staged call's tool line reads `⏸ awaiting approval` rather than a misleading
failure. Scripts settle cards without a terminal: one-shot mode prints every
staged approval id to stderr, and `fleet chat --conversation <id> --approve
<approval-id>` (or `--deny`) resolves it and prints the outcome.

`/approvals` reviews full summaries and expiry deadlines; `/approvals reload`
refreshes them from the server. `/edit {"name":"...","prompt":"...","cron":"..."}`
edits the oldest scheduled-task proposal before approval. `/approve [id] session`
and `/deny [id] session` apply to future calls to that tool in this conversation;
`pattern arg=glob` narrows that policy; a tool the bundle lists in
`agent_policy.critical_tool_no_session_approval` refuses both (the server
answers 400 and the card stays pending). `/resume <conversation-id>` switches
threads and reloads pending cards. Network failures retain cards for retry;
expired or failed actions are reported as errors. See
[Terminal approvals](TERMINAL-APPROVALS.md) for the complete command contract.

Every staged tool automatically prints its complete frozen execution arguments
as escaped JSON before `/approve`. Truncated or missing snapshots are refused.
One-shot approval retrieves and prints that review before submitting. Patterns match original string argument names
(for example `to_email`, not the summary's `to` label); matching denies win.
Handler-only policies live for the terminal session and settle each server card
individually. Handler-only patterns require the server's `pattern_args` metadata.

```
fleet bootstrap   →   fleet update   →   fleet status / fleet doctor
  (provision a box)   (roll a new version)   (check health / repair drift)
```

> **`bootstrap` and `update` use a fleet checkout for the units, scripts and
> the generic bundle, and a signed release for the binary.** They download the
> release's `fleet` binary, verify it (signature by fleet's release workflow,
> checksums, architecture, no host executor) and install it to
> `FLEET_INSTALL_DIR` (default `/opt/fleet`, the unit's `ExecStart` dir); the
> checkout is moved to the same release tag. No Go or Node on the box
> ([RELEASE-ARTIFACTS.md](RELEASE-ARTIFACTS.md)). `status`, `restart`, `stop`,
> and `logs` work off the installed binary alone.

## The env file (the one source of credentials)

**Every variable fleet reads is listed in [CONFIGURATION.md](CONFIGURATION.md)**
— its type, default, which section it belongs to, whether it is a secret,
and whether fleet's own env-file loader admits it. That page is generated from
the configuration registry ([CONFIG-REGISTRY.md](CONFIG-REGISTRY.md)), so it
cannot fall behind the code; this section covers how the file is used.

On a box, `fleet config explain [KEY…]` reports each setting's *configured*
value and its source. It draws from the sources it can see:
- the systemd unit's own environment, which outranks your shell
  (`--no-unit` leaves it out);
- your shell's environment;
- the env file;
- alias spellings (the bare `PERSONA` / `PERSONA_DEFAULT`);
- defaults, including one inherited from another setting.

It is not the running server's in-memory configuration: admin overrides made
in the app and values not yet reloaded are not shown. Secrets are always
redacted. With no KEY it lists every setting that is set or carries a note,
and `--all` lists every one, with its default.

**The legacy `CHAT_<X>`/`CUTLASS_<X>` spellings of `FLEET_<X>` settings were
removed** ([ADR-0106](adr/0106-remove-the-chat-cutlass-env-aliases.md)):
nothing reads them, so a setting spelled only that way falls back to its
default. When one is still set, `fleet serve` logs a warning at boot and
`fleet doctor` warns, each naming the `FLEET_` key to rename it to (with
`fleet env edit`, or `fleet config set-env` then `fleet config unset-env`);
`fleet config explain` notes it, and `fleet config set-env` refuses to write
one. The `CHAT_`/`CUTLASS_` names listed in [CONFIGURATION.md](CONFIGURATION.md)
(the chat rate limits, connector wire keys, script inputs) are settings of
their own and keep working.

A single **0600** env file (`FLEET_ENV_FILE`, default `.env.local`; on a box
typically `/etc/fleet/fleet.env`) carries every secret and connection string.
`deploy/fleet.service` `EnvironmentFile`s it, `fleet` parses the same file via
`config.Load`, and the `fleet` operator CLI reads it for MCP account secrets — so process
env and config-loaded values stay in sync. `bootstrap` writes/refreshes the
machine-managed keys in place (preserving your hand-edited lines and comments):

```
DATABASE_URL=postgres://fleet:…@127.0.0.1:5432/fleet?sslmode=disable
FLEET_CLIENT_CONFIG_DIR=/opt/fleet/client      # the client bundle checkout
FLEET_ENV_FILE=/etc/fleet/fleet.env            # so config.Load reads this same file
```

**One database.** fleet keeps both of its stores in the ONE database
`DATABASE_URL` names: the chat store in schema `chat`, the scheduler in schema
`sched` ([ADR-0098](adr/0098-one-database-two-schemas.md),
[ONE-DATABASE.md](ONE-DATABASE.md)). Each store's connections get its schema as
their only `search_path`, and each store creates its schema on first start.
`fleet serve`, `fleet validate-config`, the doctor and every operator CLI verb
resolve the DSN the same way (`config.ResolveDSNs`): `DATABASE_URL`, else the
`DB_*` parts (`DB_HOST`, `DB_PORT`, `DB_USER`, `DB_PASSWORD`, `DB_NAME`,
`DB_SSLMODE`; with `DB_NAME` set, user `fleet` and `localhost:5432`,
`sslmode=disable` by default). A CLI verb's `--*-database-url` flag wins, and a
flag naming the one database is given the store's schema. A verb refuses to
fall through to the bare localhost defaults when nothing is set.

**The two-database layout is gone (#2049).** The per-store keys
`FLEET_CHAT_DATABASE_URL`, `FLEET_SCHED_DATABASE_URL` and `SCHED_DATABASE_URL`
are no longer read: when one of them is still set (in the environment or the
env file), `fleet serve`, `fleet validate-config` and every DSN-using CLI verb
refuse with an error naming `DATABASE_URL`, `fleet doctor` fails the box, and
`bootstrap.sh` refuses to run. Set `DATABASE_URL` to the one database and
delete the old keys. With nothing configured at all, the bare default is one
database, user and database `fleet` on `localhost:5432`.

`fleet config set-env KEY` upserts one variable as exactly one line (value from
stdin or a hidden prompt; duplicates removed; `0600` and owner kept) — prefer
it over hand-editing. `fleet env` prints this file (the only one — the web UI reads it too) with
secret values masked, and `fleet env edit` opens it in an editor — `$EDITOR`, or an interactive
nano/vim/helix pick — restoring the 0600 mode afterwards; see
[ENV-CLI.md](ENV-CLI.md).

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
`CHAT_DISABLE_PROMPT_CACHE` / `CUTLASS_DISABLE_PROMPT_CACHE` spellings were
removed (ADR-0106) and are not read.

**Value parsing rules.** Env-file values follow standard dotenv semantics: an
`export ` prefix is tolerated; an **unquoted** value ends at the first ` #`
(inline comment) — quote any value that must contain a literal ` #`, e.g.
`DB_PASSWORD="p@ss #1"`; one layer of matching surrounding quotes is stripped
and the interior kept verbatim (`fleet mcp account set` quotes such values for
you). Quotes are also stripped from process-env values, so a
podman/docker `--env-file` deployment that keeps quotes in place behaves the
same. **Every** numeric/bool/duration knob fleet parses is strict — not just the ones
the config loader consumes (#1119 covered those; #1273 finished the job for the
knobs parsed elsewhere in the binary, such as the scheduler rate limits, the
Kata memory overhead, the agent-runtime thresholds and the SSE/notify/webpush
knobs). An **unset** knob gets its default, but a **set, malformed, or
out-of-range** value refuses to boot with one error naming every offending
variable, its value, the expected format, and — for a knob parsed outside the
loader — the package that reads it (`FLEET_LOCKDOWN_ONLY=enabled`,
`FLEET_MAX_COST_USD=5O` and `FLEET_SANDBOX_KATA_OVERHEAD_MB=lots` all refuse
rather than silently falling back to defaults). Booleans take `1/0, true/false,
yes/no, on/off`; durations take Go syntax (`30s`, `5m`, `1h30m` — a bare
number is an error).

Two things to know about that strictness:

- **A few kill-switches take the narrower Go `strconv.ParseBool` token set**
  (`1/0/t/f/true/false`, plus `TRUE`/`True`) because that is what their reader
  honors: `FLEET_DISABLE_PROMPT_CACHE`, `FLEET_DISABLE_OPENROUTER_MODELS`,
  `FLEET_SCHEDULED_AUTO_COMPACT`, `FLEET_PUSH_ON_TASK_COMPLETE`,
  `FLEET_PUSH_ON_APPROVAL_REQUEST`. `…=on` or `…=yes` is refused with a message
  saying so, instead of being read as `false` the way it used to be.
  `FLEET_CONTEXT_ELIDE_BEFORE_SUMMARY` takes the same token set and is refused
  the same way; it is on unless set to a false value.
- **One knob is deliberately lenient:** `FLEET_OTEL_SAMPLE_RATIO`. A malformed
  ratio does not stop the service (unset/unparseable/`NaN`/`≥1` all mean "sample
  everything", `≤0` means "sample nothing"), because refusing to boot over a
  tracing dial is worse than tracing too much. `fleet validate-config` still
  preflights it and reports a typo as a **warning** rather than a blocking
  failure.

Run `fleet validate-config` to preflight every knob — loader and
out-of-loader, blocking and advisory — before starting the service; config
hot-reload applies the same rules to the knobs it can reload (see
[CONFIG-RELOAD.md](CONFIG-RELOAD.md)).

**It reads the deployment the way the service does, not the way your shell
does.** On a provisioned box the unit's `UnsetEnvironment=` keeps
`FLEET_ENV_FILE` and `FLEET_CLIENT_CONFIG_DIR` out of an operator's shell, so
the preflight verbs (`validate-config`, `mcp test`, `eval`) resolve the env
file themselves and take the bundle directory from it — `--bundle-path` still
wins, then anything already in your environment, then the file. And because
rootless podman keeps one image store **per user**, **every** sandbox probe —
`podman info`, the image lookup, the OCI-runtime resolution, the allowlisted-egress
network helper, and the `/dev/kvm` gate — runs as the unit's `User=`, exactly as
`fleet status` and `fleet doctor` do, and says whose store the verdict is about.
That matters past the image: a runtime or network helper registered only in the
service user's `containers.conf` boots fine and would otherwise fail a check run
as root. Without both of those, a healthy
box preflighted as two blocking failures — a bundle that "does not exist" and a
sandbox image that was present all along.

**Less-travelled knobs** — the turn timeout, conversation retention, the
connection-pool sizing, the lockdown model list, the verifier and pinned
models and the rest — are in [CONFIGURATION.md](CONFIGURATION.md) with their
defaults; every one is optional, and the default is what an unset value means.

## The client-config checkout

fleet ships **no** client content; it loads a **client config bundle** from
`FLEET_CLIENT_CONFIG_DIR` (default `config/default`, the generic bundle; an
`--enable-service` install with no client bundle runs on a service-owned copy
of it staged at `/var/lib/fleet/bundle`, which `fleet update` refreshes — the
sandbox's SELinux relabel needs a source the unit can write, and the checkout
is not one). A real deployment checks out a client repo and points the
variable at it. `bootstrap
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
fleet bootstrap --enable-web [--domain <fqdn>]       # also enable the web tier (+ Caddy TLS with --domain); implies --enable-service
fleet bootstrap --tag vYYYY.MM.DD.N                  # install this signed release (default: the release the checkout contains)
fleet bootstrap --dry-run                            # print the plan; touch nothing
```

Under `--enable-service` (and `--enable-web`, which implies it) the credential env
file defaults to `/etc/fleet/fleet.env` — the path `deploy/fleet.service` reads —
so the one-command deploy writes secrets where the unit picks them up. Set
`FLEET_ENV_FILE` to override; plain local/dev runs still default to `.env.local`.

End to end, every run: uninstall Cockpit when `cockpit-ws` is installed
(`dnf -y remove 'cockpit*'`: the Vultr Fedora image ships its root-capable web
login on :9090, and fleet boxes are administered over ssh only;
`FLEET_KEEP_COCKPIT=1` keeps it; never fatal) → make SSH key-only (below) →
**download and verify the signed release** (runtime packages only — no Go or
Node; [RELEASE-ARTIFACTS.md](RELEASE-ARTIFACTS.md)) → ensure the 0600 env file →
resolve the client bundle (`--client-config`) → **the sandbox image** (the
release's signed image for a bundle with no Containerfile or the generic one;
otherwise built on the box with `scripts/build-sandbox-image.sh`, on top of the
verified release image when the Containerfile declares `FLEET_SANDBOX_BASE`;
skipped when the manifest pins a prebuilt `sandbox.image`) → provision the one
role and database (default `fleet`/`fleet`) idempotently via `\gexec` (local)
or validate `DATABASE_URL` (external) → write `DATABASE_URL` +
`FLEET_CLIENT_CONFIG_DIR` into the env file → optionally `enable --now` the
systemd unit. The local-mode role password is generated on the first run and
reused from the env file's `DATABASE_URL` afterwards.

With `--enable-web --domain <fqdn>`, bootstrap also names the box after that
domain, so prompts stop reading `root@vultr`. It renames only when the current
static hostname is an unset cloud-image default (`vultr.guest`, `localhost`,
`localhost.localdomain`); any other name, a short one like a laptop's included,
is left alone and bootstrap prints the `hostnamectl set-hostname <fqdn>` to run.
`FLEET_SET_HOSTNAME=1` forces the rename, `FLEET_SET_HOSTNAME=0` skips the step.
A rename runs `hostnamectl set-hostname`, writes
`/etc/cloud/cloud.cfg.d/99-preserve-hostname.cfg` (`preserve_hostname: true`,
`manage_etc_hosts: false`) when `/etc/cloud` exists so cloud-init does not undo
it, and replaces the old hostname on the `127.0.0.1`/`::1` lines in `/etc/hosts`
with `<fqdn> <short>`, keeping every other alias on the line (localhost names
are never replaced). When no line names the FQDN, it appends both loopback lines. It is idempotent,
honours `--dry-run`, never fails the bootstrap, and `fleet update` never renames
a box. (`scripts/lib/hostname.sh`; design note:
[`BOOTSTRAP-HOST-HYGIENE.md`](BOOTSTRAP-HOST-HYGIENE.md))

Bootstrap also makes SSH key-only. Vultr's cloud-init writes
`PasswordAuthentication yes`, so a fresh box takes root password guesses. When
`sshd` and `/etc/ssh/sshd_config.d` exist, bootstrap installs
`/etc/ssh/sshd_config.d/01-no-password-login.conf` (0600:
`PasswordAuthentication no`, `KbdInteractiveAuthentication no`,
`PermitRootLogin prohibit-password`), checks it with `sshd -t`, and reloads
`sshd` (or `ssh`). It skips with a warning when root and `/home/*` have no
authorized key, since that would lock everyone out; it removes the file again
when `sshd -t` fails, and warns when `sshd -T` still says passwords are on (an
earlier directive in `sshd_config` wins). `FLEET_KEEP_SSH_PASSWORDS=1` skips it.
It is idempotent, honours `--dry-run`, never fails the bootstrap, and
`fleet update` never touches sshd (`scripts/lib/ssh.sh`). Run as root,
`fleet status` and `fleet doctor` warn when sshd still accepts passwords.

## version — which build is this box running?

```
fleet version             # e.g. 2026.09.04.2 (a1b2c3d4e5f6)
fleet --version           # identical
```

Releases are **date-based** and tagged automatically on every green push to
`main` (`vYYYY.MM.DD.N`, several a day being normal), so the version string both
names a release and says how far past it this box is:

| Output | Meaning |
| --- | --- |
| `2026.09.04.2 (…)` | exactly that release |
| `2026.09.04.2+3.g<sha> (…)` | 3 commits past it — a binary built with `make build` on a development machine between releases (a box only ever runs a release) |
| `dev+g<sha> (…)` | the checkout has no release tags (fetched with `--no-tags`, or shallow) |
| `dev (…)` | an unstamped binary — built with a bare `go build` rather than `make build` |

Quote it verbatim in an issue or a security report; do not parse it. The same
full string appears in the login banner and in `fleet_version` on the admin
health summary (`GET /admin/health-summary`). `/api-info`'s `fleet_version`
carries the **version only**, without the revision, and `/healthz` carries no
version at all — it is a liveness probe. Full contract:
[`VERSIONING.md`](VERSIONING.md).

## update — install a new release in place

```
sudo fleet update                       # the newest published release: verify, swap, restart, gate, roll back on failure
sudo fleet update --tag v2026.11.07.3   # a specific release (older than running: add --allow-downgrade)
sudo fleet update --no-pull             # reinstall the release the checkout is at
fleet update --dry-run                  # print the plan
fleet update --check                    # read-only: running vs newest release, commits/bundle behind
sudo fleet release rollback             # put the previous binary (fleet.prev) back, restart, gate
```

`update` (ported from the `moc`/`gig` pattern) moves the fleet checkout to the
release **tag** it installs and `git pull`s the client-config checkout, then:

1. **verifies the release before anything uses it** (`fleet release fetch`):
   `SHA256SUMS` signed by fleet's release workflow on `main` (keyless cosign,
   pinned identity), the binary's checksum and own signature, `release.json`
   naming the requested tag, the binary's architecture, no host executor, a web
   bundle — and no downgrade unless `--allow-downgrade`;
2. **sandbox image**: a bundle with no Containerfile, or the generic one, runs
   the release's signed image (verified, pulled by digest, tagged as the
   bundle's `sandbox.tag`); a bundle with its own `sandbox/Containerfile` is
   rebuilt on the box when the Containerfile or resolved tag changed, the tag is
   missing from the service user's store, the release's base digest moved (for
   a `FROM ${FLEET_SANDBOX_BASE}` Containerfile), or the image is older than
   `FLEET_SANDBOX_MAX_AGE_DAYS` (default 7; `--sandbox-max-age <d>`; `0`
   disables) — the max-age backstop rebuilds with `--no-cache` so an unchanged
   Containerfile cannot freeze its packages; a bundle that pins
   `sandbox.image` skips this step;
3. **installs** (`fleet release install`): the running binary is kept as
   `fleet.prev`, the new one renamed over `fleet` atomically, `systemctl
   restart` (the service migrates on start — `update` runs no migrations), then
   a wait for `/readyz` (`FLEET_HEALTH_TIMEOUT`, default 90 s). If it is not
   ready, `fleet.prev` goes back, the service restarts on it, and `update`
   exits non-zero saying it rolled back (the failed binary is kept as
   `fleet.failed`).

The sandbox step runs before the install on purpose: if the image cannot be
verified, pulled or built and the resolved ref is absent from the service
user's store, `update` stops with the old binary and old service untouched.

`update` also refreshes the one shipped host file that no unit owns:
`/etc/profile.d/fleet-motd.sh` (the login-banner hook). It is shipped content
with no operator-tunable parts, rewritten only when it differs from
`deploy/`, and never fatal — a box that cannot write them keeps updating and
says so. (`fleet doctor` repairs the same file — see below — so either verb
converges it.)

If the pull changed `update.sh` itself, the script **re-execs the fresh copy**
(bash holds the pre-pull inode open, so the fix would otherwise only land on the
*next* update).

**The drain is the binary's.** `systemctl restart` sends `SIGTERM`, and
`cmd/fleet` handles it gracefully — it flips `/healthz` and `/readyz` to `503`,
lets in-flight chat turns **and** running scheduled tasks finish within
`FLEET_SHUTDOWN_GRACE_SECONDS` (default 30s, bounded by the unit's
`TimeoutStopSec`), then force-cancels stragglers and exits 0. `update`'s own
drain gate (above the steps) refuses to start while work is in flight unless
`--wait` or `--force`. While it waits it holds the update drain marker
(`<FLEET_ENV_FILE>.update.drain`), which stops the old process admitting new
work; the install stops the old process, removes the marker, and only then
starts the new binary, so the new process does not come up draining (and fail
its `/readyz` gate). See [RELEASE-ARTIFACTS.md](RELEASE-ARTIFACTS.md).

> **Honest about "zero-downtime."** This is *zero-downtime-ish* / brief-blip, not
> truly zero-downtime. fleet is a **single process on one box**, so there is an
> unavoidable window from when the old process finishes draining and exits until
> the new one binds its listeners and passes `/readyz`, during which new requests
> get a `503` (while draining) or a connection refusal (during the swap). What
> *is* graceful: **in-flight work is drained, not killed**, and a release that
> cannot become ready is rolled back automatically.

**Rolling back on purpose.** `sudo fleet release rollback` swaps `fleet.prev`
and `fleet` and restarts behind the same `/readyz` gate (running it twice undoes
itself). To go back further, or to realign the checkout too, install an older
release: `sudo fleet update --tag <tag> --allow-downgrade` (the newest ten
releases stay downloadable).

**There is no on-box build.** `fleet update` installs a published, signed
release and nothing else; `--from-source` was removed
([ADR-0101](adr/0101-remove-the-on-box-build.md)) and is refused with a message.
A box that cannot reach GitHub, or finds no published release, stops without
changing anything: retry once the network is back, or name a release with
`--tag`. A box whose installed fleet predates `fleet release` is told to run
`sudo scripts/bootstrap.sh --enable-service` again, with the flags it was
installed with: bootstrap is idempotent and installs the release over the
existing install. To run unreleased code, build it on a development machine
with `make build`.

## status — is the box healthy? (quick, read-only)

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
Run as root on a box with `sshd`, it also warns (never fails) when `sshd -T`
reports `passwordauthentication yes` or `permitrootlogin yes`.
DSN passwords are redacted in the output.

> **Sandbox check + the dedicated service user.** The systemd unit runs `fleet`
> as a dedicated **`fleet`** user with **rootless Podman** (its own subuid range +
> image store), so the sandbox image lives in *that* user's store. `fleet
> status` run as **root** therefore reports the sandbox image as not runnable
> (root's Podman can't see it) even though the service runs it fine — a false
> negative. Verify the sandbox as the service user instead, e.g.
> `sudo -u fleet env XDG_RUNTIME_DIR=/run/fleet podman run --rm <ref> true`, or
> just confirm a chat turn executes a `run_python` tool call. Use `--no-sandbox`
> to skip the check when running `status` as root. (`fleet doctor` below does
> not have this false negative — it runs its sandbox smoke *as the service
> user*.)

## doctor — diagnose AND repair box-level drift

```
sudo fleet doctor              # diagnose + fix + restart services if needed
sudo fleet doctor --check      # diagnose only, change nothing; exit 1 if anything is off
sudo fleet doctor --no-restart # fix but never restart services
fleet doctor --dry-run         # print the checklist; touch nothing (no root needed)
```

Where `status` only *reports*, `doctor` (a wrapper over `scripts/doctor.sh`)
walks every box-level prerequisite fleet depends on and **fixes what it can in
place** — it exists to kill the version lottery, where boxes provisioned at
different times drift apart and each drift surfaces later as a confusing
production-only bug. The pass covers, in order:

1. **Toolchain** — `git`/`curl`/`jq`/`podman`/`psql`/`python3` present
   (installed via dnf when missing). No Go or Node: the box installs signed
   release binaries.
2. **Package currency** — disables dnf repos whose metadata is unreachable
   (they abort *every* transaction on strict dnf builds), then upgrades the
   fleet-critical set (`podman crun passt conmon containers-common caddy`)
   to the box's repo-latest. Rootless-podman behavior differs
   meaningfully across versions; holding boxes at repo-latest beats debugging
   per-box combinations.
3. **Rootless podman for the `fleet` service user** — subuid/subgid ranges,
   `/var/lib/fleet` + `~/.config/containers` ownership (root-owned leftovers
   from debugging break every podman call), the cgroupfs `containers.conf`,
   `/run/fleet`, and a `podman info` probe **as the service user**. A stale
   pause process is reported with the exact repair (see below); doctor never
   runs `podman system migrate` itself.
4. **Installed artifacts** — functional drift of `fleet.service` / the
   `fleet-backup` and `fleet-maintenance` service + timer pairs vs `deploy/`
   (reinstall + `daemon-reload`), the retired Next.js tier (`fleet-web.service`,
   its drop-in, start shim and `/opt/fleet/web` — stopped, disabled and removed
   when still present, since fleet serves the web UI on its port; ADR-0091), the `/etc/profile.d/fleet-motd.sh` login-banner
   hook (installed from `deploy/` when missing or drifted; the banner itself is
   rendered live by `fleet motd`, so only the hook can go stale), and the
   `/usr/local/bin/fleet` symlink (a stale *copy* there shadows every update).
5. **Configuration** — `/etc/fleet/fleet.env` exists, root-owned `0600`, with
   `OPENROUTER_API_KEY` + both DB DSNs; `APP_SESSION_SECRET` for the web UI
   (carried over from the retired `fleet-web.env`, else generated); and the
   fleet-managed `/etc/caddy/Caddyfile` matches the shipped layout
   (`scripts/lib/caddyfile.sh`: the `/v1` API, `/api-info`, the A2A agent
   card and `/triggers/*` to the orchestrator, `/webhooks/*` to chat, the
   rest to the web tier). A drifted fleet-managed file — typically one written
   before the API routes shipped, which 404s every API call at the web tier —
   is rewritten (timestamped backup, `caddy validate`, `systemctl reload
   caddy`); `--check` reports it; a Caddyfile fleet did not write is never
   touched (advisory only when it routes no `/v1`).
6. **Services** — `postgresql` (when a local unit exists), `fleet`,
   `caddy` active; then the `/healthz` + `/readyz` probes, and
   `https://<domain>/api-info` fetched **through** Caddy (pinned to this box
   with `--resolve`) — the proof that the API is routed, not just that the
   process is up.
7. **Scheduled backups** — `fleet-backup.timer` installed, enabled *and
   active*, and its last run succeeded. A missing timer is an *advisory*, never
   a failure (an operator who backs up at the volume or hypervisor layer is not
   misconfigured) and doctor never installs it for you — its hint names the
   opt-in, `sudo fleet timers install --backup`
   ([docs/TIMERS.md](TIMERS.md)); a timer whose last run
   **failed** is a failure, because a box that looks covered and is not is the
   worse state. See [`docs/BACKUP_RESTORE.md`](BACKUP_RESTORE.md).
   The same step checks disk headroom and **disk hygiene**. It reclaims root's
   package cache (dnf) once it passes 1 GiB: dnf fills it as root, and the
   maintenance timer runs as `fleet`, so the timer never reaches it. (The Go
   and npm caches it once also swept came from on-box builds, which are gone —
   ADR-0101.) It caps the journal at 1G
   (`/etc/systemd/journald.conf.d/60-fleet-journal-cap.conf`) unless you set
   `SystemMaxUse`/`RuntimeMaxUse` yourself, and it *reports* images left in root's podman
   store. Doctor never deletes those images itself. `--check` only reports.
   See [`docs/DOCTOR.md`](DOCTOR.md), "Disk hygiene".
8. **Sandbox smoke** — `podman run --rm --network=none <image> true` **as the
   `fleet` user** (the image lives in *that* user's rootless store).
9. **Release + checkout** — reports the installed release against the newest
   published one (`fleet release latest`) and against the release the checkout
   is at (they differ after a `fleet release rollback`, or a hand edit), and the
   modified/untracked paths in the checkout. Report-only: installing stays
   `fleet update`'s job; doctor never deploys. (Step 4 also reads the installed
   binary's build info: a host executor compiled in, or a missing web bundle,
   is a failure.)

Exit codes: `0` healthy (or everything fixed) · `1` problems remain.

### Stale podman pause process

Rootless podman keeps a small "pause process" per user, found through
`/run/fleet`. When that process is gone or belongs to an old session — most
often after a podman/crun upgrade, a `pkill -u fleet`, or root-side debugging
in the fleet user's podman dirs; a reboot clears it — every podman call as
`fleet` fails with podman's own hint:

```
Error: invalid internal status, try resetting the pause process with "podman system migrate"
```

Doctor (step 3's `podman info`, or the step-8 sandbox smoke) reports this as a
failure — and holds any service restart that run wanted, since fleet could not
start its sandboxes on that store — but **never resets it itself**. The reset
is `podman system migrate` as the `fleet` user, and it stops every running
container of that user; the sandboxes run `--rm`, so under a live fleet it
deletes the whole warm sandbox pool while the process keeps handing out the
dead handles, and every chat turn and task fails with `no such container`
until fleet restarts. (Doctor used to run it on every pass; that is how
fleetdev lost its pool.)

So the fix is: **when nothing is running, stop fleet, run `podman system
migrate` as the `fleet` user, and start fleet again**, then re-run `sudo fleet
doctor --check`. When is "safe" is a judgment call for whoever is on the box —
check `sudo fleet sched task list --status running` (and `leased`) first.
Things that bite:

- Stopping the unit removes `/run/fleet` (its `RuntimeDirectory=`), which
  podman needs as `XDG_RUNTIME_DIR` — recreate it (`0700`, owned by `fleet`)
  before migrating.
- `/var/lib/fleet` is `0700`: change into it as `fleet` (inside the `sudo -u
  fleet` shell), not before `sudo`.
- Stopping `fleet` also stops the web UI (the same process serves it).
- Make sure fleet has really stopped (no `fleet` process left, including one
  under another supervisor) before migrating, and start it again even if the
  migrate fails.

Admins also get a **read-only** version of this report in the web UI —
**Settings → Admin → System checks** — run from inside the fleet process (DBs, disk
headroom, subuid ranges, sandbox image, unit states), with the on-box fix
command shown next to every failing check and an explicit "Run deep checks"
button for the sandbox smoke. See [`docs/DOCTOR.md`](DOCTOR.md).

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
fleet start                   # systemctl start the fleet unit
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

## notes — the shared wiki, and the proposal queue

Notes are the workspace's shared reference pages: an agent reads them by slug,
and an operator curates them from here. They live in the **sched** database, so
these verbs take `--database-url` (else `DATABASE_URL`, else the env file).

```
fleet notes set <slug> --title "..."   # body on stdin; bumps the version
fleet notes get <slug>                 # print one note
fleet notes list [--all] [--json]      # slug · version · status · title
fleet notes rm <slug>                  # archive (also: del/delete/archive)
```

`list` shows live notes; `--all` includes archived ones. `rm` archives rather
than destroys, so a slug can be brought back.

**The proposal queue is the part with an approval semantic.** An agent that
wants to change a note does not write it: it files a *proposal*, which stays
pending until an operator decides. That decision is this pair of verbs, and
there is no third outcome — a proposal is either published or rejected with a
stated reason:

```
fleet notes proposal publish <id> [--note "..."]     # apply it; the note's version bumps
fleet notes proposal reject  <id> --reason "..."     # refuse it; --reason is REQUIRED
```

Both take `--by` to record who decided (default `admin`). Publishing prints the
resulting slug and version. An unknown or already-decided id exits **2** (the
not-found code the other admin verbs use), so a script can tell "no such
proposal" from an operational failure (5).

There is **no** `notes proposal list` verb today — the CLI covers the decision,
not the browsing. To find the ids awaiting a decision, read the orchestrator's
`GET /notes/proposals?status=pending` with an admin credential (the proposal
queue, like the decisions, is admin-only).

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

fleet keeps every conversation in schema **chat**, every scheduled task in schema
**sched**, and the accounts both name in schema **core** — all three in the one
database (`DATABASE_URL`). `fleet backup` takes them with `pg_dump -Fc` /
`pg_restore`:

```
fleet backup                          # ONE dump of chat, core and sched (one snapshot): fleet-all-<UTC>.dump
fleet backup --db=chat --out /backups # dump just chat (+ core) into /backups
fleet restore --db=all FILE.dump      # restore the whole fleet from a fleet-all dump (one transaction)
fleet restore --db=chat FILE.dump     # restore one store (--clean --if-exists; overwrites it)
```

`backup` prints each dump path on stdout. `restore` has no default `--db`: it
overwrites a live database, so the target is named explicitly. Only `--db=all`
is self-contained; a sched-only restore needs the accounts already in the
database (restore chat first) — see BACKUP_RESTORE.md. Connection params,
including the password, are passed to the child processes through the
environment, never argv.

A **daily timer** (`deploy/fleet-backup.timer`, one whole-fleet dump to
`/var/backups/fleet` with a 30-day prune) is installed and enabled by
`bootstrap --enable-service` unless you pass `--no-backup-timer`, `fleet
doctor` advises when no timer is present, and `sudo fleet timers install
--backup` sets it up on an existing box in one command
([docs/TIMERS.md](TIMERS.md)). A same-host dump protects against
logical loss — a bad migration, an accidental delete — **not** against losing
this host or volume, and it does not capture attachment/upload files. See
**[`docs/BACKUP_RESTORE.md`](BACKUP_RESTORE.md)** for the full recovery runbook,
the honest scope, and the round-trip verification procedure.

## sched task export · import — moving scheduled tasks between boxes

`fleet sched task export > tasks.json` writes a versioned envelope of the box's
`scheduled` tasks; `fleet sched task import < tasks.json` writes them back,
**preserving ids** (so a re-import of the same envelope is idempotent) and the
full row, not just the portable definition. That is what makes it the cross-box
migration tool — for definition-only, name-keyed moves (git-tracked job
definitions) use `fleet task export|import` instead.

Because ids are preserved, an import can land on a row this box already has,
which is a write over **live** state rather than a create. The policy (#1267):

```
fleet sched task import < tasks.json                    # definition restore; refuses a status collision
fleet sched task import --replace-status < tasks.json   # restore surgery: the envelope's status wins
```

- A task id that does **not** exist here is inserted verbatim, status included.
- A task id that **does** exist, whose status differs from the envelope's, is
  refused — the whole import aborts before writing anything, naming the task.
  Without that, re-importing an envelope exported while a task was `scheduled`,
  after that task ran to `success`, would rewrite `success` → `scheduled` with
  the stale `scheduled_for`; the due sweep would re-queue it and the task's
  external side effects (emails, MCP writes) would run again.
- `--replace-status` is the explicit opt-in to that rewrite. Use it to restore a
  wiped box from an envelope; know that re-queueing a finished task re-runs its
  side effects.
- A task that is **running or leased** here is refused under either flag, and
  `lease_owner` / `lease_expires_at` are never importable onto an existing row.
  An import cannot null, steal, or fabricate a lease.
- An envelope carrying a transient status (`leased`, `running`, either paused
  state) is rejected at validation: those name lease/pause state no import can
  honor.

## sched task set-owner — giving a task an owner

A task's owner (`created_by`) decides whose hosted connections its runs can
mount, who gets it in the owner digest, and who may edit it. To reassign one —
for example a task an admin-key clone left ownerless before that clone kept the
source's owner:

```
fleet sched task set-owner <task_id> <username>   # also: PUT /tasks/{task_id}/owner {"username": "..."}
```

The task, the not-yet-finished later occurrences of its recurrence chain and
the chain's last occurrence (which a parked chain is replayed from) change
owner. Occurrences spawned afterwards inherit it, and finished occurrences in
between keep theirs. The API form also refuses an owner for whom the
`mcp_selection` of any of those rows cannot resolve. The CLI writes the
database directly, so it cannot run that check. See
[`docs/TASK-OWNERSHIP.md`](TASK-OWNERSHIP.md).

## Where the sandbox build fits

The execution sandbox is a **per-client bundle artifact**: each bundle ships its
own `sandbox/Containerfile` — or none. Each release publishes the generic
bundle's image, signed (`ghcr.io/elcanotek/fleet-sandbox`); a bundle with no
Containerfile, or the generic one, runs it — `bootstrap` and `update` verify it
and pull it by digest. A bundle with its own Containerfile is built on the box,
on that verified image when it declares `FLEET_SANDBOX_BASE`
([RELEASE-ARTIFACTS.md](RELEASE-ARTIFACTS.md)); `update` rebuilds it when the
Containerfile, the manifest's `sandbox.tag` or the release base changed, the tag
is missing from the service user's image store, or the installed image aged past
`FLEET_SANDBOX_MAX_AGE_DAYS` (default 7 — see
[`docs/SANDBOX-IMAGE-FRESHNESS.md`](SANDBOX-IMAGE-FRESHNESS.md)); `status`
verifies the resolved image runs.
Registry
publish stays opt-in — set `sandbox.image` in the bundle manifest to a prebuilt
ref and all three steps consume that instead of building.
