# Signed release artifacts

Design note for #2051 (decision record: [ADR-0100](adr/0100-signed-release-artifacts.md)).
Every green push to `main` is a release (ADR-0059). A release is now the tag
**and** a set of signed artifacts, and boxes install those artifacts instead of
building fleet themselves. No box needs Go or Node.

## What a release publishes

release.yml runs three jobs after CI goes green on a push to `main`:

| Job | Permissions | What it does |
| --- | --- | --- |
| `tag` | `contents: write` | mints `vYYYY.MM.DD.N` on the certified SHA (unchanged) |
| `build` (amd64 on `ubuntu-24.04`, arm64 on `ubuntu-24.04-arm`) | `contents: read` only | web bundle; static `fleet` binary; the host-executor, CGO, version and web-bundle assertions; `fleet-sandbox` and `fleet` images; SPDX SBOMs; the stamped Helm chart |
| `publish` | contents, packages, id-token, attestations: write | pushes multi-arch images, signs everything, attaches SBOM attestations and SLSA provenance, writes `release.json` + `SHA256SUMS`, publishes the GitHub release |

The build job is the only one that runs the release's code, and it holds no
write scope. The publish job runs no code from the release: it checks out the
default branch (for `scripts/version.sh` and `internal/release/cosign.lock`)
and handles the build's outputs as data.

Release assets:

| File | What it is |
| --- | --- |
| `fleet-linux-amd64`, `fleet-linux-arm64` | static binaries, web UI embedded, no host executor |
| `fleet-linux-<arch>.sigstore.json` | keyless cosign signature bundle for each binary |
| `fleet-linux-<arch>.spdx.json` | SBOM of each binary (from its Go build info) |
| `fleet-image-linux-<arch>.spdx.json`, `fleet-sandbox-image-linux-<arch>.spdx.json` | SBOMs of each image |
| `fleet-<YYYY>.<MMDD>.<N>.tgz` | the Helm chart, stamped with this release (its images default to it) |
| `fleet-linux.provenance.sigstore.json` | the SLSA provenance bundle for the binaries |
| `release.json` | `{schema, tag, commit, images}` — the image digests this release built |
| `SHA256SUMS` + `SHA256SUMS.sigstore.json` | checksums of every file above, and their signature |

Images, multi-arch (amd64 + arm64) indexes tagged with the release tag (and
`:latest` for the newest release only):

- `ghcr.io/elcanotek/fleet` — `deploy/fleet.Containerfile`: the release's own
  checked binary (copied, not rebuilt), git, python3, chromium-headless, the
  postgres client, the generic bundle at `/opt/fleet/client`.
- `ghcr.io/elcanotek/fleet-sandbox` — the generic bundle's sandbox
  (`config/default/sandbox/Containerfile`).

Each index digest is signed with cosign; each per-architecture image carries a
signed SPDX attestation (`cosign attest --type spdxjson`); both indexes and the
binaries have SLSA build provenance in GitHub's attestation store (pushed to
the registry for the images).

## The trust rule

Every signature must carry the certificate identity
`https://github.com/ElcanoTek/fleet/.github/workflows/release.yml@refs/heads/main`
issued by `https://token.actions.githubusercontent.com` — matched exactly. The
publish job verifies its own signatures against that identity before it
publishes, so a mismatch between signer and verifiers fails the release rather
than every box's next update.

Before a box uses anything from a release it checks, in order:

1. the release is not older than the running one (unless `--allow-downgrade`);
2. `SHA256SUMS` is signed by the identity above;
3. the binary and `release.json` match the signed sums;
4. the binary carries its own valid signature;
5. `release.json` names the tag that was asked for (replay guard);
6. the binary is an ELF for this box's architecture;
7. `fleet version --build-info` reports `host_executor: false` and a web bundle.

The sandbox image is used only through the digest in the verified
`release.json`, after `cosign verify` of that digest, and is pulled by digest.
A missing signature anywhere is a failure. There is no unverified mode.

The verifier is the cosign CLI at the version and SHA-256 pinned in
`internal/release/cosign.lock` — downloaded on first use into
`/var/cache/fleet` (root) and re-hashed before every run. Why cosign rather than
the sigstore-go library or `gh attestation verify` is in ADR-0100.

## Install (a new box)

```sh
git clone https://github.com/ElcanoTek/fleet.git && cd fleet
sudo scripts/bootstrap.sh --postgres=local --enable-web --domain fleet.example.com
```

bootstrap installs runtime packages only (git, curl, jq, rsync, python3, pip,
chromium-headless, gcc, podman, slirp4netns — no Go, no Node). It installs the
newest **published** release — GitHub's "latest", which names a release only
once its assets are uploaded — or the one `--tag vYYYY.MM.DD.N` names; never a
tag that merely exists in the clone, whose artifacts may still be building or
already pruned. It moves the checkout to that tag first and re-runs itself from
it, so the units and scripts it installs are the release's; it refuses when the
checkout has uncommitted changes or commits origin does not have. It then
downloads and verifies the release with `scripts/lib/release.sh` (every
download size-capped, no partial files left behind), installs it to
`/opt/fleet/fleet`, and sets up the sandbox image as described below. It needs outbound HTTPS to
github.com, ghcr.io and Sigstore's TUF CDN.

## Update

```sh
sudo fleet update                       # the newest published release
sudo fleet update --tag v2026.11.07.3   # a specific one (older: add --allow-downgrade)
sudo fleet update --no-pull             # reinstall the release the checkout is at
sudo fleet update --dry-run             # the plan, touching nothing
fleet update --check                    # "running vX, vY is published" + checkout/bundle freshness
```

What it does, in order (`scripts/update.sh`):

1. drain gate (unchanged: refuses while work is in flight unless `--wait` /
   `--force`);
2. move the fleet checkout to the release **tag** — units, scripts and the
   generic bundle then match the binary. Usually a fast-forward; a checkout
   PAST the tag (a downgrade, or a box that tracked the branch tip) is moved
   back to it, which update announces and asks about like any other range. It
   refuses with uncommitted changes, or when moving back would strand commits
   origin does not have. `--no-pull --tag <other release>` refuses (it would
   pair that binary with this checkout's content). If GitHub names a release
   the tag fetch just missed, tags are fetched once more. (The client-config
   bundle is its own checkout and is not moved back on a downgrade.)
   Only one update runs per box at a time (a lock under the state dir), and
   earlier releases' staging dirs are removed before a download;
3. pull the client-config bundle (unchanged);
4. `fleet release fetch` — download and verify (above), using the
   **installed** fleet;
5. sandbox image (below);
6. host preparation (MCP Python deps, chromium, login banner, retired-shim and
   web-tier housekeeping — unchanged);
7. `fleet release install`, run by the verified new binary, which re-verifies
   the staged files, stages the binary on the install filesystem, keeps the
   running one as `fleet.prev` (hard link), renames the new one over `fleet`
   atomically, `systemctl restart`s (the service migrates on start, as before),
   waits for `/readyz` (`FLEET_HEALTH_TIMEOUT`, default 90 s), and on failure
   restores `fleet.prev`, restarts and re-checks — the failed binary is kept
   as `fleet.failed` and the update exits non-zero saying it rolled back. The
   rollback runs under its own bounded context, so a Ctrl-C or SIGTERM that cut
   the readiness wait short still completes it. Reinstalling the binary that is
   already live swaps nothing and leaves `fleet.prev` alone. One install or
   rollback runs per install dir at a time (a lock), and the staging copy is
   refused up front when the filesystem lacks room for it. The readiness URL
   is where the deployment listens: `FLEET_SERVER_ADDR` from the environment or
   the env file, over https when `FLEET_TLS_MODE` is `manual` or `auto` — verified
   in full: the probe dials loopback with `ServerName` = `FLEET_TLS_DOMAIN`
   (or `--probe-server-name`), against the system roots plus, in manual mode,
   `FLEET_TLS_CERT_FILE`; with no domain known it refuses rather than skip the
   check.

**No systemd, or no fleet unit (a custom supervisor).** Update still installs
the verified binary — the same atomic swap, `fleet.prev` kept — but cannot
restart it: `fleet release install --no-restart`. It says so and does not
print the success banner; restart fleet with your supervisor, and if it does
not come up, `fleet release rollback --no-restart` then restart again.

## Roll back

```sh
sudo fleet release rollback             # fleet.prev ↔ fleet, restart, /readyz gate
sudo fleet update --tag <older> --allow-downgrade   # any release still published
```

`rollback` swaps the two binaries, so running it twice undoes itself, and
gates on the address the deployment listens on (`FLEET_SERVER_ADDR` /
`FLEET_TLS_MODE` from the env file, unless `--server-addr` or `--health-url`
says otherwise). The checkout and bundle are not moved by `rollback`; `fleet doctor` reports the
mismatch, and `fleet update --tag <that release> --allow-downgrade` realigns
everything when you mean to stay back.

**Retention.** The newest ten GitHub release objects are kept (they carry the
downloadable files); images stay in GHCR for every tag; tags are permanent;
each box keeps one previous binary.

**Releases are immutable.** A re-run of release.yml never rebuilds a tag that
published: the tag job skips a complete release, and REFUSES — loudly, for a
human — a partial one (a draft, a release without `SHA256SUMS`, or GHCR images
under the tag with no complete release), because a rebuild would put different
bytes (a newer base layer or RPM) under a tag boxes already verified. The
publish job checks GHCR again right before it pushes. Recovering a partial
publish is deliberate: delete it and re-run, or let the next green push
release. Overlapping runs both publish: a tag a newer one overtook while it
was building still gets its release (with `--latest=false`), so no signed
image is left without a release naming it; Latest, `:latest` and retention
follow the newest tag.

## Sandbox images and client bundles

| Bundle ships | What the box runs |
| --- | --- |
| no `sandbox/Containerfile`, or the generic one unchanged | the release's signed `fleet-sandbox`, pulled by digest and tagged as the bundle's `sandbox.tag` |
| its own Containerfile starting `ARG FLEET_SANDBOX_BASE` / `FROM ${FLEET_SANDBOX_BASE}` | built on the box on top of the verified release image; rebuilt when a release moves the base |
| any other Containerfile | built on the box from its own base, as before (with the max-age freshness backstop) |
| `sandbox.image` in its manifest | that image; nothing is pulled or built here |

A bundle Containerfile that only adds packages should become:

```dockerfile
ARG FLEET_SANDBOX_BASE=ghcr.io/elcanotek/fleet-sandbox:latest
FROM ${FLEET_SANDBOX_BASE}
RUN microdnf install -y <packages> && microdnf clean all
```

The default value is only for building it by hand; update and bootstrap always
pass the verified digest reference. `publish-sandbox-image.yml` still publishes
a bundle's image under its own name, and refuses
`ghcr.io/elcanotek/fleet-sandbox`, which belongs to release.yml.

## Kubernetes

The Helm chart defaults to `ghcr.io/elcanotek/fleet` and
`ghcr.io/elcanotek/fleet-sandbox` at `image.tag`, which defaults to
`v<appVersion>`. The chart attached to a release is stamped, so it installs
that release with no image values; a chart from a checkout needs `image.tag`.
See [DEPLOYMENT-KUBERNETES.md](DEPLOYMENT-KUBERNETES.md) for verifying the
images and building a bundle image `FROM` them.

## Verifying by hand

```sh
TAG=v2026.11.07.3
for f in SHA256SUMS SHA256SUMS.sigstore.json fleet-linux-amd64; do
  curl -fsSLO "https://github.com/ElcanoTek/fleet/releases/download/$TAG/$f"; done
cosign verify-blob --bundle SHA256SUMS.sigstore.json \
  --certificate-identity https://github.com/ElcanoTek/fleet/.github/workflows/release.yml@refs/heads/main \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com SHA256SUMS
sha256sum -c --ignore-missing SHA256SUMS
gh attestation verify fleet-linux-amd64 --repo ElcanoTek/fleet      # SLSA provenance
cosign verify-attestation --type spdxjson ghcr.io/elcanotek/fleet-sandbox@sha256:<per-arch digest> \
  --certificate-identity https://github.com/ElcanoTek/fleet/.github/workflows/release.yml@refs/heads/main \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com   # the image SBOM
```

## Knobs

| Variable | Effect |
| --- | --- |
| `FLEET_RELEASE_BASE_URL` | asset mirror (`<base>/<tag>/<file>`); signatures, not the URL, are trusted |
| `FLEET_RELEASE_LATEST_URL` | the "newest release" endpoint (GitHub's API by default) |
| `FLEET_COSIGN_URL` / `FLEET_COSIGN_BIN` | where the pinned cosign is downloaded from / a local copy — either must hash to `cosign.lock` |
| `FLEET_SIGSTORE_TRUSTED_ROOT` | a Sigstore `trusted_root.json` for offline verification |
| `FLEET_RELEASE_CACHE_DIR` | cosign + TUF cache (default `/var/cache/fleet` as root) |
| `FLEET_HEALTH_TIMEOUT` | seconds `fleet update` waits for `/readyz` (default 90) |
| `FLEET_UPDATE_TAG`, `FLEET_UPDATE_ALLOW_DOWNGRADE` | env forms of `--tag`, `--allow-downgrade` |

## How pins are kept current

Every pinned input of the release images and the workflows has an automated
update path, and `scripts/check_pins_test.go` fails CI when one does not.
Dependabot (`.github/dependabot.yml`, monthly, 7-day cooldown) covers what it
can read; `.github/workflows/pins-update.yml` (monthly, after Dependabot) runs
`scripts/update-pins.sh` over `scripts/lib/pins.tsv` for the rest and opens one
PR (or, where Actions may not open PRs, pushes the branch and files an issue
linking it).

| Pin | Where | Updater |
| --- | --- | --- |
| fedora-minimal base, tag + digest | `deploy/fleet.Containerfile`, `config/default/sandbox/Containerfile` | Dependabot `docker` (`/deploy`, `/config/default/sandbox`) |
| Python for the images, hash-locked (`--require-hashes --no-deps`) | `deploy/python/requirements.txt`, `config/default/sandbox/requirements.txt` (compiled from the `requirements.in` beside each) | Dependabot `pip` (re-compiles the lock with hashes) |
| GitHub Actions, by commit SHA | every workflow | Dependabot `github-actions` (`/`) |
| `COSIGN` — the verifier boxes and release.yml run | `internal/release/cosign.lock` | `update-pins.sh`: the new `cosign_checksums.txt` must carry Sigstore's keyless signature, verified by the cosign already pinned; both binaries hash-checked |
| `GRYPE` | `ci.yml`, `grype-scheduled.yml` | `update-pins.sh`: checksums file, its keyless signature (anchore's release workflow), the asset's SHA-256 |
| `GITLEAKS`, `ACTIONLINT`, `KUBECONFORM` | `ci.yml` | `update-pins.sh`: checksums file and the asset's SHA-256 (upstream publishes no signature) |
| `RUFF`, `SEMGREP` (`pip install x==v`) | `ci.yml`, `semgrep.yml` | `update-pins.sh`: newest final PyPI release past the cooldown |

The Playwright browser is not a pin: the control-plane image never downloads
one (`PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1`); fleet's browser runner launches the
distribution's `chromium-headless`.

**RPMs are not version-pinned, deliberately.** The images install Fedora
packages with `microdnf`, which checks every package's GPG signature against
Fedora's keys; the exact versions each release installed are in its SBOMs.
Pinning NEVRAs would break the release build whenever Fedora's `updates`
repository drops a superseded build (it keeps only the newest), would block
the security fixes the Grype gate demands, and has no updater Dependabot or
`update-pins.sh` could run. The pinned base digest plus immutable releases
(above) is what makes a published release's bytes final.

**CVE scanning.** CI's Grype job scans both release images — the sandbox and,
built around a placeholder binary, the control-plane image (the fleet binary's
Go modules are govulncheck's) — and blocks on a fixable CRITICAL/HIGH RPM
finding.

## Development boxes: `--from-source`

`fleet update --from-source` and `bootstrap.sh --from-source` keep the old
on-box `make build`, for development boxes only. They use whatever Go, Node and
npm are on PATH — nothing installs or repairs them any more — and update from
the branch tip, not a release. They restart without the readiness gate or
rollback. Every piece of this path is marked
`LEGACY-BUILD-FROM-SOURCE: remove after boxes run artifacts` so it can be found
and deleted in one pass.

## What shipped

- release.yml: the build/publish split above, multi-arch binaries and images,
  keyless signatures (binaries, `SHA256SUMS`, images), SLSA provenance
  (binaries and both images), SPDX SBOMs (binaries and images; image SBOMs also
  as signed attestations), the host-executor assertion, the stamped chart,
  retention of ten release objects, `:latest` for the newest release only.
- `internal/release` + `fleet release latest|fetch|sandbox|install|rollback`,
  `fleet version --build-info`.
- `fleet update` installs releases by default; `--check` reports releases.
- bootstrap installs from artifacts; no Go or Node on the box.
- `fleet doctor`: no node toolchain or Go/Node package currency; checks the
  installed binary's build info, `fleet.prev`, and installed vs newest release.
- Helm defaults to the published images.
- Deleted: `scripts/lib/node-version.sh`, `scripts/lib/node-release-keys.*`,
  `scripts/update-node-release-keys.sh`, `scripts/fleet-upgrade.sh` (its
  backup/readiness/rollback is `fleet release install`), `doctor --node`,
  `update --no-node-repair`, `docs/NODE-TOOLCHAIN-HANDOFF.md`, and the
  keyring half of the node reminder workflow.
- Fixed on the way: `update.sh --no-pull` on the command line used to fetch
  anyway (only the env var skipped it).

## Deviations from the issue

- **"Move what remains worth keeping into `fleet` subcommands."** The part that
  is the update itself — fetch, verify, swap, health gate, rollback — moved into
  Go (`fleet release`). The rest of `update.sh` (drain gate, the two git
  checkouts, bundle staging, unit/Caddyfile adoption, timers, web-tier
  housekeeping) stays shell: it is host orchestration with its own tests, and
  porting it would be a rewrite with no behaviour change. This is a decision,
  not a deferral.
- **"Substantially smaller."** The operator shell shrinks by about a thousand
  lines net (the node resolver, keyring tooling, `fleet-upgrade.sh`, doctor's
  node repair, the build paths), but `update.sh` and `bootstrap.sh` themselves
  are about the same size: the build paths they lost were replaced by the
  release and sandbox-image steps.
- **Provenance is published and verifiable, but the updater enforces the
  signatures, not the provenance.** Signatures (on `SHA256SUMS`, the binary and
  the image) carry the same workflow identity the provenance would; the
  provenance adds build metadata that operators can check with
  `gh attestation verify`. The provenance is generated in the publish job (the
  one with `id-token`), so it names release.yml's run rather than the build
  job specifically.
- **Retention** changed from one release object to ten (ADR-0059's "newest
  only"): release objects now carry the downloadable files.
