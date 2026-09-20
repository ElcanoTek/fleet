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

The floor is **`mcp_catalog`, `manifest_files` and `bundle_skills`**, not
`mcp_servers`, and that distinction is the substance of this note. All three
are gated whatever a caller passes.

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

Third round, same method, on copies of zeta-config with a credential-gated
declaration mutated (plus a well-formed http control that stayed green):

```
stdio command " python3 "            -> RED: mcp_catalog["pubmatic_mcp"]: command " python3 " has surrounding whitespace
http url " https://…/mcp" (gated)    -> RED: mcp_catalog["remote_gated"]: url " https://tools.example.com/mcp" has surrounding whitespace
http header "Bad Header" (gated)     -> RED: mcp_catalog["remote_gated"]: header "Bad Header" is not a valid HTTP header name
```

elcano-config (4 http servers) and reklaim-config (2) stayed `ok` throughout,
so the URL and header rules matched no real declaration.

Fourth round, floor now `mcp_catalog,manifest_files`, all seven bundles still
green on it. Negatives on zeta-config copies, each red on exactly the intended
check, plus an `always: true` http server on `:8443` as a control that stayed
green:

```
system_prompts/chat.md deleted           -> manifest_files=fail: system prompt chat.md missing
optional: true server, no gate           -> mcp_catalog=fail: mcp_catalog["dead_server"]: no activation path — set always: true or declare enabled_env / enabled_groups …
gated http url …:99999                   -> mcp_catalog=fail: mcp_catalog["remote_gated"]: url port "99999" is not in 1-65535
```

Sixth round, floor now `mcp_catalog,manifest_files,bundle_skills`, all seven
bundles green on it (a fixed account-suffix reservation tried first failed
four of them, which is why it became exact headroom):

```
plugin_roots: ["vendor/plugins"] missing   -> mcp_catalog=fail: plugin: plugin_roots: …/vendor/plugins: no such file or directory
skills/broken-skill/ without SKILL.md      -> bundle_skills=fail: skills/broken-skill: missing SKILL.md
gated server tools: [" ping "]             -> mcp_catalog=fail: mcp_catalog["remote_gated"]: tools[0] is blank or has surrounding whitespace …
```

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
- stdio: command string present **and not padded** — the loader keeps it
  verbatim and exec looks for the literal `" python3 "`, which resolves nowhere
- http: URL present, **not padded**, parses, uses an http/https scheme, **and
  has a host** — `url.Parse` accepts `https://`, `https:///mcp` and the opaque
  `http:foo` without complaint, and none can be dialled; `MCPServerConfigs`
  copies the URL verbatim and `net/http` rejects a padded one
- http: **headers valid** — token names, no case-duplicate names, no CR/LF in
  values, the same rule the plugin loader already applies to plugin headers
  (`clientconfig.ValidateHTTPHeaders`). The manifest loader validates none of
  this, and `net/http` fails the request at send time — visible only once the
  server's credentials enable it.
- `enabled_env`, `account_vars`, **`identity_env`** and every member of every
  `enabled_groups` alternative well-formed — `enabled()` looks each one up
  verbatim, so a
  padded `" API_KEY"` reads an unset var and leaves the connector silently
  disabled everywhere. An **empty** group is rejected outright: `allSet(nil)`
  is vacuously true, so `enabled_groups: [[]]` enables the server with no gate
  at all. `identity_env` is the sharp one: the loader trims each name for its
  own env-map lookup but propagates the padded original, and the named-account
  guard then reads the identity as unset and can let a variant inherit the
  default seat's routing identity.
- script args resolve to a file under the bundle (reuses
  `Bundle.ValidateMCPArgPaths()`, which already walked the full catalog)
- **an activation path.** A server that is not `always: true` and declares no
  `enabled_env` / non-empty `enabled_groups` has none: `enabled()` returns
  false for that combination on every box, so the server is declared,
  structurally perfect, and never offered anywhere. `optional: true` and
  `enabled_by_default` do not enable — they only shape the picker once a gate
  is satisfied. This is the raptive class of bug seen from the other side.
- http: **port in range** — `url.Parse` keeps `:99999` as a string and the
  transport rejects it only when it first dials.
- **names the process environment can represent** — `=` and NUL cannot occur
  in an env var name (entries split at the first `=`), so
  `enabled_env: ["API=KEY"]` can never be found and the connector stays
  silently disabled. Stdio launch fields (command, args, env keys and values)
  are also refused a NUL byte, which `exec.Cmd.Start` rejects outright.
- http: **header values with only bytes `net/http` will send** — not just no
  CR/LF but `httpguts.ValidHeaderFieldValue`, the transport's own rule; a NUL
  or DEL fails the request at send time with "invalid header field value".
- **URL diagnostics never echo the URL.** A manifest url may carry userinfo or
  a signed query string, and this output lands in JSON reports, CI job
  summaries and journals. Every URL problem names the server, never the value
  — including the parse failure, because `*url.Error` embeds the URL. A test
  plants a credential in the URL for each branch and asserts it never appears.
- **a tool-name budget.** Providers cap a tool name at 64 characters and the
  runtime emits `mcp_<server>_<tool>` with no truncation
  (`clientconfig.MaxProviderToolNameLen`), so the budget is shared. Where the
  manifest declares a `tools` allowlist every generated name is checked
  exactly; where it does not, the server name must at least leave room for a
  one-character tool — a 59-character name can never advertise anything. Once
  enabled, every model request carrying an over-long tool fails.
- http: **a hostname**, not just a host — `https://:443/mcp` parses with
  `Host == ":443"` and an empty `Hostname()`, and there is nothing to dial or
  to derive SNI from.
- stdio: **a bundle-relative command exists and is executable.** A command
  containing a path separator that is not absolute (`./mcp/server`,
  `.venv/bin/python`) is a file the bundle ships, not a runner-installed
  dependency — `probeMCPServer` already resolves it against the bundle dir —
  so checking it is structure, not installation. Bare names stay exempt
  (installation); absolute paths stay exempt (the box's filesystem); plugin
  servers stay exempt (resolved by the plugin loader against the plugin root,
  which is why `ServerDef.FromPlugin` is exported).
- **usable `tools` allowlist entries.** The allowlist is matched exactly
  against what the server advertises, so `" lookup "` excludes the real
  `lookup` and a non-empty list of blanks filters every tool. Blank or padded
  entries are rejected.
- **account-label headroom, reported — and failed only when it is zero.** A
  named seat is registered as `<server>_<account>` before the prefix and tool
  are added (`agentcore.RegisteredMCPName`), so on a server that declares
  `account_vars` the 64-char budget is also shared with the label. Labels are
  operator input at `fleet mcp account set` time with no length cap, so a
  secretless preflight cannot know them — and a fixed reservation is
  arbitrary: a 16-character one failed four real bundles (`magnite_mcp`,
  `gamma`) whose seats work today with `production`. So the check computes the
  **exact headroom** against the longest allowlisted tool, fails only when
  even a one-character label cannot fit, and otherwise reports it in the ok
  detail per server. It already says something useful: elcano-config's
  `magnite_mcp` has 8 characters of headroom, so a `production` seat there
  would exceed the cap. Capping labels at creation time is the complete fix
  and is *not* in this change — see Scope.
- **no Agent Plugin problems.** An `mcp.json` server the plugin loader skips
  as invalid never reaches `MCPCatalog`, and `Load` still succeeds —
  `checkManifest` demotes `PluginProblems()` to advisories so a running box is
  not taken down by a plugin defect. Walking only the survivors would report
  `ok` over a connector that just vanished, so the catalog check folds every
  plugin problem in as a failure. They are all decided by the bundle's own
  files, with one environmental exception (PLUGIN_DATA dir unavailable) that
  also means the catalog under test is incomplete, so red is still the honest
  answer; the workflow pins `FLEET_DATA_DIR` to a fresh `runner.temp` dir so it
  cannot arise there. Ordering matters here and is pinned by a test: there is
  deliberately **no** "empty catalog → ok" early return, because a bundle with
  no manifest servers whose *only* plugin server was rejected arrives with an
  empty `MCPCatalog` and a non-empty `PluginProblems()` — the exact case an
  early return would wave through. **Entry** problems only, though: an
  explicit `plugin_roots` dir such as `/opt/fleet/site-plugins` that is
  missing or unreadable *on the runner* is a fact about the machine — absolute
  roots exist precisely so a site can mount plugins outside the repo — and
  folding it in would pin every PR of such a bundle red. The loader now keeps
  root-availability problems apart (`PluginRootProblems`); the catalog check
  consumes `PluginEntryProblems`, and the root ones stay visible as `manifest`
  advisories, where the operator view belongs. The split is decided by
  whether the manifest named the root **absolutely**: a *relative* root
  (`vendor/plugins`) is bundle content by spec, so if that checked-in
  directory is deleted every plugin under it vanishes on every box — an
  entry-level defect the gate fails. Both halves are pinned by tests through
  the real loader.

It does **not** check whether a command resolves on `PATH`. That is
environmental, it belongs to `mcp_servers` on a real box, and putting it here
would pin the CI gate permanently red on every bundle whose servers need `uvx`
or `node`.

`mcp_catalog` is declared **non-blocking** (`Blocking: false`), so it does not
change the exit code of `fleet validate-config` on any existing operator box.
CI gets its signal from the status, not the exit code.

### `manifest_files` — the other half of the floor

`checkManifest` mixes two kinds of fact: files every box reads (`chat.md`, the
interactive base prompt, and `default.md`, the scheduled one — a missing
`chat.md` fails every interactive turn) and defaults a *box* supplies (the
persona selected by `FLEET_PERSONA`). Gating `manifest` in CI pins half the
family red for a true statement about the runner; leaving it ungated lets a
deleted `chat.md` merge green, because `Load` and `mcp_catalog` both succeed
without it and only the ungated `manifest` row noticed.

`manifest_files` carries only the first kind, so the workflow can require it
everywhere. It is a second check rather than a re-scoped `manifest` because
`manifest` is blocking and operators read its exit code: narrowing it would
change what `fleet validate-config` refuses to start on every existing box.

Measured: deleting `system_prompts/chat.md` on a bundle copy →
`manifest_files=fail: system prompt chat.md missing`, with `mcp_catalog` still
`ok` — exactly the gap.

### `bundle_skills` — the third floor check

`Load` deliberately does not fail on a malformed checked-in skill — a folder
with no `SKILL.md`, bad frontmatter, a name/folder mismatch — because a running
box should not go down over one bad skill. It *logs* `ValidateSkills()` to
stderr and skips the skill from the roster. A gate that reads the JSON report
never sees stderr, so a skill that quietly dropped out merged green.
`bundle_skills` turns `ValidateSkills()` into a check result; it is decided
entirely by the bundle's own files, so it is part of the forced floor.
Non-blocking, like the other floor checks. (Plugin skills are covered by the
plugin loader's entry problems, which `mcp_catalog` already folds in.)

Measured: a `skills/broken-skill/` folder with no `SKILL.md` on a bundle copy →
`bundle_skills=fail: skills/broken-skill: missing SKILL.md`, everything else
`ok`.

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

The floor is a **floor**, not a default. `mcp_catalog`, `manifest_files` and
`bundle_skills` are added to whatever the caller names — a caller passing
`gate_checks: manifest` alone would otherwise leave the connector check, the
prompt-file check and the skills check ungated, and a broken gated-off
connector, a deleted `chat.md` or a skill that quietly dropped out of the
roster would merge green. The summary says which checks were added and why,
so nothing is silent; a caller can add checks but never remove those three.

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
- **Symlink containment before the loader runs.** The loader follows symlinks.
  A `manifest.yaml` that is a symlink to `/proc/self/environ` would be read,
  fail to parse, and the parse error would echo the offending source line —
  the validator's own environment, runner service credentials included — into
  the JSON report the gate step publishes to the job summary. A shell step
  therefore refuses any symlink anywhere in the caller checkout whose target
  resolves outside the workspace, and a `bundle_dir` that does, before any Go
  runs. The whole workspace is scanned (minus fleet's own `.fleet-core`
  checkout) so a chain through an in-repo directory cannot route around it.
  Verified on hostile copies: `manifest.yaml → /proc/self/environ` and an
  escaping `bundle_dir` both exit 1 with a `::error::`; a clean checkout and an
  in-workspace symlink both pass.

  Two tightenings followed review. The step now runs **after** the fleet
  checkout and refuses any symlink whose target is under `.fleet-core`: with
  the default `bundle_dir: .` the trusted checkout lands *inside* the directory
  treated as bundle content, so `system_prompts/chat.md ->
  ../.fleet-core/README.md` was dangling at caller-checkout time (and passed a
  pre-checkout scan), then resolved once fleet was checked out, and
  `manifest_files` reported ok for a bundle that has no `chat.md` anywhere it
  is deployed. And targets must resolve under the **bundle dir**, not merely
  the workspace — anything outside it is not shipped with the bundle. Verified:
  the `.fleet-core` link and an in-repo-but-out-of-bundle link both exit 1;
  clean and in-bundle links pass.
- **A clean environment for the validator.** `Load` `${VAR}`-interpolates the
  whole manifest from the process environment, and a runner carries service
  credentials (`ACTIONS_RUNTIME_TOKEN`, `ACTIONS_ID_TOKEN_REQUEST_TOKEN`). An
  untrusted PR could write `name: "${ACTIONS_RUNTIME_TOKEN}"` and have the
  server label — which every diagnostic carries — deliver the substituted
  value into the job summary. The validator therefore runs under `env -i` with
  only `PATH`, `HOME`, `FLEET_MOCK_MODE`, `FLEET_DATA_DIR` and `FLEET_ENV_FILE`:
  an unknown `${VAR}` interpolates as unset and the load fails closed — red,
  nothing to leak. Measured with a planted secret in the outer environment and
  `name: "${LEAK_ME}"` in a bundle copy: **0 occurrences** in the report under
  the workflow's invocation, **3** when the same binary runs with the
  inherited environment. The clean environment is the load-bearing layer; the
  no-echo rules on URL and command values are defence in depth.

`fleet_ref` and `bundle_dir` are validated with the same allow-lists as
`build-sandbox-image.yml`, for the same reason — `fleet_ref` selects source that
is compiled and executed. Keep the lists in step.

## `bundle_env` — non-secret deployment variables

`Load` interpolates `${VAR}` over the whole manifest and fails for a bare
reference that is still unset in any field *outside* the lazily-resolved
connector env/header maps — `url:`, `command:`, `sandbox.image`
(`internal/clientconfig/manifest_env.go`). None of the seven bundles does this
today, but one that did would report `mcp_catalog: warn "skipped (bundle not
loaded)"` on a secretless runner and stay red for a bundle the production env
file makes valid.

The reusable workflow therefore takes an optional `bundle_env` input: one
`KEY=VALUE` per line, validated to a `[A-Za-z_][A-Za-z0-9_]*` key before the
loader sees it, written to a runner-local file and passed as
`FLEET_ENV_FILE`. It is for **non-secret** deployment variables only —
workflow files are repository content, and the `credentials` check is
*expected* to report absences in CI. Where a placeholder is acceptable,
`${VAR:-default}` in the manifest needs no input at all.

## Scope and deviations

Shipped **in this repository**: the reusable workflow, the `mcp_catalog` and
`manifest_files` checks and their tests, and the exported
`clientconfig.ValidMCPServerName` / `ValidateHTTPHeaders` /
`PluginEntryProblems` / `PluginRootProblems` helpers they rely on.

Coordinated, **pending merge elsewhere**: caller workflows are open as PRs in
all seven bundle repos (elcano, reklaim, zeta, omnicom, raptive, example,
example-kubernetes), with the `optional: true` template fix in example-config
and raptive-config. They reference this workflow `@main`, so this PR must merge
first; until it does their `bundle-preflight` job cannot resolve its `uses:`.
Nothing in a bundle repo is delivered by this change — this note will be
updated to "shipped" as those PRs land.

Deliberately **not** in this change, and why:

- **No length cap on account labels at creation time.** The complete fix for
  the account-suffix budget is to reject, at `fleet mcp account set` / the
  accounts API, a label that would push `mcp_<server>_<account>_<tool>` past
  the provider cap. That is a production behaviour change to an existing
  API — it could refuse labels already in use on a box — and belongs in its
  own PR with its own compatibility decision, not inside a CI-hardening
  change. The preflight does what a secretless runner honestly can: fail when
  no label could ever fit, and print each server's headroom so the operator
  picks a label that does.

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
