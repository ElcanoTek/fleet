# Generic client bundle (`config/default`)

This is the **generic, client-agnostic** bundle that ships inside fleet so the
app runs bare with no client-specific content. fleet loads a bundle from
`FLEET_CLIENT_CONFIG_DIR` and falls back to this directory when the variable is
unset.

A real deployment points `FLEET_CLIENT_CONFIG_DIR` at a checked-out client repo
(e.g. `/etc/fleet/client` or `/opt/fleet/client`) whose `manifest.yaml`
overrides the branding, model defaults, MCP catalog, and empty-state cards, and
whose `system_prompts/`, `personas/`, `protocols/`, and `mcp/` directories
supply the client's prompts, personas, playbooks, and connectors.

## The bundle contract

```
<bundle>/
  manifest.yaml        # branding, models, mcp_servers[], empty_state, pricing, sandbox
  sandbox/
    Containerfile      # the bundle's execution-sandbox image (build-on-box)
  system_prompts/
    default.md         # scheduled-agent base prompt
    chat.md            # interactive-chat base prompt
  personas/            # *.yaml — one per persona; the env file picks the defaults
  protocols/           # *.yaml | *.md — reusable playbooks
  prompts/             # *.yaml | *.yml | *.md | *.txt — Git-backed prompt library
  skills/              # <name>/SKILL.md — Agent Skills (progressive disclosure)
  plugins/             # <plugin>/plugin.json — Agent Plugins (skills + mcp.json)
  mcp/                 # the client's Python MCP servers (+ requirements.txt)
```

Which persona is the **default** is a deployment setting, not a bundle one: it
lives in the env file, and the two engines take it in different shapes.
`FLEET_PERSONA_DEFAULT` (legacy: `PERSONA_DEFAULT`) is the persona new
conversations start on and takes a bare *name* — `victoria`. `FLEET_PERSONA`
(legacy: `PERSONA`) is the scheduled-agent default and takes the bundle-relative
*path* to the file — `personas/victoria.yaml`. Both spellings of each are read,
`FLEET_`-prefixed first. Leaving them unset means `assistant`, which a bundle
that ships no `personas/assistant.yaml` will not have; `fleet validate-config`
reports the miss and lists the personas the bundle does offer. It **fails** on
`FLEET_PERSONA_DEFAULT`, because a chat turn whose persona file is unreadable
errors out, and only **warns** on `FLEET_PERSONA`, because the scheduled agent
just runs without the persona's expertise block.

The optional `prompts/` directory feeds the hybrid prompt library shown in Chat
and Operations Center. Bundle files are read-only and Git-trackable; users can
also create private or workspace-shared prompts in the UI and export the visible
library as JSON. See [`docs/PROMPT-LIBRARY.md`](../../docs/PROMPT-LIBRARY.md).

See `internal/clientconfig/clientconfig.go` for the manifest schema and the
authoritative description of each field, including the MCP catalog's declarative
enable gate (`enabled_env` / `enabled_groups` / `always`), the `${VAR}` env
interpolation, the tool allowlist, and the account-suffix vars.

## Inline HTTP tools (`http_tools`)

For simple "call this REST endpoint" needs that don't justify a full MCP server,
the manifest's `http_tools:` section declares lightweight tools inline: a `name`,
`method`, `url` (with `{param}` placeholders), an `input_schema`, optional
`headers`, `body_template`, `response_jq` filter, and a `critical` flag. Each
entry is registered as a native tool the agent sees **alongside** MCP tools
(addressed as `mcp__http_<name>`). The generic bundle ships none — see the
commented-out example in `manifest.yaml`. `http_tools` is validated at load
(method, URL, duplicate/reserved names, `input_schema` shape, and `response_jq`
syntax all fail startup loudly).

These are **bundle-author-defined and therefore trusted**, exactly like
`mcp_servers`. The security posture is identical to an MCP server's, and worth
stating plainly:

- **Credentials stay host-side.** `headers` values may carry `${ENV_VAR}`
  references; they are resolved from the **host** process env and applied to the
  outbound request at call time, in whichever process holds the connector secrets
  (the out-of-process MCP broker, else the host-side manager). The secret never
  enters the sandbox, the model context, or the logs — the model supplies only the
  declared input params and sees only the (redacted) response body.
- **The request runs host-side, NOT in the sandbox.** Like MCP credential
  brokering, the HTTP call is brokered through the same host-side seam every MCP
  tool call funnels through, so it is governed by the same policy gate, output
  redaction, and `isError` handling. There is **no** per-tool sandbox enforcement
  for these calls (they are host-side by design, because the credentials are
  host-side) — so an `http_tool`, like an MCP server, can reach any network
  endpoint its URL names. Add only tools you trust, and prefer `critical: true`
  for any tool that writes data or triggers side effects (it opts into the same
  audit/approval gate as `agent_policy.critical_tools`).
- **V1 scope.** No OAuth/token-refresh (use a pre-populated `Authorization`
  header), no streaming responses, no multipart/form-data bodies. A non-2xx
  response is returned to the model as `status <N>: <body>` so it can reason about
  the failure, not raised as an execution error.

## Outbound A2A peers (`a2a_peers`)

For delegating work to another agent that speaks the A2A protocol — another
fleet deployment, or any A2A v1.0 server — the manifest's `a2a_peers:` section
declares remote peers inline: a `name`, the peer's `rpc_url`, a bundle-authored
`description`, optional credential `headers`, and a `critical` flag. Each peer
registers **four** tools the agent sees alongside MCP tools
(`mcp__a2a_<name>_send`, `_status`, `_wait`, `_cancel`): send a task and get its
remote id back at once, poll it, wait on its event stream for a bounded time,
or cancel it. The generic bundle ships none — see the commented-out example in
`manifest.yaml`. Validated at load (name charset, `http(s)` URL without
userinfo, description required, namespace collisions all fail startup loudly).

Same trust posture as `http_tools`, plus three rules specific to talking to
another agent (docs/A2A.md "Outbound delegation", ADR-0056):

- **Credentials stay host-side.** `headers` values may carry `${ENV_VAR}`
  references, resolved from the host process env at call time in whichever
  process holds the connector secrets. They never enter the sandbox, the model
  context, results, or logs.
- **Peers are operator policy; the model never sees a remote agent card.** The
  `description` you write here is the only text about the peer the model ever
  sees — remote card text would be a prompt-injection channel into every run.
- **What the peer sends back is untrusted external content.** Results open
  with a banner saying so; text is inlined under a cap; file artifacts are
  listed by name/type/URL and **not downloaded**.
- **Outbound calls are SSRF-guarded** (`netguard` resolve-then-dial, redirects
  refused) and a fleet-to-fleet recursion guard caps delegation chains at
  `FLEET_A2A_MAX_DELEGATION_DEPTH` (default 3).
- **Prefer `critical: true`.** Delegation is a side effect whose cost lands on
  the remote side; the flag puts `_send`/`_cancel` behind the same
  audit/approval gate as `agent_policy.critical_tools`.

## Skills

The `skills/` directory holds **Agent Skills** — packaged, on-demand capabilities
following the open [Agent Skills standard](https://github.com/anthropics/skills).
Each skill is a folder containing a `SKILL.md` with YAML frontmatter (`name`,
`description`) plus an optional body and bundled scripts/reference files:

```
skills/
  example-skill/
    SKILL.md          # frontmatter (name, description) + instructions
    scripts/hello.py  # OPTIONAL: code the agent runs via bash / run_python
```

fleet uses **progressive disclosure**: only each skill's name, description, and
path go into the system prompt (Level 1). The agent reads the full `SKILL.md`
(Level 2) and any bundled scripts/resources (Level 3) on demand, by path, when a
task matches — so installing many skills costs almost no context until one is
used. The `skills/` dir is bind-mounted read-only into the sandbox and symlinked
into the per-conversation workspace exactly like `protocols/`, so a `SKILL.md`
that says `python skills/<name>/scripts/foo.py` resolves and runs inside the
rootless sandbox. Skills are just files the agent reads and runs — there is no
bespoke skill executor.

Skills are the progressive-disclosure **sibling of protocols**: a protocol is a
single markdown playbook; a skill is a folder that can also bundle executable
code and reference material. This bundle ships one annotated `example-skill` as a
template — fork it to author your own. A skill's optional `allowed-tools`
frontmatter is parsed but **not** enforced as a hard authorization boundary; the
real boundaries remain the sandbox, the MCP tool allowlists, and the critical-tool
audit gate. Because a skill can run code in the sandbox, treat bundled scripts as
trusted-but-reviewable — review them the way you review the bundle's `mcp/`
servers, and use skills only from sources you trust.

## Agent Plugins

The `plugins/` directory holds **Agent Plugins** — the open
[Agent Plugins standard](https://agent-plugins.org) (v1.0.0) that packages Agent
Skills and MCP servers together so one directory loads in fleet, Cursor, VS
Code, Copilot, Codex and other compatible clients without conversion:

```
plugins/
  my-plugin/
    plugin.json       # REQUIRED: "$schema" + "name" (+ optional metadata)
    skills/<name>/SKILL.md   # skills, in the same format as skills/ above
    mcp.json          # stdio / streamable-http MCP servers (optional)
```

fleet merges a plugin's skills into the same roster as `skills/` (the bundle's
own skill wins a name collision, a plugin's wins over a built-in) and appends
its `mcp.json` servers to the MCP catalog as always-on entries launched in the
plugin root with `PLUGIN_ROOT` / `PLUGIN_DATA` set — subject to every gate a
manifest server already has. fleet-specific knobs a plugin server needs (a
`tools` allowlist, a `fleet mcp test --deep` `probe`, the Optional-server
metadata, `disabled`) go in `plugin.json` under
`extensions["com.elcanotek.fleet"]`, the spec's namespace for client-specific
data that other clients ignore; nothing credential-shaped is accepted there.
Extra plugin directories can be listed in `manifest.yaml` under `plugin_roots:`. A plugin is bundle content with the
bundle's trust class; review it like `mcp/` and `skills/`. This generic bundle
ships no plugins; the annotated example is
`plugins/example-plugin` in [example-config](https://github.com/ElcanoTek/example-config),
and the full mapping + failure boundaries are in
[docs/AGENT-PLUGINS.md](../../docs/AGENT-PLUGINS.md).

## The execution sandbox is a bundle artifact

Agent tool calls (`bash`, `run_python`) execute inside a hardened, rootless
container. That image is a **per-client bundle artifact**, not a fleet-global
one — each bundle ships its own `sandbox/Containerfile` flavor (whose base tracks
`fedora-minimal:latest` and runs `microdnf upgrade`, so every on-box rebuild gets
the current coherent Fedora package set). The generic bundle intentionally does
not shadow RPM-owned Python packages with hand-maintained pip pins. A client
bundle can pin the base digest and package NEVRAs in its Containerfile, or set a
digest-pinned prebuilt `sandbox.image`, when byte-for-byte reproducibility is more
important than following Fedora latest; see this bundle's `sandbox/Containerfile`.

The manifest's `sandbox:` block declares it:

```yaml
sandbox:
  containerfile: sandbox/Containerfile      # built from the bundle (default path)
  tag: localhost/fleet-sandbox:latest        # local image tag when building on-box
  image: "${FLEET_SANDBOX_IMAGE:-}"          # OPTIONAL prebuilt ref; non-empty WINS
```

- **Build-on-box (default).** `scripts/build-sandbox-image.sh` builds
  `containerfile` → `tag`, and the fleet process consumes `tag`. The supply
  chain stays auditable on the deployment box. Run it as a deploy step:
  `FLEET_CLIENT_CONFIG_DIR=<bundle> scripts/build-sandbox-image.sh`.
- **Registry publish (opt-in).** Set `sandbox.image` to a prebuilt ref (e.g. a
  pushed `ghcr.io/...@sha256:...`); the process pulls/uses it and `tag` is
  ignored. An explicit `FLEET_SANDBOX_IMAGE` in the process env overrides
  everything.

`Bundle.Sandbox().ResolvedImageRef()` is the single resolution point
(`image` if set, else `tag`). fleet does **not** build at process startup.

## The agent runtime

fleet runs **one** native agent loop, in the fleet process. Every tool call's
model-authored local execution (`bash`, `run_python`, file I/O) runs inside the
rootless-Podman sandbox under host policy. MCP calls are the deliberate
host-side exception, and their credentials are isolated by the out-of-process
MCP broker — they never enter the sandbox. There is no flavor picker and no
external-agent delegation. See **[`docs/AGENT-RUNTIME.md`](../../docs/AGENT-RUNTIME.md)**
for the runtime mechanics (per-turn sandbox seal, cost/token ceilings, context
compaction, the per-task MCP credential allowlist, the scheduled end-of-run
verifier, and git-worktree isolation).
