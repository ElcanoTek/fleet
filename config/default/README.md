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
  manifest.yaml        # branding, models, mcp_servers[], empty_state, sandbox
  sandbox/
    Containerfile      # the bundle's execution-sandbox image (build-on-box)
  system_prompts/
    default.md         # scheduled-agent base prompt
    chat.md            # interactive-chat base prompt
  personas/            # *.yaml — one per persona; PERSONA_DEFAULT picks the default
  protocols/           # *.yaml | *.md — reusable playbooks
  skills/              # <name>/SKILL.md — Agent Skills (progressive disclosure)
  mcp/                 # the client's Python MCP servers (+ requirements.txt)
```

See `internal/clientconfig/clientconfig.go` for the manifest schema and the
authoritative description of each field, including the MCP catalog's declarative
enable gate (`enabled_env` / `enabled_groups` / `always`), the `${VAR}` env
interpolation, the tool allowlist, and the account-suffix vars.

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

## The execution sandbox is a bundle artifact

Agent tool calls (`bash`, `run_python`) execute inside a hardened, rootless
container. That image is a **per-client bundle artifact**, not a fleet-global
one — each bundle ships its own `sandbox/Containerfile` flavor (and pins its own
base-image **digest** for reproducibility; see this bundle's
`sandbox/Containerfile`).

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

## Runtime flavors and external agents

The manifest's `runtimes:` block declares the selectable execution flavors. This
generic bundle ships the two native flavors (`native-inprocess`, `native-acp`)
plus **documented placeholder** external entries for `claude-code` and `goose`
(`type: acp`, `network: model_only`, `delegated_policy: true`, with their
model-cred env var names). The placeholder images are `localhost/...` refs — a
real `XYZ-config` bundle overrides them with its own digest-pinned provider
images and supplies the provider model keys.

External (`type: acp`) flavors drive a third-party agent that **self-executes**
in a locked sandbox — the **containment** tier, not full governance. Before
enabling one, read **[`docs/USING-AGENTS.md`](../../docs/USING-AGENTS.md)**: it
spells out the governance tiers, the permission UI (default-deny, no
approve-all), the data-residency caveat (an external agent can send the
workspace to its own model endpoint), and a worked example. An `XYZ-config` repo
can carry its own provider `runtimes:` entries exactly like this bundle's.
