# Doctor — box-level diagnose + repair (`fleet doctor` + Settings → Admin → Doctor)

## What shipped

Two halves of one feature, split by privilege:

1. **`fleet doctor`** (wraps `scripts/doctor.sh`, patterned on chat's
   `chat doctor`) — the root-privileged pass that diagnoses **and repairs**
   box-level drift in place: toolchain floors, fleet-critical package currency
   (with broken-dnf-repo quarantine), the service user's rootless-podman
   prerequisites (subuid/subgid, dir ownership, `containers.conf`, stale pause
   namespaces), systemd unit drift vs `deploy/`, the `/usr/local/bin/fleet`
   symlink, env-file shape/permissions, the fleet-managed
   `/etc/caddy/Caddyfile`'s layout (the `/v1` API + inbound webhooks must
   route to the Go backends, not 404 at the web tier — a drifted
   fleet-managed file is rewritten from `scripts/lib/caddyfile.sh` with a
   timestamped backup, `caddy validate` and `systemctl reload caddy`; an
   operator's own Caddyfile only gets an advisory), service health + the
   `/healthz` + `/readyz` probes **and** `https://<domain>/api-info` fetched
   *through* Caddy (`--resolve` pinned to this box, so it tests the proxy's
   routing rather than DNS), the scheduled-backup timer, and a sandbox smoke run **as
   the `fleet` user** (fixing
   `fleet status`'s documented root-runs-podman false negative). Modes:
   repair (default, root), `--check` (read-only diagnosis, exit 1 on drift),
   `--no-restart`, `--dry-run` (print the checklist; no root). Source
   freshness is *report-only* — pulling/rebuilding stays `fleet update`'s job,
   and doctor never runs migrations.
2. **`GET /admin/doctor`** (`internal/boxdoctor` + the Settings → Admin →
   Doctor panel) — the **read-only** sibling run from inside the fleet
   process, for admins who don't have a shell open: chat + sched DB pings,
   `OPENROUTER_API_KEY` presence (never the value), disk headroom on the data
   dir and the podman image store (warn ≥ 85 %, fail ≥ 95 %), subuid/subgid
   ranges for the *process* user, `podman info`, sandbox image presence,
   sibling unit states (`fleet-web`, `postgresql`, `caddy`), whether the
   Caddyfile routes the API (a fleet-managed one that predates the `/v1`
   routes is a **fail**; an operator's own front that routes no `/v1` is a
   warn), and the scheduled-backup timer. Every warn/fail
   carries the on-box fix command (almost always `sudo fleet doctor`).
   `?deep=1` additionally launches a throwaway
   `podman run --rm --network=none <image> true` — the definitive smoke, but
   seconds-to-minutes slow, so the UI puts it behind an explicit
   "Run deep checks" button and the server serializes runs behind a mutex.

Division of labor across the three health verbs:

| Verb | Privilege | Mutates? | Scope |
|---|---|---|---|
| `fleet status` | none | never | quick in-process checks (bundle, env, DBs, sandbox, unit) |
| `fleet doctor` | root — `--dry-run` needs none, but **`--check` still does** (it probes the service user's rootless podman and reads 0600 env files) | **repairs** | everything status checks **plus** packages, podman prereqs, unit drift, env files — and fixes them |
| `fleet doctor --node` | root — **except `--node --check`**, the one read-only path needing none (it is what `fleet update --check` calls) | **repairs** | the node toolchain ONLY: install `nodejs<major>` + `-npm` per `web/.nvmrc`, stamp `FLEET_NODE_BIN`, assert the resolved interpreter **and that an npm belongs to it**, exit |
| `/admin/doctor` (UI) | admin session | never | doctor's *diagnosable-from-the-process* subset, with fix hints |

## Design decisions

- **`--node` is a seam, not a convenience flag.** `scripts/update.sh` is an
  updater, not a provisioner, so it must not grow its own `dnf install nodejs`;
  but an update that dies because the box is a node major behind sends the
  operator away to find the repair command. `--node` lets update call *this*
  code path — the one implementation of the node install — and then re-resolve.
  It is scoped to the node blocks deliberately: a full doctor pass adopts
  drifted units, a write `fleet update` performs only behind explicit consent
  (`--adopt-units`), so invoking one from inside update would launder a
  consent-gated write. It asserts the interpreter **and its npm** separately,
  because on Fedora npm is its own package whose shebang names its interpreter
  absolutely: a box can have node 24 while `npm` still builds on 22, which is
  how the web tier was built on the old major through green updates (see
  [`NODE-TOOLCHAIN-HANDOFF.md`](NODE-TOOLCHAIN-HANDOFF.md), "The npm
  interpreter pin"). `--node --check` drops the root requirement because it
  installs nothing and `fleet update --check` (a documented no-root dev-box
  probe) calls it. Full story:
  [`NODE-TOOLCHAIN-HANDOFF.md`](NODE-TOOLCHAIN-HANDOFF.md).

- **Repairs are shell, diagnosis is Go.** The repair pass needs root and is
  genuinely shell-shaped (dnf, useradd, install, systemctl), so it lives in
  `scripts/doctor.sh` like bootstrap/update — wrapped by `cmdDoctor` exactly
  as `update` wraps `update.sh`, with a graceful fallback to the in-process
  `status` checks on a checkout-less box. The web endpoint deliberately
  **cannot** repair: the service user has no privilege to, and a browser
  button that mutates the host would be a footgun. The UI says so and names
  the command instead.
- **`fleet doctor` is no longer an alias of `status`.** It was
  `case "status", "doctor"`; the alias is now a distinct verb. The old
  read-only contract survives as `fleet doctor --check` (deeper than status).
- **The secrets env file is read with `grep`, never sourced** — sourcing
  `/etc/fleet/fleet.env` would execute arbitrary content as root on a
  tampered box. Enforced by test.
- **The sandbox smoke runs as the service user** with the unit's exact
  `HOME`/`XDG_RUNTIME_DIR`, because the image lives in the `fleet` user's
  rootless store — probing as root reports a false negative (see
  `docs/OPERATORS.md`).
- **Unit drift compares functional bodies only** (comments/blank lines
  stripped) — same rule as `update.sh`'s adoption path, so doc churn never
  nags. Doctor *reinstalls* the shipped unit in fix mode; update's
  interactive adoption flow is unchanged. The `fleet-backup` service/timer
  ride the same check, and are optional-if-absent: a box without them is
  skipped there (the backups check owns that verdict), not nagged twice.
  Reinstalling one of them does **not** queue the post-fix restart — that
  restart bounces `fleet` (and try-restarts `fleet-web`), which is
  user-visible chat downtime, and neither backup unit has any bearing on the
  running app: the oneshot is not running, and `daemon-reload` is all systemd
  needs to pick up a rewritten timer's schedule.
- **A missing backup timer is an advisory; a failing one is a failure**
  (#966). Backups were the one operational essential doctor did not look at,
  so a box with none could report `38 ok, 0 advisories`. Absence stays
  advisory because backing up at the volume or hypervisor layer is a real
  answer — and for the same reason doctor does not install the timer, even in
  fix mode: that is `bootstrap`'s call, opt-out-able with
  `--no-backup-timer`. A timer whose last run failed *does* fail the box: the
  `oneshot` exits non-zero when a dump fails its integrity check, and a box
  that looks covered and is not is worse than one that is visibly bare. Both
  halves check `is-active` as well as `is-enabled`: `is-enabled` reads only the
  install symlink, so an enabled-but-stopped timer fires nothing while its
  service's `Result` still reads `success` — a false clean.
- **`is-active` cannot see a crash loop, so restart churn is its own check.**
  Both app units run `Restart=always`, so a unit that dies is active again ~5s
  later and `is-active` reports `active` the whole time — a unit can
  crash-loop indefinitely while every other unit check stays green. The
  `restarts: <unit>` checks read `NRestarts`, which is the right property and
  not just a convenient one: it counts only restarts driven by `Restart=`, and
  a **manual `systemctl restart` resets it to zero**, so an ordinary deploy
  cannot raise a false alarm while a self-inflicted restart accumulates a count
  that only human intervention clears. 0 is ok, 1–4 an advisory, ≥5 a failure.
  A non-numeric or absent property is a skip, never a guessed verdict.
  `Result` enriches the detail but never drives the verdict alone: it describes
  the *last* run, so on a unit that was restarted after an incident it still
  names the old failure, and a check that nags about a resolved event forever
  gets ignored.
- **What restart churn does *not* catch**, stated because it is the
  neighbouring bug: a process that segfaults during a **manual** stop — the
  `fleet-web` teardown crash in
  [`WEB-TIER-SHUTDOWN.md`](WEB-TIER-SHUTDOWN.md) — is restarted by operator
  action, so `NRestarts` stays 0. That fault is a configuration matter, which
  the next check covers.
- **`fleet-web stop policy` asserts a resolved directive, not a file.** It
  reads what systemd resolved for `TimeoutStopFailureMode` and expects `kill`.
  A config check earns its place among health checks here because this exact
  directive sat in the unit body doing nothing for a full release: a
  distro-global `/usr/lib/systemd/system/service.d/` drop-in overrides a unit
  body (Fedora sets `abort` there), so the unit file said the right thing and
  every file-comparing check passed. Only the resolved value distinguishes the
  two. It is a **warn** here while the same assertion in `doctor.sh` is a
  **fail** — deliberately: `doctor.sh` installs the drop-in and *then* checks,
  so a wrong value there means a repair did not hold, whereas this endpoint
  cannot repair anything and the consequence is hygiene rather than downtime
  (`LimitCORE=0` keeps the memory image off disk either way). Don't harmonise
  them without that changing.

## Deviations / deliberately deferred

- The `/admin/doctor` endpoint lives on the chat mux (like
  `/admin/health-summary`), so it is intentionally not part of the
  orchestrator's `docs/openapi.yaml` surface.
- `doctor.sh` targets dnf hosts (the deployment posture); on non-dnf hosts the
  package steps degrade to advisories rather than guessing at apt equivalents.
- No scheduled/automatic doctor runs — an operator (or the admin UI) invokes
  it. Wiring `--check` into cron/systemd timers was deferred until wanted.
- The in-process endpoint does not read `/etc/fleet/fleet.env` permissions or
  package currency — those need root visibility; reporting a degraded guess
  would be worse than deferring to `sudo fleet doctor`.

## Tests

- `internal/admincli/doctor_test.go` — `--dry-run` checklist smoke (CI-safe on
  any host), `--help`, unknown-flag refusal, and the load-bearing-strings
  guard (dnf `skip_if_unavailable`, `upgrade` not `install`, the bootstrap
  subuid range + `containers.conf` body, root-owned-0600 enforcement,
  never-source-the-env-file).
- `internal/boxdoctor/boxdoctor_test.go` — per-check verdicts with fake
  files/pings/env; summary/healthy invariants; the backup-timer verdict matrix
  (absent/disabled → warn, failed last run → fail).
- `internal/httpapi/doctor_test.go` — handler returns the report through the
  server's own store ping, deep defaults off, 405 on non-GET.
- `web/.../doctor/doctor.test.tsx` (vitest) + `web/e2e/mocked/admin-doctor.spec.ts`
  (Playwright) — quick-on-load / deep-behind-button, fix commands rendered,
  sub-nav entry.
