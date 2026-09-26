# Hosted MCP connector status — the #1006 OAuth pack and the catalog audit

The record for #986 ("test official MCPs and improve the catalog") and its
child #1006 ("test the OAuth flow with official MCPs"). Two kinds of evidence
are kept apart here: **live runs** — a real account, the browser consent
screen, a real tool call from a chat and from a scheduled task, a forced
token refresh, a sign-out — and **discovery probes**, which run fleet's own
`mcpoauth.Discover` and add-time guards against a vendor's published metadata
without logging in. A probe proves a server can be *added*; only a live run
proves it *works*. Dates are when the row was last verified; a row is not
re-verified by later releases. The appendix is the #986 Phase 1 inventory:
every one of the 288 built-in entries with its auth, provenance, category,
Featured flag, whether CI could exercise it, and when it was last verified.

Everything below was run against a local rig: one fleet process with the
Postgres pair, the web tier on `http://localhost:3200`, real OpenRouter
models, real vendor accounts. Vendors that need an HTTPS callback (Slack)
were reached through a temporary Cloudflare quick tunnel.

## Live runs (the manual OAuth pack)

Legend — ✓ passed · ✗ failed · — not exercised · n/a the vendor has no such
thing · **fixed by** names the fleet PR that had to land first.

| connector | auth shape | add | consent + token | tools list | tool call (chat) | scheduled task | refresh | sign-out | seats / sharing | verdict | last verified |
|---|---|---|---|---|---|---|---|---|---|---|---|
| GitHub | manual client, **secret required** | ✓ | ✓ | ✓ | ✓ `get_me` | ✓ | ✓ natural + forced (8 h token; dead refresh → `needs_reauth`) | local only (no revocation endpoint); vendor "Revoke all" → `needs_reauth` on the next 401 | 2 seats ✓ | **PASS** — fixed by #1405 #1446 #1449 #1454 #1464 #1465 #1467 | 2026-09-10 |
| Notion | dynamic registration, public client | ✓ | ✓ | ✓ | ✓ | ✓ pinned to the `test` seat | ✓ natural (headless in the scheduled run) | — | 2 seats ✓; share to a second user ✓, revoke ✓ | **PASS** | 2026-09-10 / sharing 2026-09-14 |
| Linear | dynamic registration, public client | ✓ | ✓ | ✓ | ✓ direct and deferred | ✓ | ✓ natural | ✓ real revocation endpoint; reconnect keeps the client id | — | **PASS** — fixed by #1467 (prompt roster) | 2026-09-09 |
| Slack | manual client, **secret required**, HTTPS callback | ✓ | ✓ | ✓ | ✓ deferred | ✓ `search_channels` | n/a — token has no expiry and no refresh token (rotation off) | local only (no revocation endpoint) | — | **PASS** with caveats — fixed by #1471 #1472; app needs the *Model Context Protocol* toggle | 2026-09-10 |
| Google Drive | manual client (GCP), **secret required** | ✓ | ✓ | ✓ | ✗ `The caller does not have permission` | mount + refresh ✓, call ✗ | ✓ forced (1 h token; needs `access_type=offline` + `prompt=consent`) | — | — | **BLOCKED by vendor** — Workspace MCP is a Developer Preview; a personal Gmail account is not eligible. fleet side fixed by #1465 | 2026-09-10 |
| Azure DevOps | tenant URL, manual Entra app, **secret required** | ✓ | ✓ tenant-native user | ✓ | ✓ `core_list_projects`, `repo_repository` | ✓ | ✓ forced (71 min token, rotated refresh token) | local only (no revocation endpoint) | — | **PASS** — fixed by #1481 | 2026-09-13 |
| Stripe | dynamic registration, public client | ✓ | ✓ | ✓ | ✓ four `stripe_api_read` calls in deferred mode | — | — | — | — | **PASS** (connect + tools) — fixed by #1482 (metadata location) and #1483 (required arguments in deferred mode) | 2026-09-14 |
| Grafana Cloud | dynamic registration, public client | ✓ | ✓ | ✓ | ✓ `list_users_by_org`, `list_datasources` | — | — | — | — | **PASS** (connect + tools) — fixed by #1482 | 2026-09-14 |
| Uptime Robot | dynamic registration (vendor returned a secret) | ✓ | ✓ | ✓ | ✓ `list-monitors`, `get-monitor-stats` | — | — | — | — | **PASS** (connect + tools) — fixed by #1485 (pointer only on POST) | 2026-09-14 |
| Plaid | no protected-resource metadata (origin is the AS) | ✓ | ✓ | ✓ | ✓ `list_teams` | — | — | — | — | **PASS on the 2026-09-14 rig build**; on `main` after #1485's final form discovery is refused (the vendor answers 401 under its MCP path). Not fixed — skipped by decision | 2026-09-14 |
| Cartesia | dynamic registration | ✓ (wrong URL) | ✓ | ✗ 404 | — | — | — | — | — | **catalog URL wrong**, corrected in #1501 (`/mcp`, from #1495); not re-run since | 2026-09-14 |
| Intercom | no protected-resource metadata | probe ✓ | — | — | — | — | — | — | — | discovery only; same `main` refusal as Plaid | 2026-09-14 |

Cross-cutting checks, all live:

- **Seats** (#988): two GitHub accounts and two Notion accounts on one user; a
  scheduled task pinned to a non-default seat mounts that seat and leaves the
  default untouched; a pin to a seat that does not exist skips the connector
  with a notice; a pin to a server the owner never connected dead-letters the
  task before any model spend.
- **Sharing**: a second rig user given one Notion seat sees only that seat, can
  call its tools from chat and from a scheduled task, and cannot manage it
  (every owner action answers 404); revocation takes effect on the grantee's
  next turn / next scheduled run (access is checked at mount time in
  `ConnectedServersForUser`, so a run already holding the mount finishes);
  grantees cannot see or manage the credential, but a grantee's run can trigger
  the broker-managed refresh of the owner's token row (and a 401 at mount sends
  the owner's row to `needs_reauth`).
- **Headless refresh**: expired GitHub, Google Drive, Notion and Azure DevOps
  tokens were refreshed by scheduled runs with no browser.
- **Deferred mode** (more than 128 tools): tool calls through
  `tool_search`/`tool_describe`/`tool_call` for GitHub, Notion, Slack, Linear,
  Stripe, Grafana Cloud, Uptime Robot.

## Fleet changes the pack produced

| PR | what it fixed | found by |
|---|---|---|
| #1405 | manual client without a secret sailed into a doomed exchange; `client_secret: required` on GitHub | GitHub |
| #1446 | system prompt claimed "no MCP tools connected" while hosted tools were mounted | GitHub |
| #1449 | connect failures were skipped silently; reason now logged (redacted) | GitHub |
| #1454 / #1464 | a vendor 401 at mount now marks the row `needs_reauth`; non-2xx bodies surfaced | GitHub revoke-all |
| #1465 | Google offline access; GitHub's 200-status OAuth errors; expired token without a refresh token marked | Google, GitHub |
| #1467 | live tool registry in the prompt built from the real roster | Linear |
| #1471 | Slack's resource indicator is its origin while the endpoint is `/mcp` | Slack |
| #1472 | `client_secret: required` on every manual entry whose metadata allows no public client | catalog sweep |
| #1481 | Entra's templated issuer; `offline_access` for Entra | Azure DevOps |
| #1482 | authorization-server metadata at RFC 8414's path-inserted location | audit (13 vendors) |
| #1483 | `tool_describe` shows required arguments; `tool_call` refuses a call missing one | Stripe |
| #1485 | servers with no protected-resource metadata; pointer only on POST | audit (Plaid, Uptime Robot, …) |
| #1488 | proxied authorization-server documents; registration retry as a confidential client; Auth0 `offline_access` | audit (DocuSign, ZoomInfo, Sprout Social, OVHcloud, Chargebee, Checkly) |
| #1495 | seven catalog entries with a wrong URL, auth type or docs link | audit |

## The catalog audit (2026-09-13, re-run 2026-09-14)

All 231 official entries with `auth: oauth`, `tenant` or `open` were probed
with fleet's own discovery. The per-entry appendix below is the re-run on the
code in `main` plus #1488 and #1495. Findings and their disposition:

| id | finding | disposition |
|---|---|---|
| F1 | metadata only at RFC 8414's path-inserted location (13 vendors) | fixed, #1482 merged |
| F2 | no protected-resource metadata; origin is the AS | fixed, #1485 merged; `main`'s final form refuses a vendor that answers 401 under its MCP path (Plaid, Intercom) — **skipped by decision** |
| F3 | metadata pointer only on the POST reply | fixed, #1485 merged |
| F4 | document names another issuer (copy / proxy / hybrid) | fixed, merged (#1488) |
| F5 | registration always asks for a public client | two vendors accepted it anyway; retry on refusal in #1488 |
| F6 | `offline_access` not requested where the vendor needs it | Entra (#1481) and Auth0 (#1488); IdentityServer, Keycloak, Ory unverified |
| F7 | `tool_describe` hid the required-argument list | fixed, #1483 merged |
| F8 | the broker masks a vendor's 4xx argument error as "credential-owner call failed" | open, not started |
| F9 | `tool_search` ranks other connectors above the one named in the query | observation |
| F10 | a connect failure is announced to the model as "needs re-authorization" | open, not started |
| F11 | fleet has no legacy HTTP+SSE transport; Square and Smartlead document SSE-only endpoints (Smartlead's `/sse` confirmed live and SSE-only on 2026-09-25; Square answers 403 to everything from the audit network) | **resolved in Phase 4 — both entries removed** from the shipped directory: a listing fleet cannot connect to is advertising, not onboarding. Re-add either when it offers streamable HTTP |
| C1–C9 | seven catalog data errors (Expensify, Cartesia, Octagon, Globalping, Zerodha Kite (its `login` tool session is per-turn only; no scheduled-run auth), Sage Intacct, OpenRouter); Square, Smartlead untouched | landed in #1501 (superseded #1495) |
| F12 | Bugsnag's 401 points at its metadata over plain `http://`; the vendor redirects to https, fleet's client refuses redirects, and since #1485 a failed advertised pointer is fatal (it fell through to the well-known locations before) | open, not started — a same-host `http`→`https` upgrade of the pointer would cover it |
| F13 | Saved connections retain their original URL and auth across catalog corrections until removed and re-added. | Open — reconciliation of saved rows to updated definitions is deferred, requiring a manual re-add for Cartesia, Octagon, Globalping, and after Phase 4 for Synter Ads (new URL) and Composio (new auth shape). |
| V1 | 25 entries publish no scopes anywhere | live add needed per vendor |
| V2 | GoCardless answers 403 to every unauthenticated request from the audit network (so did Square, which left the directory in Phase 4 — F11); Adobe and Wrike did so on 2026-09-14 and passed discovery on 2026-09-25 | re-probe from another network before calling it broken |
| V3 | 22 tenant entries have a placeholder in the hostname and cannot be probed | expected |
| F14 | fleet's add-time validation of an api_key connection (initialize + tools/list) **passes an invalid key** at 25 of the 51 api_key vendors measured on 2026-09-16 (53 listed after Phase 4: Perplexity rejects a wrong key; Composio needs a real server id to measure) — they check the key only at the first tools/call, so a wrong key is saved with a "connected, N tools" confirmation and fails in the first turn. The set moves with the vendors: by 2026-09-25 Braintrust rejected a wrong key and Vultr accepted one — still 25 | open, not started — Phase 2 candidate: follow tools/list with one cheap read-only call where a vendor documents one, or say on the card that the check proved reachability |
| F15 | 20 api_key entries also publish OAuth protected-resource metadata on the 401 (`resource_metadata` pointer): Braintrust, Brevo, Buffer, Censys, Coda, fal.ai, Fireflies, Instantly, Kong Konnect, Mollie, Paddle, PagerDuty, Parallel, Raygun, Razorpay, Tavily, Upsun, Vultr, Whop, and Perplexity (added in Phase 4) | observation — each could become a one-click `auth: oauth` entry after a live add; not changed |
| C10 | Composio's documented URL (`…/v3/mcp/{SERVER_ID}?user_id={USER_ID}`) answers 307 to `…/v3/mcp/{SERVER_ID}/mcp?user_id=…`, which fleet refuses to follow; and Composio's own docs require an `x-api-key` header on every MCP request (the default for new organisations) with no OAuth, so the entry's `tenant` shape could never have connected | **fixed in Phase 4** — retyped `api_key` with `api_key_header: x-api-key` on the URL the vendor redirects to (the guided form collects the placeholder values and the key together). Not live-tested: it needs a real Composio server id. The built-in shape test, which had forbidden a placeholder on an api_key entry although the product supports it, now allows it |
| C11 | Synter Ads (community, hidden by default) listed `https://syntermedia.ai/mcp`, which serves the vendor's HTML page to an MCP initialize | **fixed in Phase 4** — the server is at `https://mcp.syntermedia.ai` (answers initialize; rejects a wrong `X-Synter-Key` with 401); `setup_url` now points at the developer portal |
| V4 | `docs_url` answers 403 to a plain GET from the audit box for Coda (Leonardo.Ai's 500 on 2026-09-16 was transient — 200 since); ZoomInfo's was a real 404 | Coda looks like a bot wall, re-check from a browser; ZoomInfo's link replaced with the vendor's current page (the docs moved to its GTM AI rebrand, `docs.gtm.ai`) alongside the nightly smoke |
| C12 | three hosted endpoints missing from the directory, each verified with fleet's discovery on 2026-09-25: **Perplexity** (`api.perplexity.ai/mcp`, api_key as bearer, rejects a wrong key at the handshake), **LaunchDarkly** (`mcp.launchdarkly.com/mcp/launchdarkly`, OAuth, dynamic registration, public client), **CircleCI** (`mcp.circleci.com/v1/mcp`, OAuth, dynamic registration, confidential client via #1488, no scopes published). Zendesk's per-tenant server (`https://{subdomain}.zendesk.com/api/mcp`, OAuth + dynamic registration, scopes read/write) was verified live on Zendesk's own subdomain but held back: Zendesk documents only its MCP *client* feature, and the directory lists nothing without a vendor page to send users to | **added in Phase 4** (Perplexity, LaunchDarkly, CircleCI); Zendesk pending a vendor documentation page. Brave, Discord, MongoDB Atlas, Qdrant and Weaviate were checked and offer no connectable hosted endpoint (self-hosted, or a service-account proxy) |

### The remaining 57 entries (2026-09-16)

The 2026-09-14 probe covered the official OAuth, tenant and open entries. The
other 57 — 51 `api_key`, the four third-party platforms (Composio, Make,
Smithery, Zapier) and the two community self-hosted templates — were swept
on 2026-09-16 to complete the inventory below. An api_key entry has no
discovery to run, so each was asked three things: an MCP `initialize`
without any key; the same with an obviously invalid key attached exactly the
way fleet attaches a real one (`Authorization: Bearer <key>` by default, the
raw key under the entry's `api_key_header`, or the entry's `api_key_query`
parameter); and fleet's own add-time validation — the same code path Connect
runs, `initialize` + `tools/list` over the SSRF-safe client — with that
invalid key. The third-party OAuth entries went through `mcpoauth.Discover`
like the official ones; `docs_url` was fetched for all 57.

Results: every one of the 51 api_key endpoints is alive and speaks MCP,
except Synter Ads, whose URL serves an HTML page (C11). 26 vendors reject the
invalid key at the handshake (HTTP 401/403/400, or a JSON-RPC error that
fleet surfaces), so a mistyped key fails the add with the vendor's message,
as `docs/MCP-CATALOG.md` describes. **25 do not** (F14): they answer
`initialize` and `tools/list` to any bearer, so fleet's add-time check saves
the wrong key with a "connected, N tools" confirmation and the failure
surfaces only at the first tool call. 19 of the 51 also advertise OAuth
protected-resource metadata (F15) and could be one-click entries. Zapier and
Make discover cleanly (Make lists no `none` auth method and publishes no
scopes — V1; #1488's confidential retry covers the registration). Smithery's
authorization server lives at its origin and is found once a real `{server}`
is filled in; Composio's documented URL redirects (C10). The two self-hosted
templates have a placeholder hostname and cannot be probed. No api_key entry
is missing a `setup_hint` or `setup_url`.

### Re-check (2026-09-25)

Every entry was probed again nine days later — the same discovery probe for
the 231 official OAuth, tenant and open entries, the same three handshake
checks for the other 57, the bogus-key replay of the add-time validation for
the 51 api_key entries, and the link lint over all 288 `docs_url` — and the
result was diffed against the inventory. The catalog itself had not changed.
281 of 288 rows kept their verdict. What moved: Adobe Creativity and Wrike,
which had answered 403 to everything from the audit network, now complete
discovery (V2 is down to GoCardless, Square having left the directory in Phase 4); Braintrust started rejecting
a wrong key at the handshake and Vultr started accepting one (F14 stays at
25); Leonardo.Ai's documentation page answers 200 (V4 is down to Coda). The
links were otherwise identical: 275 OK, the same 12 warnings, ZoomInfo's
still dead until its corrected URL lands. Rows touched by the re-check carry
`2026-09-25 probe` as their last-verified date; the rest keep the date they
were last actually checked.

### Phase 4 — the directory changes (2026-09-25)

From the findings above and nothing else. Removed: Square and Smartlead
(F11 — SSE-only endpoints fleet cannot speak to). Fixed: Synter Ads (C11 —
the real server, at the `mcp.` host, was hiding behind the vendor's web
page) and Composio (C10 — retyped from `tenant` to `api_key` with the
`x-api-key` header its docs require, on the URL its server redirects to; the
built-in shape test that had forbidden a key entry with a placeholder URL
was wrong about the product and now allows it). Added, each after fleet's own
discovery probe, and for Perplexity the bogus-key replay of the add-time
validation as well: Perplexity, LaunchDarkly and CircleCI (C12). Held back:
Zendesk, whose per-tenant server is live and discoverable but undocumented
by the vendor. The count goes from 288 to 289 (two out, three in); the
Featured shelf is untouched at 20, none community. No entry lacks a
`setup_hint` or `setup_url` where the loader requires one, and no stdio-only
package is listed.

## Appendix — the inventory: every built-in entry (#986 Phase 1)

All 289 entries of `internal/clientconfig/builtin_remote_catalog.yaml` as of
the Phase 4 changes on 2026-09-25, one row each, sorted by name. Columns follow the plan in
#986: **featured** (★) is the Featured-shelf flag; **can CI hit?** says what
an automated smoke (Phase 2) could do with the entry without a human —
`yes` the endpoint answers `initialize` with no credentials (open); `key-fixture`
it needs a vendor key held as a CI secret (every api_key entry, plus the three
open entries that carry the key in the URL) — no such fixture exists yet;
`oauth-manual` the browser consent step cannot run in CI; `tenant` the URL has
a `{placeholder}` only a customer can fill; `dead-suspect` the endpoint does
not speak MCP. **last verified** is `live` for a real account on the rig (the
table at the top), `probe` for a credential-less check — the 2026-09-14
discovery probe (fleet's `mcpoauth.Discover` plus the add-time guards, run on
`main` plus #1488 and #1495; "discovery ✓" means Connect would reach the
vendor's consent screen and says nothing about tool calls) or the 2026-09-16
handshake sweep described above — and `—` when nothing could be checked
(31 tenant entries with a placeholder in the hostname, the two self-hosted
templates, and Render, whose hostname resolves to 127.0.0.1 on the audit box
while public DNS is fine). A row is not re-verified by later releases.

Counts — can CI hit?: yes 12 · key-fixture 54 · oauth-manual 183 · tenant 40 ·
dead-suspect 0. Auth: oauth 183 · tenant 39 · api_key 53 · open 14.
Provenance: official 281 · third_party 5 · community 3. Featured: 20. Last
verified: live 11 · probe 2026-09-14 184 · probe 2026-09-16 50 · probe
2026-09-25 10 · not probeable 34. These totals are derived from the table
and pinned to it and to the catalog by `scripts/check_catalog_status_test.go`.

| entry | auth | provenance | category | featured | can CI hit? | last verified | probe verdict | notes |
|---|---|---|---|---|---|---|---|---|
| adobe-creativity | oauth | official | design-media | ★ | oauth-manual | 2026-09-25 probe | discovery ✓ | self-registering, secret; AS lists no `none`; fleet asks `none`, retries confidential (#1488); advertises `offline_access`; refresh unverified; third-party authorization server host: ims-na1.adobelogin.com; answered 403 to everything on 2026-09-14 (V2) |
| ahrefs | oauth | official | marketing-social |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, secret; AS lists no `none`; fleet asks `none`, retries confidential (#1488) |
| airbyte | oauth | official | data-analytics |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, secret; AS lists no `none`; fleet asks `none`, retries confidential (#1488); no scopes published (V1) |
| airtable | oauth | official | productivity | ★ | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| airwallex | oauth | official | commerce-payments |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, secret; AS lists no `none`; fleet asks `none`, retries confidential (#1488) |
| aiven | oauth | official | databases |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| alchemy | oauth | official | finance |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, secret; AS lists no `none`; fleet asks `none`, retries confidential (#1488); no scopes published (V1) |
| algolia | oauth | official | development |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| alloydb | oauth | official | databases |  | oauth-manual | 2026-09-14 probe | discovery ✓ | manual client, secret |
| alpha-vantage | oauth | official | finance |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| amazon-ads | oauth | official | marketing-social |  | oauth-manual | 2026-09-14 probe | discovery ✓ | manual client, public client ok |
| amplitude | oauth | official | data-analytics |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok; advertises `offline_access`; refresh unverified |
| apify | oauth | official | web-search |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| apollo-io | oauth | official | crm-sales |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| asana | oauth | official | productivity | ★ | oauth-manual | 2026-09-14 probe | discovery ✓ | manual client, secret |
| atlassian | oauth | official | productivity | ★ | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| attio | oauth | official | crm-sales |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| audioscrape | oauth | official | design-media |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| avalara-avatax | oauth | official | finance |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, secret; AS lists no `none`; fleet asks `none`, retries confidential (#1488); advertises `offline_access`; refresh unverified |
| aws-knowledge | open | official | knowledge-docs |  | yes | 2026-09-14 probe | open ✓ (initialize 200) |  |
| aws-mcp | oauth | official | cloud-infrastructure |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok; no scopes published (V1) |
| axiom | oauth | official | observability |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| azure-devops | tenant | official | cloud-infrastructure |  | tenant | 2026-09-13 live | live PASS; discovery ✓ | manual client, secret |
| better-stack | oauth | official | observability |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| black-forest-labs | oauth | official | ai-ml |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok; advertises `offline_access`; refresh unverified |
| box | oauth | official | productivity |  | oauth-manual | 2026-09-14 probe | discovery ✓ | manual client, secret; no scopes published (V1) |
| braintrust | api_key | official | observability |  | key-fixture | 2026-09-25 probe | endpoint ✓ — initialize 401 without a key, 401 with a bogus key | add-time check rejects a bogus key (JSON-RPC error on the initialize reply) — it passed one on 2026-09-16, so the vendor tightened up (F14); also publishes OAuth protected-resource metadata; could be `auth: oauth` (F15) |
| brevo | api_key | official | marketing-social |  | key-fixture | 2026-09-16 probe | endpoint ✓ — initialize 401 without a key, 401 with a bogus key | add-time check rejects a bogus key (HTTP 401); also publishes OAuth protected-resource metadata; could be `auth: oauth` (F15) |
| brex | oauth | official | finance |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| bright-data | tenant | official | web-search |  | tenant | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| browserbase | api_key | official | web-search | ★ | key-fixture | 2026-09-16 probe | endpoint ✓ — initialize 200 without a key, 200 with a bogus key | **add-time check passes a bogus key** (6 tools listed; the key is checked only at tools/call) (F14) |
| browserstack | oauth | official | development |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| buffer | api_key | official | marketing-social |  | key-fixture | 2026-09-16 probe | endpoint ✓ — initialize 401 without a key, 401 with a bogus key | add-time check rejects a bogus key (HTTP 401); also publishes OAuth protected-resource metadata; could be `auth: oauth` (F15) |
| bugcrowd | api_key | official | security |  | key-fixture | 2026-09-16 probe | endpoint ✓ — initialize 401 without a key, 401 with a bogus key | add-time check rejects a bogus key (HTTP 401) |
| bugsnag | oauth | official | observability |  | oauth-manual | 2026-09-14 probe | discovery ✗ — fetch protected-resource metadata the server advertised at http://bugsnag.mcp.smartbear.co |  |
| buildkite | oauth | official | development |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| cal-com | oauth | official | productivity |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok; no scopes published (V1) |
| calendly | oauth | official | productivity |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| canva | oauth | official | design-media | ★ | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| cartesia | oauth | official | ai-ml |  | oauth-manual | 2026-09-14 live | live ✗ 404 on the old URL; corrected in #1501, not re-run; discovery ✓ | self-registering, secret; AS lists no `none`; fleet asks `none`, retries confidential (#1488) |
| cast-ai | api_key | official | cloud-infrastructure |  | key-fixture | 2026-09-16 probe | endpoint ✓ — initialize 401 without a key, 401 with a bogus key | add-time check rejects a bogus key (JSON-RPC error on the initialize reply) |
| censys | api_key | official | security |  | key-fixture | 2026-09-16 probe | endpoint ✓ — initialize 401 without a key, 200 with a bogus key | **add-time check passes a bogus key** (22 tools listed; the key is checked only at tools/call) (F14); also publishes OAuth protected-resource metadata; could be `auth: oauth` (F15) |
| chargebee | tenant | official | commerce-payments |  | tenant | 2026-09-14 probe | discovery ✓ | manual client, public client ok |
| checkly | oauth | official | observability |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| checkout-com | oauth | official | commerce-payments |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, secret; AS lists no `none`; fleet asks `none`, retries confidential (#1488) |
| chroma-package-search | api_key | official | databases |  | key-fixture | 2026-09-16 probe | endpoint ✓ — initialize 200 without a key, 200 with a bogus key | **add-time check passes a bogus key** (3 tools listed; the key is checked only at tools/call) (F14) |
| chromatic | tenant | official | development |  | tenant | — | tenant — not probeable without a real tenant value |  |
| chronosphere | tenant | official | observability |  | tenant | — | tenant — not probeable without a real tenant value |  |
| circleci | oauth | official | development |  | oauth-manual | 2026-09-25 probe | discovery ✓ | added in Phase 4 (C12); self-registering, secret; AS lists no `none`; fleet asks `none`, retries confidential (#1488); no scopes published (V1); no revocation endpoint (sign-out clears locally only) |
| clickhouse | oauth | official | databases |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| clickup | oauth | official | productivity |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| close | oauth | official | crm-sales |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| cloudflare | oauth | official | cloud-infrastructure |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok; no scopes published (V1) |
| cloudflare-docs | open | official | knowledge-docs |  | yes | 2026-09-14 probe | open ✓ (initialize 200) |  |
| cloudinary | oauth | official | design-media |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| cockroachdb | oauth | official | databases |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| coda | api_key | official | productivity |  | key-fixture | 2026-09-16 probe | endpoint ✓ — initialize 401 without a key, 401 with a bogus key | docs_url answers 403 to a plain GET (bot wall?); add-time check rejects a bogus key (HTTP 401); also publishes OAuth protected-resource metadata; could be `auth: oauth` (F15) |
| coingecko | open | official | finance |  | yes | 2026-09-14 probe | open ✓ (initialize 200) |  |
| coinmarketcap | api_key | official | finance |  | key-fixture | 2026-09-16 probe | endpoint ✓ — initialize 401 without a key, 200 with a bogus key | **add-time check passes a bogus key** (14 tools listed; the key is checked only at tools/call) (F14) |
| composio | api_key | third_party | automation |  | tenant | 2026-09-25 probe | not probeable — `{placeholder}` in the URL; the route answers 404 "MCP server not found" for a made-up server id | retyped in Phase 4 (C10): `api_key` with an `x-api-key` header and the URL the vendor's own server redirects to (`…/{SERVER_ID}/mcp?user_id=…`); not live-tested with a real server id |
| confluent | api_key | official | data-analytics |  | key-fixture | 2026-09-16 probe | endpoint ✓ — initialize 401 without a key, 401 with a bogus key | add-time check rejects a bogus key (HTTP 401) |
| contentful | oauth | official | knowledge-docs |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok; no scopes published (V1) |
| context7 | open | official | development |  | yes | 2026-09-14 probe | open ✓ (initialize 200) |  |
| coralogix | tenant | official | observability |  | tenant | — | tenant — not probeable without a real tenant value |  |
| craft | oauth | official | productivity |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok; no scopes published (V1) |
| crisp | api_key | official | customer-support |  | key-fixture | 2026-09-16 probe | endpoint ✓ — initialize 200 without a key, 200 with a bogus key | **add-time check passes a bogus key** (26 tools listed; the key is checked only at tools/call) (F14) |
| cube | tenant | official | data-analytics |  | tenant | — | tenant — not probeable without a real tenant value |  |
| customer-io | oauth | official | marketing-social |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| databricks | tenant | official | data-analytics |  | tenant | — | tenant — not probeable without a real tenant value |  |
| datadog | oauth | official | observability |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| dbt | tenant | official | data-analytics |  | tenant | — | tenant — not probeable without a real tenant value |  |
| deepwiki | open | official | development |  | yes | 2026-09-14 probe | open ✓ (initialize 200) |  |
| devin | api_key | official | development |  | key-fixture | 2026-09-16 probe | endpoint ✓ — initialize 200 without a key, 200 with a bogus key | **add-time check passes a bogus key** (22 tools listed; the key is checked only at tools/call) (F14) |
| digitalocean | oauth | official | cloud-infrastructure |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok; no scopes published (V1) |
| docusign | oauth | official | productivity |  | oauth-manual | 2026-09-14 probe | discovery ✓ | manual client, secret |
| doordash | oauth | official | travel-local |  | oauth-manual | 2026-09-14 probe | discovery ✓ | manual client, public client ok |
| dropbox | oauth | official | productivity |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| dune | oauth | official | data-analytics |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| dynatrace | tenant | official | observability |  | tenant | — | tenant — not probeable without a real tenant value |  |
| egnyte | oauth | official | productivity |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, secret; AS lists no `none`; fleet asks `none`, retries confidential (#1488); no scopes published (V1) |
| elastic-agent-builder | tenant | official | observability |  | tenant | — | tenant — not probeable without a real tenant value |  |
| etherscan | api_key | official | finance |  | key-fixture | 2026-09-16 probe | endpoint ✓ — initialize 401 without a key, 200 with a bogus key | **add-time check passes a bogus key** (21 tools listed; the key is checked only at tools/call) (F14) |
| exa | api_key | official | web-search |  | key-fixture | 2026-09-16 probe | endpoint ✓ — initialize 200 without a key, 200 with a bogus key | **add-time check passes a bogus key** (2 tools listed; the key is checked only at tools/call) (F14) |
| expensify | oauth | official | finance |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| expo | oauth | official | development |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| fal-ai | api_key | official | ai-ml |  | key-fixture | 2026-09-16 probe | endpoint ✓ — initialize 401 without a key, 401 with a bogus key | add-time check rejects a bogus key (HTTP 401); also publishes OAuth protected-resource metadata; could be `auth: oauth` (F15) |
| fellow | oauth | official | communication |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| fibery | oauth | official | productivity |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| figma | oauth | official | design-media | ★ | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, secret; AS lists no `none`; fleet asks `none`, retries confidential (#1488) |
| financial-datasets | oauth | official | finance |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| firecrawl | api_key | official | web-search |  | key-fixture | 2026-09-16 probe | endpoint ✓ — initialize 200 without a key, 200 with a bogus key | **add-time check passes a bogus key** (3 tools listed; the key is checked only at tools/call) (F14) |
| fireflies | api_key | official | communication |  | key-fixture | 2026-09-16 probe | endpoint ✓ — initialize 401 without a key, 403 with a bogus key | add-time check rejects a bogus key (HTTP 403); also publishes OAuth protected-resource metadata; could be `auth: oauth` (F15) |
| firefly | api_key | official | cloud-infrastructure |  | key-fixture | 2026-09-16 probe | endpoint ✓ — initialize 401 without a key, 401 with a bogus key | add-time check rejects a bogus key (JSON-RPC error on the initialize reply) |
| freshdesk | tenant | official | customer-support |  | tenant | — | tenant — not probeable without a real tenant value |  |
| front | oauth | official | customer-support |  | oauth-manual | 2026-09-14 probe | discovery ✓ | manual client, secret |
| galileo | api_key | official | observability |  | key-fixture | 2026-09-16 probe | endpoint ✓ — initialize 200 without a key, 200 with a bogus key | **add-time check passes a bogus key** (9 tools listed; the key is checked only at tools/call) (F14) |
| gamma | oauth | official | design-media |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| github | oauth | official | development | ★ | oauth-manual | 2026-09-10 live | live PASS; discovery ✓ | manual client, secret |
| gitlab | oauth | official | development |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, secret; AS lists no `none`; fleet asks `none`, retries confidential (#1488) |
| globalping | oauth | official | observability |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| gocardless | oauth | official | commerce-payments |  | oauth-manual | 2026-09-14 probe | discovery ✗ — 403 to every unauthenticated request from the audit network (V2) |  |
| google-calendar | oauth | official | productivity | ★ | oauth-manual | 2026-09-14 probe | discovery ✓ | manual client, secret |
| google-chat | oauth | official | communication |  | oauth-manual | 2026-09-14 probe | discovery ✓ | manual client, secret |
| google-cloud | tenant | official | cloud-infrastructure |  | tenant | — | tenant — not probeable without a real tenant value |  |
| google-docs | oauth | official | productivity |  | oauth-manual | 2026-09-14 probe | discovery ✓ | manual client, secret |
| google-drive | oauth | official | productivity | ★ | oauth-manual | 2026-09-10 live | live — tool call BLOCKED by vendor (Developer Preview); discovery ✓ | manual client, secret |
| google-gemini-agent-platform | oauth | official | ai-ml |  | oauth-manual | 2026-09-14 probe | discovery ✓ | manual client, secret |
| google-gmail | oauth | official | communication | ★ | oauth-manual | 2026-09-14 probe | discovery ✓ | manual client, secret |
| google-maps | api_key | official | travel-local |  | key-fixture | 2026-09-16 probe | endpoint ✓ — initialize 200 without a key, 200 with a bogus key | **add-time check passes a bogus key** (5 tools listed; the key is checked only at tools/call) (F14) |
| google-people | oauth | official | productivity |  | oauth-manual | 2026-09-14 probe | discovery ✓ | manual client, secret |
| google-sheets | oauth | official | productivity |  | oauth-manual | 2026-09-14 probe | discovery ✓ | manual client, secret |
| google-slides | oauth | official | productivity |  | oauth-manual | 2026-09-14 probe | discovery ✓ | manual client, secret |
| google-workspace-self-hosted | tenant | community | productivity |  | tenant | — | not probeable — `{placeholder}` in the URL |  |
| gorgias | oauth | official | customer-support |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| grafana-cloud | oauth | official | observability |  | oauth-manual | 2026-09-14 live | live PASS (connect + tools); discovery ✓ | self-registering, public client ok |
| grain | oauth | official | communication |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok; no scopes published (V1) |
| gram | tenant | official | development |  | tenant | — | tenant — not probeable without a real tenant value |  |
| granola | oauth | official | communication |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok; advertises `offline_access`; refresh unverified |
| greptile | api_key | official | development |  | key-fixture | 2026-09-16 probe | endpoint ✓ — initialize 200 without a key, 200 with a bogus key | **add-time check passes a bogus key** (21 tools listed; the key is checked only at tools/call) (F14) |
| guru | oauth | official | productivity |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| harness | oauth | official | cloud-infrastructure |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, secret; AS lists no `none`; fleet asks `none`, retries confidential (#1488) |
| heroku | oauth | official | cloud-infrastructure |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, secret; AS lists no `none`; fleet asks `none`, retries confidential (#1488) |
| hex | oauth | official | data-analytics |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| heygen | oauth | official | ai-ml |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| honeycomb | oauth | official | observability |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| hootsuite | oauth | official | marketing-social |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| hootsuite-lumen | oauth | official | marketing-social |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| hootsuite-nest | oauth | official | marketing-social |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| hubspot | oauth | official | crm-sales | ★ | oauth-manual | 2026-09-14 probe | discovery ✓ | manual client, secret; no scopes published (V1) |
| hugging-face | oauth | official | ai-ml | ★ | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, secret; AS lists no `none`; fleet asks `none`, retries confidential (#1488) |
| ideogram | oauth | official | ai-ml |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, secret; AS lists no `none`; fleet asks `none`, retries confidential (#1488) |
| incident-io | oauth | official | observability |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok; no scopes published (V1) |
| instantly | api_key | official | marketing-social |  | key-fixture | 2026-09-16 probe | endpoint ✓ — initialize 401 without a key, 200 with a bogus key | **add-time check passes a bogus key** (202 tools listed; the key is checked only at tools/call) (F14); also publishes OAuth protected-resource metadata; could be `auth: oauth` (F15) |
| intercom | oauth | official | customer-support |  | oauth-manual | 2026-09-14 probe | discovery ✗ — 401 at a metadata location; skipped by decision |  |
| jetbrains-youtrack | tenant | official | development |  | tenant | — | tenant — not probeable without a real tenant value |  |
| jina | api_key | official | web-search |  | key-fixture | 2026-09-16 probe | endpoint ✓ — initialize 200 without a key, 200 with a bogus key | **add-time check passes a bogus key** (22 tools listed; the key is checked only at tools/call) (F14) |
| jotform | oauth | official | marketing-social |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| kiwi-flights | open | official | travel-local |  | yes | 2026-09-14 probe | open ✓ (initialize 200) |  |
| klaviyo | oauth | official | marketing-social |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok; no scopes published (V1) |
| kong-konnect | api_key | official | development |  | key-fixture | 2026-09-16 probe | endpoint ✓ — initialize 401 without a key, 401 with a bogus key | add-time check rejects a bogus key (HTTP 401); also publishes OAuth protected-resource metadata; could be `auth: oauth` (F15) |
| lambdatest | oauth | official | development |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok; no scopes published (V1) |
| langfuse | api_key | official | observability |  | key-fixture | 2026-09-16 probe | endpoint ✓ — initialize 401 without a key, 401 with a bogus key | add-time check rejects a bogus key (HTTP 401) |
| langsmith | oauth | official | observability |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok; no scopes published (V1) |
| launchdarkly | oauth | official | development |  | oauth-manual | 2026-09-25 probe | discovery ✓ | added in Phase 4 (C12); self-registering, public client ok |
| lemlist | oauth | official | marketing-social |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| leonardo-ai | api_key | official | ai-ml |  | key-fixture | 2026-09-25 probe | endpoint ✓ — initialize 401 without a key, 200 with a bogus key | **add-time check passes a bogus key** (3 tools listed; the key is checked only at tools/call) (F14); docs_url answered 500 on 2026-09-16, 200 since |
| linear | oauth | official | productivity | ★ | oauth-manual | 2026-09-09 live | live PASS; discovery ✓ | self-registering, public client ok |
| linkup | api_key | official | web-search |  | key-fixture | 2026-09-16 probe | endpoint ✓ — initialize 200 without a key, 200 with a bogus key | **add-time check passes a bogus key** (4 tools listed; the key is checked only at tools/call) (F14) |
| looker | tenant | official | data-analytics |  | tenant | — | tenant — not probeable without a real tenant value |  |
| lusha | oauth | official | crm-sales |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, secret; AS lists no `none`; fleet asks `none`, retries confidential (#1488) |
| magnific-freepik | oauth | official | ai-ml |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok; advertises `offline_access`; refresh unverified |
| mailchimp-mandrill | api_key | official | marketing-social |  | key-fixture | 2026-09-16 probe | endpoint ✓ — initialize 200 without a key, 200 with a bogus key | add-time check rejects a bogus key (JSON-RPC error on the initialize reply) |
| mailerlite | oauth | official | marketing-social |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok; no scopes published (V1) |
| make | oauth | third_party | automation |  | oauth-manual | 2026-09-16 probe | discovery ✓ | self-registering, secret |
| mapbox | oauth | official | travel-local |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| mercado-libre | oauth | official | commerce-payments |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| mercado-pago | oauth | official | commerce-payments |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| mercury | oauth | official | finance |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| messari | oauth | official | finance |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok; no scopes published (V1) |
| meta-ads | oauth | official | marketing-social |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| metabase | tenant | official | data-analytics |  | tenant | — | tenant — not probeable without a real tenant value |  |
| microsoft-365-self-hosted | tenant | community | productivity |  | tenant | — | not probeable — `{placeholder}` in the URL |  |
| microsoft-dataverse | tenant | official | crm-sales |  | tenant | — | tenant — not probeable without a real tenant value |  |
| microsoft-learn | open | official | knowledge-docs |  | yes | 2026-09-14 probe | open ✓ (initialize 200) |  |
| microsoft-workiq-calendar | tenant | official | productivity |  | tenant | — | tenant — not probeable without a real tenant value |  |
| microsoft-workiq-mail | tenant | official | communication |  | tenant | — | tenant — not probeable without a real tenant value |  |
| microsoft-workiq-sharepoint | tenant | official | productivity |  | tenant | — | tenant — not probeable without a real tenant value |  |
| microsoft-workiq-teams | tenant | official | communication |  | tenant | — | tenant — not probeable without a real tenant value |  |
| miro | oauth | official | design-media |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, secret; AS lists no `none`; fleet asks `none`, retries confidential (#1488) |
| mixpanel | oauth | official | data-analytics |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| mollie | api_key | official | commerce-payments |  | key-fixture | 2026-09-16 probe | endpoint ✓ — initialize 401 without a key, 401 with a bogus key | add-time check rejects a bogus key (HTTP 401); also publishes OAuth protected-resource metadata; could be `auth: oauth` (F15) |
| monday | oauth | official | productivity | ★ | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, secret; AS lists no `none`; fleet asks `none`, retries confidential (#1488); no scopes published (V1) |
| motherduck | oauth | official | databases |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| mux | oauth | official | development |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| nansen | api_key | official | finance |  | key-fixture | 2026-09-16 probe | endpoint ✓ — initialize 200 without a key, 200 with a bogus key | **add-time check passes a bogus key** (46 tools listed; the key is checked only at tools/call) (F14) |
| neon | oauth | official | databases |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| netlify | oauth | official | cloud-infrastructure |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| netsuite | tenant | official | finance |  | tenant | — | tenant — not probeable without a real tenant value |  |
| new-relic | oauth | official | observability |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| notion | oauth | official | productivity | ★ | oauth-manual | 2026-09-14 live | live PASS (sharing 2026-09-14); discovery ✓ | self-registering, public client ok |
| octagon | oauth | official | finance |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| omni | tenant | official | data-analytics |  | tenant | — | tenant — not probeable without a real tenant value |  |
| openai-developer-docs | open | official | ai-ml |  | yes | 2026-09-14 probe | open ✓ (initialize 200) |  |
| openrouter | oauth | official | ai-ml |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok; no scopes published (V1) |
| oracle-autonomous-db | tenant | official | cloud-infrastructure |  | tenant | — | tenant — not probeable without a real tenant value |  |
| orca-security | oauth | official | security |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok; no scopes published (V1) |
| otter-ai | oauth | official | communication |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| outreach | oauth | official | crm-sales |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, secret; AS lists no `none`; fleet asks `none`, retries confidential (#1488) |
| ovhcloud | oauth | official | cloud-infrastructure |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, secret; AS lists no `none`; fleet asks `none`, retries confidential (#1488) |
| paddle | api_key | official | commerce-payments |  | key-fixture | 2026-09-16 probe | endpoint ✓ — initialize 401 without a key, 401 with a bogus key | add-time check rejects a bogus key (HTTP 401); also publishes OAuth protected-resource metadata; could be `auth: oauth` (F15) |
| pagerduty | api_key | official | observability |  | key-fixture | 2026-09-16 probe | endpoint ✓ — initialize 401 without a key, 401 with a bogus key | add-time check rejects a bogus key (HTTP 401); also publishes OAuth protected-resource metadata; could be `auth: oauth` (F15) |
| parallel-search | open | official | web-search |  | yes | 2026-09-14 probe | open ✓ (initialize 200) |  |
| parallel-task | api_key | official | web-search |  | key-fixture | 2026-09-16 probe | endpoint ✓ — initialize 401 without a key, 200 with a bogus key | **add-time check passes a bogus key** (4 tools listed; the key is checked only at tools/call) (F14); also publishes OAuth protected-resource metadata; could be `auth: oauth` (F15) |
| paypal | oauth | official | commerce-payments | ★ | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| perplexity | api_key | official | web-search |  | key-fixture | 2026-09-25 probe | endpoint ✓ — initialize 401 without a key, 401 with a bogus key | added in Phase 4 (C12); add-time check rejects a bogus key (HTTP 401); also publishes OAuth protected-resource metadata; could be `auth: oauth` (F15) |
| pika | oauth | official | ai-ml |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| pinecone-assistant | tenant | official | databases |  | tenant | — | tenant — not probeable without a real tenant value |  |
| pipedream | api_key | third_party | automation |  | key-fixture | 2026-09-16 probe | endpoint ✓ — initialize 401 without a key, 401 with a bogus key | add-time check rejects a bogus key (JSON-RPC error on the initialize reply) |
| pipedrive | oauth | official | crm-sales |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, secret; AS lists no `none`; fleet asks `none`, retries confidential (#1488) |
| plaid | oauth | official | finance |  | oauth-manual | 2026-09-14 live | live PASS on the 09-14 rig build; discovery ✗ — 401 at a metadata location; skipped by decision |  |
| plain | oauth | official | customer-support |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| plane | oauth | official | productivity |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, secret; AS lists no `none`; fleet asks `none`, retries confidential (#1488) |
| planetscale | oauth | official | databases |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, secret; AS lists no `none`; fleet asks `none`, retries confidential (#1488) |
| polar | oauth | official | commerce-payments |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| posthog | oauth | official | data-analytics |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| postman | oauth | official | development |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok; no scopes published (V1) |
| preset | oauth | official | data-analytics |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| prisma | oauth | official | databases |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| pulumi | oauth | official | cloud-infrastructure |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| pydantic-logfire | oauth | official | observability |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| pylon | oauth | official | customer-support |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| railway | oauth | official | cloud-infrastructure |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| ramp | oauth | official | finance |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| raygun | api_key | official | observability |  | key-fixture | 2026-09-16 probe | endpoint ✓ — initialize 401 without a key, 401 with a bogus key | add-time check rejects a bogus key (HTTP 401); also publishes OAuth protected-resource metadata; could be `auth: oauth` (F15) |
| razorpay | api_key | official | commerce-payments |  | key-fixture | 2026-09-16 probe | endpoint ✓ — initialize 401 without a key, 401 with a bogus key | add-time check rejects a bogus key (HTTP 401); also publishes OAuth protected-resource metadata; could be `auth: oauth` (F15) |
| read-ai | oauth | official | communication |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| recraft | oauth | official | ai-ml |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| render | oauth | official | cloud-infrastructure |  | oauth-manual | — | not probeable from the audit box (its resolver maps the host to 127.0.0.1; public DNS is fine) |  |
| replicate | oauth | official | ai-ml |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok; no scopes published (V1) |
| replit | oauth | official | cloud-infrastructure |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| retell-ai | api_key | official | ai-ml |  | key-fixture | 2026-09-16 probe | endpoint ✓ — initialize 200 without a key, 200 with a bogus key | **add-time check passes a bogus key** (3 tools listed; the key is checked only at tools/call) (F14) |
| rootly | oauth | official | observability |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| runpod | oauth | official | cloud-infrastructure |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok; no scopes published (V1) |
| runway | oauth | official | ai-ml |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| sage-intacct | oauth | official | finance |  | oauth-manual | 2026-09-14 probe | discovery ✓ | manual client, secret |
| saleor | api_key | official | commerce-payments |  | key-fixture | 2026-09-16 probe | endpoint ✓ — initialize 200 without a key, 200 with a bogus key | **add-time check passes a bogus key** (8 tools listed; the key is checked only at tools/call) (F14) |
| salesforce | tenant | official | crm-sales |  | tenant | 2026-09-14 probe | discovery ✓ | self-registering, secret; AS lists no `none`; fleet asks `none`, retries confidential (#1488); advertises `offline_access`; refresh unverified |
| sanity | oauth | official | knowledge-docs |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| scrapfly | open | official | web-search |  | key-fixture | 2026-09-14 probe | open — key in the URL; probe used a bogus key (answered 401) |  |
| scrapingbee | tenant | official | web-search |  | tenant | — | tenant — not probeable without a real tenant value |  |
| semaphore | oauth | official | development |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| semgrep | oauth | official | security |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| semrush | oauth | official | marketing-social |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok; advertises `offline_access`; refresh unverified |
| sentry | oauth | official | observability |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| servicenow | tenant | official | customer-support |  | tenant | — | tenant — not probeable without a real tenant value |  |
| shopify-storefront | tenant | official | commerce-payments |  | tenant | — | tenant — not probeable without a real tenant value |  |
| shortcut | oauth | official | productivity |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| signoz | tenant | official | observability |  | tenant | — | tenant — not probeable without a real tenant value |  |
| similarweb | api_key | official | marketing-social |  | key-fixture | 2026-09-16 probe | endpoint ✓ — initialize 401 without a key, 401 with a bogus key | add-time check rejects a bogus key (HTTP 401) |
| slack | oauth | official | communication | ★ | oauth-manual | 2026-09-10 live | live PASS with caveats; discovery ✓ | manual client, secret |
| slite | oauth | official | productivity |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| smartsheet | api_key | official | productivity |  | key-fixture | 2026-09-16 probe | endpoint ✓ — initialize 401 without a key, 401 with a bogus key | add-time check rejects a bogus key (HTTP 401) |
| smithery | tenant | third_party | automation |  | tenant | 2026-09-16 probe | not probeable — `{placeholder}` in the URL; origin answers | authorization server found at the origin: https://auth.smithery.ai/{server} |
| snowflake | tenant | official | databases |  | tenant | — | tenant — not probeable without a real tenant value |  |
| socket | oauth | official | security |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| sourcegraph | tenant | official | development |  | tenant | — | tenant — not probeable without a real tenant value |  |
| spacelift | tenant | official | cloud-infrastructure |  | tenant | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| sprout-social | oauth | official | marketing-social |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, secret; AS lists no `none`; fleet asks `none`, retries confidential (#1488) |
| stainless | tenant | official | development |  | tenant | — | tenant — not probeable without a real tenant value |  |
| stripe | oauth | official | commerce-payments | ★ | oauth-manual | 2026-09-14 live | live PASS (connect + tools); discovery ✓ | self-registering, public client ok |
| supabase | oauth | official | databases |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, secret; AS lists no `none`; fleet asks `none`, retries confidential (#1488) |
| superhuman-mail | oauth | official | communication |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| surveymonkey | oauth | official | marketing-social |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, secret; AS lists no `none`; fleet asks `none`, retries confidential (#1488) |
| synter-ads | api_key | community | marketing-social |  | key-fixture | 2026-09-25 probe | endpoint ✓ — initialize 200 without a key, 401 with a bogus key | URL corrected in Phase 4 (C11: the catalog pointed at the vendor's web page; the server is at the `mcp.` host); add-time check rejects a bogus key (HTTP 401); the setup link redirects to a sign-in page until the user has an account |
| tavily | api_key | official | web-search |  | key-fixture | 2026-09-16 probe | endpoint ✓ — initialize 401 without a key, 401 with a bogus key | add-time check rejects a bogus key (HTTP 401); also publishes OAuth protected-resource metadata; could be `auth: oauth` (F15) |
| teamwork | oauth | official | productivity |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| tenable | api_key | official | security |  | key-fixture | 2026-09-16 probe | endpoint ✓ — initialize 401 without a key, 400 with a bogus key | add-time check rejects a bogus key (HTTP 400) |
| thirdweb | open | official | finance |  | key-fixture | 2026-09-14 probe | open — key in the URL; probe used a bogus key (answered 401) |  |
| ticktick | oauth | official | productivity |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| tigris | oauth | official | cloud-infrastructure |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| tinybird | tenant | official | databases |  | tenant | — | tenant — not probeable without a real tenant value |  |
| todoist | oauth | official | productivity |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| trello | oauth | official | productivity |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| twelve-data | oauth | official | finance |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, secret; AS lists no `none`; fleet asks `none`, retries confidential (#1488) |
| twilio | open | official | communication |  | yes | 2026-09-14 probe | open ✓ (initialize 200) |  |
| typeform | oauth | official | marketing-social |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok; advertises `offline_access`; refresh unverified |
| upstox | oauth | official | finance |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok; no scopes published (V1) |
| upsun | api_key | official | cloud-infrastructure |  | key-fixture | 2026-09-16 probe | endpoint ✓ — initialize 401 without a key, 200 with a bogus key | **add-time check passes a bogus key** (19 tools listed; the key is checked only at tools/call) (F14); also publishes OAuth protected-resource metadata; could be `auth: oauth` (F15) |
| uptime-robot | oauth | official | observability |  | oauth-manual | 2026-09-14 live | live PASS (connect + tools); discovery ✓ | self-registering, secret; AS lists no `none`; fleet asks `none`, retries confidential (#1488) |
| val-town | oauth | official | cloud-infrastructure |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| vapi | api_key | official | ai-ml |  | key-fixture | 2026-09-16 probe | endpoint ✓ — initialize 401 without a key, 401 with a bogus key | add-time check rejects a bogus key (HTTP 401) |
| vercel | oauth | official | cloud-infrastructure |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, secret; AS lists no `none`; fleet asks `none`, retries confidential (#1488); advertises `offline_access`; refresh unverified |
| victoriametrics-cloud | api_key | official | observability |  | key-fixture | 2026-09-16 probe | endpoint ✓ — initialize 200 without a key, 200 with a bogus key | **add-time check passes a bogus key** (24 tools listed; the key is checked only at tools/call) (F14) |
| vimeo | oauth | official | design-media |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| vultr | api_key | official | cloud-infrastructure |  | key-fixture | 2026-09-25 probe | endpoint ✓ — initialize 401 without a key, 200 with a bogus key | **add-time check passes a bogus key** (191 tools listed; the key is checked only at tools/call) (F14) — it rejected one on 2026-09-16, so the vendor loosened up; also publishes OAuth protected-resource metadata; could be `auth: oauth` (F15) |
| wandb | api_key | official | observability |  | key-fixture | 2026-09-16 probe | endpoint ✓ — initialize 401 without a key, 200 with a bogus key | **add-time check passes a bogus key** (30 tools listed; the key is checked only at tools/call) (F14) |
| webflow | oauth | official | design-media |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok; no scopes published (V1) |
| whop | api_key | official | commerce-payments |  | key-fixture | 2026-09-16 probe | endpoint ✓ — initialize 401 without a key, 401 with a bogus key | add-time check rejects a bogus key (HTTP 401); also publishes OAuth protected-resource metadata; could be `auth: oauth` (F15) |
| wix | oauth | official | design-media |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| wiz | oauth | official | security |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| wordpress-com | oauth | official | design-media |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, public client ok |
| wrike | oauth | official | productivity |  | oauth-manual | 2026-09-25 probe | discovery ✓ | manual client, secret; PRM resource `https://mcp.wrike.com/app/mcp` differs from the endpoint (Slack shape; handled since #1471); no revocation endpoint (sign-out clears locally only); answered 403 to everything on 2026-09-14 (V2) |
| x-docs | open | official | knowledge-docs |  | yes | 2026-09-14 probe | open ✓ (initialize 200) |  |
| xero | oauth | official | finance |  | oauth-manual | 2026-09-14 probe | discovery ✓ | manual client, secret |
| zapier | oauth | third_party | automation | ★ | oauth-manual | 2026-09-16 probe | discovery ✓ | self-registering, public client ok |
| zenhub | api_key | official | productivity |  | key-fixture | 2026-09-16 probe | endpoint ✓ — initialize 200 without a key, 200 with a bogus key | **add-time check passes a bogus key** (21 tools listed; the key is checked only at tools/call) (F14) |
| zerodha-kite | open | official | finance |  | yes | 2026-09-14 probe | open ✓ (initialize 200) | `login` tool session lives only within the calling turn; does not persist across turns and cannot authenticate in scheduled runs |
| zoom | oauth | official | communication |  | oauth-manual | 2026-09-14 probe | discovery ✓ | manual client, secret |
| zoominfo | oauth | official | crm-sales |  | oauth-manual | 2026-09-14 probe | discovery ✓ | self-registering, secret; AS lists no `none`; fleet asks `none`, retries confidential (#1488); docs_url was 404 — replaced with the vendor's current page on `docs.gtm.ai` (ZoomInfo's GTM AI rebrand) |
