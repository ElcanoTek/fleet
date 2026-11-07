# deploy/fleet.Containerfile — the fleet control-plane image release.yml
# publishes as ghcr.io/elcanotek/fleet (docs/RELEASE-ARTIFACTS.md).
#
# It does NOT build fleet. It copies in the exact binary the release built,
# checked and signed (dist/fleet-linux-<arch>, with the web bundle embedded and
# the host executor provably absent), so the image and the downloadable binary
# are the same bytes and there is one build to trust, not two. Build context is
# the repo root with dist/ populated:
#
#   podman build -f deploy/fleet.Containerfile --build-arg FLEET_ARCH=amd64 \
#     -t localhost/fleet:dev .
#
# The runtime packages are what the control plane shells out to host-side:
# git (worktrees, bundle checkouts), python3 + pip (bundle MCP servers, the
# Playwright browser tool), chromium-headless (built-in browser automation) and
# the postgresql client (`fleet backup` runs pg_dump / pg_restore --list).
# The generic client bundle is baked in at /opt/fleet/client; mount your own
# bundle over it (or at another FLEET_CLIENT_CONFIG_DIR) — ADR-0006.

# Base pinned by tag + digest; Dependabot (docker, /deploy) bumps both monthly.
FROM registry.fedoraproject.org/fedora-minimal:45@sha256:ef221f00e46f2e51c950188e6c64fdb45cfc375b0480f575be150ca0bda4c3cc

ARG FLEET_ARCH

# The Python packages (Playwright and its dependencies) are hash-locked in
# deploy/python/requirements.txt (compiled from requirements.in; Dependabot's
# pip entry keeps it current) and installed --require-hashes --no-deps.
# Playwright's own browser download is never used: the browser is the
# distribution's chromium-headless RPM (signature-checked by microdnf), which
# fleet's browser runner launches by path — PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD
# keeps it that way.
COPY deploy/python/requirements.txt /tmp/fleet-requirements.txt
RUN microdnf install -y --setopt=install_weak_deps=0 \
        git ca-certificates tzdata python3 python3-pip chromium-headless postgresql \
    && PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1 pip3 install --no-cache-dir --require-hashes --no-deps \
        -r /tmp/fleet-requirements.txt \
    && rm -f /tmp/fleet-requirements.txt \
    && microdnf clean all

COPY dist/fleet-linux-${FLEET_ARCH} /usr/local/bin/fleet
COPY config/default /opt/fleet/client

ENV FLEET_CLIENT_CONFIG_DIR=/opt/fleet/client \
    HOME=/var/lib/fleet \
    PLAYWRIGHT_SKIP_BROWSER_DOWNLOAD=1

# (`fleet version --web` and the host-executor check run on this exact binary
# in release.yml's build job, before this image is built.)
RUN chmod 0755 /usr/local/bin/fleet \
    && mkdir -p /var/lib/fleet \
    && chown 1000:1000 /var/lib/fleet

USER 1000
WORKDIR /var/lib/fleet
ENTRYPOINT ["/usr/local/bin/fleet"]
CMD ["serve"]
