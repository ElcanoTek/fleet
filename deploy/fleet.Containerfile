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

FROM registry.fedoraproject.org/fedora-minimal:latest

ARG FLEET_ARCH

RUN microdnf install -y --setopt=install_weak_deps=0 \
        git ca-certificates tzdata python3 python3-pip chromium-headless postgresql \
    && pip3 install --no-cache-dir playwright \
    && microdnf clean all

COPY dist/fleet-linux-${FLEET_ARCH} /usr/local/bin/fleet
COPY config/default /opt/fleet/client

ENV FLEET_CLIENT_CONFIG_DIR=/opt/fleet/client \
    HOME=/var/lib/fleet

RUN chmod 0755 /usr/local/bin/fleet \
    && /usr/local/bin/fleet version --web \
    && mkdir -p /var/lib/fleet \
    && chown 1000:1000 /var/lib/fleet

USER 1000
WORKDIR /var/lib/fleet
ENTRYPOINT ["/usr/local/bin/fleet"]
CMD ["serve"]
