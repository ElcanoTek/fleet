# The MCP connector catalog — bundled vs third-party trust classes

Issue #538. fleet exposes two classes of MCP connectors, and the difference is
a **trust boundary**, not a cosmetic grouping:

| | Bundled (`mcp_servers`) | Third-party hosted (`remote_mcp_catalog`) |
| --- | --- | --- |
| Who wrote/ships it | Your operator's client bundle | The named external vendor |
| Where it runs | Inside the mandatory sandbox on this box | On the vendor's infrastructure |
| Credentials | Brokered host-side, never leave the deployment | Your own OAuth login with the vendor |
| Who sees tool traffic | Only this deployment (plus the connector's own upstream API) | The vendor, under its own terms |
| How it's enabled | Per conversation, in the Tools picker | Per user: Settings → Connections → Add + Connect |

## The curated directory (`remote_mcp_catalog`)

A bundle may curate a directory of **official vendor-hosted** MCP servers in
`manifest.yaml`:

```yaml
remote_mcp_catalog:
  - name: github
    display_name: GitHub
    description: >-
      Read and manage GitHub repositories, issues, and pull requests through
      GitHub's hosted MCP server.
    url: "https://api.githubcopilot.com/mcp/"
    vendor: GitHub, Inc.
    docs_url: "https://docs.github.com/..."
```

Load-time validation (fail-loud, like every manifest section): unique `name`
that must not collide with a bundled `mcp_servers` name, required
`display_name`/`description`, and an `https://` `url`. The generic bundle
ships a starter directory (`config/default/manifest.yaml`) and
`TestDefaultBundleRemoteMCPCatalogValid` keeps every shipped entry https, with
a named vendor and a docs link.

**A catalog entry is a listing, not a connection.** fleet never contacts the
URL until a user explicitly adds the server, and the add goes through the
existing per-user remote-MCP OAuth flow (#443) — discovery, dynamic client
registration, PKCE, encrypted token storage. The catalog only saves the user
from typing a URL; it grants nothing by itself.

## API

`GET /mcp-catalog` (chat server; auth + member) returns both classes with
explicit trust tags:

```json
{
  "bundled":     [{"name": "...", "trust": "bundled", "tool_count": 12, ...}],
  "third_party": [{"name": "...", "trust": "third_party", "url": "https://...", "vendor": "...", ...}],
  "remote_mcp_enabled": true
}
```

`bundled` is the Optional-server catalog snapshot (the same source as
`/mcp-servers`; always-on servers need no opt-in decision so they are not
listed). `remote_mcp_enabled` reports whether the OAuth flow is configured
(`FLEET_MCP_OAUTH_ENCRYPTION_KEY` + `FLEET_PUBLIC_BASE_URL`), so the UI can
render one-click Add vs an explanatory hint.

## UI

Settings → Connections gains a collapsible "Connector catalog" panel:

- **Bundled by your workspace** — green "Bundled" badge, with copy explaining
  the sandbox + host-side-credential posture. Informational: these are toggled
  per conversation in the Tools picker, not connected here.
- **Third-party hosted** — amber "Third-party" badge, vendor name + docs link,
  with copy stating plainly that tool calls (which can include conversation
  content) go to the vendor under the vendor's terms. "Add" prefills the
  existing add-server flow; entries whose URL is already added show "Added".

### Tenant-scoped entries

A few vendors host their MCP servers **per org/store/workspace** — the endpoint
contains a `{placeholder}` (Databricks, Google Cloud/Workspace, Salesforce,
Shopify Storefront in the shipped list). These are listed for discoverability,
but they can't be one-click added: the UI shows "Needs your URL" and the user
pastes their own tenant endpoint into the manual add form.

## Curation guidance for bundle authors

- List only **official, vendor-operated** endpoints, verified against the
  vendor's docs (that's what `docs_url` is for). Community mirrors of a
  vendor's API don't belong in a curated directory.
- Trim the shipped starter list to what your users should see; every entry is
  an implicit endorsement.
- The directory is client content: each client bundle decides its own list.

## Honest scope

- The catalog does not pin, proxy, or scan the third-party endpoint; the trust
  labeling and the user's own OAuth consent are the control. TLS-hardening
  knobs (`tls:` pinning, #280) apply to bundled http servers, not to per-user
  remote connections.
- Endpoint URLs rot as vendors move; the shipped list is a snapshot the bundle
  author maintains. A stale URL fails at add/connect time with the normal
  discovery error — nothing silent.
