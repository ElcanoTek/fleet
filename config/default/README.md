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
  mcp/                 # the client's Python MCP servers (+ requirements.txt)
```

See `internal/clientconfig/clientconfig.go` for the manifest schema and the
authoritative description of each field, including the MCP catalog's declarative
enable gate (`enabled_env` / `enabled_groups` / `always`), the `${VAR}` env
interpolation, the tool allowlist, and the account-suffix vars.

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
