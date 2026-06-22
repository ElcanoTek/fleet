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
  manifest.yaml        # branding, models, mcp_servers[], empty_state
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
