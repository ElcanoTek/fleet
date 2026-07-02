# The MCP connector directory — trust classes, built-in catalog, provenance

Issues #538 (trust-labeled catalog) and the directory expansion (built-in
catalog + categories + provenance tiers). fleet exposes two classes of MCP
connectors, and the difference is a **trust boundary**, not a cosmetic
grouping:

| | Bundled (`mcp_servers`) | Hosted directory (`remote_mcp_catalog`) |
| --- | --- | --- |
| Who wrote/ships it | Your operator's client bundle | The named external operator |
| Where it runs | Inside the mandatory sandbox on this box | On the operator's infrastructure |
| Credentials | Brokered host-side, never leave the deployment | Your own OAuth login / key with the operator |
| Who sees tool traffic | Only this deployment (plus the connector's own upstream API) | The operator, under its own terms |
| How it's enabled | Per conversation, in the Tools picker | Per user: Settings → Connections → Add + Connect |

## The built-in directory

fleet ships a large curated directory of hosted MCP servers **embedded in the
binary** (`internal/clientconfig/builtin_remote_catalog.yaml`, ~275 entries
across ~19 categories), so every deployment gets a rich, searchable connector
directory without each client bundle copying hundreds of lines of listings.
Every bundle inherits it by default.

**A directory entry is a listing, not a connection — and that property is
load-bearing.** fleet never contacts an entry's URL until a user explicitly
adds the server, and the add goes through the existing per-user remote-MCP
OAuth flow (#443) — discovery, dynamic client registration, PKCE, encrypted
token storage. The directory only saves the user from typing a URL; it grants
nothing by itself. Any change that makes directory entries auto-connect must
revisit the inherit-by-default decision here.

### Manifest knobs

```yaml
remote_mcp_catalog_builtin: false     # opt out of the built-in directory entirely
remote_mcp_catalog_community: true    # ALSO inherit community-provenance entries (default false)
remote_mcp_catalog_hidden: [x, y]     # tombstone individual built-in entries by name
remote_mcp_catalog:                   # bundle-authored entries; same-name entries
  - name: github                      # override the built-in ones
    ...
```

Merge rules (`internal/clientconfig/remote_catalog_builtin.go`): bundle
entries lead the listing and override built-ins by name; hidden names drop;
community-provenance built-ins are inherited only on explicit opt-in; a
built-in entry whose name collides with a bundled `mcp_servers` connector is
dropped **with a loud log line**, never silently. Bundle-authored entries keep
the hard validation error for that collision.

The `remote_mcp_catalog_hidden` tombstone doubles as the kill switch: a static
shipped catalog rots (endpoints move, vendors sunset betas), and an operator
can delist a dead or compromised entry with a config-only change between fleet
releases.

### Entry schema

```yaml
- name: github                    # stable id, unique
  display_name: GitHub
  description: >-
    Repositories, issues, pull requests, CI/CD workflow runs, ...
  url: "https://api.githubcopilot.com/mcp/"   # https only
  vendor: GitHub, Inc.            # who OPERATES the endpoint
  docs_url: "https://docs.github.com/..."     # vet it: vendor documentation
  repo_url: "https://github.com/github/github-mcp-server"  # vet it: source
  category: development           # directory grouping (lowercase kebab-case)
  tags: [git, repositories, issues, pull-requests, ci]     # search keywords
  provenance: official            # official | third_party | community
  auth: oauth                     # oauth | api_key | open | tenant
```

**Provenance is the hosting-operator trust tier** — who runs the endpoint,
distinct from the bundled/third_party *class*:

- `official` — the service's own vendor operates the endpoint (GitHub hosting
  GitHub's server).
- `third_party` — an identifiable platform company hosts access to **other**
  vendors' services (Zapier, Make, Pipedream, Composio, Smithery). That
  operator — not the underlying vendor — sees the traffic and often holds the
  delegated tokens.
- `community` — an identifiable maintainer who is neither. Community entries
  are **not inherited by default** (`remote_mcp_catalog_community` gates them)
  and must carry a `repo_url` so users can vet the code. An endpoint that
  cannot be attributed to a named operator with vettable source does not get
  listed at all.

In the built-in catalog `provenance`, `category`, `vendor`, `docs_url`, and
`auth` are **required on every entry** — trust and grouping are never
inherited by omission in the file every deployment silently receives
(`TestBuiltinRemoteCatalog` fails CI on a violation, including the
`{placeholder}`-URL ⟺ `auth: tenant` invariant). Bundle-authored entries keep
back-compat defaults: absent `provenance` → `official` (every pre-existing
bundle's entries were vendor-official), absent `category` → grouped under
"Other".

## Load-time validation

Fail-loud, like every manifest section: unique `name` not colliding with a
bundled `mcp_servers` name, required `display_name`/`description`, `https://`
`url` and `repo_url`, `provenance`/`auth` from their closed sets, lowercase
kebab-case `category` (the set is open — a bundle may invent its own grouping
— but the shape is not), lowercase `tags`.

## API

`GET /mcp-catalog` (chat server; auth + member) returns both classes with
explicit trust tags plus the directory metadata:

```json
{
  "bundled":     [{"name": "...", "trust": "bundled", "tool_count": 12, ...}],
  "third_party": [{"name": "...", "trust": "third_party", "url": "https://...",
                   "vendor": "...", "category": "development", "tags": ["..."],
                   "provenance": "official", "auth": "oauth",
                   "repo_url": "https://github.com/...", ...}],
  "remote_mcp_enabled": true
}
```

`bundled` is the Optional-server catalog snapshot (the same source as
`/mcp-servers`; always-on servers need no opt-in decision so they are not
listed). `remote_mcp_enabled` reports whether the OAuth flow is configured
(`FLEET_MCP_OAUTH_ENCRYPTION_KEY` + `FLEET_PUBLIC_BASE_URL`), so the UI can
render one-click Add vs an explanatory hint.

## UI

Settings → Connections renders the "Connector directory" panel (open by
default; the search/grouping/badge helpers live in
`web/src/app/settings/connections/catalog.ts`, unit-tested):

- **Search** across name, description, vendor, category, and tags, plus
  category filter chips with counts; results grouped under category headers.
- **Provenance badges** — green "Official", amber "Aggregator"
  (`third_party`), amber "Community". An unknown provenance value renders as
  Community — bad input never inflates trust.
- **Vet-it links** — every card links the vendor `docs` and, when present, the
  `source` repository.
- **Auth hints** — "No sign-in needed" (`open`), "API key", or "Needs your
  URL" for tenant-scoped entries.
- **Consent step** — adding a non-official entry opens an explicit,
  operator-named confirmation stating that the operator receives tool-call
  arguments (which can include conversation content) and, for OAuth flows,
  holds the delegated access token, with the docs/source links inline. A badge
  alone gets scrolled past; the consent gate keys off the operator identity,
  so a mislabeled provenance degrades to *more* friction, not silent egress.
- **Bundled by your workspace** — unchanged: green "Bundled" badge,
  informational (toggled per conversation in the Tools picker).

### Tenant-scoped entries

Some vendors host their MCP servers **per org/store/workspace** — the endpoint
contains a `{placeholder}` (Databricks, Salesforce, Snowflake, NetSuite, …).
These are listed for discoverability, but they can't be one-click added: the
UI shows "Needs your URL" and the user pastes their own tenant endpoint into
the manual add form.

## Curation guidance

The built-in catalog was assembled from vendor documentation with every
endpoint verified against a primary source (the `docs_url`); popular services
with **no** hosted endpoint (stdio-only packages) are deliberately absent —
this directory lists only endpoints a fleet user can actually connect to.
For bundle authors:

- Prefer overriding/hiding built-ins over re-listing; keep bundle-authored
  additions to endpoints you have verified against the vendor's docs.
- Set `provenance` explicitly on anything that is not vendor-operated; never
  list an endpoint you cannot attribute to a named operator.
- Every listing is an implicit endorsement; `remote_mcp_catalog_hidden` exists
  so you can trim without forking the whole directory.

## Honest scope

- The directory does not pin, proxy, health-check, or scan third-party
  endpoints; the trust labeling, the consent step, and the user's own OAuth
  consent are the control. TLS-hardening knobs (`tls:` pinning, #280) apply to
  bundled http servers, not to per-user remote connections.
- Endpoint URLs rot as vendors move; the shipped list is a snapshot maintained
  in fleet releases, with `remote_mcp_catalog_hidden` as the between-release
  kill switch. A stale URL fails at add/connect time with the normal discovery
  error — nothing silent.
- `auth` is a UI hint derived from vendor docs at curation time, not an
  enforced contract.
