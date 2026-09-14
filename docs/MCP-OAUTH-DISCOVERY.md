# MCP OAuth discovery

How fleet finds the authorization server for a remote MCP server, what it will
and will not accept from that server's metadata, and why each rule is drawn
where it is. This is the design note for the discovery capability as a whole —
the increments that built it (#1481, #1482, #1485, #1487, #1488) are recorded
here rather than as one file each, so there is a single place to read the rules
and the deviations together.

The invariant-level summary lives in
[`AGENT-RUNTIME.md`](AGENT-RUNTIME.md) under the hosted-connector notes; this
page is the longer form: what shipped, what deviated, and what was deliberately
deferred.

## The chain

`mcpoauth.Discover` walks the MCP authorization discovery chain:

1. **Probe** the MCP server for a `401` with a `WWW-Authenticate`
   `resource_metadata` pointer. Uptime Robot names it only on a POST, so the
   probe tries both.
2. **Fetch** the RFC 9728 Protected Resource Metadata from the advertised
   location, else from the well-known candidates.
3. **Pick** the authorization server from `authorization_servers`.
4. **Fetch** its RFC 8414 / OIDC document and verify PKCE S256.

Two backwards-compatibility rules matter. A server that publishes **no** PRM at
all — every location 404s — falls back to the MCP spec's 2025-03-26 rule: the
server's own origin is the authorization server. A server that publishes a PRM
with a valid `resource` but no `authorization_servers` gets the same fallback,
keeping the document's other fields. A server that *told* us where its metadata
is and then failed to serve it, or answered 5xx, is a modern server having a
bad moment, and the failure is surfaced rather than silently synthesized.

## What it accepts, and what it refuses

### The strict issuer check

RFC 8414 §3.3 wants a document's `issuer` to equal the URL it was fetched from,
and that is the primary path. `issuerMatches` allows exactly one documented
deviation: Microsoft Entra ID's multi-tenant endpoints serve the literal
`{tenantid}` template, which is not a URL anyone can dial, so the PRM-named
issuer is kept as the recorded identity (#1481).

### Proxied authorization-server metadata

Five official vendors (DocuSign, ZoomInfo, Sprout Social, OVHcloud, Chargebee)
name the MCP host as their authorization server, and that host serves a document
whose `issuer` is some other URL — a copy of the real server's document, a proxy
in front of Okta, or a hybrid. `confirmProxiedIssuer` accepts one of these
**only as a last resort**, after every metadata location has failed the strict
check, so a valid issuer-specific document anywhere in the candidate order still
wins (#1485's decoy-skipping order is preserved).

Every endpoint the document names must be vouched for by one of two legs:

- **confirmed** — the claimed issuer's own metadata, fetched from the claimed
  issuer's well-known location with this confirmation switched **off** so
  documents cannot chain, names the same endpoint; or
- **same-origin** — the endpoint is on the origin the resource itself named as
  its authorization server, and that URL is a **bare host**.

Neither leg extends trust the plain path lacks: a PRM may name the claimed
issuer directly, and the named host could have published a compliant document
with its own endpoints. What both legs refuse is the actual mix-up — an endpoint
belonging to neither party.

### Tenant scoping

An authorization server scoped to a single tenant gets neither leg on another
tenant's terms.

| Scoping | Fallback |
| --- | --- |
| Path (`https://as.example/tenantA`) | Its own **origin-level** document only — the measured Chargebee shape |
| Query, fragment, userinfo | **None.** The well-known lookup keeps only scheme, host and path, so the scoping is dropped before any fetch and an origin document would merely self-confirm, silently replacing the tenant with the unscoped issuer |

"Scoped" is read off the **escaped** path, so `/%2F` — which `url.Parse` decodes
to `//` — stays a tenant rather than reading as a bare origin, and a literal
`//` does too. Both the authorization-server URL and the claimed `issuer` are
parsed as written for this reason. A claim whose identity would change under the
resolver's own trailing-slash trim is refused outright, since no document could
faithfully confirm it.

A sibling tenant is self-consistent too: accepting one would send the user
through the wrong tenant's authorization endpoint, which is why the confirmation
leg alone is not enough.

### URL identity

One rule, applied everywhere in the confirmation path:

- **Origin** — scheme and host lowercased, the scheme's default port dropped, an
  IPv6 literal's brackets kept so the host/port boundary stays unambiguous
  (`https://[2001:db8::1]:8443` and `https://[2001:db8::1:8443]` are different
  servers).
- **Path and query** — byte-for-byte, with a **single tolerated trailing
  slash**: `/token` and `/token/` are one endpoint, `/token//` is not. A bare
  `?` (`ForceQuery`) is a distinct request target and is compared as one.
- **Absolute only** — an endpoint must be `http(s)` with a hostname. A relative
  endpoint parses without error and normalizes to an empty origin, so two of
  them would confirm each other; `https://:443` has a non-empty authority and
  no hostname at all.
- **No userinfo** — `net/http` turns URL userinfo into a Basic `Authorization`
  header whenever the request does not set one, so a confirmed endpoint carrying
  it would be dialled with authentication fleet never chose to send. Refused,
  and redacted from every error that names a URL.

The same canonicalization backs `CanonicalResourceURI`, the single identity
function behind the DB key, the encryption AAD, the RFC 8707 resource indicator
and the broker routing key.

### Metadata reconciliation

When the token endpoint turns out to be the claimed issuer's **own**, that
issuer's document is authoritative for how to authenticate there: it supplies
`token_endpoint_auth_methods_supported`, and fills in `scopes_supported` and
`mfa_challenge_endpoint` where a trimmed copy omitted them. Losing either costs
the connection its refresh token. A populated list is never overwritten — a
proxy may legitimately offer fewer scopes than the issuer behind it — and a
proxy's own token endpoint keeps the proxy's values, since a proxy's registered
clients authenticate to the proxy.

## Dynamic client registration

fleet asks to be a **public client** (`token_endpoint_auth_method: none`, PKCE
protected) first, whatever the metadata says: of the 22 official servers whose
metadata lists no `none`, the two met live (Uptime Robot, Cartesia) registered
fleet anyway, one returning a secret fleet stores and uses.

A server that refuses the method (RFC 7591 §3.2.2 `invalid_client_metadata`)
gets **one** retry with a confidential method it lists — `client_secret_basic`
before `client_secret_post`, and `client_secret_basic` when it advertises no
list at all, which RFC 8414 §2 defines to mean exactly that.

RFC 7591 §3.2.1 lets the server substitute the metadata it granted, so the
response's effective `token_endpoint_auth_method` is what fleet stores for the
client. Three rules around that:

- A `none` echo does **not** narrow the advertised list: fleet asked to be
  public, and a server in the #1006 audit echoed `none` while returning a
  secret anyway.
- A method fleet cannot perform (`client_secret_jwt`, `private_key_jwt`, an
  mTLS method) is **refused at registration**, not silently replaced by the
  advertised list — the exchange and the revocation would both fail after the
  user completed consent.
- A **confidential** grant with no `client_secret` is refused on either
  response, not just the retry.

## Refresh-token scopes

Two vendors mint a refresh token only when `offline_access` is requested, and
neither resource declares it:

- **Microsoft Entra ID** — measured on Azure DevOps, whose PRM declares only its
  `.default` scope (#1481).
- **Auth0** — recognized by its proprietary `mfa_challenge_endpoint`, or an
  `*.auth0.com` issuer matched on **hostname** so an explicit port cannot hide
  it. Checkly's resource lists fifteen `checkly:*` scopes and not that one.

Scope tokens compare **exactly**: RFC 6749 §3.3 makes them "space-delimited,
case-sensitive strings", so `Offline_Access` is a different scope and treating
it as already present would send an authorize request carrying only a spelling
the server does not recognize.

Deliberately **not** generalized to "the server advertises `offline_access`":
GitHub advertises it and refreshes without it, and a vendor that validates
scopes may refuse an unrequested one. Other providers with the same rule (Ory's
`offline`, IdentityServer) join the clause once a live connection shows the
need.

## Deviations and deferred work

- **`issuerMatches` is the exact-match check and stays that way.** Where the
  proxied path needs a canonical spelling, the value handed *to* it is
  normalized rather than the check relaxed. One known lenience: it trims
  trailing slashes with `TrimRight`, so on the strict path a document claiming
  `host//` already satisfies the check for `host`. That is pre-existing and
  same-host — no second party vouches for anything — so it is recorded here
  rather than changed. Narrowing it to at most one trailing slash would keep
  the common `https://as.example/` vendor spelling working and close it.
- **`scopes_supported` is never narrowed by fleet.** The PRM's list is the
  resource's word; the only addition is the `offline_access` clause above.
- **Plaid and Intercom still fail discovery.** #1485's "a non-404 at a metadata
  location is operational" rule meets vendors whose MCP path prefix answers 401
  to every sub-path. Reported separately with a proposed one-line fix; not
  changed here because the rule it would relax is the one that keeps a modern
  server's bad moment from being silently synthesized into a legacy-origin
  configuration.
- **No catalog changes.** Which servers are offered is bundle content
  (`FLEET_CLIENT_CONFIG_DIR`), not engine code.

## Where the code is

| Piece | File |
| --- | --- |
| The chain, PRM location, candidate order | `internal/mcpoauth/discovery.go` |
| Proxied-issuer confirmation, URL identity helpers | `internal/mcpoauth/discovery.go` |
| Canonical identity (DB key, AAD, resource indicator) | `internal/mcpoauth/canonical.go` |
| Dynamic client registration | `internal/mcpoauth/register.go` |
| Authorize/token/refresh, auth-method selection | `internal/mcpoauth/flow.go` |
| Add-server wiring and persistence | `internal/remotemcp/service.go` |
