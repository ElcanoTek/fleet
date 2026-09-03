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

## Connector copy — `display_name` and `description`

Two strings decide whether a user can tell what a connector is: the
`display_name` and `description` a bundle attaches to each `mcp_servers`
entry. Chat's Tools picker (the wrench popover) and Settings → Connections
both render `display_name || name` as the row title and `description` beneath
it — and render **nothing** when the description is empty. A connector that
ships neither appears as a raw snake_case identifier over a blank body, which
reads as a broken row rather than an unlabelled one.

fleet cannot author that copy — bundles are data, the engine is not — so it
does the two things an engine can:

- **A derived label floor.** A missing `display_name` falls back to a
  humanized form of the server name (`openx_mcp` → "Openx",
  `knowledge_base` → "Knowledge Base"), so the worst case is a plain label
  rather than a wire identifier. It is a floor, not a substitute: the
  derivation cannot know that `openx_mcp` is spelled "OpenX".
- **A loud boot warning.** Each connector missing either field logs one
  `clientconfig: warning:` line naming the connector and the field. Neither
  gap fails the load: display copy is cosmetic, and taking a deployment down
  over a missing sentence would be the wrong trade.

Because it only warns, the enforcement lives where the data lives — bundle
repos assert the house style below in their own `manifest.yaml` tests.

### The house style

**`display_name`** — the service as a person names it, in the vendor's own
casing: "OpenX", "Index Exchange", "Amazon SES Email". Title Case, ≤ 40
characters. No underscores, and none of the words "MCP", "server", or
"connector" — the user already knows they are looking at a connector list.

**`description`** — one or two plain-text sentences, ≤ 200 characters total.

1. **Capability first.** Open with a bare imperative verb and name the
   concrete system and the objects it acts on: "Search and read inbound
   email reports stored by Amazon SES in the tenant S3 archive." Not "This
   server provides…", not "An MCP server for…", not "Tools for…", and not a
   restatement of the display name.
2. **Then gating, if any.** A connector behind `enabled_env` /
   `enabled_groups` ends with one clause naming the real variables:
   "Appears once `MAGNITE_ACCESS_KEY` and `MAGNITE_SECRET_KEY` are set."
   (A long family of variables may collapse to a prefix — "once the
   `FEEDS_AWS_*` credentials are set" — but never to a vague "once
   configured".) Never write that clause for an ungated connector: the
   picker shows it regardless and the sentence becomes a lie. An ungated
   connector either stops after the capability sentence or says so plainly —
   "No credentials required."

No markdown, no emoji, no trailing whitespace; end with a period. Keep
example prompts out of it: the row is a two-line label, and onboarding
guidance belongs in `setup_hint` (directory entries) or the bundle's own
docs.

The same two fields, with the same style, are **required** on
`remote_mcp_catalog` entries and on every built-in directory entry — there
the load fails outright on a gap, because a directory listing has no other
identity to fall back on.

## The built-in directory

fleet ships a large curated directory of hosted MCP servers **embedded in the
binary** (`internal/clientconfig/builtin_remote_catalog.yaml`, ~275 entries
across ~19 categories), so every deployment gets a rich, searchable connector
directory without each client bundle copying hundreds of lines of listings.
Every bundle inherits it by default.

**A directory entry is a listing, not a connection — and that property is
load-bearing.** fleet never contacts an entry's URL until a user explicitly
adds the server, and the add goes through the per-user remote-MCP flow (#443)
— for OAuth entries: discovery, dynamic client registration, PKCE, encrypted
token storage; for API-key entries: the pasted key is sealed with the same
cipher and replayed host-side. The directory only saves the user from typing a
URL; it grants nothing by itself. Any change that makes directory entries
auto-connect must revisit the inherit-by-default decision here.

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
  # Onboarding guidance (connector-directory onboarding):
  setup_hint: >-                  # VISIBLE on the card: where the URL/key comes
    Create a key under ...        # from, prerequisites (plan tier, app
                                  # registration, self-hosted deployment)
  setup_url: "https://..."        # the vendor page that walks through connecting
  api_key_header: X-API-Key       # api_key only: header NAME the key is sent
                                  # under ("" = Authorization: Bearer <key>)
  client_registration: manual     # the vendor's AS has no dynamic client
                                  # registration; the card collects a
                                  # bring-your-own OAuth client ID (+ secret)
  featured: true                  # curated Featured-shelf pick (kept small:
                                  # 8–20 built-in entries, never community)
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
(`TestBuiltinRemoteCatalog` fails CI on a violation). Two more shape
invariants: `auth: tenant` requires a `{placeholder}` URL, and a
`{placeholder}` URL requires `auth` tenant **or** open (open covers vendors
that authenticate via the URL itself, e.g. a key as a query parameter); and
every entry a user cannot one-click add (`tenant` or `api_key`) **must carry a
`setup_hint`** — a listing that names a prerequisite without saying how to
satisfy it is advertising, not onboarding. Bundle-authored entries keep
back-compat defaults: absent `provenance` → `official` (every pre-existing
bundle's entries were vendor-official), absent `category` → grouped under
"Other", `setup_hint` optional.

## Load-time validation

Fail-loud, like every manifest section: unique `name` not colliding with a
bundled `mcp_servers` name, required `display_name`/`description`, `https://`
`url` and `repo_url`, `provenance`/`auth` from their closed sets, lowercase
kebab-case `category` (the set is open — a bundle may invent its own grouping
— but the shape is not), lowercase `tags`.

The bundled (`mcp_servers`) side is deliberately softer: a missing
`display_name` is filled from the derived label and a missing `description`
warns, both without failing the load. See "Connector copy" above for why the
two sides differ.

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

Entries also carry the onboarding fields when present: `setup_hint`,
`setup_url`, `api_key_header`, `client_registration`.

`bundled` is the Optional-server catalog snapshot (the same source as
`/mcp-servers`; always-on servers need no opt-in decision so they are not
listed). `remote_mcp_enabled` reports whether the per-user remote flow is
configured (`FLEET_MCP_OAUTH_ENCRYPTION_KEY` + `FLEET_PUBLIC_BASE_URL`), so
the UI can render one-click Add vs an explanatory hint (admins see the env
vars to set; members are told to ask their administrator).

## UI

Settings → Connections renders the "Connector directory" panel (open by
default; the search/grouping/badge helpers live in
`web/src/app/settings/connections/catalog.ts`, unit-tested):

- **Featured shelf** — a short curated section of household-name connectors
  (Gmail, Drive, Notion, Slack, Stripe, GitHub, …) rendered before the
  category listing on the unfiltered view. `featured: true` in the catalog;
  a test caps the built-in shelf at 8–20 entries and forbids featuring
  community entries (hidden by default, so a featured one would silently
  vanish).
- **Search** across name, description, vendor, category, and tags, plus
  category filter chips with counts; results grouped under category headers.
- **Provenance badges** — green "Official", amber "Aggregator"
  (`third_party`), amber "Community". An unknown provenance value renders as
  Community — bad input never inflates trust.
- **Vet-it links** — every card links the vendor `docs` and, when present, the
  `source` repository and the `setup guide`.
- **Setup hints** — an entry's `setup_hint` renders visibly on the card (not a
  tooltip), with the `setup_url` walkthrough linked inline.
- **Auth hints** — "No sign-in needed" (`open`), "Needs an API key", or "Needs
  your URL" for tenant-scoped entries.
- **Guided add forms** — entries that can't be one-click added open an inline
  form on the card instead of dead-ending: tenant entries get one input per
  `{placeholder}` with a live preview of the resulting endpoint URL; `api_key`
  entries get a write-only key field (sealed server-side, never echoed);
  `client_registration: manual` entries get bring-your-own OAuth client ID +
  secret fields. The submitted add goes through the same consent gate and the
  same POST as everything else.
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
contains a `{placeholder}` (Databricks, Salesforce, Snowflake, NetSuite, the
Microsoft Work IQ suite, …). The card's guided form renders one input per
placeholder (with the entry's `setup_hint` explaining where the value comes
from) and previews the resulting URL before adding. `auth: tenant` entries
then run the normal OAuth discovery; `auth: open` placeholder entries (vendors
whose key/account id rides in the URL itself) connect immediately.

### API-key entries

`auth: api_key` entries connect by pasting a vendor key into the card's
write-only field. The key is sealed at rest with the same AES-256-GCM cipher
as OAuth tokens (AAD bound to purpose + user + canonical URL) and replayed
host-side on every MCP request — under `Authorization: Bearer <key>` by
default, or the entry's `api_key_header` name. It never enters the sandbox,
the model context, a log line, or any HTTP response; rotation is
`PUT /remote-mcp-servers/{id}/key` ("Update key" on the connection row).

Because api_key and open adds have no OAuth login step to prove the
connection, both are **validated at add time with a real MCP handshake**
(initialize + tools/list over the SSRF-safe client) before anything is
stored: a rejected key or unreachable URL fails the add with an actionable
error and the guided form keeps the typed values, while a successful add
confirms with the observed tool count. Rotation validates the new key the
same way and keeps the old key on rejection.

### Self-hosted entries

A few community entries (`google-workspace-self-hosted`,
`microsoft-365-self-hosted`) describe servers **you deploy yourself** to cover
gaps the vendors' hosted servers leave (Google's preview servers lack
Docs/Sheets; Microsoft's Work IQ servers require a Copilot license). Their URL
placeholder is your own deployment's hostname and the `setup_url` is the
deployment guide. Like all community-provenance entries they are hidden unless
the bundle opts in via `remote_mcp_catalog_community: true`.

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
