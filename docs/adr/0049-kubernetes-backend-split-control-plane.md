# ADR-0049: Kubernetes as a first-class deployment — split control plane, pluggable sandbox backend

- **Status:** Accepted
- **Date:** 2026-08-22
- **Deciders:** fleet maintainers
- **Amends:** [ADR-0004](0004-single-box-vm-native-deployment.md) (supersedes
  its "no k8s manifest, Helm chart, or operator in the tree" enforcement
  clause and its cluster-work-is-out-of-scope consequence; the single-box
  default install it decides **stands**)

## Context

ADR-0004 made fleet VM-native on one box: systemd, Caddy, rootless Podman
co-located with the process. That remains the right default install for
individuals and small teams. But Kubernetes-native organizations were left
with no supported path (issue #989): `deploy/` shipped only systemd units, the
sandbox was **always** co-located with the fleet process, and the only k8s
document was a hand-verified EKS recipe that ran rootless Podman inside a
privileged pod — an operator workaround, not a product.

The owner decision on #989: ship the **enterprise path in one pass** — the
fleet control plane separate from execution runners, with a pluggable sandbox
backend — and do **not** build a co-located "fleet pod + Podman on a
privileged node" packaging track as a stepping stone.

## Decision

1. **The sandbox backend is pluggable, selected by one knob.** The internal
   per-sandbox interface (`internal/sandbox`'s `impl`) gains a third
   implementation: alongside the rootless-Podman backend (`containerImpl`) and
   the test-only host executor, `k8sImpl` runs each sandbox as an **ephemeral
   Kubernetes Pod** exec'd over the apiserver. `FLEET_SANDBOX_BACKEND`
   (overriding the bundle manifest's `sandbox.backend`) selects
   `podman` (default) or `kubernetes`, mirroring `sandbox.runtime`'s
   precedence exactly (ADR-0010). An unrecognized value refuses to boot.
2. **The kubernetes backend fails closed at boot.** Selecting it triggers a
   preflight — apiserver reachable, RBAC verbs present (pods CRUD +
   `pods/exec`), the shared workspace claim exists, the sealed-egress
   NetworkPolicy object exists, the RuntimeClass exists when configured — and
   any failure aborts boot. There is no fallback to podman or host execution
   (the ADR-0010 no-degrade posture, applied to backends).
3. **The workspace is a shared ReadWriteMany claim, mounted same-path.** The
   control plane and every sandbox pod mount the same PVC at the same absolute
   path, preserving the invariant (ADR-0036 territory) that an absolute
   workspace path means the same thing to the process, host-side brokers, and
   sandboxed bash/python.
4. **Sealing is expressed as labels + a required NetworkPolicy.** Sandbox pods
   carry `fleet.elcanotek.com/egress=none|open`; the Helm chart ships a
   deny-all policy selecting `none`. fleet verifies the policy **object**
   exists; enforcement is the CNI's, and the docs say so plainly rather than
   implying a seal fleet cannot provide (the podman `--network=none` namespace
   seal has no per-pod apiserver equivalent).
5. **Enterprise packaging is one Helm chart** (`deploy/helm/fleet`):
   single-replica control-plane Deployment (strategy Recreate, no replica
   knob), the runner RBAC, workspace storage, the NetworkPolicies, optional
   in-cluster Postgres / web / Ingress. No operator, no CRDs in v1.
6. **The API client is hand-rolled, not client-go.** The backend needs five
   verbs plus WebSocket exec streaming; client-go would add dozens of modules
   to a tree gated by govulncheck and image CVE scans. `internal/sandbox`
   speaks plain REST via net/http and `v4.channel.k8s.io` exec framing via
   gorilla/websocket (already a dependency). If the backend ever needs
   watches/informers or exotic auth, revisit client-go rather than growing the
   hand-rolled client. Kubeconfig support is deliberately narrow — token,
   token-file, client-cert; exec plugins and `insecure-skip-tls-verify` are
   refused.

## What does not change

- **The single-box podman install stays the default** and its story is
  untouched: bootstrap, systemd units, Caddy, `fleet timers`. ADR-0004's
  decision section stands for that install.
- **The sandbox is still mandatory** (ADR-0002): the kubernetes backend is a
  different *where*, not a weaker *whether*. Pods run read-only-rootfs,
  non-root, all capabilities dropped, seccomp RuntimeDefault (or an
  operator-installed Localhost profile), `automountServiceAccountToken=false`.
- **Credentials stay in the control plane** (ADR-0003): sandbox pods get no
  env, no secrets, no service-account token — only the workspace mount. The
  MCP broker never moves.
- **One governed loop** (ADR-0001): the backend swap is entirely below
  `agentcore`; no second governance path exists.
- **Single-owner control plane:** one fleet replica, ever. Horizontal scale of
  *work* is more sandbox pods / bigger node pools, not more fleet processes.
- **Poison-and-retire (#796)** carries over: a cancelled or timed-out call
  deletes the whole pod with zero grace — destroying its PID namespace and
  every straggler — and retires the sandbox.

## Explicit non-goals (v1)

- Co-located "fleet + Podman in a privileged pod" as the supported enterprise
  story (the EKS recipe remains a hand-verified operator document).
- Multi-replica / active-active fleet; a Kubernetes operator or CRDs.
- The **allowlisted** egress mode under the kubernetes backend: the host-side
  egress proxy is unreachable from pods, so the mode is refused at boot
  (fail-closed) instead of silently granting open egress. Cluster-side egress
  shaping via NetworkPolicy is the replacement.
- Per-pod pids limits (not expressible in a Pod spec), the bundled seccomp
  JSON (nodes take a Localhost profile instead), `podman stats` resource
  telemetry (#263), and same-path supporting-doc bind mounts — each recorded
  as an honest deviation in `docs/DEPLOYMENT-KUBERNETES.md`.

## Consequences

- Kubernetes-native organizations get a supported, preflighted, CI-linted
  path: `helm install` + two images they build. The EKS privileged-pod recipe
  stops being the only answer.
- `deploy/` now contains cluster artifacts, so ADR-0004's enforcement clause
  ("no k8s manifest, Helm chart, or operator in the tree") is superseded; its
  index row and status note point here.
- A second execution substrate must be kept honest: the backend seam
  (`sandbox.Backend`-shaped `impl`) is now a contract two production backends
  implement, and behavior-affecting changes must land in both or say why not.
- The chart is linted and template-rendered in CI (`helm` job in ci.yml /
  dev-ci.yml) but not exercised against a live cluster there; the kind
  walkthrough in `docs/DEPLOYMENT-KUBERNETES.md` is the verified end-to-end
  path.

## Alternatives considered

- **client-go.** Rejected for dependency weight against a five-verb surface;
  recorded above as the explicit revisit trigger.
- **Chart-only first, backend later.** Rejected by the issue itself: a chart
  that still requires privileged Podman-in-pod would enshrine the workaround.
- **A Kubernetes operator/CRD.** Unnecessary for v1 — Helm + RBAC covers
  install; fleet's own scheduler owns runtime orchestration.
- **Running sandbox pods in a dedicated namespace by default.** The RBAC story
  is marginally nicer, but a PVC cannot be mounted across namespaces, so the
  default topology shares the release namespace; a split namespace remains
  possible with static same-export PVs and is documented.
