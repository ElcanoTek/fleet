# The node toolchain handoff (`fleet update` ⇄ `fleet doctor --node`)

Design note for the change that made an existing box cross a node major without
the operator knowing the right command order in advance.

Companion reading: [`WEB-TIER-SHUTDOWN.md`](WEB-TIER-SHUTDOWN.md) ("Choosing the
interpreter" — why the major is declared once in `web/.nvmrc`, and why
`ExecStart` is a shim) and [`DOCTOR.md`](DOCTOR.md).

The last section, **"The npm interpreter pin"**, is a follow-up: the change
below pinned `node` for the build and not `npm`, and the "Not verified" note it
closes with named the exact gap that left. Read it for the current behaviour of
the build gate.

## What was broken

The web tier moved to node 24. `web/.nvmrc` is the one declaration point, and
three entry points read it — but they disagreed about whose job installing node
was:

| script | on a box a major behind |
| --- | --- |
| `scripts/bootstrap.sh` | installs `nodejs<major>` + `-npm` |
| `scripts/doctor.sh` | installs `nodejs<major>`, stamps `FLEET_NODE_BIN` |
| `scripts/update.sh` | installed nothing — it `die`d |

Four separate defects fell out of that, each reproduced before it was fixed.

**1. The documented order was the order that does not work.**
`docs/OPERATORS.md` taught `fleet bootstrap → fleet update → fleet status /
fleet doctor` — doctor *after* update. On a node-22 box that sequence hits the
refusal on the second step. No document anywhere stated the working order.

**2. Running doctor first would not have helped either.** Both scripts read the
floor from the **checkout's** `web/.nvmrc`, and `doctor.sh` deliberately never
pulls. On a box provisioned before `.nvmrc` existed, a doctor run *before* the
update reads the old hardcoded floor (`20`), sees node 22, and passes. The only
sequence that ever worked was update (fails) → doctor → update — and it worked
only because the failed update had already fast-forwarded the checkout, which
is the state the refusal message explicitly denied.

**3. The gate ran after the expensive work.** The refusal lived inside step 4,
i.e. after step 1 fast-forwarded the checkout and after step 3 could spend two
to three minutes rebuilding the sandbox image and then prune the superseded
layers. It still printed *"nothing has been built or installed yet"*. The
script states the standard it was violating: a gate that can abort must run
before the first side effect, or it is not a gate.

**4. `--dry-run` and `--check` could not answer the question they exist for.**
The plan printed `would resolve node >= web/.nvmrc` **without ever calling the
resolver**, then closed with a green `✓ fleet rebuilt at <sha>` banner and
exit 0 — on a box where the real run refuses to build anything. `fleet update
--check` counted commits and nothing else, so it printed "run `fleet update` to
upgrade" and exited 0 while recommending a command that could not succeed.

## What shipped

**`scripts/doctor.sh --node`** — a scoped repair that runs only step 1's node
blocks (install `nodejs<major>` and `nodejs<major>-npm`, stamp
`FLEET_NODE_BIN`, assert the resolved value) and exits. `--node --check` is the
read-only probe and is the one doctor path that does **not** require root,
because `fleet update --check` calls it and that command is documented as a
dev-box probe.

Scoped, rather than "update just calls doctor", for a specific reason: a full
doctor pass adopts drifted systemd units, and `fleet update` performs that write
only behind explicit consent (`--adopt-units`). Invoking a full doctor from
inside update would launder a consent-gated write through the node repair.

**`scripts/update.sh`** now hands a shortfall to that path and re-resolves,
instead of dying:

```
! no node >= 24 (web/.nvmrc) — have v22.22.2.
» repairing the node toolchain first: scripts/doctor.sh --node (the checkout
  was fast-forwarded; nothing was built, installed or restarted)
  ↻ installed node v24.x at /usr/bin/node-24
  ↻ pointed fleet-web at /usr/bin/node-24 — read back from /etc/fleet/fleet-web.env
✓ node toolchain repaired in place — no re-run needed
✓ web tier will build+run on /usr/bin/node-24 (v24.x)
```

The gate also **moved**, to just after both pulls and just before the sandbox
rebuild. That position is forced from both sides: it cannot run earlier because
the floor is declared in the checkout being pulled (defect 2), and it must not
run later because everything after it is expensive or destructive (defect 3).
The refusal messages now name the state the box is actually in — the checkout
*has* been fast-forwarded by then — rather than claiming an untouched box.

`--dry-run` runs the resolver for real (it is pure and needs no root) and
reports either the resolved interpreter or the blocker, and no longer closes a
dry run with the real run's success banner. `fleet update --check` shells out to
`doctor.sh --node --check` and exits non-zero when the box cannot build the web
tier.

**`scripts/lib/node-version.sh`** — the gate resolves an interpreter and then
says the web tier "will build+run" on it. The build half of that was not true on
the layout the whole design targets. `fleet_node_build_path` prefixed PATH with
the resolved binary's *directory*, on the reasoning that this makes npm's
`#!/usr/bin/env node` shebang pick it up. Fedora's streams are
parallel-installable **into the same directory**, so `/usr/bin` holds both
`node-24` and the default stream's `node` — and prefixing `/usr/bin` still
resolves the bare name to the old major. Measured with a stand-in of that exact
layout: a build "on node-24" ran under node 22. It now puts a private shim
directory holding a single `node` symlink in front. The shim is `mktemp -d`,
never a predictable `/tmp` name: a fixed world-writable path at the head of
root's PATH during a build is a hijack, not a convenience. Both callers remove
it, on the failure path too, and `update.sh` now reports the version the build
*actually* ran under, read back from the build PATH.

**`scripts/fleet-upgrade.sh`** — audited as the same class of problem, and it
was worse: it never hits the node wall because it never builds the web tier at
all, but `deploy/fleet-web.service` carries `BindsTo=fleet.service`, so its
`systemctl restart fleet` **stopped the web tier and never brought it back**,
after which it printed `✓ fleet upgraded + healthy`. Its readiness gate polls
the Go backend's `/readyz` and says nothing about the web tier. It now restarts
`fleet-web` on both the upgrade and rollback paths, asserts `systemctl
is-active`, and the final banner reports what it measured.

## What an adversarial pass over the finished diff found

Worth recording, because three of the findings were in the *new* code and one of
them was this change set committing the exact fault it was written to remove.

- **The new `fleet-upgrade.sh` banner over-claimed.** It reported "backend
  /readyz 2xx" from a variable that only tracked the web tier, so on the
  no-systemd and no-curl paths — where the gate is skipped — it printed two
  specific measurements, neither taken, one line under a message saying the gate
  had been skipped. The run now tracks whether the gate actually ran and says
  "health NOT verified" when it did not.
- **`doctor.sh --node --check` reported a false failure when unprivileged.**
  Relaxing the root requirement was correct; not distinguishing "the 0600 env
  file is unreadable" from "the stamp is unset" was not. A healthy, correctly
  stamped box reported *"FLEET_NODE_BIN is unset — the tier would serve on the
  wrong major"*, and `fleet update --check` turned that into a non-zero exit on
  every production box where the operator did not `sudo`. Reporting a fault you
  could not observe is the same error as reporting a success you could not.
- **`--node` installed only half of what it advertised.** Four documents and its
  own plan line said it installs `nodejs<major>` **and** `nodejs<major>-npm`; the
  versioned npm only ever came from the missing-tool loop, which is skipped when
  `command -v npm` succeeds — and it does succeed on the target box, because the
  OLD stream's npm is on PATH. Same parallel-stream trap, one package over.
- **A scoped `--node` run exited before the restart a full pass would do**, so it
  printed "repaired" while the live `fleet-web` still served on the old
  interpreter. It now advises the restart it cannot perform itself.
- **The tests read machine-global state.** They probed this box's real
  `/etc/fleet/fleet-web.env`, so a stale stamp there failed them for reasons
  unrelated to the code — which is exactly what happened. `WEB_ENV_FILE` is now
  overridable and the tests point it at a path that does not exist.

## Judgment calls

**Should `update` install node itself? No.** `update.sh` is an updater and
`doctor.sh` is the provisioner; that separation is stated in doctor's own header
and is worth keeping. Growing a `dnf install` inside update would make three
scripts that install node instead of two — the same multiplication
`scripts/lib/node-version.sh` exists to prevent. Delegating keeps one
implementation while removing the operator's homework.

**Should the handoff ask first? It does not**, and that is a deliberate
asymmetry with `--adopt-units`. Adopting a unit *overwrites an operator's
hand-edit*, so it needs consent. This installs a parallel-installable
interpreter package and stamps `FLEET_NODE_BIN` — a value `update.sh` already
rewrote unconditionally later in the same run. Refusing to install the
interpreter it is about to point the tier at would be incoherent. Boxes whose
node comes from nvm, NodeSource or a base image opt out with
`--no-node-repair` (`FLEET_UPDATE_NODE_REPAIR=0`), which restores the old
refusal — now with the exact one-command fix in the message.

**`--dry-run` still exits 0, even when it reports a blocker.** The repo's three
other scripts all treat `--dry-run` as a plan printer that succeeds if the plan
printed, and `--check` as the diagnostic exit code — `doctor.sh --check` already
exits 1 on problems. Making `update.sh --dry-run` the one exception would be a
surprise, so the blocker is unmissable in the plan and the plan points at
`fleet update --check`, which does exit non-zero. A reviewer could reasonably
want the opposite; this is a consistency call, not a claim that exit 0 is more
informative.

**Every new success claim is a read-back.** `doctor.sh` reported "pointed
fleet-web at X" on `upsert_web_env` returning 0, which proves a file was
written and not that the tier resolves to X; it now re-reads the value through
the same last-wins reader systemd's `EnvironmentFile` uses. `update.sh` does not
trust `doctor.sh --node`'s exit code either — it re-runs the resolver, because
the claim being made is "this box can now build the web tier", and only the
resolver proves that.

## What was verified, and what was not

Reproduced first, on a box that genuinely runs node 22 with `web/.nvmrc`
declaring 24: the original refusal, the green-and-exit-0 dry run, `doctor.sh
--help`'s stale `Node >= 20` floor, `fleet-upgrade.sh --help` printing raw shell,
and `update --check`'s exit-0 recommendation.

The fresh-operator path was then exercised end to end with a fake `dnf` on PATH
that installs a versioned `node-24` stub: the gate detects the shortfall, hands
off, the versioned stream installs, `FLEET_NODE_BIN` is stamped and read back,
and the update continues without a re-run.

Every new assertion was then broken on purpose to check it fails: a repair path
that reports success while installing nothing is caught by the re-resolve; a
writer that stores the wrong `FLEET_NODE_BIN` is caught by the read-back; a
focus effect that fires on every close is caught by the new users-page test; and
removing either `restart_web_tier` call fails the fleet-upgrade guard. One
assertion — `--node`'s decisive exit — survives neutralising the re-resolve
alone, because the failure rollup covers it independently; it fails when both
are neutralised, which is the honest statement of what that guard is worth.

**Not verified:** a real Fedora `dnf` transaction (this box has no dnf), so the
exact file layout of `nodejs<major>` / `nodejs<major>-npm` is taken from
`bootstrap.sh`'s long-standing package list rather than observed; and anything
requiring systemd as PID 1 — unit adoption, the `BindsTo` stop, and the
`fleet-upgrade.sh` web-tier restart are code changes backed by the unit file and
by the equivalent, long-standing logic in `update.sh`, plus a fake-`systemctl`
harness, not by a live run. `go test` caches results and does not track the shell
scripts these tests exec, so any mutation check on a script needs `-count=1` —
one of ours silently passed from cache before that was noticed.

## The npm interpreter pin (follow-up, 2026-08-22)

The change above ends with a "Not verified" note that turned out to name the
exact hole it left: *"a real Fedora `dnf` transaction (this box has no dnf), so
the exact file layout of `nodejs<major>` / `nodejs<major>-npm` is taken from
`bootstrap.sh`'s long-standing package list rather than observed."* The layout
matters, and the guess was wrong.

### What was broken

Every `fleet update` on the target box printed the gate's success and then npm's
contradiction of it, a few lines apart in the same run:

```
✓ web tier will build+run on /usr/bin/node-24 (v24.x)
...
✓ refreshed /usr/local/bin/fleet-web-start.sh
npm warn EBADENGINE Unsupported engine {
npm warn EBADENGINE   package: 'fleet-web@0.1.0',
npm warn EBADENGINE   required: { node: '>=24' },
npm warn EBADENGINE   current: { node: 'v22.23.1', npm: '10.9.8' }
npm warn EBADENGINE }
```

The update was otherwise green: the warning is the *only* symptom, and it is a
warning, so a build that engine-check-failed the whole point of the node bump
completed and deployed.

`fleet_node_build_path` put a private shim directory holding a single `node`
symlink at the head of PATH, and its docstring justified that with "npm's
shebang is `#!/usr/bin/env node`". On Fedora it is not. From
[`nodejs22.spec`](https://src.fedoraproject.org/rpms/nodejs22/blob/rawhide/f/nodejs22.spec)
(`%install`, "Adjust npm scripts to use the renamed interpreter"):

```
readonly SHEBANG_ERE='^#!/usr/bin/(env\s+)?node\b'
readonly SHEBANG_FIX='#!%{_bindir}/node-%{node_version_major}'
```

It *has* to be absolute. The streams are parallel-installable, so a relative
`env node` shebang would make `npm-22` run under whichever stream happens to be
the default — npm and its interpreter must be welded together for either stream
to be usable at all. The consequence for us is that `/usr/bin/npm` →
`npm-22` → `.../npm-cli.js` begins `#!/usr/bin/node-22` and runs on node 22
**no matter what PATH says**. A `node` symlink at the head of PATH moves `next`
and every other `env node` shebang onto the resolved interpreter and cannot move
npm itself.

So the tier was built on 22 and served on 24 — the same shape as the two bugs
this document already records (a PATH edit that looks like it pins the
interpreter and does not), one link further down. Three smaller faults fell out
of the same root:

1. **The read-back measured the half that already worked.** `update.sh` reported
   the build interpreter as `PATH="$_build_path" node -v` — i.e. it asked the
   symlink it had just created. That is not a measurement of anything; it could
   only ever agree. The component that decided the build was never asked.
2. **npm was never gated.** The gate resolved *node* and then claimed the tier
   "will build+run" on it. On Fedora npm is a separate package
   (`nodejs<major>-npm`) with its own interpreter binding, so "a node 24 is
   installed" does not imply "npm will build on node 24" — and `doctor.sh`'s
   `command -v npm` probe cannot see the difference, because the old stream's
   npm satisfies it. This document already recorded that trap for the
   *install*; the *build* had it too.
3. **`next` was one npm behaviour away from the same fate.** npm launches
   lifecycle scripts with the package's `node_modules/.bin` dirs and its
   node-gyp shim prepended to the *inherited* PATH (`@npmcli/run-script`'s
   `set-path.js`) and does **not** inject node's own directory, so the shim
   survives into `next build`. Verified by reading the installed npm rather
   than assumed, because if npm did prepend `dirname(process.execPath)` the
   `node` symlink would be defeated for every child process too.

### What shipped

**`scripts/lib/node-version.sh`** gained the pin and the measurement:

- `fleet_resolve_npm_cli NODE_BIN` — the `npm-cli.js` belonging to `NODE_BIN`.
  For a versioned interpreter it looks only for the stream's own `npm-<major>`
  and **refuses** rather than falling back to the unversioned `npm`: that is
  the default stream's npm, i.e. precisely the wrong answer. A box with
  `nodejs24` and no `nodejs24-npm` is a real, repairable state, so it gets a
  refusal naming the one missing package instead of a silent downgrade.
- `fleet_node_build_path` now writes `npm` and `npx` wrappers into the shim
  alongside the `node` symlink, each `exec`ing the resolved interpreter against
  the matching CLI js — the one form of the answer no shebang can override. It
  also builds a shim **unconditionally**, where it used to return early when
  the interpreter's directory already resolved `node` to it: that early return
  answered the node question and left the npm one to PATH, which on a box with
  two streams in one directory is the bug above.
- `fleet_npm_node_version BUILD_PATH [CWD]` — the read-back, asked of npm
  (`npm version --json`, which is read-only with no version argument) rather
  than of PATH.
- `fleet_node_build_path_cleanup` is marker-gated (`.fleet-node-shim`) now that
  the shim holds more than one file, and still refuses to remove a directory
  holding anything it did not write.

**`scripts/update.sh`** — `node_probe` resolves npm separately and returns a
distinct code (`3`) for "a *versioned* node qualifies, its npm does not", so the
gate can hand *that* to the same `doctor.sh --node` repair (which installs both
packages) and say which half was short. Versioned only: that is the
parallel-stream layout, where the bare `npm` provably belongs to a different
interpreter and the missing package has a name. On a single-node layout
(Debian/nodesource/nvm) the same miss means npm ships in a shape the probe
cannot read, and there the `node` pin is genuinely sufficient because npm's
shebang really is the relative `env node` — refusing that box would be inventing
a blocker out of an unread file. It says so and builds, and the read-back is
what reports the outcome. The build step reads the interpreter back from npm and
**refuses** below the `web/.nvmrc` floor rather than warning:

```
✓ web tier will build+run on /usr/bin/node-24 (v24.x)
✓ web tier will build with /usr/lib/node_modules_24/npm/bin/npm-cli.js — pinned to that interpreter, not to PATH
...
✓ web app built on v24.x (origin=…, app=…)
```

**`scripts/doctor.sh`** checks and repairs the pairing in step 1, so
`fleet doctor`, `fleet doctor --node` and `fleet update --check` all fail on it
— the last of those being the command `fleet update` tells operators to run.
Installing the versioned npm deliberately does **not** set `restart_needed`: the
unit runs `next start` under `FLEET_NODE_BIN` and never invokes npm, so the
repair changes the next *build*, not the running tier.

**`scripts/bootstrap.sh`** gates on the same pair before building and reports
the version the build actually ran under.

### Judgment calls

**Refuse, rather than build with a warning.** npm already warns (EBADENGINE) and
that warning is exactly what nobody acted on for as long as this bug lived.
`web/package.json` declares `"node": ">=24"`; building anyway produces a bundle
whose engine constraint was violated, and the failure mode is a runtime surprise
in the served app rather than a red build. The refusal names the one `dnf`
command that fixes it and leaves the previous web deployment untouched.

**Pin, rather than ask the operator to fix their `alternatives`.** Switching the
box's default stream would fix `npm` for us and change the interpreter for every
other consumer of `node` on that machine. The build pins what the build needs and
touches nothing global — the same reasoning as the `node` shim.

**Blocker only where the diagnosis is nameable.** The refusal above fires for a
versioned interpreter and nowhere else, for the same reason `doctor.sh` advises
rather than fails on a single-node layout: "I could not resolve an npm-cli.js" is
an observation about a file this code cannot read, not evidence that the build
would use the wrong interpreter. Turning every unread layout into a blocked
update would be the mirror image of the bug — a claim the box cannot support,
pointed the other way.

**A read-back that cannot be asked is a warning, not a refusal.** If
`npm version --json` yields nothing, the build proceeds and says the interpreter
is UNVERIFIED. Reporting a fault you could not observe is the same error as
reporting a success you could not — a rule this document already states, applied
in the other direction.

### What was verified, and what was not

The parallel-stream layout is now **reproduced** rather than assumed, in
`internal/admincli/scripts_node_npm_test.go`: a fixture bindir holding
`node-22`, `node-24`, `npm-22`, `npm-24`, unversioned `node`/`npm` pointing at
the default stream, and fake `npm-cli.js` files carrying absolute shebangs. The
regression test first asserts that the **old** mechanism still fails on that
fixture (a bindir prefix lands on 22), so a fixture that quietly stopped
reproducing the layout cannot make the test vacuous, then asserts the new build
PATH lands on 24. Both new assertions were broken on purpose and observed to
fail: dropping the `npm` wrapper, and letting `fleet_resolve_npm_cli` fall back
to the unversioned `npm`.

Exercised end to end against a stand-in `/usr/bin/node-24` + `/usr/bin/npm-24`
on a node-22 box: the passing two-line gate, the `no npm belongs to it —
install nodejs24-npm` blocker, `doctor --node --check`'s new pass line, and — the
decisive one — an `npm-cli.js` whose shebang names a nonexistent interpreter,
which cannot be executed directly (`cannot execute: required file not found`)
and runs fine through the pin. The read-back was checked against a wrapper that
*lies* about its version (a fake `node-24` that execs node 22): it reported
`v22.22.2`, the version npm actually ran under, which is the property the old
`node -v` probe did not have.

**Not verified:** a real `dnf install nodejs24-npm` (this box still has no dnf),
and a real `npm ci && npm run build` under two genuinely different node majors —
this container has one interpreter, so the pin is proven by shebang override and
by the read-back, not by two successful builds. The Fedora shebang rewrite is
quoted from the packaging source rather than observed on a live box.
