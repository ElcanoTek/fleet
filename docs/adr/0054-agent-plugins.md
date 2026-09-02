# ADR-0054: Agent Plugins load as bundle content, translated onto the existing skills + MCP primitives

- **Status:** Accepted
- **Date:** 2026-09-02
- **Deciders:** fleet maintainers
- **Relates to:** ADR-0003 (host-side MCP credential brokering), ADR-0006
  (external client-config bundle), ADR-0036 (sandboxed file tools and the
  host-I/O exception list), ADR-0042 (child-side MCP scope authorization)

## Context

The [Agent Plugins](https://agent-plugins.org) specification v1.0.0 defines a
portable directory format — `plugin.json` + `skills/*/SKILL.md` + `mcp.json` —
that Cursor, VS Code, Copilot, Codex, Kiro and others load. fleet already
implements both component types the format carries: Agent Skills from the
bundle's `skills/` and MCP servers from `manifest.yaml`'s `mcp_servers[]`. Issue
#1166 asks fleet to load the portable package so third-party plugins need no
conversion and fleet-authored packages work elsewhere.

Two forces constrain *how*:

1. **The bundle is the only content boundary.** ADR-0006 makes every
   deployment-specific thing — servers, personas, skills — arrive as data in
   `FLEET_CLIENT_CONFIG_DIR`, never as fleet code, and the trust model treats
   that checkout as a reviewed supply chain. A plugin format that could load
   from anywhere else (a user's home, a download) would open a second content
   boundary with a weaker review story.
2. **Governance is one core, with a fixed host-side exception list.** MCP
   servers run host-side under the credential broker (ADR-0003), the child
   re-derives what it may bind from the bundle it loads itself (ADR-0042), and
   the set of host-side operations is enumerated in ADR-0036. A plugin loader
   that spawned its own processes, or fed servers to the child by a side
   channel, would fork a second, weaker path.

The specification also fixes precise failure boundaries (an unknown manifest
field is reported, not fatal; a bad server entry skips itself, not the plugin;
every read path must resolve inside the plugin root). Portability depends on
implementing those exactly, not approximately.

## Decision

1. **Plugins are bundle content.** fleet discovers plugins only from
   `<bundle>/plugins/` and the manifest's `plugin_roots:`; there is no user-,
   env- or download-sourced plugin path. A plugin therefore has the bundle's
   trust class — its stdio server is launched by the broker exactly as a
   `mcp_servers[]` entry is, and its skills are files in the sandbox's read-only
   skills mount. ADR-0036's host-side exception list is **unchanged**.
2. **The loader translates; it does not execute.** `internal/clientconfig/plugins.go`
   maps each valid `mcp.json` entry onto one `ServerDef` appended to the MCP
   catalog (`stdio` → stdio in the plugin root with `PLUGIN_ROOT`/`PLUGIN_DATA`
   set last; `streamable-http` → http with literal headers; `sse` → reported
   and skipped), and each valid skill folder onto the merged skills tree
   between the built-in pack and the bundle's own `skills/`. Every downstream
   consumer — prompt roster, sandbox mount, `/skills`, the broker child, hot
   reload, the critical-tool gate, the always-on connections list — sees plugin
   content through the seams it already has. No new spawn path, no new mount.
3. **Plugin servers are always-on and literal.** The portable format has no
   credential gate because it forbids secrets, so an entry is `always: true`,
   and its `env`/`headers` bypass fleet's `${VAR}` interpolation: the spec's
   only permitted expansion (`${PLUGIN_ROOT}`, `${PLUGIN_DATA}`, one pass, in
   `args`/`env` values/`cwd` only) is applied by the loader, and everything
   else stays literal. Names are used as declared; a collision with a manifest
   server, an `http_tool`, `_http`, or an earlier plugin skips the entry.
4. **Failure boundaries follow the spec exactly.** Unknown top-level
   `plugin.json` fields and a non-object `extensions` are reported and ignored;
   any other manifest violation rejects the plugin; a bad `skills` location or
   `mcp.json` top level disables that component type only; a bad skill or
   server entry skips itself only; every file read or executed must resolve,
   after symlinks, inside the plugin root, enforced at the narrowest boundary.
   Nothing about a plugin can fail the bundle load.
5. **`com.elcanotek.fleet` carries fleet's governance knobs, and nothing
   credential-shaped.** The portable format has no field for a per-server
   `tools:` allowlist, a `probe:` canary, or the Optional-server metadata, so
   fleet reads them from its own reverse-domain namespace in `plugin.json`'s
   `extensions` (spec §8.1) — the place the spec assigns to client-specific
   data, which every other client ignores. `env`, `enabled_env`,
   `account_vars` and `identity_env` are deliberately not expressible there: a
   plugin must not carry secrets, and fleet does not add a second credential
   path beside the manifest's. The namespace is lenient (report and ignore),
   so it can never reject a plugin.

## Enforcement

- `internal/clientconfig/plugins.go` — the only code that reads `plugin.json`
  or `mcp.json`; it produces `ServerDef`s and skill overlays and nothing else.
- `internal/clientconfig/plugins_test.go` — one test per boundary row
  (manifest deviations, two-stage MCP validation, cwd forms, escapes via
  symlink, collisions, precedence, `plugin_roots`, duplicate names, single-pass
  expansion), all through `clientconfig.Load`.
- `ServerDef.dir` / `literalEnv` / `plugin` are **unexported**, so strict
  manifest decoding cannot set them: only the plugin loader can mark a server
  as plugin-sourced.
- The child broker (`fleet mcp-broker`) loads the bundle itself
  (`loadMCPBrokerConfig` → `clientconfig.Load`), so plugin servers reach it by
  the ADR-0042 path — there is no parent→child hand-off to audit.
- `internal/evals/fingerprint.go` hashes `plugins/` as bundle content.

## Consequences

- **Easier:** a plugin authored for any compatible client is a `git add` into
  the bundle's `plugins/`; a bundle's skills + servers can be published as a
  plugin for other tools without a fleet-specific layout.
- **Unchanged:** the security posture. A plugin server is reviewed like
  `mcp/*.py`, brokered like `mcp_servers[]`, and gated like any tool; a plugin
  skill is reviewed like `skills/*`.
- **Costs accepted:** a plugin server's allowlist and probe live in fleet's
  extension rather than a portable field, so they are invisible to other
  clients (by design) and absent unless the author or operator adds them —
  an un-annotated plugin exposes every tool it advertises, like an
  un-annotated manifest server; a credential-gated plugin server is not
  possible (use the manifest); fleet's own
  spawn-time `${FLEET_WORKSPACE}`/`${FLEET_TASK_ID}` substitution still runs
  over plugin env values (a documented deviation from "no other expansion");
  legacy `sse` servers are skipped; on the kubernetes backend plugin skills are
  rostered but not readable inside the sandbox, as for the built-in pack.
  Adding a plugin or changing its `mcp.json` requires a reload (skills follow
  the disk on read).

## Alternatives considered

- **A dedicated plugin runtime** (own process spawner, own mount, own registry).
  Rejected: it would be a second governance path beside `agentcore` and the
  broker, the exact thing the "one core" invariant forbids, for no capability
  the translation does not already deliver.
- **Loading plugins from outside the bundle** (`~/.fleet/plugins`, a URL, an
  admin upload). Rejected for v1: it creates a content boundary with no review
  step and no fingerprint, contradicting ADR-0006. The `plugin_roots` knob keeps
  the *decision* in the bundle manifest even when the *files* live elsewhere on
  the box.
- **Namespacing plugin server names** (`<plugin>_<server>`). Rejected: the
  spec says field names need not match a client-native format either way, but
  a declared name shows up unchanged in `fleet mcp test`, the connections UI
  and tool names, which is what an author debugging their plugin expects;
  collisions are rare and are reported rather than silently renamed.
- **Putting fleet's per-server knobs in `manifest.yaml`** (a `plugins:` block
  overriding vendored plugins' servers) instead of the extension. Rejected for
  v1: the spec already provides a place for exactly this data, it travels with
  the plugin (a fleet-authored plugin is self-describing), and `disabled` in
  the extension gives the operator the one override they need without a second
  schema. A manifest-side override can be added later if vendored plugins
  updated in place turn out to churn their `plugin.json`.
