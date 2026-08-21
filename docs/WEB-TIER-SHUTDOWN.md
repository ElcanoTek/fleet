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

**Fix:** get the supervisor out of the cgroup.

```ini
ExecStart=/usr/local/bin/fleet-web-start.sh
```

npm bought nothing here — it only re-read `package.json` to exec this same
command. Removing it deletes this crash outright and lets SIGTERM reach
next-server with nothing in between to relay it.

That `ExecStart` first named node directly
(`/usr/bin/node node_modules/next/dist/bin/next start -H ${FLEET_WEB_HOST}`) and
now names a shim, for a reason unrelated to this crash — *which* node runs; see
[Choosing the interpreter](#choosing-the-interpreter). **The shim is not the
wrapper coming back.** npm's fault was *staying alive* as a supervisor and
relaying a signal to a child it had already reaped; the shim `exec`s, so the
shell is replaced by node and the cgroup holds exactly one process. Verified:
same pid, `next-server`, SIGTERM → `exit(143)` in 15 ms.

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

**What the drop-in is and is not worth.** It decides *how* an overrun stop
dies — SIGKILL rather than SIGABRT. It is **not** what keeps the core dump
away: `LimitCORE=0` does that on its own, for every signal, because
systemd-coredump stores a dump only "when the related process resource limits
(`RLIMIT_CORE`) are sufficient" (systemd-coredump(8)) — and the kernel's
pipe-handler exception does not change that, since systemd-coredump re-checks
the limit itself. So a box missing the drop-in has a correctness and hygiene
gap, not a returning 130 MB-per-restart dump pile. Earlier revisions of this
note and of `doctor.sh` said otherwise; that was wrong.

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
  action rather than a code fix: run a different node build and see whether the
  teardown dump stops.

  The repo now targets **node 24** (Active LTS; 22 is maintenance-only), which
  is the forward version of that experiment — see
  [Choosing the interpreter](#choosing-the-interpreter). Be clear about what
  that is and is not: node 24.19.0 was verified here to build the web tier,
  pass its suite, and survive five start→SIGTERM cycles cleanly
  (`exit(143)`, no segfault). It is **not** verified to fix fault 3, because
  fault 3 does not reproduce off the affected box — 22.22.2 exits cleanly here
  too. Confirming it needs a run on a box that actually crashes.

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
| npm `uv_kill` segfault, every stop | **fixed** | no supervisor left in the cgroup: `ExecStart` is an `exec`ing shim; `FLEET_WEB_HOST` default moved into the unit |
| SIGABRT on an overrun stop | **fixed** | `TimeoutStopFailureMode=kill` — unit body **plus** the `fleet-web.service.d/10-timeout-kill.conf` drop-in Fedora's global `abort` drop-in requires, and `doctor.sh` now asserts the *resolved* value (cause of the overrun still unknown) |
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

## Choosing the interpreter

Fault 3 pointed at the node build, which made "run a different node" an
operational requirement — and that turned out to carry its own trap.

Fedora's node packages are **parallel-installable**. `dnf install nodejs24`
gives you `/usr/bin/node-24`, while `/usr/bin/node` keeps pointing at whichever
stream the release designated default (22 on F44). Two consequences:

- `dnf upgrade nodejs` can **never** cross a major. A box can be told to
  upgrade node indefinitely and stay on 22 — which is what happened here.
- A hardcoded `ExecStart=/usr/bin/node` would keep serving the old major even
  with the new one installed. Installing 24 and still running 22 is the same
  shape of failure as a systemd directive that never takes effect.

systemd cannot resolve this itself: it does **not** expand a variable used as
the executable — `ExecStart=${FLEET_NODE_BIN} …` is taken as a literal path
(verified with `systemd-analyze verify`). So the resolution happens inside a
fixed program, `deploy/fleet-web-start.sh`. It prefers `$FLEET_NODE_BIN`
(written by bootstrap/doctor/update), falls back to `node` on PATH for distros
shipping a single unversioned interpreter, and logs the interpreter and version
it chose — so "what node is actually serving?" is a `journalctl` grep instead of
an investigation. That question was hard to answer while diagnosing fault 3.

The major is declared **once**, in `web/.nvmrc`:

| consumer | how it reads the major |
| --- | --- |
| CI (6 workflow jobs) | `actions/setup-node` `node-version-file: web/.nvmrc` |
| `scripts/bootstrap.sh` | installs `nodejs<major>`, stamps `FLEET_NODE_BIN` |
| `scripts/doctor.sh` | `NODE_FLOOR` from `.nvmrc`; installs the versioned package; asserts the *resolved* `FLEET_NODE_BIN` |
| `scripts/update.sh` | refuses to build the web tier on an older node; refreshes the shim + `FLEET_NODE_BIN` |
| `web/package.json` | `engines.node` |

Before this, CI pinned `'22'` as a literal in six jobs across four workflow
files while
`doctor.sh`'s floor said `20` and the box ran whatever `dnf install nodejs`
meant; nothing reconciled the three. Dependabot could never have caught that
drift — it updates action *refs*, not the inputs passed to them, and nothing it
watches covers an OS package. `.github/dependabot.yml` records that at the top
so the next person does not go looking for a bot that was never watching.

## Verifying it, rather than assuming it

The through-line of every fault above is the same mistake: **a directive was
written and never checked against what systemd resolved.** The original
`TimeoutStopFailureMode=kill` sat in the unit body looking correct and did
nothing on Fedora for a full release. The first attempt to guard it compared
*files* — which cannot see a stale checkout, a drop-in installed to the wrong
directory, a later-sorting drop-in in the same directory, or a forgotten
`daemon-reload`.

So `scripts/doctor.sh` asserts the resolved value instead:

```sh
systemctl show -p TimeoutStopFailureMode --value fleet-web.service   # must print: kill
```

`kill` passes; anything else fails with a pointer to `systemctl cat fleet-web`
(which shows the unit plus every drop-in in application order, so the winner is
visible). An empty result means pre-246 systemd, which has no such property —
reported as an advisory, not a failure. Run that one command by hand after any
deploy that touches the unit; it is the only output that proves the fix is live.

Installation is spread across three places, each with a different consent rule,
so all three had to learn about the drop-in:

| path | installs the drop-in? |
| --- | --- |
| `sudo fleet doctor` | **yes**, by default (it is the repair path; `--check` only reports) |
| `sudo fleet update --adopt-units` | yes |
| `sudo fleet update` (on a TTY) | prompts; `y` adopts the unit *and* the drop-in |
| `sudo fleet update` (`--yes` / non-interactive) | no — warns with a copy-paste hint |
| `scripts/bootstrap.sh --enable-service` | yes, on every run |

That bootstrap row used to read "only when `fleet-web.service` was absent",
which inverted the intent: bootstrap is re-runnable, and a box provisioned
before the drop-in shipped *already has* the unit — so the one case that needed
the drop-in was the one case that never got it. The install is now separate
from the unit's "if absent" branch. Overwriting it is safe because it carries a
single directive and no operator-tunable content; unit drift itself stays
`doctor.sh` / `update.sh`'s business.

The same resolved-value assertion also runs in-process, so it reaches the admin
UI rather than only a root shell: `internal/boxdoctor` reports it as
`fleet-web stop policy` in Settings → Admin → Doctor, alongside new
`restarts: <unit>` checks that catch a unit systemd is restarting *by itself*
(`is-active` cannot — `Restart=always` makes a crash-looping unit read as
`active`). Note that restart churn would **not** have caught the crashes in
this document: they happened on operator-initiated stops, and a manual
`systemctl restart` resets `NRestarts` to zero. The stop-policy check is the
one that covers this fault; see [`DOCTOR.md`](DOCTOR.md).

Note also that `doctor.sh` does **not** pull, so it reconciles against whatever
`deploy/` the box's checkout holds. On a stale checkout the shipped file does
not exist and the file-level step skips silently — which is exactly why the
resolved-value assertion above runs regardless.

The changed unit reaches a running box the usual way — `sudo fleet doctor`
(step 4 reinstalls a unit that drifted from `deploy/`) or `sudo fleet update` —
followed by `systemctl daemon-reload`. Existing dumps are reclaimed with
`coredumpctl purge`; run `coredumpctl info` first if you still want a backtrace
from one.
