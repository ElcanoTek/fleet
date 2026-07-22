# Doctor — box-level diagnose + repair (`fleet doctor` + Settings → Admin → Doctor)

## What shipped

Two halves of one feature, split by privilege:

1. **`fleet doctor`** (wraps `scripts/doctor.sh`, patterned on chat's
   `chat doctor`) — the root-privileged pass that diagnoses **and repairs**
   box-level drift in place: toolchain floors, fleet-critical package currency
   (with broken-dnf-repo quarantine), the service user's rootless-podman
   prerequisites (subuid/subgid, dir ownership, `containers.conf`, stale pause
   namespaces), systemd unit drift vs `deploy/`, the `/usr/local/bin/fleet`
   symlink, env-file shape/permissions, service health + the `/healthz` +
   `/readyz` probes, and a sandbox smoke run **as the `fleet` user** (fixing
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
   ranges for the *process* user, `podman info`, sandbox image presence, and
   sibling unit states (`fleet-web`, `postgresql`, `caddy`). Every warn/fail
   carries the on-box fix command (almost always `sudo fleet doctor`).
   `?deep=1` additionally launches a throwaway
   `podman run --rm --network=none <image> true` — the definitive smoke, but
   seconds-to-minutes slow, so the UI puts it behind an explicit
   "Run deep checks" button and the server serializes runs behind a mutex.

Division of labor across the three health verbs:

| Verb | Privilege | Mutates? | Scope |
|---|---|---|---|
| `fleet status` | none | never | quick in-process checks (bundle, env, DBs, sandbox, unit) |
| `fleet doctor` | root (except `--check`/`--dry-run`) | **repairs** | everything status checks **plus** packages, podman prereqs, unit drift, env files — and fixes them |
| `/admin/doctor` (UI) | admin session | never | doctor's *diagnosable-from-the-process* subset, with fix hints |

## Design decisions

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
  interactive adoption flow is unchanged.

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
  files/pings/env; summary/healthy invariants.
- `internal/httpapi/doctor_test.go` — handler returns the report through the
  server's own store ping, deep defaults off, 405 on non-GET.
- `web/.../doctor/doctor.test.tsx` (vitest) + `web/e2e/mocked/admin-doctor.spec.ts`
  (Playwright) — quick-on-load / deep-behind-button, fix commands rendered,
  sub-nav entry.
