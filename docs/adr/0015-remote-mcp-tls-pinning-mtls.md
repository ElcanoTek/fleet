# ADR-0015: TLS pinning and mTLS for remote MCP servers

- **Status:** Accepted
- **Date:** 2026-06-30
- **Deciders:** fleet maintainers

## Context

fleet connects to remote (HTTP) MCP servers declared in the client-config
bundle manifest (ADR-0006), credentialed host-side with static headers
(ADR-0003). Those connections run over HTTPS, but until now they used Go's
**default transport security only**: verification against the system root CA
store, no certificate pinning, and no client certificate.

For sensitive MCP servers — an internal API, a credential store, a payments
backend — that is weaker than operators need:

- A rogue or compromised CA that the system trusts can mint a certificate for
  the server's hostname and transparently MITM the connection. Default
  verification accepts it.
- An internal or self-signed server has no publicly-trusted chain at all, so the
  only previous way to reach it was to weaken verification globally — exactly the
  wrong direction.
- Some servers want to authenticate fleet itself (mutual TLS), which the bare
  `http.Client` could not present.

This is a transport-security boundary that was previously **undocumented and
unenforced**. Per the ADR convention, adding it is recorded here.

## Decision

We add an **opt-in, per-server TLS hardening block** to a manifest HTTP MCP
server. It is declared under `mcp_servers[].tls` and threads, unchanged in
meaning, from the manifest through to the one place the HTTP transport is built:

```yaml
mcp_servers:
  - name: internal_api
    type: http
    url: https://mcp.internal.example
    tls:
      ca_cert: /etc/fleet/mcp/internal-ca.pem      # pin trust to this CA bundle
      client_cert: /etc/fleet/mcp/fleet-client.pem # mTLS: present this cert…
      client_key:  /etc/fleet/mcp/fleet-client.key #       …and this key
      pinned_sha256: 9b8e…                          # pin the server's SPKI
      server_name: mcp.internal                     # SNI / verified-name override
```

Semantics (`internal/mcp.TLSOptions`):

- **`ca_cert`** replaces the system roots with the given PEM bundle for this
  server — the trust anchor for an internal or self-signed server.
- **`client_cert` + `client_key`** present a client certificate (mTLS). Both or
  neither; a manifest with only one fails to load.
- **`pinned_sha256`** is a hex SHA-256 of the server leaf certificate's
  `SubjectPublicKeyInfo`. It is checked **in addition to** normal chain
  verification (via `tls.Config.VerifyConnection`, which runs *after* the
  standard handshake), so a substituted certificate is rejected **even if it
  chains to an otherwise-trusted CA**. The pin survives certificate renewal as
  long as the key is reused.
- **`server_name`** overrides the SNI / verified hostname.

We **never** set `InsecureSkipVerify`. A self-signed server is reached by
supplying its certificate as `ca_cert`, not by disabling verification — so there
is no config that produces a silently-insecure connection. The cert/key/CA files
are operator-supplied paths on the fleet host, read at connect time; like all
MCP credentials they never enter the sandbox or the model context (ADR-0003).

A `tls:` block requires an **`https://`** url. Go's `http.Transport` applies a
`TLSClientConfig` only to https requests, and stdio servers have no TLS at all,
so hardening on a plaintext `http://` url or a stdio server would be *silently
ignored* — leaving an operator believing a connection is pinned when it is not.
We reject that at manifest load AND fail closed at registration, rather than
connect unverified.

The default (no `tls:` block, or an empty one) is byte-for-byte the prior
behavior: system-root verification, no client cert, no pin.

## Enforcement

- **Builder + pin check:** `internal/mcp/tlsconfig.go` (`TLSOptions.build`,
  `NormalizePinSHA256`) constructs the `*tls.Config`; the pin is enforced in a
  `VerifyConnection` callback layered on top of chain verification.
  `internal/mcp/tlsconfig_test.go` proves it end-to-end against an
  `httptest.NewTLSServer`: a correct CA+pin connects, a wrong pin is rejected,
  and a self-signed server with no CA is rejected (confirming verification is
  never disabled).
- **Single application point:** `mcp.Client.AddHTTPServerWithOptions` is the only
  place an HTTP MCP transport's client is built; it applies `TLSOptions` when no
  full `HTTPClient` override is supplied. Both manifest registration paths route
  through it — scheduled (`agent.BuildMCPClient`) and interactive
  (`agentcore.BindMCPSelection`).
- **Load-time validation:** `internal/clientconfig.validateServerTLS` rejects a
  block that is malformed (mTLS both-or-neither, well-formed pin) OR can't apply
  (non-`https` url, or a stdio server) at manifest load; `clientconfig_test.go`
  covers it. The transport additionally fails closed on a non-https url. File
  read/parse errors surface at connect time with a named error.

## Consequences

- Operators can pin sensitive MCP servers to a specific CA and/or public key and
  authenticate fleet with a client cert, defending against CA-substitution MITM —
  configured declaratively in the bundle, no code change.
- A pinned server whose key rotates without updating `pinned_sha256` will fail to
  connect (a best-effort HTTP server is skipped with a warning; a required one
  aborts the run). This is the intended fail-closed behavior; the pin is
  documented as a key pin, not a cert pin.
- The blast radius is small: the carrier type lives in the leaf `internal/mcp`
  package and threads through the existing `Headers` plumbing, so no governance
  or credential-handling path changes.
- **Scope (honest):** this covers manifest-declared HTTP MCP servers. Per-user
  OAuth remote servers (ADR-0009) dial through the SSRF-safe client and are not
  covered here; extending pinning to them is a follow-on. There is no UI; pins
  are bundle config.

## Alternatives considered

- **`InsecureSkipVerify` + manual verification for self-signed servers.** Equally
  secure in theory when paired with a pin, but it is a well-known footgun and a
  lint/security smell, and a misconfiguration (pin omitted) silently disables all
  verification. Supplying the server's CA via `ca_cert` reaches self-signed
  servers without ever disabling verification. Rejected.
- **Replacing chain verification with the pin (pin-only trust).** Makes a renewed
  certificate with a new key unreachable and discards hostname/expiry checks.
  Layering the pin on top of normal verification is strictly stronger. Rejected.
- **A global TLS policy instead of per-server.** Remote MCP servers differ (public
  SaaS vs. internal). Per-server pinning matches how servers are already declared
  (per-entry URL/headers). Rejected.
- **base64 (HPKP-style) pin encoding.** Hex is what the documented `openssl`
  one-liner emits and is easier to eyeball; we accept an optional `sha256:`
  prefix and `:` separators for paste-friendliness. Rejected base64 to avoid two
  ambiguous formats.
