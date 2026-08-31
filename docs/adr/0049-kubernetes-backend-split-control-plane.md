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
document was a hand-verified EKS recipe (`docs/EKS-DEPLOYMENT.md`, since
removed) that ran rootless Podman inside a privileged pod — an operator
workaround, not a product.

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
4. **Sealing is expressed as labels + required NetworkPolicies.** *(Amended
   2026-08-28 — the requirement now covers BOTH labels, not just `none`.)*
   Sandbox pods carry `fleet.elcanotek.com/egress=none|open`; the Helm chart
   ships a deny-all policy selecting `none` and a shaping policy selecting
   `open`. fleet verifies the policy **objects** exist; enforcement is the
   CNI's, and the docs say so plainly rather than implying a seal fleet cannot
   provide (the podman `--network=none` namespace seal has no per-pod apiserver
   equivalent).

   The original decision required only the deny-all object, which left the
   `open` half asymmetric with podman in a way this ADR did not intend.
   Podman's open posture is bounded by construction — rootless
   pasta/slirp4netns is outbound-only and structurally cannot reach the fleet
   process — while an open **pod** that no policy selects is a full citizen of
   the pod network. Measured on a stock k3s install during the #1264
   validation, such a sandbox reached the fleet Service, the in-cluster
   Postgres, the apiserver and the public internet. The docs already called
   that policy required; boot now requires it too whenever the default network
   mode is `open`, so "the docs say required" and "fleet requires it" stopped
   being different statements.

   One escape hatch, because fleet must not claim knowledge it lacks: egress
   may legitimately be shaped by policy fleet cannot see (a
   CiliumNetworkPolicy, a Calico GlobalNetworkPolicy, a mesh, a namespace
   default-deny). `FLEET_SANDBOX_K8S_OPEN_EGRESS_ACKNOWLEDGED=true` states that
   deliberately and is logged as a warning on every boot — an unverified
   posture keeps saying so rather than being agreed to once.
5. **Enterprise packaging is one Helm chart** (`deploy/helm/fleet`):
   single-replica control-plane Deployment (strategy Recreate, no replica
   knob), the runner RBAC, workspace storage, the NetworkPolicies, optional
   in-cluster Postgres / web / Ingress. No operator, no CRDs in v1.
6. **REST is hand-rolled; exec streaming is client-go.** *(Amended 2026-08-25
   — the original decision put BOTH on a hand-rolled client to keep the
   dependency tree small, with a recorded revisit trigger. The trigger fired
   on the first real cluster (#1264's kind rehearsal): the hand-rolled
   `v4.channel.k8s.io` client lost exec stdin nondeterministically for
   payloads beyond a few KB — the 28KB bridge upload wedged on ~4 of 5
   attempts, churning every warm pod on a two-minute cycle — while client-go's
   remotecommand executor moved the identical payloads 5 of 5 on the same
   cluster and pod, as did kubectl at 7MB. A transport demonstrably less
   reliable than the reference implementation is not a dependency saving.)*
   Exec now rides `k8s.io/client-go/tools/remotecommand` (protocol
   `v5.channel.k8s.io`, real stdin half-close), the transport kubectl itself
   uses. The adoption is deliberately narrow: client-go is a transport, never
   a config loader — pod CRUD and the preflight stay on the hand-rolled REST
   client (five plain verbs that demonstrably work), and kubeconfig support
   stays fleet's own strict parser — token, token-file, client-cert; exec
   plugins and `insecure-skip-tls-verify` are refused; the `rest.Config`
   handed to client-go is built from that already-validated material. If the
   backend ever needs watches/informers, extend the client-go usage rather
   than growing the hand-rolled client.

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
  story. The EKS recipe that documented it is **removed** rather than kept as
  a parallel path — an unmaintained privileged-pod recipe beside a first-class
  unprivileged one would imply support it does not have.
- Multi-replica / active-active fleet; a Kubernetes operator or CRDs.
- The **allowlisted** egress mode under the kubernetes backend: the host-side
  egress proxy is unreachable from pods, so the mode is refused at boot
  (fail-closed) instead of silently granting open egress. Cluster-side egress
  shaping via NetworkPolicy is the replacement.
- Per-pod pids limits (not expressible in a Pod spec), the bundled seccomp
  JSON (nodes take a Localhost profile instead), and `podman stats` resource
  telemetry (#263) — each recorded as an honest deviation in
  `docs/DEPLOYMENT-KUBERNETES.md`.
- Same-path supporting-doc bind mounts: a pod has no host filesystem to bind
  from. fleet does not synthesize them (no ConfigMap projection, no
  control-plane push into the workspace claim — both would put bundle content
  on a writable, agent-reachable surface). Instead the sandbox IMAGE may carry
  the bundle's doc dirs at the same absolute paths, and
  `sandbox.kubernetes.bundle_docs_in_image` declares that, which keeps those
  roots' **read-only** fileop anchors valid inside a pod. A declaration, not a
  probe: fleet cannot inspect an image, so it is trusted the way
  `sandbox.image` and `runtime_class` are — and it can only re-admit reads of
  operator-configured paths, executed inside the sandbox, so a wrong
  declaration degrades to not-found rather than widening any boundary.

## Consequences

- Kubernetes-native organizations get a supported, preflighted, CI-linted
  path: `helm install` + two images they build. The EKS privileged-pod recipe
  is retired in its favor.
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

- **client-go for everything.** Originally rejected for dependency weight
  against a five-verb surface, with a recorded revisit trigger. The trigger
  fired for exec streaming (see decision 6's amendment); CRUD remains
  hand-rolled since plain request/response REST has no streaming semantics to
  get wrong.
- **Chart-only first, backend later.** Rejected by the issue itself: a chart
  that still requires privileged Podman-in-pod would enshrine the workaround.
- **A Kubernetes operator/CRD.** Unnecessary for v1 — Helm + RBAC covers
  install; fleet's own scheduler owns runtime orchestration.
- **Running sandbox pods in a dedicated namespace by default.** The RBAC story
  is marginally nicer, but a PVC cannot be mounted across namespaces, so the
  default topology shares the release namespace; a split namespace remains
  possible with static same-export PVs and is documented.
