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
| `publish` | contents, packages, id-token, attestations: write | scans the image archives with Grype (CI's pin and policy) before anything is pushed, pushes multi-arch images, signs everything, attaches SBOM attestations and SLSA provenance, writes `release.json` + `SHA256SUMS`, publishes the GitHub release |
| `promote-latest` (one at a time: concurrency group `release-promote-latest`) | `contents: read`, `packages: write` | the only writer of the `:latest` image tags: recomputes the newest complete published release, verifies its `SHA256SUMS` signature and `release.json`, and points `fleet:latest` then `fleet-sandbox:latest` at the digests it records (a no-op when they already match) |

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
signed SBOM attestation BY REFERENCE (`cosign attest --type
https://github.com/ElcanoTek/fleet/sbom-reference/v1`,
`scripts/sbom-reference.sh`): it names that image's SPDX SBOM release asset —
file name, SHA-256, download URL — and the asset is covered by the signed
`SHA256SUMS`. Not the SBOM inline: the sandbox image's SBOM is ~28 MB, and a
DSSE envelope that large is refused by Rekor (the upload fails with a 502 on
every attempt, which is how the first releases failed). Both indexes and the
binaries have SLSA build provenance in GitHub's attestation store (pushed to
the registry for the images). release.yml runs on `workflow_run`, where the
job's own commit is the default-branch head when the release ran, possibly a
later commit than the one built, so the publish job writes the predicate
itself (`scripts/slsa-predicate.sh`). Its first dependency
(`resolvedDependencies[0]`) is that workflow source, the default branch at the
run commit, because GitHub's attestation API requires the first dependency to
match the signing certificate and rejects anything else ("values do not
match", which is how v2026.11.07.19 failed). The second dependency is the
release tag at the release commit. The publish job verifies every attestation
with `gh attestation verify` and requires the release commit in the tag's
dependency (`scripts/check-attested-commit.sh`) before it writes
`release.json`. The Sigstore certificate likewise records the workflow's
own run commit: that is the signer, not the source.

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

**A published release is required.** Nothing builds fleet on a box any more
([ADR-0101](adr/0101-remove-the-on-box-build.md)). A box with no network, or
with no published release to install, stops with a message naming that and
installs nothing; there is no build fallback. `--from-source` and
`FLEET_BOOTSTRAP_FROM_SOURCE=1` are refused with the same explanation.

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
   atomically, restarts the service as `systemctl stop`, remove the update
   drain marker, `systemctl start` (the service migrates on start, as before),
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

   **The drain marker comes off between the stop and the start.** The drain
   gate leaves `<FLEET_ENV_FILE>.update.drain` on disk so the old process
   admits no new work while it is idle-checked and stopped. fleet reads that
   marker on every `/readyz` and every task claim, so a process *started* under
   it answers `503 {"draining":"shutting_down"}` until the marker goes — in
   v2026.11.07.21 update held it through the install, the new binary never
   became ready, and every update without `--force` rolled back. Now
   update.sh hands the marker to `fleet release install` (its path in
   `FLEET_UPDATE_DRAIN_FILE`, its owner in `FLEET_DRAIN_OWNER_TOKEN`), and
   the install (and `fleet release rollback`, and the automatic rollback
   inside an install) removes it after the old process has exited and before
   the new one starts — every start, so a rolled-back binary also comes back
   serving. It removes only a marker that is this update's, or one whose owner
   process is gone; a marker another live `fleet restart`/`stop` holds, or one
   an operator created by hand, is left in place and named in the output.
   Once the stop is issued, a Ctrl-C or SIGTERM cannot cut it short, and a
   start always follows; both run detached from the signal but bounded by the
   install's or rollback's own deadline (at most 3 minutes each), so an
   interrupted stop never leaves fleet down and `FLEET_HEALTH_TIMEOUT` /
   the rollback budget still hold. If `systemctl stop` reports an error, the
   old process may still be running and a plain start would be a no-op the
   old process answers `/readyz` for; so the install restarts the unit
   instead, keeping the marker in place until that restart has replaced the
   old process (it comes off before the readiness probe), and if the restart
   fails too it makes sure fleet runs and fails the install (rolling back as
   usual) rather than report the new release live. Taking the marker's lock
   waits at most 30 seconds, so a stalled holder cannot block the start. The stop error is printed with the result. If the install is
   SIGKILLed between the steps, the update's exit trap still
   clears the marker, and fleet ignores one older than 15 minutes in any case.
   (There is no other restart path: the on-box `--from-source` build that
   restarted itself was removed by ADR-0101.)

**No systemd, or no fleet unit (a custom supervisor).** Update still installs
the verified binary — the same atomic swap, `fleet.prev` kept — but cannot
restart it: `fleet release install --no-restart`. It says so and does not
print the success banner; restart fleet with your supervisor, and if it does
not come up, `fleet release rollback --no-restart` then restart again.

**Where nothing unverified can run.** The release's staging dir lives under
the root-owned release cache (`/var/cache/fleet/release/<tag>`), never under
the checkout, and must be private — owned by root, mode 0700, every parent
closed to other writers or sticky — or fetch, sandbox and install refuse it.
Every staged file is opened without following links and must be a private
regular file; `SHA256SUMS` and its signature are verified on a private copy of
the exact bytes that are then parsed; the binary is hashed from an open
descriptor and installed by copying from that same descriptor (hashed again
while copying) — it is never reopened by name after it was verified. Before
the checkout moves to a release tag, update and bootstrap check the tag's
signed manifest (with the cosign pinned in the checkout already running) and
require `release.json` to name the commit the tag resolves to locally, so a
moved or locally created tag cannot get its code checked out, let alone run.
The pinned cosign itself runs only from a private cache (an operator-supplied
`FLEET_COSIGN_BIN` is copied in from the bytes that were hashed), and the
install dir must be writable by root alone.

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
publish job checks GHCR again right before it pushes
(`scripts/release-ghcr-gate.sh`), and nothing is ever pushed over an existing
tag: a tag holding another build's image (compared by config digest) refuses
the publish, while one holding THIS run's images is reused as is. That makes
a publish that failed after pushing (a Sigstore outage while signing)
resumable: **"Re-run failed jobs" on the original run** re-runs the publish
job on the same build artifacts, finds its own images, signs them again (a
second signature on the same digest is harmless) and publishes. "Re-run all
jobs" rebuilds, so the tag job refuses it; for anything else, delete the
partial publish and re-run, or let the next green push release. A re-run uses
the workflow file of the ORIGINAL run, so it resumes only on a release.yml
that already has the gate.

**Transient Sigstore and registry failures retry.** Every cosign `sign`,
`attest`, `sign-blob` and image `verify` in the publish job goes through
`scripts/retry-transient.sh`: 5 attempts over about three minutes, ONLY on a
5xx, a timeout or a dropped connection — a refused signature fails at once.
Each SLSA provenance step (an action, which cannot be wrapped) gets one retry
after a minute.

**Tagging.** Tagging retries with jittered backoff until an ordinal is free
(40 attempts as a backstop), so any number of green pushes landing together
all get tagged — but only a rejection that IS contention (the tag already
exists, a non-fast-forward: `scripts/release-push-rejection.sh`) retries; any
other refusal stops at once with its reason. One refusal is expected and
handled: GitHub will not let the job's token create a ref that would add or
change a workflow file relative to the default branch, so once a NEWER commit
on main has changed `.github/workflows/`, an older green commit cannot be
tagged at all. The tag job checks for exactly that before it pushes (the
commit is not main's tip and its workflow files differ from the tip's) and
skips the commit as **superseded**, with a notice: nothing is built or
published for it, and the newer commit's release, which contains it, covers
it. This amends "every built tag publishes" below: a superseded commit is
never tagged, so never built. (Creating the tag through the REST refs API
instead is not attempted: nothing documents it lifting the rule for an App
token.)

Overlapping runs both publish: a tag a newer one overtook while it
was building still gets its release, so no signed image is left without a
release naming it. The Latest mark follows the newest successfully
PUBLISHED release (non-draft, with `SHA256SUMS`;
`scripts/release-newer-published.sh`), checked before and after the create —
not the newest tag, so a newer tag whose build failed does not keep a complete
release from being Latest. The `:latest` image tags follow the same rule but
are moved only by the serialized `promote-latest` job
(`scripts/release-promote-latest.sh`), which never promotes "its own" run's
release: overlapping publish jobs cannot move `:latest` back to an older
digest or leave `fleet:latest` and `fleet-sandbox:latest` on different
releases. Retention counts published releases.

## Sandbox images and client bundles

| Bundle ships | What the box runs |
| --- | --- |
| no Containerfile (none at `sandbox/Containerfile` and none named by `sandbox.containerfile`), or the generic build context unchanged (its Containerfile, `requirements.txt` and `requirements.in`, and no other file) | the release's signed `fleet-sandbox`, pulled by digest and tagged as the bundle's `sandbox.tag` |
| its own Containerfile starting `ARG FLEET_SANDBOX_BASE` / `FROM ${FLEET_SANDBOX_BASE}` | built on the box on top of the verified release image; rebuilt when a release moves the base |
| any other Containerfile | built on the box from its own base, as before (with the max-age freshness backstop) |
| `sandbox.image` in its manifest | that image; nothing is pulled or built here |

The Containerfile is the manifest's `sandbox.containerfile` (relative to the
bundle) when set, else `sandbox/Containerfile` — resolved by one helper
(`scripts/lib/sandbox-bundle.sh`) that `build-sandbox-image.sh`, `bootstrap`
and `update` share. A manifest that names a Containerfile which does not exist
stops the run; the generic image is never used in its place.

A bundle that customizes nothing should ship **no** Containerfile and name
none, keeping only `sandbox.tag`; every fleet box's bundle now does. `update`
and `bootstrap` never build such a bundle's sandbox: they pull the release
image. When it cannot be pulled, `update` keeps the image already in the
service user's store, or stops before installing when there is none, and
`bootstrap` warns that the box has no runnable sandbox image. Only a by-hand
`scripts/build-sandbox-image.sh` run — CI, release.yml, or an operator
rebuilding on purpose — builds fleet's own `config/default` Containerfile
under the bundle's `sandbox.tag`. A copy of the generic Containerfile that has
drifted from fleet's (an old boilerplate) counts as a custom image and is
built on the box, so delete it rather than keeping it in sync.

A bundle that **does** ship its own Containerfile is still a supported
feature, and it is still built on the box (layered on the release image when
it declares `ARG FLEET_SANDBOX_BASE`). That build is of the bundle's sandbox
image, not of fleet, and needs only podman; ADR-0101 left it in place.
`update` rebuilds it when anything in its build context changes — the
Containerfile or any file beside it (a `requirements.txt`, a script it
copies), compared by one hash of the whole directory: file contents,
directories (an empty one too) and permission bits (`mode & 0755`, so a
`chmod +x` counts but a umask-dependent group/other write bit does not). It
ignores VCS metadata and whatever the context's `.containerignore` /
`.dockerignore` excludes. The first update from a release that hashed less
(only the Containerfile, or contents only) rebuilds once.

After pulling a new release image by digest, `update` removes the previous
one (it keeps its `@sha256` name, so `podman image prune` never would),
without `--force`. When a container still uses it, the removal is refused and
the ref is kept in `sandbox-image.reclaim` under the state directory; `update`
retries every listed ref after the service restart and on each later run, and
drops it once it is removed or gone. The current release ref, the bundle's
tag and their image are never removed.

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
`ghcr.io/elcanotek/fleet-sandbox`. The chart attached to a release is
stamped with that release's signed image digests (`image.signedDigest`,
`sandbox.signedImageDigest` — release.yml runs `scripts/helm-stamp-digests.sh` before
`SHA256SUMS` covers the chart), so it deploys both images BY DIGEST
(`repository@sha256:…`), the exact bytes the release signed, with no image
values. The tag (`image.tag`, defaulting to `v<appVersion>`) is only the
fallback for an unstamped chart from a checkout, which then needs `image.tag`;
CI renders a stamped chart and requires the digest form. The signed digests
apply only to the official repositories: an overridden `image.repository` or
`sandbox.imageRepository` (your own image, a mirror) is deployed by its own
`image.digest` / `sandbox.imageDigest` when set, else by tag, and the install
notes warn that it is not digest-pinned.
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
cosign verify-attestation --type https://github.com/ElcanoTek/fleet/sbom-reference/v1 \
  ghcr.io/elcanotek/fleet-sandbox@sha256:<per-arch digest> \
  --certificate-identity https://github.com/ElcanoTek/fleet/.github/workflows/release.yml@refs/heads/main \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  | jq -r '.payload | @base64d | fromjson | .predicate.sbom'   # names the SBOM asset and its SHA-256
# The SBOM itself: download the named asset, then check its hash against BOTH
# the signed SHA256SUMS and the attested digest (sha256sum -c above skipped it
# with --ignore-missing because it was not downloaded yet).
SBOM=fleet-sandbox-image-linux-amd64.spdx.json   # the asset the attestation names
curl -fsSLO "https://github.com/ElcanoTek/fleet/releases/download/$TAG/$SBOM"
grep " ${SBOM}\$" SHA256SUMS | sha256sum -c -                # matches the signed sums
sha256sum "$SBOM"                                               # compare with .predicate.sbom's sha256
```

The SBOM files are release assets, so they share the release's retention:
when an old release is pruned, its SBOM URLs stop resolving, while its GHCR
images and their attestations remain (the attested SHA-256 still identifies
the SBOM if you kept a copy).

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
| `GRYPE` | `ci.yml`, `grype-scheduled.yml`, `release.yml` | `update-pins.sh`: checksums file, its keyless signature (anchore's release workflow), the asset's SHA-256 |
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
Go modules are govulncheck's) — and blocks on a fixable CRITICAL/HIGH finding
in a Fedora RPM or in a package the build pip-installs under `/usr/local`
(Python dist-info that Fedora RPMs ship stays report-only:
`scripts/check-grype-policy.sh`). The release itself scans what it publishes:
the publish job runs the same pinned Grype and policy over all four
`*.oci.tar` archives (both images, amd64 and arm64) before anything is pushed
or signed, and a violation fails the release.

## No on-box build (2026-11-07, ADR-0101)

The `--from-source` path this design first kept for development boxes is gone.
Once all twelve fleet boxes ran signed release v2026.11.07.21 with
artifact-based updates, and Go, Node and their caches had been removed from
every box, [ADR-0101](adr/0101-remove-the-on-box-build.md) deleted it:

- `update.sh --from-source`, `bootstrap.sh --from-source`,
  `FLEET_UPDATE_FROM_SOURCE=1` and `FLEET_BOOTSTRAP_FROM_SOURCE=1` stop with a
  message that names the removal. `FLEET_UPDATE_FROM_SOURCE=0` is still
  accepted, because an `update.sh` from before the removal forwards it when it
  re-executes the new copy. Both variables are listed as retired in
  [CONFIGURATION.md](CONFIGURATION.md).
- `update.sh` no longer verifies a first release with the shell verifier when
  the installed fleet predates `fleet release`. Such a box (a from-source
  install that never took a release) is told to re-run `bootstrap.sh
  --enable-service` with its original flags; bootstrap is idempotent and
  installs the release over the existing install.
- An update that cannot reach origin or GitHub, or finds no published
  release, stops and changes nothing; it has no build to fall back to.
- Doctor's root-cache reclaim covers the dnf cache only; the Go and npm caches
  it also swept came from on-box builds.

`make build` and `make web` are unchanged developer and CI targets. To run
unreleased code, build it on a development machine.

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
  one with `id-token`), so its builder is release.yml's run rather than the
  build job specifically; its source is the release commit (above).
- **Retention** changed from one release object to ten (ADR-0059's "newest
  only"): release objects now carry the downloadable files.
