# Deploying fleet on Kubernetes

> The first-class Kubernetes path (issue #989 /
> [ADR-0049](adr/0049-kubernetes-backend-split-control-plane.md)): the fleet
> control plane as a single-replica Deployment, with agent sandboxes running as
> **ephemeral pods** via the pluggable sandbox backend
> (`FLEET_SANDBOX_BACKEND=kubernetes`). The single-box podman install
> ([`DEPLOYMENT.md`](DEPLOYMENT.md)) remains the default and an equally
> supported path; come here when Kubernetes is your platform standard.
>
> **This page is the engine side.** For a complete client bundle already shaped
> for this path — the two Containerfiles, a documented values overlay, and an
> empty-cluster-to-working-fleet walkthrough — fork
> [`ElcanoTek/example-kubernetes-config`](https://github.com/ElcanoTek/example-kubernetes-config).
> It is the Kubernetes sibling of
> [`ElcanoTek/example-config`](https://github.com/ElcanoTek/example-config),
> which targets the single-box podman install. Reading both together is the
> fastest way to see which parts of a bundle are deployment-shaped and which
> are not.

## The model

Same agent loop, same security model, one backend switch:

| Piece | Where it runs |
| --- | --- |
| fleet control plane (chat + orchestrator + MCP broker) | one Deployment replica — **never more**; the scheduler leases and worker semaphore are single-owner |
| Agent sandboxes (bash, run_python, file ops) | **ephemeral pods**, one per turn / sealed run / persistent-REPL conversation, created and exec'd by the control plane over the apiserver |
| MCP credentials | the control-plane process, always (ADR-0003) — sandbox pods carry no env, no secrets, no service-account token |
| Workspace | one **ReadWriteMany** PVC mounted at the *same absolute path* in the control plane and every sandbox pod |

```
            browser ──TLS──▶ Ingress ──▶ web (optional) ──▶ fleet Service
                                                             │ chat :8080
                                                             │ orchestrator :8000
        ┌────────────────────────────────────────────────────┴──────────┐
        │  fleet control plane pod (1 replica, Recreate)                │
        │  agent loop · scheduler · MCP broker (credentials stay here)  │
        └───────┬──────────────────────────────┬────────────────────────┘
                │ pods/exec (WebSocket)        │ Postgres (managed, or the
                ▼                              ▼  chart's eval StatefulSet)
   fleet-sandbox-<hex> pods (ephemeral)    chat + sched databases
   read-only rootfs · non-root · no caps
   no ServiceAccount token · egress by label
                │
                └── workspace PVC (RWX) — same path as the control plane
```

The backend is selected by `FLEET_SANDBOX_BACKEND` (`podman`, the default, or
`kubernetes`), overriding the bundle manifest's `sandbox.backend` — exactly the
precedence `FLEET_SANDBOX_RUNTIME` / `sandbox.runtime` uses
([SANDBOX-RUNTIMES.md](SANDBOX-RUNTIMES.md)). An unrecognized value refuses to
boot; there is no silent fallback.

**Fail-closed preflight.** With `kubernetes` selected, fleet refuses to start
unless, at boot: the apiserver is reachable with valid credentials; RBAC grants
`create/get/list/delete pods` and **both** `create` and `get` on `pods/exec` in
the sandbox namespace; the workspace claim exists; the sealed-egress
NetworkPolicy object exists; and the RuntimeClass exists when one is configured.
`fleet validate-config` runs the same cluster checks.

`get pods/exec` is the one people get wrong when hand-writing a Role. fleet
streams exec over a WebSocket upgrade, which is an HTTP **GET**, and the
apiserver derives the RBAC verb from the method — so `get` is what every
`bash` / `run_python` / file-tool call is actually authorized against. A Role
granting only `create` passes the rest of the preflight and then 403s on the
first tool call. The preflight now checks both verbs, so this fails at boot
where you can see it.

Beyond the verbs above the preflight also GETs three objects directly, so the
Role needs `get` on `persistentvolumeclaims` and `networkpolicies` in that
namespace, plus cluster-scoped `get runtimeclasses` when a RuntimeClass is
configured. The chart grants all of these; a hand-written Role must too.

## Build the two images

fleet publishes no images — you build both and push them to a registry your
nodes can pull from (ECR, GAR, ACR, GHCR, …). A `localhost/` build-on-box tag
cannot work outside a single-node kind cluster.

**Sandbox image** — the bundle artifact the sandboxes run, unchanged from the
single-box install:

```sh
scripts/build-sandbox-image.sh
podman tag localhost/fleet-sandbox:latest REGISTRY/fleet-sandbox:v1
podman push REGISTRY/fleet-sandbox:v1
```

(Or let CI publish it — see `.github/workflows/publish-sandbox-image.yml`.)

**Control-plane image** — the `fleet` binary plus your client bundle. A
reproducible multi-stage Containerfile (build it from the repo root; the
builder stage's Go minor is pinned to `go.mod` by
`scripts/check_versions_test.go`, so a stale copy of this stage fails CI):

```dockerfile
# ── build stage ──
FROM docker.io/library/golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags "-X github.com/ElcanoTek/fleet/internal/version.version=$(cat VERSION)" -o /out/fleet ./cmd/fleet

# ── runtime stage ──
FROM registry.fedoraproject.org/fedora-minimal:latest
RUN microdnf install -y git ca-certificates tzdata && microdnf clean all
COPY --from=build /out/fleet /usr/local/bin/fleet
# Bake the client bundle in (the generic one here; substitute your own).
COPY config/default /opt/fleet/client
ENV FLEET_CLIENT_CONFIG_DIR=/opt/fleet/client
RUN mkdir -p /var/lib/fleet && chown 1000:1000 /var/lib/fleet
USER 1000
ENTRYPOINT ["/usr/local/bin/fleet"]
```

An out-of-repo client bundle can be baked into your image the same way, or
mounted from a ConfigMap/volume at `FLEET_CLIENT_CONFIG_DIR` — either satisfies
the engine/bundle split (ADR-0006).

**Web image** (optional) — build `web/` with its own `next build` stage and set
`web.image` in the chart; the chart wires `CHAT_SERVER_URL` /
`ORCHESTRATOR_URL` at the fleet Service automatically.

## 15-minute path (kind)

Prereqs: `kind`, `kubectl`, `helm`, `podman` or `docker` to build images, and
an OpenRouter API key.

```sh
# 1. A cluster.
kind create cluster --name fleet

# 2. Build both images and load them into kind. Save the control-plane
#    Containerfile from "Build the two images" above as Containerfile.fleet.
scripts/build-sandbox-image.sh
podman save localhost/fleet-sandbox:latest -o /tmp/sandbox.tar
kind load image-archive /tmp/sandbox.tar --name fleet
podman build -t localhost/fleet:dev -f Containerfile.fleet .
podman save localhost/fleet:dev -o /tmp/fleet.tar
kind load image-archive /tmp/fleet.tar --name fleet

# 3. Secrets (the API key; DB URLs come from the chart's eval Postgres).
kubectl create namespace fleet
kubectl -n fleet create secret generic fleet-secrets \
  --from-literal=OPENROUTER_API_KEY=sk-or-...

# 4. Install. kind is single-node, so the default (ReadWriteOnce) storage
#    class works for the shared workspace — every pod lands on the one node.
helm install fleet deploy/helm/fleet --namespace fleet \
  --set image.repository=localhost/fleet --set image.tag=dev \
  --set image.pullPolicy=Never \
  --set sandbox.image=localhost/fleet-sandbox:latest \
  --set 'workspace.accessModes={ReadWriteOnce}' \
  --set postgres.enabled=true \
  --set config.existingSecret=fleet-secrets

# 5. Watch boot — the sandbox preflight logs its verdict before serving.
kubectl -n fleet logs deploy/fleet -f | grep -E 'sandbox|preflight'

# 6. Talk to it, and watch a sandbox pod appear for the turn.
kubectl -n fleet port-forward svc/fleet 8080:8080 &
kubectl -n fleet get pods -l app.kubernetes.io/name=fleet-sandbox -w
```

A chat turn that runs bash or python creates a `fleet-sandbox-<hex>` pod,
execs into it, and deletes it when the turn ends. Cancelling a turn deletes
the pod immediately (zero grace) — the same poison-and-retire containment the
podman backend guarantees (#796).

## Production checklist

1. **Storage: the workspace claim must be ReadWriteMany** (EFS, NFS, CephFS,
   Azure Files). A ReadWriteOnce class only works when every pod shares one
   node (kind). Verify:
   `kubectl -n fleet get pvc fleet-workspace -o jsonpath='{.spec.accessModes}'`.
2. **Database: managed Postgres.** Put `FLEET_CHAT_DATABASE_URL` and
   `FLEET_SCHED_DATABASE_URL` in your `config.existingSecret` and leave
   `postgres.enabled=false`. The chart's Postgres is an evaluation
   convenience: one replica, one PVC, no backups.
3. **NetworkPolicy enforcement is your CNI's job.** fleet verifies the
   deny-all policy *object* exists; only a CNI that implements NetworkPolicy
   (Calico, Cilium, the EKS VPC CNI's network-policy agent, GKE Dataplane V2,
   Azure CNI with policy) makes it real. Verify from a sealed sandbox:
   ```sh
   kubectl -n fleet run seal-test --restart=Never --rm -it \
     --labels=app.kubernetes.io/name=fleet-sandbox,fleet.elcanotek.com/egress=none \
     --image=busybox -- wget -T 5 -q -O- https://example.com && echo "NOT SEALED"
   ```
   A CNI that enforces the policy times that request out.
4. **Shape open-sandbox egress.** Non-lockdown sandboxes are labeled
   `egress=open` and unrestricted by default (they need PyPI etc.). Set
   `networkPolicies.openEgress.create=true` with your cluster/node CIDRs in
   `blockedCIDRs` so an open sandbox can reach the internet but not your
   Services.
5. **Registry, not build-on-box.** Both images in a registry the nodes pull
   from; set `sandbox.kubernetes.imagePullSecret` for private registries (on
   EKS, node-role ECR access covers sandbox pulls without a secret).
6. **Hypervisor isolation** (optional): install Kata Containers on the sandbox
   nodes, create a `kata` RuntimeClass, set
   `sandbox.kubernetes.runtimeClass=kata`. Preflighted fail-closed, mirroring
   `FLEET_SANDBOX_RUNTIME` (ADR-0010). Note `FLEET_SANDBOX_RUNTIME` itself is
   a podman knob and is **refused** under this backend.
7. **One replica.** Do not add an HPA or `replicas: 2` for the control plane.
   Scale work by raising `FLEET_MAX_CONCURRENT_AGENTS` and giving the sandbox
   namespace more node capacity; scale the control plane vertically
   (`resources` in values). Size for peak: the control plane runs the agent
   loop + brokers; the sandboxes' cost lives in their own pods, so warm-pool
   pods hold their requests while parked — size `FLEET_SANDBOX_WARM_SIZE`
   accordingly.
8. **Give runners their own node pool.** Label (and usually taint) a dedicated
   pool, then set `sandbox.kubernetes.nodeSelector` + `.tolerations` in the
   chart — sandbox pods pin there and autoscale the pool, while the control
   plane stays on your general nodes. This is the horizontal scaling story:
   more runner capacity is a bigger pool, never a second fleet.
9. **Run `fleet validate-config`** (`kubectl -n fleet exec deploy/fleet --
   fleet validate-config`) after any config change: it runs the same
   fail-closed preflight boot does, plus everything else the verb checks.

## Day-2 operations

The systemd timers and host scripts (`bootstrap.sh`, `fleet update`,
`scripts/doctor.sh`, `fleet timers install`) are single-box tooling and do not
apply here ([TIMERS.md](TIMERS.md)). Their cluster equivalents:

| Single-box | Kubernetes |
| --- | --- |
| `fleet update` | build + push a new control-plane image, `helm upgrade` (strategy Recreate = a brief restart; in-flight turns drain per `FLEET_SHUTDOWN_GRACE_SECONDS`, **bounded by the pod's `terminationGracePeriodSeconds`** — the chart sets 90s; raise both together for long turns, because raising only the fleet knob does nothing) |
| `fleet-backup.timer` | a CronJob running `fleet backup --db=all --prune` — or skip it entirely by using managed-database backups (RDS/Cloud SQL snapshots), the recommended posture |
| `fleet-maintenance.timer` | **nothing to schedule.** `fleet cleanup` prunes dangling *podman* image layers and Go build caches; a control-plane pod has neither, so the job would print two disk lines and exit. Node-local image GC is the kubelet's job. fleet's own hourly maintenance loop (reclamation, disk backpressure, stuck-task backstops — [MAINTENANCE.md](MAINTENANCE.md)) runs **inside** the control plane on both paths |
| journald | `kubectl logs` / your log stack; set `FLEET_LOG_FILE` only if you also mount somewhere rotatable |
| Grafana node dashboards | scrape the control plane's `/metrics` (orchestrator port). NOTE it is **admin-API-key gated** (`X-API-Key`) — cost/token data must not be public — and stock Prometheus cannot send custom headers, so use a scraper that can (Grafana Alloy, vmagent) or a small header-injecting sidecar. Sandbox pods are ordinary pods your cluster metrics already see |

Backup CronJob, only needed when you want `fleet backup`'s application-level
dumps alongside (or instead of) managed-database snapshots. Three things about
it are easy to get wrong, and all three are in the manifest below:

- **`pg_dump` must be in the image.** `fleet backup` shells out to `pg_dump -Fc`
  and verifies with `pg_restore --list`; the control-plane image built earlier
  in this guide installs neither. Add `postgresql` to it, or run this job from
  an image that has the client.
- **`--out` defaults to `FLEET_BACKUP_DIR`, else the working directory.** With
  no `WORKDIR` that is `/`, not the PVC this job mounts — the dumps land in the
  container's writable layer and vanish with the pod. Set it explicitly.
- **Point `secretRef` at the Secret that actually holds the DSNs.** With
  `postgres.enabled=true` they live in the chart-generated
  `<release>-postgres` Secret, not in your `fleet-secrets`.

Note also that `fleet-data` is ReadWriteOnce and already mounted by the
Deployment, so on most CSI drivers this job only schedules onto the same node.
Give it its own RWX claim, or use managed snapshots — the recommended posture.

```yaml
apiVersion: batch/v1
kind: CronJob
metadata: {name: fleet-backup, namespace: fleet}
spec:
  schedule: "0 2 * * *"
  jobTemplate:
    spec:
      template:
        spec:
          restartPolicy: Never
          containers:
            - name: backup
              # The control-plane image PLUS the postgres client — see above.
              image: REGISTRY/fleet-backup:v1
              args: ["backup", "--db=all", "--prune"]
              env:
                - name: FLEET_BACKUP_DIR
                  value: /var/lib/fleet/backups
              envFrom:
                # Where the DB URLs really are when postgres.enabled=true.
                - secretRef: {name: fleet-postgres}
                - secretRef: {name: fleet-secrets}
              volumeMounts: [{name: data, mountPath: /var/lib/fleet}]
          volumes:
            - name: data
              persistentVolumeClaim: {claimName: fleet-backups}
```

## Bundle docs inside a sandbox pod

A bundle's `protocols/`, `personas/`, `system_prompts/` and `skills/` are how a
protocol-driven deployment works at all: the system prompt lists them by
relative path and the agent reads them on demand. Under podman fleet
bind-mounts each read-only at its own absolute path and symlinks them into the
per-conversation workspace, so `protocols/foo.yaml` resolves for `view_file`,
`bash` and `run_python` alike.

A sandbox pod mounts only the workspace claim. There is no host filesystem to
bind from, so by default fleet drops those roots — and because the fileop path
anchor only trusts roots that are actually mounted, `view_file
protocols/foo.yaml` is *refused* (`fileop root is not inside a sandbox bind
mount`) rather than attempted. The workspace symlinks still point at the
bundle's absolute paths, so `bash`/`run_python` reads fail too, as not-found.

The one exception is a read-only root that lives *inside* the claim: the
shared file library's staged tree (`<workspace>/shared`,
[SHARED-FILES.md](SHARED-FILES.md)) reaches every pod by construction, and the
pod spec re-mounts that subPath of the same claim read-only, so the library is
readable — and only readable — with no image rebuild and no host bind.

The fix is the sandbox image. Build it with the bundle's doc dirs baked in at
the **same absolute paths** the control plane uses (`FLEET_CLIENT_CONFIG_DIR`),
then declare it:

```yaml
sandbox:
  kubernetes:
    bundleDocsInImage: true      # chart values → FLEET_SANDBOX_K8S_BUNDLE_DOCS_IN_IMAGE
```

```dockerfile
# derived sandbox image; the bundle's doc dirs at the SAME absolute paths the
# control plane reads them from (its FLEET_CLIENT_CONFIG_DIR).
FROM REGISTRY/fleet-sandbox:v1
USER root
# One COPY per directory, on purpose: a multi-source COPY copies each source's
# CONTENTS into the destination, which would merge all four into one flat dir —
# and the paths have to match the control plane's exactly for the anchors to
# resolve.
COPY protocols/       /opt/fleet/client/protocols/
COPY personas/        /opt/fleet/client/personas/
COPY system_prompts/  /opt/fleet/client/system_prompts/
COPY skills/          /opt/fleet/client/skills/
RUN chown -R 0:0 /opt/fleet/client && chmod -R a-w,a+rX /opt/fleet/client
USER 1000
```

With the declaration, the anchors for those roots stay valid, so the file tools
read them out of the pod's image layer and the symlinked relative paths work
for bash and python. Four things to be honest about:

- **It is a declaration, not a probe.** fleet cannot inspect an image's
  contents. It also cannot widen anything: the flag only re-admits *read-only*
  anchors for roots the operator already configured, and the read still runs
  inside the sandbox. A wrong declaration surfaces as a not-found read.
- **Only the bundle's own doc dirs are covered.** Other entries in the mount
  list (the uploads root) live in control-plane state no image can contain;
  they stay dropped, with a log line each. Chat attachments don't need that
  mount here: under this backend the chat server copies each validated
  non-image attachment into the conversation's workspace directory
  (`<workspace>/<convID>/attachments/`) — inside the claim every pod mounts —
  and the prompt block advertises the staged path, so `view_file`/`bash`
  reads work without the uploads root. (Image attachments reach the model
  host-side as vision input on both backends and need no staging.)
- **The merged skills tree is never covered** — see the honest-scope list.
- **The baked copy is a snapshot.** Build and roll the control-plane and
  sandbox images from the same bundle commit, or the agent reads one release's
  protocols while the control plane runs another's. Nothing enforces this.

Boot logs the drop/keep decision for these roots: with the declaration it names
each root it kept and each it will not vouch for; undeclared it logs a count,
plus — always, declared or not — the merged-skills-tree case by name.
`kubectl logs deploy/fleet | grep 'kubernetes backend'` catches all of those
lines; narrower greps miss the skills one, which is the case most likely to
surprise you.

## Provider notes

- **EKS**: EFS (via the EFS CSI driver) is the standard RWX workspace class;
  ECR for both images (node-role pull, no secret needed); enable the VPC CNI
  network-policy agent or run Calico/Cilium so the deny-all policy is
  enforced; ALB via the AWS Load Balancer Controller for `ingress`. RDS for
  Postgres. Kata needs a bare-metal (`*.metal`) node group for `/dev/kvm`.
- **GKE**: Filestore CSI for RWX; Dataplane V2 enforces NetworkPolicy natively;
  Artifact Registry with Workload Identity; Cloud SQL.
- **AKS**: Azure Files (NFS) for RWX; enable Azure Network Policy or Cilium;
  ACR with the kubelet identity; Azure Database for PostgreSQL.
- **Bare metal / on-prem**: any NFS/CephFS class for RWX; Calico or Cilium for
  policy; your own registry.

## Configuration reference

Every knob can come from env (the chart sets these) or the bundle manifest's
`sandbox:` block (env wins, field by field):

| Env | Manifest | Meaning |
| --- | --- | --- |
| `FLEET_SANDBOX_BACKEND` | `sandbox.backend` | `podman` (default) or `kubernetes` |
| `FLEET_SANDBOX_K8S_NAMESPACE` | `sandbox.kubernetes.namespace` | sandbox pod namespace (default: the control plane's own, else `fleet-sandboxes`) |
| `FLEET_SANDBOX_K8S_WORKSPACE_CLAIM` | `…workspace_claim` | **required** — the shared RWX PVC name |
| `FLEET_SANDBOX_K8S_SERVICE_ACCOUNT` | `…service_account` | identity stamped on sandbox pods (no token is mounted) |
| `FLEET_SANDBOX_K8S_IMAGE_PULL_SECRET` | `…image_pull_secret` | pull secret for the sandbox image |
| `FLEET_SANDBOX_K8S_RUNTIME_CLASS` | `…runtime_class` | hypervisor isolation (kata); preflighted |
| `FLEET_SANDBOX_K8S_SECCOMP_PROFILE` | `…seccomp_profile` | node-local Localhost seccomp profile; empty = RuntimeDefault |
| `FLEET_SANDBOX_K8S_KUBECONFIG` | `…kubeconfig` | out-of-cluster auth (token / client-cert kubeconfigs only); empty = in-cluster |
| `FLEET_SANDBOX_K8S_NETWORK_POLICY` | `…network_policy` | deny-all policy name the preflight requires (default `fleet-sandbox-deny-all`) |
| `FLEET_SANDBOX_K8S_BUNDLE_DOCS_IN_IMAGE` | `…bundle_docs_in_image` | the sandbox image carries the bundle's doc dirs at the same absolute paths — keeps their fileop read anchors valid in a pod ([above](#bundle-docs-inside-a-sandbox-pod)); a non-boolean refuses to boot |
| `FLEET_SANDBOX_K8S_NODE_SELECTOR` | `…node_selector` | pin sandbox pods to a dedicated runner pool — env form `"pool=sandboxes,arch=amd64"`, manifest form a map; a malformed value refuses to boot |
| `FLEET_SANDBOX_K8S_TOLERATIONS` | `…tolerations` | tolerations for a tainted runner pool — env form a JSON array of `{key,operator,value,effect}`, manifest form a YAML list |

The shared sandbox knobs apply to both backends: `FLEET_SANDBOX_IMAGE`,
`FLEET_SANDBOX_MEMORY` / `_CPUS` (converted to pod resource limits),
`FLEET_SANDBOX_DISK_GB` (the pod's ephemeral-storage limit),
`FLEET_SANDBOX_WARM_SIZE` / `_WARM_TTL` (the warm pool holds pre-started
pods), and the python REPL knobs.

## Troubleshooting

- **Boot fails with "kubernetes sandbox preflight"** — the message names the
  exact missing piece (RBAC verb, claim, NetworkPolicy, RuntimeClass). The
  chart's `fleet-runner` Role carries every needed verb; if you wrote your own
  RBAC, diff it against `deploy/helm/fleet/templates/rbac.yaml`.
- **First turn fails with `ErrImagePull` / `ImagePullBackOff`** — the sandbox
  image ref isn't pullable *from the nodes* (fleet fails the pod fast with the
  kubelet's reason instead of burning the start timeout). Check the ref and
  `sandbox.kubernetes.imagePullSecret`.
- **`sandbox pod … not ready before start timeout`** — usually scheduling
  (no node fits the sandbox requests) or a slow first pull; `kubectl describe
  pod fleet-sandbox-…` shows which.
- **Workspace files owned by the wrong uid** — the claim's storage class must
  honor `fsGroup` (1000) or be provisioned world-writable at the root; both
  the control plane and sandbox pods run uid/gid 1000.
- **A sealed turn can still reach the network** — your CNI is not enforcing
  NetworkPolicy (checklist item 3). The policy *object* existing is not
  enforcement.
- **Turn cancelled but you want proof nothing survived** — cancellation
  deletes the pod with zero grace; `kubectl get pods -l
  app.kubernetes.io/name=fleet-sandbox` should not show the pod after the
  cancel completes. A pod that lingers past a crash is reclaimed by the
  boot-time orphan sweep on the next control-plane start.

## Honest scope — what the kubernetes backend does differently

Recorded here so nobody discovers them in production:

- **Egress sealing is delegated.** Podman's `--network=none` is a kernel
  namespace with no interface; the k8s equivalent is a label
  (`fleet.elcanotek.com/egress=none`) matched by a deny-all NetworkPolicy.
  fleet verifies the object exists — it cannot verify the CNI enforces it.
- **`FLEET_DEFAULT_NETWORK_MODE=allowlisted` is refused** at boot: the
  host-side egress proxy (ADR-0012) is unreachable from pods. Use `lockdown`
  or `open` + NetworkPolicy shaping.
- **No per-pod pids limit.** `FLEET_SANDBOX_PIDS` has no Pod-spec equivalent;
  runaway process counts are bounded by pod memory/CPU limits and node
  `podPidsLimit` if you configure the kubelet.
- **A bundle's Python MCP servers run in the control-plane pod.** The broker
  spawns them host-side, which on this path means inside the control plane — so
  that image needs `python3` and your bundle's `mcp/requirements.txt`. The
  generic bundle ships no servers, so the walkthrough's image works until you
  substitute a real bundle, and then the servers fail at spawn with a
  per-server error in the log rather than a boot failure.
- **No per-sandbox resource telemetry (#263).** `podman stats` has no
  in-process counterpart here; task resource summaries are absent. Use your
  cluster's metrics stack on the `fleet-sandbox` pods.
- **The bundled seccomp profile does not apply, and `RuntimeDefault` is
  weaker.** fleet's bundled profile is a default-*deny* allowlist that blocks
  `ptrace`, `process_vm_readv`, `io_uring_setup`, `userfaultfd`,
  `perf_event_open`, `keyctl`, `bpf`, `personality` and `unshare` (#219).
  `RuntimeDefault` is a default-*allow* denylist that permits several of those.
  To match podman's posture, install `internal/sandbox/seccomp-default.json` on
  the sandbox nodes' kubelet seccomp root and point
  `FLEET_SANDBOX_K8S_SECCOMP_PROFILE` at it. Setting the podman
  `FLEET_SANDBOX_SECCOMP_PROFILE` under this backend refuses to boot rather
  than being silently ignored.
- **Supporting-doc bind mounts don't apply** — the podman backend bind-mounts
  persona/protocol dirs same-path into containers; a pod only mounts the
  workspace claim. Bake them into the sandbox image and declare it
  (`sandbox.kubernetes.bundle_docs_in_image`) to get the reads back; see
  [Bundle docs inside a sandbox pod](#bundle-docs-inside-a-sandbox-pod).
  Undeclared, in-sandbox reads of those paths do not resolve at all.
- **A bundle inheriting fleet's built-in skills pack cannot serve in-sandbox
  skill reads**, declaration or not: the merged tree is materialized under the
  control plane's data dir, so no sandbox image can carry it. `skills_builtin:
  false` in the bundle manifest makes `skills/` the bundle's own (bake-able)
  dir at the cost of the built-in pack. There is no setting that gives you
  both.
- **Disk quota is per-pod ephemeral storage**, which caps the writable layer
  and the scratch emptyDirs. The workspace claim sits outside it — and unlike
  podman there is *no* per-file cap either: podman always applies
  `--ulimit fsize`, and that reaches its workspace bind mount, where a pod has
  no equivalent. One runaway write can fill the shared RWX volume. Size the
  claim and monitor it. Note too that exceeding the ephemeral-storage limit
  **evicts the pod** mid-turn, where podman would return `ENOSPC` to the
  process.
- **Warm-pool pods hold cluster resources while parked.** Sandbox pods set
  requests *equal to* limits (Guaranteed QoS), so a parked warm pod reserves
  its full CPU, memory and ephemeral-storage allocation before any turn runs.
  Do the arithmetic: the reservation is
  `warmSize × (sandbox.cpus, sandbox.memory, sandbox.diskGB)`, and it must be
  schedulable on the runner pool on top of peak concurrency. Note that
  `FLEET_SANDBOX_WARM_SIZE=0` does **not** mean "no warm pool" — unset, fleet
  derives the depth from `FLEET_MAX_CONCURRENT_AGENTS`, clamped to 2..8, so
  eight concurrent agents parks eight pods. Set it explicitly.

- **An open sandbox pod is a full citizen of the cluster network.** Podman's
  non-lockdown default is rootless pasta/slirp4netns with no host-loopback:
  outbound-only, and structurally unable to reach the fleet process itself. A
  pod has no such limit. With `networkPolicies.openEgress.create=false` (the
  default) nothing selects an `egress=open` sandbox, so model-authored code can
  reach the fleet Service, the in-cluster Postgres, the apiserver, and the
  node's cloud metadata endpoint at `169.254.169.254` — which hands out the
  node's IAM credentials. Treat the open-egress policy as **required**, not
  optional; the chart's default `blockedCIDRs` starts with the metadata range,
  and you should add your Pod/Service/node ranges to it.

- **A sealed sandbox's network calls hang rather than failing fast.** Podman
  lockdown is `--network=none`: no interface, so a connect fails instantly with
  `ENETUNREACH`. A deny-all NetworkPolicy *drops* packets instead, so a DNS
  lookup or TCP connect in a sealed turn blocks until it times out, spending
  the turn's budget instead of erroring immediately.

- **No PID-1 reaper.** Podman runs the sandbox under `--init` so that a
  SIGKILLed python kernel's zombie is reaped (#213). A pod's PID 1 is
  `sleep infinity`, which reaps nothing, so a long persistent-REPL conversation
  accumulates zombies for the pod's lifetime — bounded only by the node's
  `podPidsLimit`, not by fleet.

- **`FLEET_SANDBOX_PIDS` is ignored, not refused.** The podman fork-bomb
  containment (default 128) has no pod-spec equivalent wired up, so the pod
  inherits the node's `podPidsLimit`, which most distros leave unset. Unlike
  the seccomp and runtime knobs, this one does not fail the boot — set
  `podPidsLimit` on the sandbox nodes if you need the ceiling.

- **The sealed-egress NetworkPolicy is verified by name only.** The preflight
  proves an object with that name exists in the namespace; it does not yet
  decode it to confirm the selector matches sandbox pods, that `policyTypes`
  includes `Egress`, or that the rule list is empty. A policy with the right
  name and the wrong shape passes boot. Use the chart's policy, or diff yours
  against `deploy/helm/fleet/templates/networkpolicy.yaml`.

- **The pod start budget is a fixed 2 minutes** and has no knob. A cold node
  pulling a ~1.3 GB sandbox image can exceed it; pre-pull onto runner nodes
  rather than letting the first turn pay for it.

- **The bridge and file-op helper scripts live in a sandbox-writable
  `emptyDir`.** Podman passes the file-op helper inline and bind-mounts the
  bridge read-only, so neither can be altered by the code they contain. Here a
  `bash` call can rewrite them and change what later file-tool calls report
  upward. It grants no access `bash` lacks — the workspace claim is already
  mounted — but it does mean file-tool *results* are not tamper-evident.
- **kind e2e is a documented walkthrough, not a CI job.** CI lints and
  template-renders the chart (`helm` job) and unit-tests the backend against a
  fake apiserver (including exec streaming and the poison path); it does not
  stand up a cluster.
