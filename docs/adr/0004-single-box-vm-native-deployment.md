# ADR-0004: Single-box, VM-native deployment (no Kubernetes)

- **Status:** Accepted
- **Date:** 2026-06-28 (documents a decision that predates this record)
- **Deciders:** fleet maintainers

## Context

fleet is a self-hosted, general-purpose agent platform aimed at running on **one
box, on a budget**. The default reach for "production" is Kubernetes, which buys
horizontal scale and rolling deploys at the cost of a control plane, a registry
pipeline, network policy, and a large operational surface — overhead that
dominates the workload fleet actually targets.

## Decision

fleet deploys **VM-native on a single box**, supervised by **systemd**, fronted
by **Caddy** (see `deploy/`). One Go process runs interactive chat and the
scheduling engine together; the tool sandbox is rootless Podman on the same
host (ADR-0002). There is no Kubernetes, no cluster control plane, and no
required container orchestrator for fleet itself.

The sandbox base image tracks `fedora-minimal:latest` *by choice*
(`config/default/sandbox/Containerfile`) rather than being pinned to a digest —
a deliberate trade of reproducibility for staying current on the minimal base.

## Enforcement

- `deploy/` ships the systemd unit and Caddyfile for the single-box model;
  there is no k8s manifest, Helm chart, or operator in the tree.
- The bootstrap/update/status scripts in `scripts/` target a single host.

## Consequences

- Operations are "boring on purpose": `systemctl`, journald, one Postgres, one
  process to reason about. Backups and upgrades are scripted, not orchestrated
  (see `docs/BACKUP_RESTORE.md`).
- Scaling is **vertical** (a bigger box), not horizontal. Work that assumes a
  cluster (pod autoscaling, multi-node scheduling) is out of scope unless a
  future ADR supersedes this one.
- New features should not introduce a hard dependency on a cluster-only
  primitive.

## Alternatives considered

- **Kubernetes-native.** Rejected for the target deployment: the control-plane
  and pipeline overhead outweighs the benefit for a single-box, budget-minded
  install. A cluster operator could still run fleet's process under k8s, but the
  project does not require or ship for it.
- **Pinning the sandbox base to a digest.** Considered and deliberately not
  done; staying on the rolling minimal base is the chosen trade-off. Re-pinning
  would need its own ADR.
