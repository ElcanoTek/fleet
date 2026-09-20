# Bundle preflight in CI

Design note for `.github/workflows/validate-bundle.yml`, the reusable workflow a
client-config bundle repo calls to gate its own manifest, and for the
`mcp_catalog` check in `fleet validate-config` that exists to make that gate
mean something.

## Why it exists

On 2026-09-20 `fleet validate-config` on `fleet.raptivevic.com` exited 1 with a
blocking `credentials` failure:

```
✗ credentials: 0/6 referenced vars present; required gate var(s) missing
  for non-optional server(s): EXAMPLE_API_KEY
```

The box was healthy and serving. `example_api` was template scaffolding
inherited verbatim when raptive-config was rebranded from example-config; it
points at `api.example.com` and nobody ever held a key for it. At runtime fleet
resolved the unsatisfied `enabled_env` gate and cleanly disabled the server —
which is why nothing was broken — but `validate-config` treats a non-`optional`
server with an unsatisfiable gate as blocking, so the preflight and the runtime
disagreed.

Three bundles (omnicom-config, example-kubernetes-config, and one other) had
each independently patched around this with `optional: true`. The template never
did. So every rebrand inherited the same landmine, and each one found it the
same way: at deploy time, on a customer's box.

Two things were missing. The template fix (`optional: true` in example-config,
so no future rebrand inherits it) is the durable fix for *that* bug. This
workflow is the answer to the more general question — **no bundle repo validated
its own manifest at all.**

## What the gate actually proves

Deliberately narrower than it first appears, and the workflow's header comment
says so at length because the honest scope is the whole value of the thing.

`validate-config` resolves **declarations**. The manifest parses; personas and
plugins resolve; a script arg names a file that exists in the bundle; a command
name is present; an http URL is well-formed. It does **not** spawn a server or
complete an MCP handshake. A connector whose command exists but whose Python
dependency is missing, or that crashes in `initialize`, passes this gate.

Liveness is `fleet mcp test --all` against a configured box
([`docs/MCP-TESTING.md`](MCP-TESTING.md)). It needs credentials and an installed
runtime, which a secretless CI runner does not have and should not have.

## Why `gate_checks` is an input, and why the floor is `mcp_catalog`

Two checks cannot be gated uniformly across the bundle family:

- **`credentials`** is *expected* to report absences here. A real bundle's
  production secrets are legitimately not on a CI runner, so gating on it would
  pin elcano-config, reklaim-config and zeta-config permanently red for a
  correct reason. It still runs, and the whole check table lands in the job
  summary, so a reviewer sees that line on every PR — which is the visibility
  that would have made the raptive entry obvious in review.
- **`manifest`** mixes bundle-intrinsic correctness with box-dependent defaults.
  A bundle whose default persona is selected by `FLEET_PERSONA` on the box
  reports `fail` on a runner that sets no such variable. True about the runner,
  not a defect in the bundle.

Measured against all seven real bundles before shipping: `manifest` passes on
four and is unsatisfiable on elcano/reklaim/zeta. So each caller names what it
can honestly gate today and tightens it as its environment contract firms up,
rather than the workflow hard-coding a set that is wrong for half the family.

The floor is **`mcp_catalog`**, not `mcp_servers`, and that distinction is the
substance of this note.

### The `mcp_servers` trap

`Bundle.MCPServerConfigs()` skips any server whose enable gate is unsatisfied.
`checkMCPServers` therefore only ever inspects the **credential-enabled subset**,
and when that subset is empty it returns a cheerful `ok: no enabled MCP servers`.

On a secretless runner most of the family's connectors are gated off, so a gate
on `mcp_servers` is close to a gate on nothing. The raptive box demonstrates it
directly — this is the *same run* whose `credentials` check failed:

```
✓ mcp_servers: knowledge_base: ok, plugin_notes: ok
```

`example_api`, the broken entry, is not in that list. It was filtered out before
the check looked.

It is worse than "the gated-off servers go unchecked", and the detail is worth
recording because it is easy to get wrong in review. `checkMCPServers` *does*
fold in `Bundle.ValidateMCPArgPaths()`, which walks the full unfiltered
catalog — so it is tempting to conclude that script-arg breakage was already
covered catalog-wide and only command presence was at risk. It was not. The
`len(cfg.MCPServers) == 0` early return fires **before** `ValidateMCPArgPaths()`
is ever called, so on a runner where every server is gated off, script args are
not validated either.

Measured, on a copy of zeta-config (all three servers gated off on a secretless
runner) with the script file of the credential-gated `magnite_mcp` deleted:

```
gate=mcp_servers  -> GREEN (missed)
gate=mcp_catalog  -> RED (caught)
```

Two more negatives, run the same way before the follow-up rules shipped, each
red with the exact defect named:

```
gated server renamed magnite.mcp  -> RED: mcp_catalog["magnite.mcp"]: name must be 1-64 chars of letters, digits, '_' or '-' …
plugin server renamed has.dot     -> RED: plugin: plugins/example-plugin/mcp.json: server "has.dot": name must be … ; skipped
```

The second is the case `mcp_servers` could never see: the loader skipped the
server before any check ran, `Load` succeeded, and the only trace was a plugin
problem that `checkManifest` files as an advisory.

### `mcp_catalog`

A check that walks the **full** `bundle.MCPCatalog` regardless of enable gates —
the credential-independent question CI is entitled to ask. It validates
structure and deliberately **not** installation:

- server name present, unique, and **provider-safe** — the same
  `[A-Za-z0-9][A-Za-z0-9_-]{0,63}` the plugin loader already enforces
  (`clientconfig.ValidMCPServerName`). The name becomes part of every tool
  name (`mcp_<server>_<tool>`), and providers reject a dot or a space there.
  The loader deliberately does *not* enforce this for manifest servers (it
  would take a running box down over a name that has worked for months), so
  without this check a credential-gated `sales.api` passes boot and breaks the
  first turn that enables it.
- stdio: command string present
- http: URL present, parses, uses an http/https scheme, **and has a host** —
  `url.Parse` accepts `https://`, `https:///mcp` and the opaque `http:foo`
  without complaint, and none can be dialled
- `enabled_env`, `account_vars` **and every member of every `enabled_groups`
  alternative** well-formed — `enabled()` looks each one up verbatim, so a
  padded `" API_KEY"` reads an unset var and leaves the connector silently
  disabled everywhere. An **empty** group is rejected outright: `allSet(nil)`
  is vacuously true, so `enabled_groups: [[]]` enables the server with no gate
  at all.
- script args resolve to a file under the bundle (reuses
  `Bundle.ValidateMCPArgPaths()`, which already walked the full catalog)
- **no Agent Plugin problems.** An `mcp.json` server the plugin loader skips
  as invalid never reaches `MCPCatalog`, and `Load` still succeeds —
  `checkManifest` demotes `PluginProblems()` to advisories so a running box is
  not taken down by a plugin defect. Walking only the survivors would report
  `ok` over a connector that just vanished, so the catalog check folds every
  plugin problem in as a failure. They are all decided by the bundle's own
  files, with one environmental exception (PLUGIN_DATA dir unavailable) that
  also means the catalog under test is incomplete, so red is still the honest
  answer; the workflow pins `FLEET_DATA_DIR` to a fresh `runner.temp` dir so it
  cannot arise there.

It does **not** check whether a command resolves on `PATH`. That is
environmental, it belongs to `mcp_servers` on a real box, and putting it here
would pin the CI gate permanently red on every bundle whose servers need `uvx`
or `node`.

`mcp_catalog` is declared **non-blocking** (`Blocking: false`), so it does not
change the exit code of `fleet validate-config` on any existing operator box.
CI gets its signal from the status, not the exit code.

## Gating on `ok`, not on "not `fail`"

The first version of the gate selected checks whose status was `fail`. That was
a bug, and a quiet one: `checkMCPServers` is non-blocking and reports every
per-server problem as `warn`, never `fail`. So did the "bundle/config not
loaded" path. A bundle whose manifest did not load at all would have printed
"Every gated check passed".

A gated check now passes only on `ok`. Anything that is not `ok` is a reason to
look, so anything that is not `ok` stops the merge.

An **empty `gate_checks`** is refused for the same reason. A caller that passes
`""` — typically a repository variable that was never set — would otherwise get
"Every gated check passed" over a run that gated nothing, which is the exact
failure this workflow exists to prevent. The table is still written first so
the reason is visible in the job summary.

## Untrusted-caller hardening

This workflow is `workflow_call`ed from other repositories, and on a
`pull_request` run the caller checkout is attacker-influenced content. Three
properties are load-bearing:

- **`persist-credentials: false`** on both checkouts. Nothing here fetches,
  pushes or calls the API, so the token buys nothing and costs a credential
  sitting in `.git/config` for the life of the job.
- **`GOWORK=off`** on the build. The trusted fleet checkout is a *child* of the
  caller checkout, and Go's default `GOWORK=auto` walks containing directories
  for a `go.work`. A caller repo that commits one at its root could `replace` a
  fleet dependency with code from its own PR and have this step compile it into
  the binary the next step executes.
- **`python3 -I`**, running from `runner.temp`. A plain `python3 -` puts the
  current directory at the front of `sys.path`, so a caller repo committing a
  top-level `json.py` gets it executed by the gate script's `import json`.
  Verified both ways before the fix landed: with `python3 -` the shadowing file
  runs; with `python3 -I -` the standard library is imported instead.

`fleet_ref` and `bundle_dir` are validated with the same allow-lists as
`build-sandbox-image.yml`, for the same reason — `fleet_ref` selects source that
is compiled and executed. Keep the lists in step.

## Scope and deviations

Shipped: the reusable workflow, the `mcp_catalog` check and its tests, and
caller workflows in all seven bundle repos (elcano, reklaim, zeta, omnicom,
raptive, example, example-kubernetes), plus the `optional: true` template fix in
example-config and raptive-config.

Deliberately not shipped:

- **No liveness probe.** Spawning each server and handshaking would need
  credentials and installed runtimes in CI. `fleet mcp test --all` owns that and
  runs against a real box.
- **No gate on `credentials`.** Unsatisfiable by construction on a secretless
  runner; see above.
- **`manifest` is not the default gate.** Three bundles cannot satisfy it until
  their persona-default contract moves into the bundle. Those three gate on
  `mcp_catalog` only, and the note in each caller says why.
- **No cross-bundle sync.** Per the coupling doctrine in `AGENTS.md`, the
  bundles are peers; each caller workflow was added as its own reviewed PR and a
  future change to the shared workflow is made here, once.
