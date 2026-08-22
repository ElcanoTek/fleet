# Deploying fleet on Kubernetes

> The first-class Kubernetes path (issue #989 /
> [ADR-0049](adr/0049-kubernetes-backend-split-control-plane.md)): the fleet
> control plane as a single-replica Deployment, with agent sandboxes running as
> **ephemeral pods** via the pluggable sandbox backend
> (`FLEET_SANDBOX_BACKEND=kubernetes`). The single-box podman install
> ([`DEPLOYMENT.md`](DEPLOYMENT.md)) remains the default; come here when
> Kubernetes is your platform standard. For the older hand-verified
> "one privileged pod running Podman inside" recipe, see
> [`EKS-DEPLOYMENT.md`](EKS-DEPLOYMENT.md) — that is an operator workaround,
> not this path.

## The model

Same agent loop, same security model, one backend switch:

| Piece | Where it runs |
| --- | --- |
| fleet control plane (chat + orchestrator + MCP broker) | one Deployment replica — **never more**; the scheduler leases and worker semaphore are single-owner |
| Agent sandboxes (bash, run_python, file ops) | **ephemeral pods**, one per turn / sealed run / persistent-REPL conversation, created and exec'd by the control plane over the apiserver |
| MCP credentials | the control-plane process, always (ADR-0003) — sandbox pods carry no env, no secrets, no service-account token |
| Workspace | one **ReadWriteMany** PVC mounted at the *same absolute path* in the control plane and every sandbox pod |

The backend is selected by `FLEET_SANDBOX_BACKEND` (`podman`, the default, or
`kubernetes`), overriding the bundle manifest's `sandbox.backend` — exactly the
precedence `FLEET_SANDBOX_RUNTIME` / `sandbox.runtime` uses
([SANDBOX-RUNTIMES.md](SANDBOX-RUNTIMES.md)). An unrecognized value refuses to
boot; there is no silent fallback.

**Fail-closed preflight.** With `kubernetes` selected, fleet refuses to start
unless, at boot: the apiserver is reachable with valid credentials; RBAC grants
`create/get/list/delete pods` and `create pods/exec` in the sandbox namespace;
the workspace claim exists; the sealed-egress NetworkPolicy object exists; and
the RuntimeClass exists when one is configured. `fleet validate-config` runs
the same checks.

## 15-minute path (kind)

Prereqs: `kind`, `kubectl`, `helm`, `podman` or `docker` to build images, and
an OpenRouter API key.

```sh
# 1. A cluster.
kind create cluster --name fleet

# 2. Build the two images and load them into kind.
#    Sandbox image (the bundle's Containerfile):
scripts/build-sandbox-image.sh            # produces localhost/fleet-sandbox:latest
podman save localhost/fleet-sandbox:latest -o /tmp/sandbox.tar
kind load image-archive /tmp/sandbox.tar --name fleet

#    Control-plane image — the fleet binary + the client bundle. A minimal
#    Containerfile (run `make build` first so ./fleet exists):
cat > /tmp/Containerfile.fleet <<'EOF'
FROM registry.fedoraproject.org/fedora-minimal:latest
RUN microdnf install -y git ca-certificates && microdnf clean all
COPY fleet /usr/local/bin/fleet
COPY config/default /opt/fleet/client
ENV FLEET_CLIENT_CONFIG_DIR=/opt/fleet/client
RUN mkdir -p /var/lib/fleet && chown 1000:1000 /var/lib/fleet
USER 1000
ENTRYPOINT ["/usr/local/bin/fleet"]
EOF
podman build -t localhost/fleet:dev -f /tmp/Containerfile.fleet .
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
   (Calico, Cilium, EKS VPC CNI with the network policy agent, …) makes it
   real. Verify from a sealed sandbox:
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
5. **Registry, not build-on-box.** Both images must live in a registry the
   nodes pull from; a `localhost/` tag cannot work outside kind. Set
   `sandbox.kubernetes.imagePullSecret` for private registries.
6. **Hypervisor isolation** (optional): install Kata Containers on the sandbox
   nodes, create a `kata` RuntimeClass, set
   `sandbox.kubernetes.runtimeClass=kata`. Preflighted fail-closed, mirroring
   `FLEET_SANDBOX_RUNTIME` (ADR-0010). Note `FLEET_SANDBOX_RUNTIME` itself is
   a podman knob and is **refused** under this backend.
7. **Scheduled jobs.** systemd timers don't exist here; run the equivalents as
   CronJobs — daily `fleet backup --db=all --prune` and daily `fleet cleanup`
   (see [TIMERS.md](TIMERS.md) and [MAINTENANCE.md](MAINTENANCE.md)).
8. **One replica.** Do not add an HPA or `replicas: 2` for the control plane.
   Scale work by raising `FLEET_MAX_CONCURRENT_AGENTS` and giving the sandbox
   namespace more node capacity; scale the control plane vertically.
9. **Run `fleet validate-config`** (e.g. `kubectl exec` into the control-plane
   pod) after any config change: it runs the same fail-closed preflight boot
   does, plus everything else the verb checks.

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

The shared sandbox knobs apply to both backends: `FLEET_SANDBOX_IMAGE`,
`FLEET_SANDBOX_MEMORY` / `_CPUS` (converted to pod resource limits),
`FLEET_SANDBOX_DISK_GB` (the pod's ephemeral-storage limit),
`FLEET_SANDBOX_WARM_SIZE` / `_WARM_TTL` (the warm pool holds pre-started
pods), and the python REPL knobs.

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
- **No per-sandbox resource telemetry (#263).** `podman stats` has no
  in-process counterpart here; task resource summaries are absent. Use your
  cluster's metrics stack on the `fleet-sandbox` pods.
- **The bundled seccomp profile does not apply.** Pods run `RuntimeDefault`,
  or a profile you install on the nodes yourself via
  `FLEET_SANDBOX_K8S_SECCOMP_PROFILE`. Setting the podman
  `FLEET_SANDBOX_SECCOMP_PROFILE` under this backend refuses to boot rather
  than being silently ignored.
- **Supporting-doc bind mounts don't apply.** The podman backend bind-mounts
  persona/protocol dirs same-path into containers; a pod only mounts the
  workspace claim. In-sandbox reads of those host paths degrade exactly like
  the podman missing-dir case.
- **Disk quota is per-pod ephemeral storage**, which caps the writable layer
  and scratch emptyDirs — a *stronger* cap than podman's per-file ulimit — but
  the workspace claim is still unbounded by it, same as the bind mount is
  under podman: many files still add up.
- **Warm-pool pods hold cluster resources while parked.** Requests equal
  limits; size `FLEET_SANDBOX_WARM_SIZE` accordingly.
- **kind e2e is a documented walkthrough, not a CI job.** CI lints and
  template-renders the chart (`helm` job) and unit-tests the backend against a
  fake apiserver (including exec streaming and the poison path); it does not
  stand up a cluster.
