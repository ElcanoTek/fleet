# Web-tier shutdown — why `systemctl restart fleet-web` was dumping core

The public Next.js tier (`deploy/fleet-web.service`) segfaulted on **nearly
every stop**, writing hundreds of megabytes of core dumps to
`/var/lib/systemd/coredump`. This note records what was actually wrong, what
this repo fixed, what is *not* ours to fix, and — importantly — one plausible
theory that measurement **refuted**, so nobody re-derives it.

## Symptom

`/var/lib/systemd/coredump` held ~793 MB of `node-22` dumps from uid 983
(`fleet-web`), clustered on deploy days: Aug 19 13:59 / 16:36 / 17:27,
Aug 20 10:44, Aug 21 05:42 / 10:52 / 11:23. Every one lines up with a service
restart; none occurred mid-request. `Restart=always` meant the tier always came
back, so nothing user-visible ever broke — the only signal was disk.

Three distinct faults were hiding in that pile.

## Fault 1 — the npm wrapper segfaulting on its own child (FIXED)

`ExecStart` was `/usr/bin/npm run start`, so the cgroup held **two** processes:
npm, and the `next-server` it spawned.

On `systemctl stop`, systemd's default `KillMode=control-group` SIGTERMs
*everything* in the cgroup. next-server exits; npm — which also installs its own
signal forwarding — then tries to relay the signal to a child it has already
reaped, and crashes inside `uv_kill` → `node::Kill`, called from
`ProcessWrap::OnExit`. A core dump per restart, from a process whose only
remaining job was to exit.

**Fix:** run next directly, no wrapper.

```ini
ExecStart=/usr/bin/node node_modules/next/dist/bin/next start -H ${FLEET_WEB_HOST}
```

npm bought nothing here — it only re-read `package.json` to exec this same
command. Removing it deletes this crash outright and lets SIGTERM reach
next-server with nothing in between to relay it.

Two details the wrapper had been carrying, now carried explicitly:

- **The loopback bind.** `web/package.json`'s start script was
  `next start -H ${FLEET_WEB_HOST:-127.0.0.1}` — a *shell* default that no
  longer runs. The unit now sets `Environment=FLEET_WEB_HOST=127.0.0.1`
  **before** its `EnvironmentFile=`, so the default holds and
  `/etc/fleet/fleet-web.env` can still override it. This matters: `next start`
  with no `-H` binds `0.0.0.0` and would expose :3000 past Caddy.
- **The port.** Still `PORT` from the env file — `next start` reads it directly,
  exactly as it did under npm.

`web/package.json`'s `start` script is unchanged and still fine for local use;
the live e2e path (`scripts/e2e-boot-server.sh`) already invoked
`npx next start` directly, so it was never affected.

## Fault 2 — SIGABRT after a stop that overran its deadline (FIXED)

Two dumps (Aug 19 15:05) were `SIGABRT`, not `SIGSEGV`. Cause: Fedora ships a
`systemd-system.conf` drop-in setting `TimeoutStopFailureMode=abort`, so when a
stop overruns `TimeoutStopSec=30s` systemd **aborts** the process — which dumps
core — instead of killing it.

**Fix:** state the escalation in the unit rather than inheriting the distro's
choice — at the precedence level that actually wins.

```ini
TimeoutStopSec=30s
TimeoutStopFailureMode=kill
```

The unit body says this, but it is **not sufficient on Fedora**: the distro's
`abort` lives in the global `/usr/lib/systemd/system/service.d/` drop-in
directory, and drop-ins are read *after* the unit body, so it overrides the
unit's own line (verified on Fedora 44 — `systemctl show fleet-web` reported
`abort` with the unit line present). The fix therefore also ships
`deploy/fleet-web.service.d/10-timeout-kill.conf`, a per-unit drop-in that
restates `kill` at the precedence level that wins. Install both files.

A stop that overran its deadline is a hang to diagnose from the journal, not a
crash to dissect from a memory image. SIGKILL reports the same timeout and
writes no dump. (`TimeoutStopFailureMode` needs systemd ≥ 246; older systemd
logs an unknown-key warning and keeps its default.)

Note this fixes the *dump*, not whatever caused the overrun — which remains
unexplained and was rare (twice out of seven events). If it recurs, the journal
around the stop is now the whole record.

## Fault 3 — next-server segfaulting in teardown (NOT ours)

The residual and most common dump: `next-server (v16.3.0)` taking `SIGSEGV` at
address `0x0` — a null-pc jump during V8/libuv teardown *after* SIGTERM, i.e.
after it has stopped serving. Nothing in this repo can patch it.

What we checked, so the next person doesn't repeat it:

- **Not fixed upstream.** Next.js 16.3.1 and 16.3.2 release notes contain
  nothing about shutdown, SIGTERM, exit, teardown, or crashes — they are
  Turbopack, routing, image and caching fixes. **Bumping `next` is not the
  remedy**, so this repo stays on 16.3.0 rather than implying a fix it does not
  get.
- **Points at the node build.** With the *same* Next 16.3.0 build, repeated
  SIGTERM under **node 22.22.2** exits cleanly every time (status 143, no
  dump). The box that crashes runs **node 22.23.1** (`nodejs22-22.23.1-2.fc44`).
  That is suggestive, not conclusive — the two environments differ in more than
  node — but it is the strongest lead, and it makes the remedy an **operator**
  action, not a code change: pin an earlier 22.x, or move to 24.x, and see
  whether the teardown dump stops.

Until then the dump's *cost* is bounded rather than its cause fixed — see
`LimitCORE=0` below.

## Why `LimitCORE=0` on this unit

The web tier no longer writes core dumps at all. The security reason is the
primary one, and it stands on its own regardless of these crashes: **a core
dump is a full memory image**, and this process holds the deployment's shared
secrets — `CHAT_SERVER_TOKEN`, `ORCHESTRATOR_SERVER_TOKEN`,
`APP_SESSION_SECRET` — plus every in-flight user's session and request body.
Persisting that to disk on each crash writes down precisely what fleet's
credential invariant says must never reach logs or disk. 793 MB of such images
had accumulated unnoticed.

The practical reason is secondary: fault 3 is an exit-path crash on some node
builds, so its dumps describe teardown rather than a serving fault, at ~130 MB
each.

This **does not mask the failure.** systemd still logs the signal and the unit
still records the failed stop, so `systemctl status fleet-web` and `journalctl
-u fleet-web` show it exactly as before. Only the memory image is declined. No
crash signal is papered over: `SuccessExitStatus=143` (added after live
verification) names only Next's *deliberate* exit code — Next catches SIGTERM
and calls `exit(143)` instead of dying by the signal, which systemd's default
clean set does not cover, so every clean stop was logged as a failed one.
SIGSEGV/SIGABRT exits still fail loudly. To capture a dump deliberately while
debugging:

```sh
systemctl edit fleet-web     # [Service] / LimitCORE=infinity
# ...reproduce, collect with coredumpctl, then remove the override
```

## Refuted theory: the graceful-drain hypothesis

Worth recording because it is the obvious explanation and it is **wrong** for
this version.

Next.js's self-hosting guide documents a graceful shutdown: "The Next.js server
will finish in-flight requests and execute any pending `after()` callbacks
before exiting. Platforms should allow a configurable drain period (10-30
seconds is recommended)." This app proxies four **open-ended** `text/event-stream`
routes (chat turn, stream reattach, summarize, orchestrator task stream) whose
responses stay open as long as a browser tab is attached. So the tidy story is:
an attached tab pins an in-flight request open forever → the drain can never
finish → 30s timeout → SIGABRT. It even explains fault 2.

**Measured, and it does not happen.** Against a production `next start`
(Next 16.3.0), SIGTERM with an open-ended SSE response in flight exits in
**~105 ms** — versus ~24 ms fully idle. Both are far below any drain period,
and the figure was the same whether the stream honored a shutdown signal or
ignored it entirely. Next 16.3.0 simply does not block its exit on an
in-flight streaming response.

Consequences, which is why this section exists:

- There is **no SSE-drain hang to fix**, so no application-side "abort our
  streams on SIGTERM" machinery was added. It would have touched four hot
  request paths to fix a problem that does not exist, and shipping it as a
  segfault fix would have been an unsupported claim.
- Fault 2's rare 30s overrun therefore has **some other, still-unknown cause**.
  `TimeoutStopFailureMode=kill` bounds its cost; the journal is the lead if it
  returns.

## What landed

| Fault | Status | Change |
| --- | --- | --- |
| npm `uv_kill` segfault, every stop | **fixed** | `ExecStart` runs node directly; `FLEET_WEB_HOST` default moved into the unit |
| SIGABRT on an overrun stop | **dump eliminated** | `TimeoutStopFailureMode=kill` — unit body **plus** the `fleet-web.service.d/10-timeout-kill.conf` drop-in Fedora's global `abort` drop-in requires (cause of the overrun still unknown) |
| next-server teardown segfault | **not fixable here** | no upstream fix in 16.3.1/16.3.2; lead is the node build — operator action |
| ~793 MB of dumps, secrets in each | **fixed** | `LimitCORE=0`, with the failure still logged |
| Clean stops logged as failures | **fixed** | `SuccessExitStatus=143` names Next's deliberate SIGTERM exit code; crash signals still fail |

**Live verification (fleetdev, Fedora 44, node 22.23.1, Next 16.3.0):** with
the new unit + drop-in installed, `systemctl restart fleet-web` produced a
fully clean stop — "Deactivated successfully", no core dump, no segfault in
the journal, tier back to Ready in ~150ms and serving. The residual teardown
segfault (fault 3) did not fire on this stop; whether removing the npm relay
also eliminates it in practice, or it recurs on some stops, is now visible in
the journal without the dump pile.

The changed unit reaches a running box the usual way — `sudo fleet doctor`
(step 4 reinstalls a unit that drifted from `deploy/`) or `sudo fleet update` —
followed by `systemctl daemon-reload`. Existing dumps are reclaimed with
`coredumpctl purge`; run `coredumpctl info` first if you still want a backtrace
from one.
