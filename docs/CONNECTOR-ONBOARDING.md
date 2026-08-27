# Connector-directory onboarding — guided setup, API keys, BYO OAuth clients

The connector directory (docs/MCP-CATALOG.md) shipped as a rich *listing*, but
~100 of its entries were advertising without onboarding: `tenant` entries said
"Needs your URL" in a hover tooltip, `api_key` entries had **no working connect
path at all** (the per-user flow spoke only OAuth and open), and OAuth entries
with prerequisites — Google Workspace needs your own GCP OAuth client,
Microsoft Work IQ needs an Entra app registration — failed mid-discovery with
no explanation of what to do. This feature makes every directory entry either
connectable in place or honestly, visibly documented on the card.

## Bundle-managed inbound email reports

An SES-to-S3 email-report archive is not a connector-directory entry and is not
provisioned by Fleet. It combines external AWS/DNS resources with a read-only
MCP server owned by the client-config bundle. Use the external canonical
[new-client email-report runbook](https://github.com/ElcanoTek/ses-s3-setup/blob/main/docs/NEW-CLIENT-EMAIL-SETUP.md)
for domain/MX decisions, tenant infrastructure, IAM separation, bundle wiring,
historical mailbox migration, validation, and rollback. Keep real client
identifiers in that client's bundle deployment record, never in this engine
repository.

## What shipped

**API-key auth for per-user remote MCP** (migration 039, `internal/store`,
`internal/remotemcp`, `internal/agent`):

- `remote_mcp_servers` gains `auth_kind` (`oauth` | `open` | `api_key`,
  backfilled from `issuer` on existing rows), `api_key_header` (header NAME,
  non-secret), and `api_key_enc`. The key is sealed with the same AES-256-GCM
  cipher as OAuth tokens, AAD-bound to `(purpose, owner email, canonical URL)`.
- `AddServer` accepts `auth: "api_key"` + the key + optional header name; the
  header name is vetted against an RFC 7230 token shape and a denylist of
  transport-owned headers, and the key against header-safe ASCII — nothing the
  user types can smuggle CRLF into the replayed request.
- The run loop's single credential path (`AcquireTokenByID`) returns the
  decrypted key; the overlay sends it as `Authorization: Bearer <key>` or, when
  the connection carries a header name, as `<header>: <key>` raw. Shared
  connections decrypt under the **owner's** AAD, exactly like OAuth.
- Rotation: `PUT /remote-mcp-servers/{id}/key` (owner-scoped, write-only).
  The UI's connection row shows "Update key" instead of Connect/Reconnect;
  `open` connections show neither.

**Add-time validation handshake**: `open` and `api_key` adds — the two flows
with no OAuth login step to prove the connection — are validated with a real
MCP handshake (initialize + tools/list over the SSRF-safe client, bounded by
the HTTP timeout) before anything is stored. A wrong key or mistyped tenant
URL fails the add with an actionable error ("the server did not accept this
API key…") and the guided form keeps the user's typed values for correction;
a successful add reports the observed tool count ("connected — 12 tools
available"). Key rotation validates the NEW key the same way and leaves the
old key untouched on rejection. OAuth adds are unchanged — their login flow
is the validation.

**Catalog setup-guidance fields** (`internal/clientconfig`, surfaced through
`GET /mcp-catalog`):

- `setup_hint` — one or two sentences rendered VISIBLY on the directory card:
  where the URL or key comes from, what must exist first. Required (CI-enforced)
  in the built-in catalog for every `tenant`/`api_key` entry.
- `setup_url` — the vendor page that actually walks through connecting
  (distinct from `docs_url`, which describes what the server does).
- `api_key_header` — the header name the vendor expects (validated shape).
- `client_registration: manual` — the vendor's authorization server has no
  RFC 7591 dynamic registration; the card collects a bring-your-own OAuth
  client ID (+ optional secret) up front and passes it to the existing manual
  client path in `AddServer`, instead of letting the add fail mid-discovery.

**Guided add forms in Settings → Connections** (`DirectoryCard`):

- Tenant entries: one input per `{placeholder}` in the URL template
  (humanized labels), a live preview of the resulting endpoint, then the
  normal add. `auth: tenant` = your URL + OAuth discovery; `auth: open` with a
  placeholder covers vendors whose key/account id rides in the URL itself
  (Scrapfly, thirdweb, Smartlead — their hints disclose that the key becomes
  part of the connection URL).
- API-key entries: a write-only key field; the add lands `connected`
  immediately.
- `client_registration: manual` entries: client ID + optional secret fields.
- The consent gate for non-official provenance is unchanged and now also
  carries the guided form's collected values through the confirm.
- The "remote MCP isn't configured" dead-end is admin-aware: admins see the
  env vars to set; members are told to ask their administrator.

**Catalog data refresh** (`builtin_remote_catalog.yaml`, 289 → 292 entries):

- Setup hints + setup URLs authored for **all 37 tenant and 60 api_key
  entries**, researched against each vendor's primary docs; verified
  non-Bearer header names recorded (`x-api-key` for Exa, `X-Goog-Api-Key` for
  Google Maps, `X-CMC-MCP-API-KEY` for CoinMarketCap, …).
- Google Workspace: hints + `client_registration: manual` on the four
  Developer Preview servers (Gmail, Drive, Calendar, Chat) pointing at
  Google's configure-mcp-servers guide; **new** `google-people` (Contacts)
  entry; **new** community `google-workspace-self-hosted` entry
  (taylorwilsdon/google_workspace_mcp) covering Docs/Sheets and the rest of
  the suite without preview gating.
- Microsoft 365: hints on the four Work IQ (Agent 365 preview) tenant entries
  — tenant ID location, Copilot-license + Entra-app prerequisites — plus
  **new** community `microsoft-365-self-hosted` entry
  (softeria/ms-365-mcp-server) for Graph access without a Copilot license.
- Corrections where research contradicted the data: `aws-mcp` is OAuth, not
  api_key (no pasteable key exists); Zapier's hint explains the
  create-your-MCP-server-first prerequisite; Slack's and HubSpot's
  fixed-client/registered-app OAuth is expressed as `client_registration:
  manual` with hints naming the admin-approval requirements.
- **Curation audit**: 25 entries removed as low-quality or unusable — docs-
  search-only servers for niche products (the major-platform docs servers
  stay: Microsoft Learn, OpenAI, Cloudflare, Context7, DeepWiki, AWS
  Knowledge), entries with unworkable auth (an expiring phone-app passcode,
  keys only obtainable by contacting the vendor's API team, a vendor-
  disclaimed unsupported beta), and obscure vendors duplicating stronger
  same-category listings.
- **15 additions**, each verified live against the vendor's endpoint and
  primary docs: Docusign, Adobe for Creativity, WordPress.com, Contentful,
  Sanity, Algolia, Cloudinary, Amazon Ads, DoorDash, ServiceNow (per-
  instance), Microsoft Dataverse/Dynamics 365, Vimeo, Egnyte, Granola, and
  Jotform. Notable checked-but-absent (no official hosted endpoint yet, so
  deliberately not listed): Zendesk, Xero, QuickBooks, Discord, Perplexity,
  ElevenLabs, Workday, LinkedIn, Reddit, eBay — revisit in future releases.

**Featured shelf + catalog curation pass**: entries can set `featured: true`
to appear in a short curated section rendered before the category listing
(unfiltered view only). The built-in shelf holds the household names — the
Google Workspace trio, GitHub, Notion, Slack, Linear, Atlassian, Asana,
monday.com, Airtable, Stripe, PayPal, HubSpot, Canva, Figma, Zapier, Hugging
Face — and a test caps it at 8–20 entries, never community provenance. The
same pass audited the directory for dead/low-quality listings and added
newly-verified official endpoints (see CHANGELOG).

## Deviations / honest scope

- **`auth: tenant` semantics narrowed**: it now means "your URL + OAuth". The
  previous catalog conflated URL shape with protocol; the shape invariant in
  `TestBuiltinRemoteCatalog` allows `{placeholder}` only on `tenant` and
  `open` entries.
- **One credential per connection.** Vendors needing two headers (Firefly's
  access+secret pair, Zenhub's workspace header) or non-static schemes
  (Pipedream's client-credentials tokens) can't be fully expressed; their
  hints say so plainly rather than pretending. Basic/Token schemes work by
  pasting the full value (`Basic <base64>`, `Token <key>`) with
  `api_key_header: Authorization` — the raw value is sent unprefixed.
- **Validation is a point-in-time probe.** The add/rotate handshake proves
  the credential works at that moment; a key revoked later still surfaces at
  run time (the run skips the server and reports it). There is no periodic
  re-probe.
- **Community self-hosted entries stay opt-in** behind
  `remote_mcp_catalog_community: true` — the trust posture for
  community-provenance listings is unchanged even though *you* host these two.
- **AWS SigV4 signing** remains deferred (docs/OPEN-REMOTE-MCP.md).
- The manual "Add remote server" form still has no client-ID fields; the
  backend's actionable error covers that path, and directory entries that
  need one carry the guided fields.
