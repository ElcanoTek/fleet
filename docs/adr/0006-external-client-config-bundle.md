# ADR-0006: Client content lives in an external config bundle

- **Status:** Accepted
- **Date:** 2026-06-28 (documents a decision that predates this record)
- **Deciders:** fleet maintainers

## Context

fleet is a generic engine, but a *deployment* is specific: it has branding, an
MCP catalog, personas, protocols, skills, and a sandbox `Containerfile`. If that
client-specific content lived in this repo, the open-source engine and every
client's private configuration would be fused — secrets and bespoke content
would leak into the public tree, and "fleet" would stop being generic.

## Decision

Client content is **external**. It lives in an out-of-repo client-config bundle
selected by `FLEET_CLIENT_CONFIG_DIR`. This repo ships **only** the generic
`config/default` bundle (a vanilla `fedora-minimal` sandbox base, a neutral MCP
catalog, default personas) so that fleet runs bare with no private input. The
loader (`internal/clientconfig`) resolves the active bundle at runtime; do
**not** add client-specific branding, catalogs, personas, or skills to this
repository.

## Enforcement

- `config/default` is the only bundle in the tree and is what the binaries fall
  back to; CI points the binaries at it via `FLEET_CLIENT_CONFIG_DIR`.
- `gitleaks` (ADR-0003) backs the "no client secrets in the repo" half of this.
- The split is described in [`../../AGENTS.md`](../../AGENTS.md) ("Client content
  is external").

## Consequences

- The public repo stays generic and shippable; a client's branding and catalog
  evolve in their own bundle repo on their own cadence.
- A client that wants a prebuilt sandbox image or a custom catalog publishes it
  from their bundle and sets it in their manifest — that path is out of scope
  for core CI.
- Contributors must resist the convenience of "just adding it to
  `config/default`" when the content is really client-specific.

## Alternatives considered

- **Bake client content into the repo behind flags.** Rejected: fuses the engine
  with client config and invites secret leakage into a public tree.
- **A plugin registry downloaded at runtime.** Heavier than needed for the
  single-box model (ADR-0004); a directory selected by one env var is enough.
