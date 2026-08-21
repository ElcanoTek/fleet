# The node toolchain handoff (`fleet update` ⇄ `fleet doctor --node`)

Design note for the change that made an existing box cross a node major without
the operator knowing the right command order in advance.

Companion reading: [`WEB-TIER-SHUTDOWN.md`](WEB-TIER-SHUTDOWN.md) ("Choosing the
interpreter" — why the major is declared once in `web/.nvmrc`, and why
`ExecStart` is a shim) and [`DOCTOR.md`](DOCTOR.md).

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

**`scripts/fleet-upgrade.sh`** — audited as the same class of problem, and it
was worse: it never hits the node wall because it never builds the web tier at
all, but `deploy/fleet-web.service` carries `BindsTo=fleet.service`, so its
`systemctl restart fleet` **stopped the web tier and never brought it back**,
after which it printed `✓ fleet upgraded + healthy`. Its readiness gate polls
the Go backend's `/readyz` and says nothing about the web tier. It now restarts
`fleet-web` on both the upgrade and rollback paths, asserts `systemctl
is-active`, and the final banner reports what it measured.

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

**Not verified:** a real Fedora `dnf install nodejs24` (this box has no dnf), and
anything requiring systemd as PID 1 — unit adoption, the `BindsTo` stop, and the
`fleet-upgrade.sh` web-tier restart are code changes backed by the unit file and
by the equivalent, long-standing logic in `update.sh`, not by a live run.
