# Nightly MCP catalog smoke — dead links and live handshakes for the built-in directory

The built-in remote MCP directory
(`internal/clientconfig/builtin_remote_catalog.yaml`, ~290 entries) is a static
snapshot of other people's endpoints and documentation. It rots the way any
link list rots: vendors move pages, sunset betas and rename endpoints, and
until #986 the only way fleet found out was a user hitting "failed to connect".
This note records the automated half of #986 ("test official MCPs and improve
the MCP library"): what runs without a human, what it proves, and what it
deliberately leaves to people. The manual half — the OAuth pack, one real
account per vendor — is [`MCP-CATALOG-STATUS.md`](MCP-CATALOG-STATUS.md).

## What runs, and when

`.github/workflows/mcp-catalog-smoke.yml` runs daily at 06:30 UTC and on
manual dispatch. It is **never a PR gate**: nothing in `CI gate` depends on
it, and a red run files an issue (the same alarm the other scheduled lanes
use) instead of blocking a merge. Three jobs:

| job | what it does | fails when |
|---|---|---|
| `links` | `scripts/mcp-catalog-lint.sh` fetches every entry's `docs_url` (other link fields on dispatch) | a link answers 404/410 or its host does not resolve |
| `smoke` | `go test -run TestCatalogLive ./internal/remotemcp/` with `FLEET_CATALOG_LIVE=1` | an `open` entry fails fleet's handshake or lists no tools; an armed api_key fixture fails |
| `alarm` | files or comments on "Scheduled Nightly MCP catalog smoke run failed" | only on a scheduled failure of either job |

Both checks are read-only against the vendors and carry no credential unless
a fixture secret is configured. The smoke runs the same handshake a user's
Connect runs — `remotemcp.probeServer`, `initialize` + `tools/list` over the
SSRF-safe client — on entries the catalog loader has already validated; it
does not re-run Connect's own URL and header checks, which the loader's rules
make redundant for a shipped entry. It reads the directory as shipped, not a
bundle's merged view, so an entry the default bundle hides is still checked.

## The link lint

`scripts/mcp-catalog-lint.sh` needs only bash, awk and curl. It groups links by
host, runs hosts in parallel and each host's links one at a time with a delay,
tries HEAD before GET, follows up to five redirects, and retries a 429/503
honouring `Retry-After`. Verdicts:

- **OK** — final status 2xx/3xx.
- **DEAD** — 404, 410, or the hostname does not resolve. These fail the run,
  because they are the one thing a documentation link cannot legitimately do.
- **WARN** — everything else: 401/403/405/406/429 (help-centre platforms
  routinely refuse a non-browser client), 5xx, TLS or timeout errors.
  Reported, not failing: a curl-shaped request being turned away is not
  evidence the page is gone. `--strict` (the `strict_links` dispatch input)
  turns warnings into failures for a deliberate sweep.

The first full runs (2026-09-16 and 2026-09-25) over all 288 `docs_url`
links: 275 OK, 12 WARN, 1 DEAD, identical both times. The warnings were nine Zendesk/Salesforce-style help centres and
developer portals answering 403 to curl, two 429s that persisted through the
retries (Contentful, Wiz), one 405 (OpenRouter), and one TLS error that was
the audit box's own resolver (Render). The dead link was ZoomInfo's, replaced
with the vendor's current page (its documentation moved to the GTM AI
rebrand, `docs.gtm.ai`) in the same change that added this lane.

Run it locally with `make lint-catalog-links`, or directly:

```sh
scripts/mcp-catalog-lint.sh --only stripe,linear          # a few entries
scripts/mcp-catalog-lint.sh --fields docs_url,setup_url,repo_url
scripts/mcp-catalog-lint.sh --list                        # extraction only, no network
scripts/mcp-catalog-lint.sh --report links.tsv            # machine-readable
```

It is not part of `make lint`: it touches ~250 third-party hosts, and a
vendor's bot wall must never redden a PR. The Go test
`scripts/check_mcp_catalog_lint_test.go` drives the script against a local
server that plays every verdict and pins that the extraction yields exactly
one `docs_url` per catalog entry, so a change to the YAML layout that broke the
awk shows up in the PR gate, not as a silently smaller nightly.

## The live handshake

`internal/remotemcp/catalog_live_test.go` holds two tests, both skipped unless
`FLEET_CATALOG_LIVE=1`:

- **`TestCatalogLiveOpenEntries`** — every `auth: open` entry whose URL has
  no `{placeholder}` (12 as of 2026-09-16) must complete an unauthenticated
  handshake and list at least one tool. One subtest per entry, so a single
  dead vendor names itself.
- **`TestCatalogLiveAPIKeyFixtures`** — a table of five `api_key` entries,
  each armed by a repository secret named `FLEET_CATALOG_KEY_<ENTRY>` (the
  entry name upper-cased, `-` → `_`), which the workflow's `env:` block
  forwards; `scripts/check_catalog_smoke_fixtures_test.go` fails CI if the
  Go table and that block drift apart:

  | entry | secret | key shape it exercises | rejects a wrong key at the handshake |
  |---|---|---|---|
  | tavily | `FLEET_CATALOG_KEY_TAVILY` | `Authorization: Bearer` (the default) | yes |
  | pagerduty | `FLEET_CATALOG_KEY_PAGERDUTY` | raw key under a named `Authorization` header | yes |
  | exa | `FLEET_CATALOG_KEY_EXA` | `x-api-key` header | no |
  | browserbase | `FLEET_CATALOG_KEY_BROWSERBASE` | `browserbaseApiKey` query parameter | no |
  | firecrawl | `FLEET_CATALOG_KEY_FIRECRAWL` | `Authorization: Bearer`, versioned path | no |

  A missing secret skips that fixture; it never fails. The real key goes
  first, so a vendor outage reads as a failed handshake and not as a bad
  secret. Where the vendor checks the key at the handshake, the test then
  sends an obviously invalid key and requires a credential-shaped refusal (an
  HTTP 401/403 or a JSON-RPC error reply) — the only way to know the header
  *shape* is right and not merely that the endpoint is up. Where the vendor does not (F14 in the
  status page: 25 of the 51 built-in api_key vendors check the key only at the
  first tool call), the fixture proves reachability and the tool list, and the
  table says so. No secret is configured yet; the fixtures are ready for the
  first one.

Run locally:

```sh
FLEET_CATALOG_LIVE=1 go test -tags fleet_host_executor -count=1 -v -run TestCatalogLive ./internal/remotemcp/
FLEET_CATALOG_LIVE=1 FLEET_CATALOG_KEY_TAVILY=tvly-… go test -tags fleet_host_executor -run TestCatalogLiveAPIKeyFixtures/tavily -v ./internal/remotemcp/
```

## What it deliberately does not do

- **No OAuth.** 182 entries connect through a browser consent screen that
  cannot run headless; they are verified by hand and recorded in
  [`MCP-CATALOG-STATUS.md`](MCP-CATALOG-STATUS.md). The 2026-09-14 discovery
  probe there (fleet's `mcpoauth.Discover` against each vendor's published
  metadata) is the automatable part of that path and is a candidate for this
  lane; it is not in it yet.
- **No tool calls.** The handshake proves a listing points at a live MCP
  server that will talk to fleet, not that its tools work.
- **No tenant entries.** 40 entries carry a `{placeholder}` only a customer
  can fill.
- **Not a health monitor.** It runs once a day against fleet's *shipped
  listing*; it does not watch third-party services for operators (out of scope
  in #986, and [`MCP-CATALOG.md`](MCP-CATALOG.md)'s honest-scope section still
  holds: the running product does not health-check endpoints).
- **Not a gate.** A vendor's outage or bot wall must never redden a PR.

## Deviations from #986

The plan's Phase 2 asked for the link lint, a nightly `tools/list` for open
entries, a table-driven api_key test for 3–5 servers, and CI failure on catalog
shape regressions. The first three are above. The fourth was already largely
in place — the built-in loader and `TestBuiltinRemoteCatalog` rejected missing
`setup_hint`, bad provenance, featured community entries and the other shape
faults the issue lists — so that item added only two rules (https `docs_url`;
no two entries on one endpoint). The api_key fixtures ship unarmed: the
repository has no vendor keys, so the first nightly runs prove the open shelf
and skip the fixtures until a secret is added.
