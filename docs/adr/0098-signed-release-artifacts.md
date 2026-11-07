# ADR-0098: Every release publishes signed artifacts; boxes install them instead of building

- **Status:** Accepted
- **Date:** 2026-11-07
- **Deciders:** fleet maintainers
- **Extends:** [ADR-0059](0059-date-based-rolling-releases.md) (a release is now
  a tag **and** its signed artifacts; the release-object retention changes from
  "newest only" to "newest ten")
- **Amends:** [ADR-0078](0078-track-the-latest-node-major.md) (the node major is
  a CI/release concern only; no box installs, repairs or runs node)
- **Issue:** #2051

## Context

ADR-0059 made every green push to `main` a release, but a release was only a
tag. Every box then rebuilt fleet from source: `npm ci`, the Vite build, the Go
build and the sandbox image build, on the box, on every update. That needed Go
and a pinned Node on every box (ADR-0078 and its ~430-line node resolver, a
vendored node release keyring, a `doctor --node` repair path), took 5–15
minutes and ~1.3 GB of disk per update, and failed for reasons unrelated to the
release (registry hiccups, disk, toolchain drift). CI already built exactly
these artifacts on every push and threw them away. The Helm chart had no image
to pull at all.

The owner controls every box; all of them run on-box Postgres; behaviour
changes are acceptable when reasonable and documented.

## Decision

**1. release.yml publishes artifacts, split by privilege.** After the tag job,
a `build` job per architecture (native `ubuntu-24.04` and `ubuntu-24.04-arm`
runners, `contents: read` and nothing else) builds the web bundle, a static
`CGO_ENABLED=0` fleet binary, the `fleet-sandbox` image (generic bundle) and the
`fleet` control-plane image (`deploy/fleet.Containerfile`, which copies the
checked binary rather than building a second one), an SPDX SBOM for each, and
the stamped Helm chart. A `publish` job (contents/packages/id-token/attestations
write) that runs no code from the release pushes multi-arch indexes to
`ghcr.io/elcanotek/fleet` and `ghcr.io/elcanotek/fleet-sandbox`, signs the
images and attaches the image SBOMs as cosign attestations, adds SLSA build
provenance (`actions/attest-build-provenance`) for the binaries and both
images, writes `release.json` (tag, commit, image digests) and `SHA256SUMS` over
every published file, signs `SHA256SUMS` and each binary keylessly, verifies
those signatures with the exact identity boxes pin, and publishes the GitHub
release with every file.

**2. The host executor's absence is a property of the published artifact.**
The build job fails the release when `go version -m` records
`-tags=…fleet_host_executor` or the binary's compile-time marker
(`fleet version --build-info` → `host_executor`) is not `false`; it also
requires `CGO_ENABLED=0`, the release's version stamp and an embedded web
bundle. `fleet release fetch` re-checks the marker and the bundle on the box.

**3. Trust is one pinned signer identity.** A release signature must carry the
certificate identity
`https://github.com/ElcanoTek/fleet/.github/workflows/release.yml@refs/heads/main`
(release.yml runs on `workflow_run`, whose ref is always the default branch)
from issuer `https://token.actions.githubusercontent.com`, matched exactly —
never by regexp. Another workflow, branch, repository or person fails.

**4. Verification uses the cosign CLI, pinned by digest and fetched on
demand.** `internal/release/cosign.lock` pins the cosign version and its
SHA-256 per architecture (taken from cosign's own signed checksums). fleet
downloads it on first use, re-hashes it before every run, and refuses any other
bytes; `FLEET_COSIGN_BIN` may point at a local copy, which must hash the same.
The same lock is read by `scripts/lib/release.sh` (bootstrap, and the first
artifact update of a box whose fleet predates `fleet release`) and by
release.yml (the signer), so signer and every verifier run identical bytes.

**5. What a box checks, in order, before anything uses a release:** the
downgrade guard (older than running needs `--allow-downgrade`); the signature
on `SHA256SUMS`; each file's SHA-256 against the verified sums; the binary's own
signature; `release.json` names the requested tag (a correctly signed OLD
release served under a new tag's URL is refused); the binary is an ELF for this
architecture; its build info (no host executor, web bundle present). A missing
signature at any step is a failure. There is no unverified mode and no flag to
skip verification.

**6. `fleet update` installs releases.** The default (`--artifact`) moves the
checkout (units, scripts, generic bundle) to the release tag, fetches and
verifies the binary, and `fleet release install` stages it on the install
filesystem, keeps the running binary as `fleet.prev` (hard link), renames the
new one over `fleet` atomically, restarts (the service runs its own migrations
on start, as before), waits for `/readyz`, and rolls back to `fleet.prev`
automatically when it is not ready (the failed binary is kept as
`fleet.failed`). `fleet release rollback` performs the same swap on demand.

**7. Sandbox images.** A bundle that ships **no** Containerfile, or the generic
one unchanged, runs the release's signed `fleet-sandbox` image: its digest comes
from the verified `release.json`, its cosign signature is checked, and it is
pulled **by digest** into the service user's store and tagged as the bundle's
`sandbox.tag`. A bundle with its **own** Containerfile is still built on the
box; one that starts `ARG FLEET_SANDBOX_BASE` / `FROM ${FLEET_SANDBOX_BASE}`
is built on the verified release image (passed as a digest reference), and a
new release's base digest triggers its rebuild. A bundle that sets
`sandbox.image` keeps using that image. Bundles may also publish their own
images (`publish-sandbox-image.yml`), now under their own names only.

**8. Retention.** Release objects carry the downloadable files, so the newest
**ten** are kept (it was one) — the window for
`fleet update --tag <older> --allow-downgrade`. GHCR images are kept for every
tag. Each box also keeps its previous binary as `fleet.prev`. Tags remain
permanent.

**9. Toolchains leave the box.** bootstrap installs no Go and no Node; update
no longer resolves, repairs or builds with them; `scripts/lib/node-version.sh`,
the vendored node keyring, `doctor --node` and `scripts/fleet-upgrade.sh` are
deleted. `--from-source` remains on `update` and `bootstrap` for development
boxes, using whatever toolchain is on PATH, and every piece of it is marked
`LEGACY-BUILD-FROM-SOURCE: remove after boxes run artifacts`.

**10. Helm defaults to the published images** at `image.tag`, which defaults to
`v` + the chart's `appVersion` (stamped on the chart each release attaches); an
unstamped checkout chart requires `image.tag`.

## Enforcement

- `scripts/check_release_artifacts_test.go` — release.yml's signer identity,
  issuer and image names equal `internal/release`'s; the build job holds only
  `contents: read`, the publish job checks out no release ref and builds
  nothing; the host-executor, CGO and web-bundle assertions are present; three
  provenance steps; the fleet image copies the checked binary; the shell
  verifier pins the same identity, issuer and cosign.lock, never by regexp.
- `internal/release` tests — every rejection (wrong checksum, wrong signature,
  wrong identity, missing signature, wrong architecture, replayed release,
  downgrade, edited staging dir, non-pinned cosign bytes) with fakes; the swap,
  rollback and readiness logic with a fake service and prober; and an opt-in
  run of the real pinned cosign, offline, against a recorded keyless bundle
  (`FLEET_TEST_COSIGN`).
- `scripts/release_lib_test.go` — the shell verifier refuses non-pinned cosign
  bytes and (with `FLEET_TEST_COSIGN`) unsigned releases.
- `internal/admincli` script tests — the update plan verifies before the
  sandbox step and installs after it, builds nothing, and every `make build`
  sits in a LEGACY-marked block; `fleet version --build-info` always reports
  `host_executor`.
- CI's helm job — the stamped chart renders the release images and validates;
  the unstamped chart refuses to render without `image.tag`.
- release.yml itself verifies every signature it makes with the pinned
  identity before it publishes, so signer/verifier drift fails the release, not
  the next update on every box.

## Consequences

- Updates no longer need Go, Node, npm or ~1.3 GB of build cache, and cannot
  fail on a toolchain problem. A box needs outbound HTTPS to github.com
  (release assets, the cosign download), ghcr.io (images) and Sigstore's TUF
  CDN (cosign's trust root; `FLEET_SIGSTORE_TRUSTED_ROOT` serves air-gapped
  boxes).
- A box runs exactly what CI built and certified, on the release tag — not the
  branch tip. Commits on `main` reach a box only once release.yml tags and
  publishes them.
- A box's checkout sits at a release tag, so `git status` reads clean and
  `fleet version` names the release exactly.
- Supply-chain trust now rests on GitHub Actions' OIDC, Sigstore's public-good
  instance (Fulcio, Rekor) and the release workflow file on `main`. Anyone who
  can change release.yml on `main` can sign a release — the same people who can
  already merge code to it.
- release.yml's build job checks out the release tag on a `workflow_run`
  trigger. It holds no write scope, which is why that is acceptable here; the
  scanners' untrusted-checkout rules are aimed at privileged jobs, and the
  privileged publish job checks out only the default branch.
- Custom bundle Containerfiles still build on the box (podman stays a
  dependency regardless, for the sandbox); bundles that adopt
  `FROM ${FLEET_SANDBOX_BASE}` shrink that build to their own layers.
- `scripts/fleet-upgrade.sh`, `doctor --node`, `update --no-node-repair` and
  the node keyring tooling are gone; the reminder workflow keeps only the "new
  node major" check that CI's build needs.

## Alternatives considered

- **The sigstore-go library in-process.** The strongest isolation (no exec of a
  downloaded program) and no download at update time. Rejected for now because
  it adds a large dependency tree (TUF client, protobuf specs, in-toto,
  transparency-log clients) to the fleet binary that AGENTS.md asks us to
  justify, every module of which govulncheck and Dependabot then track — and
  because it cannot verify the FIRST binary on a box: bootstrap has no fleet
  yet. The pinned cosign serves Go and shell identically, and its digest pin
  makes "which verifier ran" a checked fact. Revisit if the library's
  dependency footprint shrinks.
- **`gh attestation verify`.** Verifies provenance well, but needs the gh CLI
  and, in practice, a GitHub token on every box — a credential on the box for
  the sake of reading public data, and a tool to install and keep current.
  Operators can still run it by hand against the published provenance.
- **Unsigned artifacts with checksums only.** A checksum fetched from the same
  place as the binary proves nothing about who built it.
- **A long-lived signing key.** A key to guard, rotate and distribute; keyless
  signing ties each signature to the workflow run that produced it and logs it
  in Rekor.
- **Bundles publishing their own sandbox images as the only path.** Kept as an
  option (`sandbox.image`), not required: most bundles change nothing or add a
  few packages, and building `FROM` the verified base on the box costs little.
- **Keeping the from-source update as the default.** It is exactly what the
  issue set out to remove; it stays only behind `--from-source`, marked for
  deletion.
