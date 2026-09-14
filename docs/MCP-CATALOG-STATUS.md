# Hosted MCP connector status — the #1006 OAuth pack and the catalog audit

The record for #986 ("test official MCPs and improve the catalog") and its
child #1006 ("test the OAuth flow with official MCPs"). Two kinds of evidence
are kept apart here: **live runs** — a real account, the browser consent
screen, a real tool call from a chat and from a scheduled task, a forced
token refresh, a sign-out — and **discovery probes**, which run fleet's own
`mcpoauth.Discover` and add-time guards against a vendor's published metadata
without logging in. A probe proves a server can be *added*; only a live run
proves it *works*. Dates are when the row was last verified; a row is not
re-verified by later releases.

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
| Cartesia | dynamic registration | ✓ (wrong URL) | ✓ | ✗ 404 | — | — | — | — | — | **catalog URL wrong**, corrected in #1495 (`/mcp`); not re-run since | 2026-09-14 |
| Intercom | no protected-resource metadata | probe ✓ | — | — | — | — | — | — | — | discovery only; same `main` refusal as Plaid | 2026-09-14 |

Cross-cutting checks, all live:

- **Seats** (#988): two GitHub accounts and two Notion accounts on one user; a
  scheduled task pinned to a non-default seat mounts that seat and leaves the
  default untouched; a pin to a seat that does not exist skips the connector
  with a notice; a pin to a server the owner never connected dead-letters the
  task before any model spend.
- **Sharing**: a second rig user given one Notion seat sees only that seat, can
  call its tools from chat and from a scheduled task, cannot manage it (every
  owner action answers 404), and loses it the moment the owner revokes; the
  owner's token row is never touched by the grantee's use.
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
| F4 | document names another issuer (copy / proxy / hybrid) | fixed, #1488 open |
| F5 | registration always asks for a public client | two vendors accepted it anyway; retry on refusal in #1488 |
| F6 | `offline_access` not requested where the vendor needs it | Entra (#1481) and Auth0 (#1488); IdentityServer, Keycloak, Ory unverified |
| F7 | `tool_describe` hid the required-argument list | fixed, #1483 merged |
| F8 | the broker masks a vendor's 4xx argument error as "credential-owner call failed" | open, not started |
| F9 | `tool_search` ranks other connectors above the one named in the query | observation |
| F10 | a connect failure is announced to the model as "needs re-authorization" | open, not started |
| F11 | fleet has no legacy HTTP+SSE transport; Square and Smartlead document SSE-only endpoints | **skipped by decision**; entries left as they are |
| C1–C9 | seven catalog data errors (Expensify, Cartesia, Octagon, Globalping, Zerodha Kite, Sage Intacct, OpenRouter); Square, Smartlead untouched | #1495 open |
| F12 | Bugsnag's 401 points at its metadata over plain `http://`; the vendor redirects to https, fleet's client refuses redirects, and since #1485 a failed advertised pointer is fatal (it fell through to the well-known locations before) | open, not started — a same-host `http`→`https` upgrade of the pointer would cover it |
| V1 | 25 entries publish no scopes anywhere | live add needed per vendor |
| V2 | GoCardless, Adobe, Square, Wrike answer 403 to every unauthenticated request from the audit network | re-probe from another network before calling them broken |
| V3 | 22 tenant entries have a placeholder in the hostname and cannot be probed | expected |


## Appendix — every official OAuth, tenant and open entry, probed 2026-09-14

Probe = fleet's `mcpoauth.Discover` plus the add-time guards, run against the catalog as of #1495 with the code in `main` plus #1488. "discovery ✓" means Connect would reach the vendor's consent screen; it says nothing about tool calls. A tenant entry whose hostname carries a `{placeholder}` cannot be probed without a customer's value. Counts — discovery ✓: 177 · tenant: 31 · open ✓: 12 · discovery ✗: 7 · open: 3 · not probeable from the audit box: 1.

| entry | auth | probe verdict | client shape | issuer | notes |
|---|---|---|---|---|---|
| adobe-creativity | oauth | discovery ✗ — 403 to every unauthenticated request from the audit network (V2) | — |  |  |
| ahrefs | oauth | discovery ✓ | self-registering, secret | https://api.ahrefs.com/ | AS lists no `none`; fleet asks `none`, retries confidential (#1488) |
| airbyte | oauth | discovery ✓ | self-registering, secret | https://mcp.airbyte.ai/ | AS lists no `none`; fleet asks `none`, retries confidential (#1488); no scopes published (V1) |
| airtable | oauth | discovery ✓ | self-registering, public client ok | https://airtable.com/oauth2/v1 |  |
| airwallex | oauth | discovery ✓ | self-registering, secret | https://mcp.airwallex.com/mcp | AS lists no `none`; fleet asks `none`, retries confidential (#1488) |
| aiven | oauth | discovery ✓ | self-registering, public client ok | https://api.aiven.io |  |
| alchemy | oauth | discovery ✓ | self-registering, secret | https://auth.alchemy.com | AS lists no `none`; fleet asks `none`, retries confidential (#1488); no scopes published (V1) |
| algolia | oauth | discovery ✓ | self-registering, public client ok | https://dashboard.algolia.com |  |
| alloydb | oauth | discovery ✓ | manual client, secret | https://accounts.google.com |  |
| alpha-vantage | oauth | discovery ✓ | self-registering, public client ok | https://mcp.alphavantage.co |  |
| amazon-ads | oauth | discovery ✓ | manual client, public client ok | https://lwa.amazon.com |  |
| amplitude | oauth | discovery ✓ | self-registering, public client ok | https://mcp.amplitude.com | advertises `offline_access`; refresh unverified |
| apify | oauth | discovery ✓ | self-registering, public client ok | https://console-backend.apify.com |  |
| apollo-io | oauth | discovery ✓ | self-registering, public client ok | https://mcp.apollo.io |  |
| asana | oauth | discovery ✓ | manual client, secret | https://app.asana.com |  |
| atlassian | oauth | discovery ✓ | self-registering, public client ok | https://auth.atlassian.com/VCeDsk8ZHncYF1g234fKtc4lNipbBhu3 |  |
| attio | oauth | discovery ✓ | self-registering, public client ok | https://app.attio.com |  |
| audioscrape | oauth | discovery ✓ | self-registering, public client ok | https://mcp.audioscrape.com |  |
| avalara-avatax | oauth | discovery ✓ | self-registering, secret | https://identity.avalara.com | AS lists no `none`; fleet asks `none`, retries confidential (#1488); advertises `offline_access`; refresh unverified |
| aws-knowledge | open | open ✓ (initialize 200) | n/a |  |  |
| aws-mcp | oauth | discovery ✓ | self-registering, public client ok | https://us-east-1.oauth.signin.aws | no scopes published (V1) |
| axiom | oauth | discovery ✓ | self-registering, public client ok | https://authorization.axiom.co |  |
| azure-devops | tenant | discovery ✓ | manual client, secret | https://login.microsoftonline.com/organizations/v2.0 |  |
| better-stack | oauth | discovery ✓ | self-registering, public client ok | https://betterstack.com |  |
| black-forest-labs | oauth | discovery ✓ | self-registering, public client ok | https://uhjidycotobjggwyjdww.supabase.co/auth/v1 | advertises `offline_access`; refresh unverified |
| box | oauth | discovery ✓ | manual client, secret | https://api.box.com | no scopes published (V1) |
| brex | oauth | discovery ✓ | self-registering, public client ok | https://api.brex.com |  |
| bright-data | tenant | discovery ✓ | self-registering, public client ok | https://brightdata.com |  |
| browserstack | oauth | discovery ✓ | self-registering, public client ok | https://mcp.browserstack.com/ |  |
| bugsnag | oauth | discovery ✗ — fetch protected-resource metadata the server advertised at http://bugsnag.mcp.smartbear.co | — |  |  |
| buildkite | oauth | discovery ✓ | self-registering, public client ok | https://mcp.buildkite.com |  |
| cal-com | oauth | discovery ✓ | self-registering, public client ok | https://mcp.cal.com | no scopes published (V1) |
| calendly | oauth | discovery ✓ | self-registering, public client ok | https://calendly.com/ |  |
| canva | oauth | discovery ✓ | self-registering, public client ok | https://mcp.canva.com |  |
| cartesia | oauth | discovery ✓ | self-registering, secret | https://mcp.cartesia.ai/ | AS lists no `none`; fleet asks `none`, retries confidential (#1488) |
| chargebee | tenant | discovery ✓ | manual client, public client ok | https://app.chargebee.com |  |
| checkly | oauth | discovery ✓ | self-registering, public client ok | https://auth.checklyhq.com/ |  |
| checkout-com | oauth | discovery ✓ | self-registering, secret | https://access.mcp.checkout.com/payment-operations | AS lists no `none`; fleet asks `none`, retries confidential (#1488) |
| chromatic | tenant | tenant — not probeable without a real tenant value | — |  |  |
| chronosphere | tenant | tenant — not probeable without a real tenant value | — |  |  |
| clickhouse | oauth | discovery ✓ | self-registering, public client ok | https://mcp.clickhouse.cloud |  |
| clickup | oauth | discovery ✓ | self-registering, public client ok | https://mcp.clickup.com |  |
| close | oauth | discovery ✓ | self-registering, public client ok | https://api.close.com |  |
| cloudflare | oauth | discovery ✓ | self-registering, public client ok | https://mcp.cloudflare.com | no scopes published (V1) |
| cloudflare-docs | open | open ✓ (initialize 200) | n/a |  |  |
| cloudinary | oauth | discovery ✓ | self-registering, public client ok | https://asset-management.mcp.cloudinary.com |  |
| cockroachdb | oauth | discovery ✓ | self-registering, public client ok | https://cockroachlabs.cloud/mcp |  |
| coingecko | open | open ✓ (initialize 200) | n/a |  |  |
| contentful | oauth | discovery ✓ | self-registering, public client ok | https://mcp.contentful.com | no scopes published (V1) |
| context7 | open | open ✓ (initialize 200) | n/a |  |  |
| coralogix | tenant | tenant — not probeable without a real tenant value | — |  |  |
| craft | oauth | discovery ✓ | self-registering, public client ok | https://mcp.craft.do/my/auth | no scopes published (V1) |
| cube | tenant | tenant — not probeable without a real tenant value | — |  |  |
| customer-io | oauth | discovery ✓ | self-registering, public client ok | https://mcp.customer.io |  |
| databricks | tenant | tenant — not probeable without a real tenant value | — |  |  |
| datadog | oauth | discovery ✓ | self-registering, public client ok | https://mcp.datadoghq.com/v1/mcp |  |
| dbt | tenant | tenant — not probeable without a real tenant value | — |  |  |
| deepwiki | open | open ✓ (initialize 200) | n/a |  |  |
| digitalocean | oauth | discovery ✓ | self-registering, public client ok | https://cloud.digitalocean.com | no scopes published (V1) |
| docusign | oauth | discovery ✓ | manual client, secret | https://account.docusign.com |  |
| doordash | oauth | discovery ✓ | manual client, public client ok | https://identity.doordash.com |  |
| dropbox | oauth | discovery ✓ | self-registering, public client ok | https://www.dropbox.com |  |
| dune | oauth | discovery ✓ | self-registering, public client ok | https://dune.com/oauth/mcp |  |
| dynatrace | tenant | tenant — not probeable without a real tenant value | — |  |  |
| egnyte | oauth | discovery ✓ | self-registering, secret | https://mcp-oauth.egnyte.com/egnyte-connect | AS lists no `none`; fleet asks `none`, retries confidential (#1488); no scopes published (V1) |
| elastic-agent-builder | tenant | tenant — not probeable without a real tenant value | — |  |  |
| expensify | oauth | discovery ✓ | self-registering, public client ok | https://www.expensify.com |  |
| expo | oauth | discovery ✓ | self-registering, public client ok | https://mcp.expo.dev |  |
| fellow | oauth | discovery ✓ | self-registering, public client ok | https://fellow.app |  |
| fibery | oauth | discovery ✓ | self-registering, public client ok | https://mcp.fibery.io/ |  |
| figma | oauth | discovery ✓ | self-registering, secret | https://api.figma.com | AS lists no `none`; fleet asks `none`, retries confidential (#1488) |
| financial-datasets | oauth | discovery ✓ | self-registering, public client ok | https://mcp.financialdatasets.ai |  |
| freshdesk | tenant | tenant — not probeable without a real tenant value | — |  |  |
| front | oauth | discovery ✓ | manual client, secret | https://app.frontapp.com |  |
| gamma | oauth | discovery ✓ | self-registering, public client ok | https://auth.gamma.app |  |
| github | oauth | discovery ✓ | manual client, secret | https://github.com/login/oauth |  |
| gitlab | oauth | discovery ✓ | self-registering, secret | https://gitlab.com | AS lists no `none`; fleet asks `none`, retries confidential (#1488) |
| globalping | oauth | discovery ✓ | self-registering, public client ok | https://mcp.globalping.dev |  |
| gocardless | oauth | discovery ✗ — 403 to every unauthenticated request from the audit network (V2) | — |  |  |
| google-calendar | oauth | discovery ✓ | manual client, secret | https://accounts.google.com |  |
| google-chat | oauth | discovery ✓ | manual client, secret | https://accounts.google.com |  |
| google-cloud | tenant | tenant — not probeable without a real tenant value | — |  |  |
| google-docs | oauth | discovery ✓ | manual client, secret | https://accounts.google.com |  |
| google-drive | oauth | discovery ✓ | manual client, secret | https://accounts.google.com |  |
| google-gemini-agent-platform | oauth | discovery ✓ | manual client, secret | https://accounts.google.com |  |
| google-gmail | oauth | discovery ✓ | manual client, secret | https://accounts.google.com |  |
| google-people | oauth | discovery ✓ | manual client, secret | https://accounts.google.com |  |
| google-sheets | oauth | discovery ✓ | manual client, secret | https://accounts.google.com |  |
| google-slides | oauth | discovery ✓ | manual client, secret | https://accounts.google.com |  |
| gorgias | oauth | discovery ✓ | self-registering, public client ok | https://mcp.gorgias.com/ |  |
| grafana-cloud | oauth | discovery ✓ | self-registering, public client ok | https://mcp.grafana.com/mcp |  |
| grain | oauth | discovery ✓ | self-registering, public client ok | https://api.grain.com | no scopes published (V1) |
| gram | tenant | tenant — not probeable without a real tenant value | — |  |  |
| granola | oauth | discovery ✓ | self-registering, public client ok | https://mcp-auth.granola.ai | advertises `offline_access`; refresh unverified |
| guru | oauth | discovery ✓ | self-registering, public client ok | https://mcp.api.getguru.com |  |
| harness | oauth | discovery ✓ | self-registering, secret | https://id.harness.io/idp/realms/HarnessIDP | AS lists no `none`; fleet asks `none`, retries confidential (#1488) |
| heroku | oauth | discovery ✓ | self-registering, secret | https://mcp.heroku.com | AS lists no `none`; fleet asks `none`, retries confidential (#1488) |
| hex | oauth | discovery ✓ | self-registering, public client ok | https://auth.app.hex.tech |  |
| heygen | oauth | discovery ✓ | self-registering, public client ok | https://api2.heygen.com |  |
| honeycomb | oauth | discovery ✓ | self-registering, public client ok | https://ui.honeycomb.io |  |
| hootsuite | oauth | discovery ✓ | self-registering, public client ok | https://platform.hootsuite.com |  |
| hootsuite-lumen | oauth | discovery ✓ | self-registering, public client ok | https://app.talkwalker.com/app/ |  |
| hootsuite-nest | oauth | discovery ✓ | self-registering, public client ok | https://platform.hootsuite.com |  |
| hubspot | oauth | discovery ✓ | manual client, secret | https://mcp.hubspot.com | no scopes published (V1) |
| hugging-face | oauth | discovery ✓ | self-registering, secret | https://huggingface.co | AS lists no `none`; fleet asks `none`, retries confidential (#1488) |
| ideogram | oauth | discovery ✓ | self-registering, secret | https://mcp.ideogram.ai/ | AS lists no `none`; fleet asks `none`, retries confidential (#1488) |
| incident-io | oauth | discovery ✓ | self-registering, public client ok | https://mcp.incident.io/mcp | no scopes published (V1) |
| intercom | oauth | discovery ✗ — 401 at a metadata location; skipped by decision | — |  |  |
| jetbrains-youtrack | tenant | tenant — not probeable without a real tenant value | — |  |  |
| jotform | oauth | discovery ✓ | self-registering, public client ok | https://oauth2.jotform.com |  |
| kiwi-flights | open | open ✓ (initialize 200) | n/a |  |  |
| klaviyo | oauth | discovery ✓ | self-registering, public client ok | https://mcp.klaviyo.com | no scopes published (V1) |
| lambdatest | oauth | discovery ✓ | self-registering, public client ok | https://auth.lambdatest.com | no scopes published (V1) |
| langsmith | oauth | discovery ✓ | self-registering, public client ok | https://api.smith.langchain.com | no scopes published (V1) |
| lemlist | oauth | discovery ✓ | self-registering, public client ok | https://app.lemlist.com |  |
| linear | oauth | discovery ✓ | self-registering, public client ok | https://mcp.linear.app |  |
| looker | tenant | tenant — not probeable without a real tenant value | — |  |  |
| lusha | oauth | discovery ✓ | self-registering, secret | https://auth.lusha.com | AS lists no `none`; fleet asks `none`, retries confidential (#1488) |
| magnific-freepik | oauth | discovery ✓ | self-registering, public client ok | https://auth.magnific.com/realms/mcp | advertises `offline_access`; refresh unverified |
| mailerlite | oauth | discovery ✓ | self-registering, public client ok | https://mcp.mailerlite.com | no scopes published (V1) |
| mapbox | oauth | discovery ✓ | self-registering, public client ok | https://mcp.mapbox.com |  |
| mercado-libre | oauth | discovery ✓ | self-registering, public client ok | https://mcp.mercadolibre.com/mcp |  |
| mercado-pago | oauth | discovery ✓ | self-registering, public client ok | https://mcp.mercadopago.com/mcp |  |
| mercury | oauth | discovery ✓ | self-registering, public client ok | https://mcp.mercury.com/ |  |
| messari | oauth | discovery ✓ | self-registering, public client ok | https://mcp.messari.io | no scopes published (V1) |
| meta-ads | oauth | discovery ✓ | self-registering, public client ok | https://www.facebook.com/ads |  |
| metabase | tenant | tenant — not probeable without a real tenant value | — |  |  |
| microsoft-dataverse | tenant | tenant — not probeable without a real tenant value | — |  |  |
| microsoft-learn | open | open ✓ (initialize 200) | n/a |  |  |
| microsoft-workiq-calendar | tenant | tenant — not probeable without a real tenant value | — |  |  |
| microsoft-workiq-mail | tenant | tenant — not probeable without a real tenant value | — |  |  |
| microsoft-workiq-sharepoint | tenant | tenant — not probeable without a real tenant value | — |  |  |
| microsoft-workiq-teams | tenant | tenant — not probeable without a real tenant value | — |  |  |
| miro | oauth | discovery ✓ | self-registering, secret | https://mcp.miro.com/ | AS lists no `none`; fleet asks `none`, retries confidential (#1488) |
| mixpanel | oauth | discovery ✓ | self-registering, public client ok | https://mcp.mixpanel.com/mcp |  |
| monday | oauth | discovery ✓ | self-registering, secret | https://auth.monday.com/mcp | AS lists no `none`; fleet asks `none`, retries confidential (#1488); no scopes published (V1) |
| motherduck | oauth | discovery ✓ | self-registering, public client ok | https://mcp-auth.motherduck.com |  |
| mux | oauth | discovery ✓ | self-registering, public client ok | https://auth.mux.com |  |
| neon | oauth | discovery ✓ | self-registering, public client ok | https://mcp.neon.tech |  |
| netlify | oauth | discovery ✓ | self-registering, public client ok | https://netlify-mcp.netlify.app/ |  |
| netsuite | tenant | tenant — not probeable without a real tenant value | — |  |  |
| new-relic | oauth | discovery ✓ | self-registering, public client ok | https://oauth2.service.newrelic.com |  |
| notion | oauth | discovery ✓ | self-registering, public client ok | https://mcp.notion.com |  |
| octagon | oauth | discovery ✓ | self-registering, public client ok | https://login.octagonai.co |  |
| omni | tenant | tenant — not probeable without a real tenant value | — |  |  |
| openai-developer-docs | open | open ✓ (initialize 200) | n/a |  |  |
| openrouter | oauth | discovery ✓ | self-registering, public client ok | https://mcp.openrouter.ai | no scopes published (V1) |
| oracle-autonomous-db | tenant | tenant — not probeable without a real tenant value | — |  |  |
| orca-security | oauth | discovery ✓ | self-registering, public client ok | https://auth.orcasecurity.io | no scopes published (V1) |
| otter-ai | oauth | discovery ✓ | self-registering, public client ok | https://otter.ai |  |
| outreach | oauth | discovery ✓ | self-registering, secret | https://api.outreach.io | AS lists no `none`; fleet asks `none`, retries confidential (#1488) |
| ovhcloud | oauth | discovery ✓ | self-registering, secret | https://mcp.eu.ovhcloud.com/oauth-proxy | AS lists no `none`; fleet asks `none`, retries confidential (#1488) |
| parallel-search | open | open ✓ (initialize 200) | n/a |  |  |
| paypal | oauth | discovery ✓ | self-registering, public client ok | https://mcp.paypal.com |  |
| pika | oauth | discovery ✓ | self-registering, public client ok | https://ecyvlzfbufloietjsmtj.supabase.co/auth/v1 |  |
| pinecone-assistant | tenant | tenant — not probeable without a real tenant value | — |  |  |
| pipedrive | oauth | discovery ✓ | self-registering, secret | https://oauth.pipedrive.com | AS lists no `none`; fleet asks `none`, retries confidential (#1488) |
| plaid | oauth | discovery ✗ — 401 at a metadata location; skipped by decision | — |  |  |
| plain | oauth | discovery ✓ | self-registering, public client ok | https://signin.auth.plain.com |  |
| plane | oauth | discovery ✓ | self-registering, secret | https://mcp.plane.so/http | AS lists no `none`; fleet asks `none`, retries confidential (#1488) |
| planetscale | oauth | discovery ✓ | self-registering, secret | https://api.planetscale.com | AS lists no `none`; fleet asks `none`, retries confidential (#1488) |
| polar | oauth | discovery ✓ | self-registering, public client ok | https://api.polar.sh |  |
| posthog | oauth | discovery ✓ | self-registering, public client ok | https://oauth.posthog.com |  |
| postman | oauth | discovery ✓ | self-registering, public client ok | https://mcp.postman.com | no scopes published (V1) |
| preset | oauth | discovery ✓ | self-registering, public client ok | https://api.superset.sh |  |
| prisma | oauth | discovery ✓ | self-registering, public client ok | https://auth.prisma.io |  |
| pulumi | oauth | discovery ✓ | self-registering, public client ok | https://mcp.ai.pulumi.com |  |
| pydantic-logfire | oauth | discovery ✓ | self-registering, public client ok | https://logfire-us.pydantic.dev |  |
| pylon | oauth | discovery ✓ | self-registering, public client ok | https://o.auth.usepylon.com |  |
| railway | oauth | discovery ✓ | self-registering, public client ok | https://backboard.railway.com |  |
| ramp | oauth | discovery ✓ | self-registering, public client ok | https://mcp.ramp.com |  |
| read-ai | oauth | discovery ✓ | self-registering, public client ok | https://authn.read.ai/ |  |
| recraft | oauth | discovery ✓ | self-registering, public client ok | https://mcp.recraft.ai |  |
| render | oauth | not probeable from the audit box (its resolver maps the host to 127.0.0.1; public DNS is fine) | — |  |  |
| replicate | oauth | discovery ✓ | self-registering, public client ok | https://mcp.replicate.com | no scopes published (V1) |
| replit | oauth | discovery ✓ | self-registering, public client ok | https://replit.com/oidc |  |
| rootly | oauth | discovery ✓ | self-registering, public client ok | https://rootly.com |  |
| runpod | oauth | discovery ✓ | self-registering, public client ok | https://mcp.getrunpod.io | no scopes published (V1) |
| runway | oauth | discovery ✓ | self-registering, public client ok | https://mcp.runwayml.com |  |
| sage-intacct | oauth | discovery ✓ | manual client, secret | https://mcp.intacct.com |  |
| salesforce | tenant | discovery ✓ | self-registering, secret | https://login.salesforce.com | AS lists no `none`; fleet asks `none`, retries confidential (#1488); advertises `offline_access`; refresh unverified |
| sanity | oauth | discovery ✓ | self-registering, public client ok | https://mcp.sanity.io |  |
| scrapfly | open | open — key in the URL; probe used a bogus key (answered 401) | n/a |  |  |
| scrapingbee | tenant | tenant — not probeable without a real tenant value | — |  |  |
| semaphore | oauth | discovery ✓ | self-registering, public client ok | https://mcp.semaphoreci.com/mcp/oauth |  |
| semgrep | oauth | discovery ✓ | self-registering, public client ok | https://login.semgrep.dev |  |
| semrush | oauth | discovery ✓ | self-registering, public client ok | https://oauth.semrush.com | advertises `offline_access`; refresh unverified |
| sentry | oauth | discovery ✓ | self-registering, public client ok | https://mcp.sentry.dev |  |
| servicenow | tenant | tenant — not probeable without a real tenant value | — |  |  |
| shopify-storefront | tenant | tenant — not probeable without a real tenant value | — |  |  |
| shortcut | oauth | discovery ✓ | self-registering, public client ok | https://api.app.shortcut.com |  |
| signoz | tenant | tenant — not probeable without a real tenant value | — |  |  |
| slack | oauth | discovery ✓ | manual client, secret | https://mcp.slack.com |  |
| slite | oauth | discovery ✓ | self-registering, public client ok | https://slite.com/api/mcp/oauth |  |
| smartlead | open | open — key in the URL; probe used a bogus key (answered 404) | n/a |  |  |
| snowflake | tenant | tenant — not probeable without a real tenant value | — |  |  |
| socket | oauth | discovery ✓ | self-registering, public client ok | https://api.socket.dev |  |
| sourcegraph | tenant | tenant — not probeable without a real tenant value | — |  |  |
| spacelift | tenant | discovery ✓ | self-registering, public client ok | https://fleet-probe.app.spacelift.io |  |
| sprout-social | oauth | discovery ✓ | self-registering, secret | https://identity.sproutsocial.com/oauth2/84e39c75-d770-45d9- | AS lists no `none`; fleet asks `none`, retries confidential (#1488) |
| square | oauth | discovery ✗ — 403 to every unauthenticated request from the audit network (V2) | — |  |  |
| stainless | tenant | tenant — not probeable without a real tenant value | — |  |  |
| stripe | oauth | discovery ✓ | self-registering, public client ok | https://access.stripe.com/mcp |  |
| supabase | oauth | discovery ✓ | self-registering, secret | https://api.supabase.com | AS lists no `none`; fleet asks `none`, retries confidential (#1488) |
| superhuman-mail | oauth | discovery ✓ | self-registering, public client ok | https://mcp.auth.mail.superhuman.com |  |
| surveymonkey | oauth | discovery ✓ | self-registering, secret | https://mcp.surveymonkey.com | AS lists no `none`; fleet asks `none`, retries confidential (#1488) |
| teamwork | oauth | discovery ✓ | self-registering, public client ok | https://teamwork.com |  |
| thirdweb | open | open — key in the URL; probe used a bogus key (answered 401) | n/a |  |  |
| ticktick | oauth | discovery ✓ | self-registering, public client ok | https://ticktick.com |  |
| tigris | oauth | discovery ✓ | self-registering, public client ok | https://mcp.storage.dev |  |
| tinybird | tenant | tenant — not probeable without a real tenant value | — |  |  |
| todoist | oauth | discovery ✓ | self-registering, public client ok | https://todoist.com |  |
| trello | oauth | discovery ✓ | self-registering, public client ok | https://auth.atlassian.com/VCeDsk8ZHncYF1g234fKtc4lNipbBhu3 |  |
| twelve-data | oauth | discovery ✓ | self-registering, secret | https://mcp.twelvedata.com/ | AS lists no `none`; fleet asks `none`, retries confidential (#1488) |
| twilio | open | open ✓ (initialize 200) | n/a |  |  |
| typeform | oauth | discovery ✓ | self-registering, public client ok | https://api.typeform.com | advertises `offline_access`; refresh unverified |
| upstox | oauth | discovery ✓ | self-registering, public client ok | https://mcp.upstox.com | no scopes published (V1) |
| uptime-robot | oauth | discovery ✓ | self-registering, secret | https://mcp.uptimerobot.com | AS lists no `none`; fleet asks `none`, retries confidential (#1488) |
| val-town | oauth | discovery ✓ | self-registering, public client ok | https://www.val.town/oauth |  |
| vercel | oauth | discovery ✓ | self-registering, secret | https://vercel.com | AS lists no `none`; fleet asks `none`, retries confidential (#1488); advertises `offline_access`; refresh unverified |
| vimeo | oauth | discovery ✓ | self-registering, public client ok | https://mcp.vimeo.com/ |  |
| webflow | oauth | discovery ✓ | self-registering, public client ok | https://mcp.webflow.com | no scopes published (V1) |
| wix | oauth | discovery ✓ | self-registering, public client ok | https://mcp.wix.com |  |
| wiz | oauth | discovery ✓ | self-registering, public client ok | https://mcp.app.wiz.io |  |
| wordpress-com | oauth | discovery ✓ | self-registering, public client ok | https://public-api.wordpress.com |  |
| wrike | oauth | discovery ✗ — 403 to every unauthenticated request from the audit network (V2) | — |  |  |
| x-docs | open | open ✓ (initialize 200) | n/a |  |  |
| xero | oauth | discovery ✓ | manual client, secret | https://identity.xero.com |  |
| zerodha-kite | open | open ✓ (initialize 200) | n/a |  |  |
| zoom | oauth | discovery ✓ | manual client, secret | https://zoom.us |  |
| zoominfo | oauth | discovery ✓ | self-registering, secret | https://okta-login.zoominfo.com/oauth2/default | AS lists no `none`; fleet asks `none`, retries confidential (#1488); docs_url 404 |
