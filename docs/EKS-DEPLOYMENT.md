# Deploying fleet on Amazon EKS (one pod, one big node)

> Operator recipe for organizations whose platform standard is Kubernetes. It
> keeps fleet's single-process, single-node model intact — one pod on one large
> node, scaled vertically — and does **not** try to spread the Podman sandboxes
> across worker nodes. For the supported single-host install see
> [`docs/DEPLOYMENT.md`](DEPLOYMENT.md).

## Read this first (scope, honesty, and what is not shipped)

- **Check the first-class Kubernetes path first.** Since
  [ADR-0049](adr/0049-kubernetes-backend-split-control-plane.md) fleet ships a
  Helm chart (`deploy/helm/fleet`) and a kubernetes sandbox backend
  (`FLEET_SANDBOX_BACKEND=kubernetes`) that runs sandboxes as ephemeral pods —
  no Podman on the node, no privileged pod. That path, documented in
  [`DEPLOYMENT-KUBERNETES.md`](DEPLOYMENT-KUBERNETES.md), supersedes this
  recipe for most clusters. **This guide remains** for the co-located
  topology it describes — one pod on one big node with rootless Podman
  *inside* it, the podman backend unchanged — which some operators still want
  (e.g. to keep the bundled seccomp profile, per-sandbox telemetry, and the
  allowlisted egress proxy, none of which the kubernetes backend provides).
- **fleet's shipped default deployment stays a single VM under systemd**
  ([ADR-0004](adr/0004-single-box-vm-native-deployment.md)); the single-box
  decision stands. This co-located-EKS recipe is hand-verified, **not
  CI-exercised** (CI lints the Helm chart and unit-tests the kubernetes
  backend, but stands up no cluster) — everything below you own and must
  validate on your own cluster.
- **No fleet container image ships either.** `deploy/` contains systemd units, not
  images. You build two images yourself (§3): the fleet runtime image (Go binary
  **+ Podman inside it**) and the Next.js web image. The sandbox image stays what
  it already is — a per-client *bundle* artifact.
- **One pod. One replica. Forever.** Scheduled-task crash recovery uses
  single-owner database leases and the concurrency cap is a per-process
  semaphore. Two fleet pods against one pair of databases is a **correctness
  bug**, not a capacity increase. No `Deployment` with rolling updates, no HPA,
  no `replicas: 2`.
- **In this recipe, the sandbox stays local to the process.** Every agent tool
  call's data plane runs in a rootless-Podman container that `agentcore` starts
  and `podman exec`s into on the same host as the run loop
  ([ADR-0002](adr/0002-mandatory-rootless-podman-sandbox.md)); the remote worker
  registry was deliberately removed
  ([ADR-0011](adr/0011-remove-worker-node-registry.md)). "Put the runners on
  their own node group" IS now a configuration — but it is the
  `FLEET_SANDBOX_BACKEND=kubernetes` path in
  [`DEPLOYMENT-KUBERNETES.md`](DEPLOYMENT-KUBERNETES.md), not this one. This
  guide keeps the podman backend and runs Podman **inside the fleet pod**,
  which is why the pod needs the privileges in §2.
- **What you gain** by doing this at all: your existing ECR/IRSA/ALB/Secrets
  Manager/observability plumbing, one node group to patch, and node-failure
  rescheduling. **What you give up** versus the systemd path: `bootstrap.sh`,
  `fleet update`, and `scripts/doctor.sh` all assume a systemd host — you replace
  them with image rebuilds and `kubectl exec` (§9).

## The objections a Kubernetes-native reviewer will raise

Answer these before the design review, not during it. Each links to the section
that implements it.

| "This isn't Kubernetes-native because…" | Answer |
|---|---|
| "…there's no Helm chart / it's not GitOps" | There is one now — `deploy/helm/fleet` — but it installs the **split control-plane/runner** topology ([`DEPLOYMENT-KUBERNETES.md`](DEPLOYMENT-KUBERNETES.md)), not this co-located one. If you specifically want this recipe, package the §7 manifests as Kustomize or a thin chart and sync with Argo CD or Flux — [§7 GitOps](#packaging-these-manifests-for-gitops). Two Argo-specific gotchas are called out there. |
| "…a privileged pod will never pass admission" | It won't under `restricted`/`baseline` Pod Security Standards. You need a labelled namespace and a scoped policy exception — [§7 admission control](#namespace-admission-control-and-identity). If your org forbids privileged pods outright, [§2](#if-your-policy-forbids-privileged-pods) is the unprivileged variant and its costs. |
| "…one replica isn't highly available" | Correct, and it cannot be: single-owner task leases + a per-process semaphore. HA here means fast, *graceful* recovery, not zero downtime — [§6 availability](#az-pinning-node-loss-and-what-ha-means-here) states the RTO/RPO plainly. |
| "…we can't autoscale it" | Scale vertically: raise `FLEET_MAX_CONCURRENT_AGENTS` and the pod resources together ([§6](#resource-requests-count-the-sandboxes)). HPA and VPA are both actively harmful here ([§8](#cluster-integration-gotchas)). |
| "…the workloads are invisible to the cluster" | True and worth naming: sandboxes are Podman containers inside the pod, so they never appear in `kubectl get pods` or cAdvisor. Where to see them instead: [§8](#cluster-integration-gotchas). |
| "…it pins itself to one AZ" | It does — `ReadWriteOnce` EBS. Make the node group single-AZ deliberately rather than discovering it during an incident ([§6](#az-pinning-node-loss-and-what-ha-means-here)). |
| "…NetworkPolicy can't govern what the agent runs" | It can. Sandbox egress traverses the pod's network namespace via the rootless network helper, so pod-level NetworkPolicy applies to agent-executed code too ([§7](#networkpolicy), [§8](#cluster-integration-gotchas)). |
| "…secrets are in a `Secret`" | Swap in External Secrets Operator or the Secrets Store CSI driver ([§7](#secrets)). fleet's own guarantee is stronger than either: MCP credentials are brokered host-side and never enter a sandbox. |
| "…nothing here is CI-tested" | Also true. Run [§10](#10-verification-checklist) as an acceptance gate in your own pipeline; that is the substitute. |

## 1. Topology

Everything that was a process on the single box becomes a container in **one
pod**, so the loopback wiring the code expects still holds (containers in a pod
share a network namespace, so `127.0.0.1:8080` from the web container reaches the
chat listener):

```
                       ┌──────────────── EKS node (dedicated, one big instance) ───────────────┐
 Internet ─TLS─▶ ALB ──┼─▶ Service :3000 ─▶ pod                                                │
   (ACM cert)          │      ┌───────────────────────────────────────────────────────────┐    │
                       │      │ container: web      Next.js, 0.0.0.0:3000                 │    │
                       │      │    │ server-side proxy over loopback                      │    │
                       │      │    ├─▶ 127.0.0.1:8080  chat      ┐                         │    │
                       │      │    └─▶ 127.0.0.1:8000  orchestr. ┘ container: fleet        │    │
                       │      │                                     (one process:          │    │
                       │      │                                      chat + orchestrator    │    │
                       │      │                                      + scheduler + pool)    │    │
                       │      │                                        │ podman (rootless,  │    │
                       │      │                                        │ in-container)      │    │
                       │      │                                        ├─▶ sandbox ctr 1    │    │
                       │      │                                        ├─▶ sandbox ctr 2    │    │
                       │      │                                        └─▶ … up to          │    │
                       │      │                                   FLEET_MAX_CONCURRENT_AGENTS│   │
                       │      └───────────────────────────────────────────────────────────┘    │
                       └──────────────────────────────────────────────────────────────────────┘
                                     │
                                     └─▶ RDS PostgreSQL (two databases: chat + sched)
```

The Go listeners stay **loopback-only**. The orchestrator in particular is
impersonation-load-bearing and must remain on `127.0.0.1` — do not bind it to the
pod IP (see §8 for how metrics scraping works without breaking that).

## 2. The hard part: rootless Podman inside a pod

The sandbox is mandatory and fails closed, so the pod must be able to run
rootless Podman. Concretely fleet shells out to
`podman run --userns=keep-id:uid=1000,gid=1000 --read-only --cap-drop=ALL
--security-opt=no-new-privileges --security-opt seccomp=… --memory=… --cpus=…
--pids-limit=… …` and then `podman exec`s each tool call into it. Network posture
is per-turn: normal turns pass **no** `--network` flag (podman's rootless default
— pasta on ≥ 5.0, slirp4netns before it), lockdown and scheduled runs get
`--network=none`, and the allowlisted-egress posture explicitly requests
`--network=slirp4netns:allow_host_loopback=true`. That needs, inside the fleet
container:

| Requirement | Why | How |
|---|---|---|
| `/etc/subuid` + `/etc/subgid` ranges for the container's user | `--userns=keep-id` maps uids into the range; without it Podman fails with a `newuidmap` mapping error | baked into the image (§3) |
| `newuidmap`/`newgidmap` with their file capabilities intact | performs the uid/gid mapping | `shadow-utils` in the image **and** `allowPrivilegeEscalation: true` (file caps are neutralized by `NoNewPrivileges` — the same reason `deploy/fleet.service` sets `NoNewPrivileges=no`) |
| a writable, **persistent** graph root (`$HOME/.local/share/containers`) | holds the ~1.5 GB sandbox image + per-container writable layers | the PVC mounted at `/var/lib/fleet` (§5) |
| an overlay-capable storage driver | `vfs` copies the whole ~1.5 GB image per container start — fatal for a per-turn warm pool | native `overlay` (privileged) or `fuse-overlayfs` + `/dev/fuse` |
| `/dev/net/tun` | the rootless network helper: **pasta** on Podman ≥ 5.0 (podman's own default, used by normal turns), or **slirp4netns**, which the allowlisted-egress posture specifically requires | privileged, or a device plugin |
| a **writable cgroup subtree** | otherwise Podman silently ignores `--memory`/`--cpus`, so the per-sandbox caps and per-task `sandbox_limits` **do not bind** | privileged (rw `/sys/fs/cgroup`); the analogue of `Delegate=yes` in the systemd unit |
| cgroup **v2** on the node | project-quota/limit behavior above | Amazon Linux 2023 nodes default to cgroup v2 |

### Recommendation: run the fleet container privileged on a dedicated node

```yaml
securityContext:
  privileged: true
  allowPrivilegeEscalation: true
  runAsUser: 1000        # the image's fleet user, NOT root
  runAsGroup: 1000
```

**The container must run as uid 1000, not root — even privileged.** Podman
running as real root is *rootful*, and rootful Podman **ignores**
`--userns=keep-id`. The sandbox's uid 1000 then no longer maps to the process
that owns the workspace directory, so the agent can neither `chdir` into its
per-conversation workspace nor write files there — the failure the
`keep-id`/same-path invariant tests in `internal/sandbox` exist to catch. Keep
`runAsUser: 1000` and give the image a fixed uid-1000 user with subuid ranges
(§3b).

This is the configuration that reliably satisfies all six rows above. It is also
the honest trade: **a privileged container is not a security boundary**, so the
security model becomes "the *node* is the blast radius, and the pod owns it."
That is materially weaker than the systemd deployment, where the fleet process is
an unprivileged system user. Mitigate deliberately:

- **Dedicate the node group to fleet** — taint it and schedule nothing else there
  (§6). Never co-schedule other tenants' workloads.
- **Block IMDS from pods** on that node group
  (`--metadata-options http-put-response-hop-limit=1`) so agent-executed code
  cannot mint the node role's credentials. Give fleet its own IRSA role with only
  what it needs (ECR pull; Secrets Manager read if you use it).
- **Give the node role the minimum**, and keep the cluster's own secrets out of
  the namespace.
- **NetworkPolicy** on the namespace: ingress only from the ALB target group,
  egress only to RDS, your model provider, and the MCP endpoints you intend.
- The **inner** hardening is unchanged and still does the real work per turn:
  read-only rootfs, `--cap-drop=ALL`, no-new-privileges, the default-deny seccomp
  profile, `--network=none` for lockdown/scheduled runs, per-container
  memory/CPU/pid caps, and the credential broker keeping MCP secrets host-side
  (they never enter a sandbox — [ADR-0003](adr/0003-host-side-mcp-credential-brokering.md)).

### If your policy forbids privileged pods

The unprivileged variant needs, at minimum, `SYS_ADMIN` plus device access to
`/dev/fuse` and `/dev/net/tun` (a device plugin such as smarter-device-manager,
because containerd's default device cgroup denies both), `fuse-overlayfs` as the
driver, and a `RuntimeDefault` seccomp profile that permits `unshare`/`clone`
with `CLONE_NEWUSER`. Expect to fight the cgroup-delegation row above — and
**verify that `--memory` actually binds** (§10) rather than assuming it, because
if it doesn't, a `pandas` job takes the whole pod down instead of one sandbox.

`kata`/`libkrun` microVM runtimes ([`docs/SANDBOX-RUNTIMES.md`](SANDBOX-RUNTIMES.md))
need read-write `/dev/kvm` inside the pod. On EC2 that means a `.metal` instance
(nested KVM is not exposed on normal instance types), plus device access. Fleet's
boot preflight is fail-closed, so a missing `/dev/kvm` aborts startup rather than
silently downgrading to a shared kernel. Leave `sandbox.runtime` at the default
unless you have committed to metal nodes.

## 3. Build the images

### 3a. Sandbox image → ECR

Unchanged from the single-host path: the Containerfile is a **bundle** artifact
(`<bundle>/sandbox/Containerfile`), and fleet **never builds it at startup**.
Build and push it in CI — the repo already ships the reusable workflow
`.github/workflows/publish-sandbox-image.yml` (`workflow_call`) for exactly this,
which builds with `scripts/build-sandbox-image.sh` and pushes an immutable
`{git-sha}` tag. It exposes the pushed `image_ref` and `image_digest` as workflow
outputs, so a deploy job can consume the exact digest this section wants without
re-deriving it. Point it at ECR instead of GHCR, or mirror the GHCR tag into ECR.

**Pull credentials depend on the package's visibility**, which in this org
follows the publishing repo (measured 2026-08-20): the images from the public
`fleet` and `example-config` repos are anonymously pullable, so a cluster needs
no `imagePullSecret` for them; the client-bundle images come from private repos
and do. GitHub's docs describe a private-by-default that these packages did not
follow, so verify a new package's visibility rather than assuming — an image you
expect to pull anonymously failing with a 403 at pod start is the symptom.

The workflow publishes but does **not** pin: adoption is the explicit step
below. (It used to open a PR pinning `sandbox.image` in the client repo; that
step never once succeeded and was removed on 2026-08-20 — see the reusable
workflow's header.)

Then set `sandbox.image` in the bundle's `manifest.yaml` to the immutable ref, or
override it per deployment with `FLEET_SANDBOX_IMAGE`
(`<acct>.dkr.ecr.<region>.amazonaws.com/fleet-sandbox@sha256:…`). Pin by digest —
`:latest` in a rebuilt-nightly registry means a turn's execution environment can
change under you.

### 3b. fleet runtime image

Podman lives in this image. A Fedora base keeps you on the same `crun`/
`slirp4netns`/`fuse-overlayfs` versions the project develops against:

```dockerfile
# Containerfile.fleet — build with: podman build -f Containerfile.fleet -t <ecr>/fleet:<sha> .
FROM golang:1.27 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN make build            # → ./fleet and ./fleet-admin

FROM fedora:44
# podman + the rootless stack fleet actually invokes; curl for the exec probes (§7).
# Install BOTH rootless network helpers: passt/pasta is podman >= 5.0's default
# (normal turns), and slirp4netns is required by the allowlisted-egress posture —
# where a missing binary now aborts boot with a fail-closed preflight rather than
# erroring on every turn. awscli2 is for the ECR-login init container (§7); curl
# is for the exec probes.
RUN dnf install -y --setopt=install_weak_deps=False \
      podman crun conmon passt slirp4netns fuse-overlayfs containers-common \
      shadow-utils catatonit iptables-nft git curl ca-certificates awscli2 \
 && dnf clean all
# Fixed unprivileged user, uid 1000 — matches --userns=keep-id:uid=1000 and the
# sandbox image's USER, so bind-mounted workspace files line up from both sides.
RUN useradd --uid 1000 --home-dir /var/lib/fleet --shell /sbin/nologin fleet \
 && echo 'fleet:100000:65536' > /etc/subuid \
 && echo 'fleet:100000:65536' > /etc/subgid
# Same rootless-Podman settings scripts/bootstrap.sh writes for the service user:
# cgroupfs avoids needing a systemd user D-Bus session; the file events logger
# avoids journald permissions. fleet also passes --cgroup-manager=cgroupfs itself.
RUN install -d -o fleet -g fleet -m 0755 /etc/containers \
 && printf '[engine]\ncgroup_manager = "cgroupfs"\nevents_logger = "file"\n' \
      > /etc/containers/containers.conf
COPY --from=build /src/fleet /usr/local/bin/fleet
# The client config bundle. Bake it in for an immutable deploy (recommended) or
# mount it from a PVC/initContainer git clone; either way it must be WRITABLE by
# uid 1000, because the sandbox bind-mounts bundle dirs with SELinux relabeling.
COPY --chown=fleet:fleet config/default /opt/fleet/client
USER fleet
ENV HOME=/var/lib/fleet \
    XDG_RUNTIME_DIR=/var/lib/fleet/run \
    FLEET_CLIENT_CONFIG_DIR=/opt/fleet/client
WORKDIR /var/lib/fleet
# `fleet serve` is the explicit server verb (bare `fleet` also serves).
ENTRYPOINT ["/usr/local/bin/fleet", "serve"]
```

Notes that matter:

- **`XDG_RUNTIME_DIR` must be writable and on the PVC-or-emptyDir**, not on the
  read-only image layer — it holds per-container runtime state.
- **`FLEET_ENV_FILE` is a real choice, not a leftover.** With config injected as
  pod env (the manifests below), leave it unset — but know the consequence:
  config hot-reload re-reads the **env file** and honors boot's
  process-env-over-file precedence, so anything pinned in the pod's environment
  is **fixed until the pod restarts**, and `SIGUSR2` /
  `POST /admin/reload-config` will report it under `skipped`. That is the right
  trade for immutable-config deployments. If you want the reload path
  ([`docs/CONFIG-RELOAD.md`](CONFIG-RELOAD.md)) to work, mount the credential
  file from a Secret instead (e.g. `/etc/fleet/fleet.env`), point
  `FLEET_ENV_FILE` at it, and do **not** also inject those keys as env vars.
- **Leave `FLEET_LOG_FILE` unset** so the process log goes to stdout/stderr for
  your normal cluster log pipeline.
- Keep the bundle **writable by uid 1000** — the sandbox mounts `protocols/`,
  `personas/`, `skills/`, and `system_prompts/` with `:Z`, which needs to write
  the `security.selinux` xattr. This is the same reason `deploy/fleet.service`
  lists `/opt/fleet/client` in `ReadWritePaths`.

### 3c. web image

```dockerfile
FROM node:24 AS build
WORKDIR /app
COPY web/package*.json ./
RUN npm ci
COPY web/ .
RUN npm run build
FROM node:24-slim
WORKDIR /app
COPY --from=build /app ./
ENV NODE_ENV=production PORT=3000
USER node
CMD ["npm", "run", "start"]
```

## 4. PostgreSQL

Use RDS (or Aurora PostgreSQL). fleet needs **two databases** in the same
cluster — chat and sched are deliberately separate
([ADR-0005](adr/0005-separate-chat-and-sched-databases.md)) — and each service
**self-migrates on first start**, so create empty databases and roles only:

```sql
CREATE ROLE chat  LOGIN PASSWORD '…';  CREATE DATABASE chat  OWNER chat;
CREATE ROLE sched LOGIN PASSWORD '…';  CREATE DATABASE sched OWNER sched;
```

```
FLEET_CHAT_DATABASE_URL=postgres://chat:…@<rds-endpoint>:5432/chat?sslmode=require
FLEET_SCHED_DATABASE_URL=postgres://sched:…@<rds-endpoint>:5432/sched?sslmode=require
```

Use `sslmode=require` or stricter (`verify-full` with the RDS CA bundle mounted).
Both pools are **critical readiness checks** — if either is down, `/readyz`
returns 503, which is exactly the signal you want the ALB to see. Tune
`FLEET_CHAT_DB_MAX_CONNS` / `FLEET_SCHED_DB_MAX_CONNS` against the instance
class's connection limit. `fleet migrate status` (via `kubectl exec`) reports
applied vs pending migrations; see [`docs/MIGRATIONS.md`](MIGRATIONS.md).

Running Postgres in-cluster works but buys you a second stateful single-writer
workload to babysit; managed is the better trade here, and it lowers the pod's
base footprint.

## 5. Storage

One `ReadWriteOnce` EBS volume mounted at `/var/lib/fleet` carries **everything
stateful in the pod**: the rootless Podman graph root, the per-conversation
workspaces, the data dir (attachments/uploads, audit), and `XDG_RUNTIME_DIR`.

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: fleet-gp3-xfs
provisioner: ebs.csi.aws.com
parameters:
  type: gp3
  iops: "6000"
  throughput: "500"
  fsType: xfs           # xfs + prjquota is what makes --storage-opt size work
mountOptions:
  - prjquota
allowVolumeExpansion: true
volumeBindingMode: WaitForFirstConsumer
```

**Why `xfs` + `prjquota`:** the two disk caps are **layered, not either/or**. A
per-file `--ulimit fsize` cap is applied on every container regardless of
filesystem, and it is what bounds writes to the workspace bind mount. On top of
that, fleet adds `--storage-opt size=…` — a hard **total** cap on the writable
layer — but Podman only accepts that on a quota-capable driver (overlay+xfs with
pquota, btrfs, zfs — **not** overlay+ext4, and not vfs). fleet probes this once at
boot; where it isn't supported, the total-size cap is simply omitted and an agent
can still fill the writable layer with many individually-legal files. The omission
is logged at startup. Making the probe succeed is the point of this StorageClass.

Sizing: the sandbox image (~1.5 GB) + one writable layer per concurrent sandbox
(`FLEET_SANDBOX_DISK_GB`, default 5 GiB each) + persistent workspaces + uploads
(`FLEET_UPLOAD_MAX_BYTES`, default 1 GiB per file). Start from the disk column of
the sizing table in [`docs/DEPLOYMENT.md`](DEPLOYMENT.md#choosing-a-host-sizing)
and add your workspace retention. `allowVolumeExpansion` matters — a full volume
is an outage.

*Optional:* on an instance with local NVMe you can put the graph root on the
instance store (a `hostPath` plus a `storage.conf` `graphroot`) and keep only
workspaces/data on the PVC. Images and warm-container layers are reconstructible,
so ephemeral is fine for them, and you get much faster container starts. Verify
the quota probe still passes on that filesystem.

## 6. Node group and scheduling

One dedicated managed node group, one instance, nothing else on it:

```
eksctl create nodegroup --cluster <c> --name fleet \
  --node-type m7i.12xlarge --nodes 1 --nodes-min 1 --nodes-max 1 \
  --node-zones <single-az> \
  --node-taints dedicated=fleet:NoSchedule \
  --node-labels workload=fleet \
  --node-volume-size 200 --node-volume-type gp3 \
  --metadata-options httpPutResponseHopLimit=1
```

- **AMI: Amazon Linux 2023** (cgroup v2 by default, ordinary writable
  containerd). Bottlerocket and other minimal/immutable AMIs are **untested for
  nested Podman** here — if you must, prove out §10's checks first. If your nodes
  enforce SELinux, confirm the `:z`/`:Z` relabels the sandbox performs actually
  succeed; AL2023's permissive default is what this recipe assumes.
- **Instance family:** memory-per-vCPU is the binding constraint (~1.5–3 GB of
  RAM per concurrent agent for `pandas`/`matplotlib` work), so `r7i`/`r7a` beats
  `c7i`. Size from the arithmetic in the next subsection, not from the vCPU
  count: the 32-agent worked example used below and in the appendix needs
  ~36 vCPU / 72 GiB once the base and web tier are counted, so **`m7i.12xlarge`
  (48 vCPU / 192 GiB)** fits it with headroom while a 32-vCPU instance is already
  short. Step up to `r7i.12xlarge` (48/384) if you intend to raise
  `FLEET_SANDBOX_MEMORY` to 4–8 GiB for heavy `pandas`/`matplotlib` work, and to
  `r7i.24xlarge` (96/768) for `FLEET_MAX_CONCURRENT_AGENTS=64` at those per-agent
  sizes. Don't buy the memory before you've raised the per-sandbox cap that would
  use it — the default is 512 MiB.
- **`maxPods`:** the sandboxes are Podman containers *inside* the pod, so they
  don't consume pod IPs or count against `maxPods`. Only fleet's own pod does.
- **Karpenter:** annotate the pod `karpenter.sh/do-not-disrupt: "true"`. Node
  consolidation on a single stateful pod means unplanned restarts.
- **Cluster Autoscaler / HPA:** neither applies. Do not attach an HPA.
- **PodDisruptionBudget:** don't set a blocking one (`minAvailable: 1` on a
  single-replica workload blocks node drains indefinitely). A single-pod
  deployment means node replacement is a **planned downtime window** — the honest
  consequence of the single-writer design, same as rebooting the single box.

### AZ pinning, node loss, and what "HA" means here

Decide this deliberately — it is the question your reviewer will press hardest on.

- **The pod is pinned to one Availability Zone.** A `ReadWriteOnce` EBS volume
  exists in exactly one AZ, and `WaitForFirstConsumer` binds it where the pod
  first scheduled. If your node group spans AZs, a replacement node in a
  different AZ **cannot** mount the volume and the pod stays `Pending` with a
  volume-node-affinity conflict. Make the node group **single-AZ on purpose** so
  a replacement node always lands where the volume is. (EFS as an alternative
  gets you cross-AZ at the cost of a network filesystem under the Podman graph
  root and per-conversation workspaces — don't.)
- **Node loss does not self-heal quickly.** When a node goes `NotReady`, a
  StatefulSet pod is *not* recreated until the old one is confirmed gone —
  Kubernetes will not risk two writers, which is the same invariant fleet needs.
  Recovery is: the node object is deleted (or you `kubectl delete pod --force`),
  the EBS volume detaches, and the new pod attaches it on a fresh node in the
  same AZ. Budget minutes, and prefer letting the node group replace the instance
  over force-deleting by hand.
- **What HA actually means for this workload:** RTO is one pod restart plus
  volume reattach; RPO for conversations, tasks, and run history is your RDS
  backup window (in-flight turns are lost, and the graceful drain is what keeps
  that number near zero for planned restarts). There is no zero-downtime rolling
  upgrade, on EKS or on the single box — that is a property of the single-writer
  design, not of this recipe.
- **PodDisruptionBudget:** leave it unset, or `maxUnavailable: 1`. A
  `minAvailable: 1` PDB on a one-replica workload blocks every node drain
  indefinitely and will page someone at 3am during a routine AMI upgrade.

### Resource requests: count the sandboxes

The sandbox containers' cgroups nest **under the pod's cgroup**, so their memory
and CPU count against the pod's limits. Size the pod, not just the process:

```
pod limit ≈ base (2 vCPU / 6 GB: Go process + Next app)
          + FLEET_MAX_CONCURRENT_AGENTS × (FLEET_SANDBOX_CPUS, FLEET_SANDBOX_MEMORY)
          + headroom
```

With `FLEET_MAX_CONCURRENT_AGENTS=32` and per-sandbox caps of `2g`/`1.0` CPU:
≈ 34 vCPU and ≈ 70 GB. Set **`requests == limits`** (Guaranteed QoS) and leave
real headroom: if the pod cgroup hits its memory limit, the kernel OOM killer
picks the biggest process in the cgroup — which can be the fleet process itself,
turning one runaway sandbox into a full restart. Raise the per-sandbox ceilings
(`FLEET_SANDBOX_MEMORY`, `FLEET_SANDBOX_CPUS`, `FLEET_SANDBOX_PIDS`, and the
operator maxima `FLEET_SANDBOX_{MEMORY_MAX_MB,CPUS_MAX,PIDS_MAX}`) and the pod
limits **together** — the defaults are 512 MiB / 1.0 CPU / 128 pids per sandbox,
and heavy analysis workloads get OOM-killed against that default long before your
node runs out of RAM.

## 7. Manifests

### Namespace, admission control, and identity

**A privileged pod is rejected outright under the `baseline` or `restricted` Pod
Security Standards.** This is the single most likely reason a first deploy fails
in a governed cluster, and it fails at admission with no pod to debug. Label the
namespace so PSA permits it, and keep the exception scoped to this one namespace:

```yaml
apiVersion: v1
kind: Namespace
metadata:
  name: fleet
  labels:
    # Required for the privileged fleet container (§2). Scope the exception to
    # THIS namespace; do not relax the cluster-wide default.
    pod-security.kubernetes.io/enforce: privileged
    pod-security.kubernetes.io/enforce-version: latest
    # Keep the warnings visible so you can see exactly which controls you gave up.
    pod-security.kubernetes.io/audit: baseline
    pod-security.kubernetes.io/warn: baseline
    # AWS Load Balancer Controller: hold the pod un-Ready until the ALB target is
    # registered, so a restart doesn't briefly 5xx (§7 ingress).
    elbv2.k8s.aws/pod-readiness-gate-inject: enabled
```

If you run **Kyverno** or **Gatekeeper** as well, PSA labels are not enough —
those policies evaluate independently. Add a narrowly-scoped exception (namespace
`fleet`, the `fleet` StatefulSet, the specific rules: privileged,
`allowPrivilegeEscalation`, host devices) rather than a blanket exemption, and
write the §2 mitigations into the exception's justification field so the next
auditor finds the reasoning instead of just the hole.

Identity: **fleet needs no Kubernetes API access at all** — nothing in the
process talks to the API server. Its ServiceAccount exists only to carry an AWS
role for ECR pulls (and Secrets Manager, if you use it), so it gets **no Role or
RoleBinding**, which is a useful thing to be able to say in review:

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: fleet
  namespace: fleet
  annotations:
    # IRSA. EKS Pod Identity (eks-pod-identity-agent + a PodIdentityAssociation)
    # is the newer equivalent and avoids the OIDC-trust-policy boilerplate; use
    # whichever your platform standardizes on. Neither needs IMDS, which is why
    # the §6 hop-limit-1 hardening is safe.
    eks.amazonaws.com/role-arn: arn:aws:iam::<acct>:role/fleet
# No RBAC Role/RoleBinding: fleet makes zero Kubernetes API calls.
automountServiceAccountToken: false
```

The attached IAM policy needs only `ecr:GetAuthorizationToken`,
`ecr:BatchGetImage`, `ecr:GetDownloadUrlForLayer`, and
`ecr:BatchCheckLayerAvailability` on the two repositories — plus
`secretsmanager:GetSecretValue` on your specific secret ARNs if you use External
Secrets with this role.

### Secrets

The literal `Secret` below is the minimum. For a GitOps repo, replace it with an
`ExternalSecret` (External Secrets Operator) or a `SecretProviderClass` (Secrets
Store CSI driver) pointing at Secrets Manager or Parameter Store — the pod spec
is unchanged either way, since both project a normal `Secret`. Note that rotating
these takes effect on **pod restart** unless you use the env-file mount described
in §3b.

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: fleet-env
  namespace: fleet
stringData:
  OPENROUTER_API_KEY: "…"
  FLEET_CHAT_DATABASE_URL: "postgres://chat:…@rds:5432/chat?sslmode=require"
  FLEET_SCHED_DATABASE_URL: "postgres://sched:…@rds:5432/sched?sslmode=require"
  FLEET_SERVER_TOKEN: "…"          # web container's CHAT_SERVER_TOKEN must match
  ADMIN_API_KEY: "…"               # orchestrator admin key
  APP_SESSION_SECRET: "…"          # signs the web session cookie
  # plus the MCP connector credentials the bundle's manifest names
```

The workload — a `StatefulSet`, because it gives at-most-one-pod semantics with
an `RWO` volume (a `Deployment`'s rolling update would briefly run two fleet
processes against one database pair, which the single-owner leases forbid):

```yaml
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: fleet
  namespace: fleet
spec:
  replicas: 1                      # never raise this
  serviceName: fleet
  podManagementPolicy: OrderedReady
  updateStrategy:
    type: RollingUpdate            # for a single replica: terminate, then create
  selector:
    matchLabels: { app: fleet }
  template:
    metadata:
      labels: { app: fleet }
      annotations:
        karpenter.sh/do-not-disrupt: "true"
    spec:
      serviceAccountName: fleet
      # A freshly provisioned EBS volume is root-owned; without fsGroup the
      # uid-1000 process cannot create the Podman graph root, the workspace, or
      # XDG_RUNTIME_DIR, and the pod crash-loops on startup. OnRootMismatch keeps
      # restarts fast once the volume is large (no full recursive rechown).
      securityContext:
        fsGroup: 1000
        fsGroupChangePolicy: OnRootMismatch
      nodeSelector: { workload: fleet }
      tolerations:
        - key: dedicated
          value: fleet
          effect: NoSchedule
      # Must exceed FLEET_SHUTDOWN_GRACE_SECONDS: on SIGTERM fleet stops
      # admitting work, flips /healthz + /readyz to 503 so the ALB drains it,
      # then drains in-flight chat turns AND scheduled tasks before exiting.
      terminationGracePeriodSeconds: 90

      initContainers:
        # Pre-pull the sandbox image into the rootless store on the PVC. fleet
        # never builds it and this keeps the first turn off a 1.5 GB download
        # (and keeps a registry outage from surfacing as a failed turn).
        - name: pull-sandbox
          image: <acct>.dkr.ecr.<region>.amazonaws.com/fleet:<sha>
          command: ["/bin/sh", "-c"]
          args:
            - |
              set -e
              aws ecr get-login-password --region "$AWS_REGION" \
                | podman login --username AWS --password-stdin "${ECR_REGISTRY}"
              podman pull "$FLEET_SANDBOX_IMAGE"
          env:
            - { name: AWS_REGION,  value: "<region>" }
            - { name: ECR_REGISTRY, value: "<acct>.dkr.ecr.<region>.amazonaws.com" }
            - { name: FLEET_SANDBOX_IMAGE, value: "<acct>.dkr.ecr.<region>.amazonaws.com/fleet-sandbox@sha256:…" }
            - { name: HOME, value: /var/lib/fleet }
            - { name: XDG_RUNTIME_DIR, value: /var/lib/fleet/run }
          securityContext:
            privileged: true       # same reasons as the main container (§2)
            runAsUser: 1000
          volumeMounts:
            - { name: state, mountPath: /var/lib/fleet }

      containers:
        - name: fleet
          image: <acct>.dkr.ecr.<region>.amazonaws.com/fleet:<sha>
          envFrom:
            - secretRef: { name: fleet-env }
          env:
            # Listeners stay loopback — the web container reaches them through
            # the shared pod network namespace. The orchestrator MUST stay on
            # 127.0.0.1 (it is impersonation-load-bearing).
            - { name: FLEET_SERVER_ADDR,       value: "127.0.0.1:8080" }
            - { name: FLEET_ORCHESTRATOR_ADDR, value: "127.0.0.1:8000" }
            - { name: FLEET_CLIENT_CONFIG_DIR, value: "/opt/fleet/client" }
            # Absolute, not CWD-relative: don't depend on WORKDIR surviving an
            # image refactor.
            - { name: FLEET_DATA_DIR,       value: "/var/lib/fleet/data" }
            - { name: FLEET_WORKSPACE_ROOT, value: "/var/lib/fleet/workspace" }
            - { name: HOME,            value: "/var/lib/fleet" }
            - { name: XDG_RUNTIME_DIR, value: "/var/lib/fleet/run" }
            - { name: FLEET_SANDBOX_IMAGE, value: "<acct>.dkr.ecr.<region>.amazonaws.com/fleet-sandbox@sha256:…" }
            # Sizing knobs — keep in lockstep with the pod resources below (§6).
            - { name: FLEET_MAX_CONCURRENT_AGENTS, value: "32" }
            - { name: FLEET_SANDBOX_MEMORY,        value: "2g" }
            - { name: FLEET_SANDBOX_CPUS,          value: "1.0" }
            - { name: FLEET_SANDBOX_WARM_SIZE,     value: "4" }
            - { name: FLEET_SHUTDOWN_GRACE_SECONDS, value: "60" }
            - { name: FLEET_TIMEZONE, value: "UTC" }
            # Trust the ALB's X-Forwarded-For only from in-pod/in-VPC hops.
            - { name: FLEET_TRUSTED_PROXIES, value: "127.0.0.1,::1" }
          securityContext:
            privileged: true
            allowPrivilegeEscalation: true   # newuidmap/newgidmap file caps
            runAsUser: 1000
            runAsGroup: 1000
          resources:
            requests: { cpu: "34", memory: "70Gi" }
            limits:   { cpu: "34", memory: "70Gi" }
          # Probes are exec, not httpGet: kubelet dials the POD IP, which cannot
          # reach a 127.0.0.1-only listener. curl is in the image for this.
          startupProbe:
            exec: { command: ["curl", "-fsS", "http://127.0.0.1:8080/readyz"] }
            periodSeconds: 10
            failureThreshold: 30            # DB self-migration + warm-pool fill
          livenessProbe:
            exec: { command: ["curl", "-fsS", "http://127.0.0.1:8080/livez"] }
            periodSeconds: 30
            failureThreshold: 4
          readinessProbe:
            exec: { command: ["curl", "-fsS", "http://127.0.0.1:8080/readyz"] }
            periodSeconds: 10
          volumeMounts:
            - { name: state, mountPath: /var/lib/fleet }

        - name: web
          image: <acct>.dkr.ecr.<region>.amazonaws.com/fleet-web:<sha>
          ports:
            - { name: http, containerPort: 3000 }
          env:
            - { name: CHAT_SERVER_URL,         value: "http://127.0.0.1:8080" }
            - { name: ORCHESTRATOR_SERVER_URL, value: "http://127.0.0.1:8000" }
            - { name: CHAT_SERVER_TOKEN,       valueFrom: { secretKeyRef: { name: fleet-env, key: FLEET_SERVER_TOKEN } } }
            - { name: ORCHESTRATOR_SERVER_TOKEN, valueFrom: { secretKeyRef: { name: fleet-env, key: ADMIN_API_KEY } } }
            - { name: APP_SESSION_SECRET,      valueFrom: { secretKeyRef: { name: fleet-env, key: APP_SESSION_SECRET } } }
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: false     # Next writes .next/cache at runtime
            capabilities: { drop: ["ALL"] }
          resources:
            requests: { cpu: "500m", memory: "1Gi" }
            limits:   { cpu: "2",    memory: "2Gi" }
          readinessProbe:
            httpGet: { path: /, port: 3000 }
            periodSeconds: 10
          # SIGTERM reaches every container at once, so without this the public
          # tier can die while fleet is still draining a turn — the browser sees a
          # dropped stream instead of a finished answer. Sleep past the ALB's
          # deregistration delay, then let Next exit.
          lifecycle:
            preStop:
              exec: { command: ["sleep", "20"] }

  volumeClaimTemplates:
    - metadata: { name: state }
      spec:
        accessModes: ["ReadWriteOnce"]
        storageClassName: fleet-gp3-xfs
        resources: { requests: { storage: 400Gi } }
```

Service + ALB ingress (TLS terminates at the ALB with an ACM cert, so no Caddy
container is needed — the Next app remains the only public entrypoint):

```yaml
apiVersion: v1
kind: Service
metadata: { name: fleet, namespace: fleet }
spec:
  selector: { app: fleet }
  ports: [{ name: http, port: 3000, targetPort: 3000 }]
---
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: fleet
  namespace: fleet
  annotations:
    alb.ingress.kubernetes.io/scheme: internet-facing
    alb.ingress.kubernetes.io/target-type: ip
    alb.ingress.kubernetes.io/listen-ports: '[{"HTTPS":443}]'
    alb.ingress.kubernetes.io/certificate-arn: arn:aws:acm:…
    alb.ingress.kubernetes.io/ssl-redirect: "443"
    # SSE: agent turns stream for minutes. The default 60s idle timeout cuts them off.
    alb.ingress.kubernetes.io/load-balancer-attributes: idle_timeout.timeout_seconds=1800
    alb.ingress.kubernetes.io/healthcheck-path: /
spec:
  ingressClassName: alb
  rules:
    - host: fleet.example.com
      http:
        paths:
          - path: /
            pathType: Prefix
            backend: { service: { name: fleet, port: { number: 3000 } } }
```

**The ALB idle timeout is the one setting people get wrong.** Chat responses are
SSE streams that can run for many minutes; the ALB's 60-second default idle
timeout will sever them mid-turn. Raise it (1800s above) — the equivalent of
`flush_interval -1` + `read_timeout 30m` in `deploy/Caddyfile`.

Set `FLEET_PUBLIC_URL` / `FLEET_PUBLIC_BASE_URL` to the public origin so
notification links and share URLs resolve. Login works exactly as on the single
box (email + password, optional magic-link, optional OIDC SSO — all in the Next
layer); see [`docs/DEPLOYMENT.md`](DEPLOYMENT.md) for the login model.

### NetworkPolicy

Worth stating explicitly because it answers a real objection: **agent-executed
code is covered by pod-level NetworkPolicy.** Sandbox containers have no pod IP of
their own — their egress is NAT'd through the fleet pod's network namespace by the
rootless network helper (pasta on Podman ≥ 5.0, slirp4netns before it) — so a
policy on this pod governs what the model's `bash` and `run_python` can reach. This composes with, and does not replace, fleet's own
egress controls: `--network=none` for lockdown and scheduled runs is the hard
seal, and the allowlisted-egress proxy mode is
[ADR-0012](adr/0012-sandbox-egress-allowlist.md) /
[ADR-0031](adr/0031-chat-sandbox-egress.md).

Requires a policy-enforcing CNI — the **VPC CNI enforces NetworkPolicy** only
with `enableNetworkPolicy: true` (EKS 1.25+); otherwise use Calico or Cilium.

```yaml
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: { name: fleet, namespace: fleet }
spec:
  podSelector: { matchLabels: { app: fleet } }
  policyTypes: ["Ingress", "Egress"]
  ingress:
    - from: [{ ipBlock: { cidr: <vpc-cidr> } }]   # ALB target-type: ip
      ports: [{ port: 3000, protocol: TCP }]
  egress:
    - to: [{ namespaceSelector: { matchLabels: { kubernetes.io/metadata.name: kube-system } },
             podSelector: { matchLabels: { k8s-app: kube-dns } } }]
      ports: [{ port: 53, protocol: UDP }, { port: 53, protocol: TCP }]
    - to: [{ ipBlock: { cidr: <rds-subnet-cidr> } }]
      ports: [{ port: 5432, protocol: TCP }]
    # Model provider, ECR, and the MCP endpoints you intend. Narrow this as far as
    # your provider's addressing allows; it is the boundary on agent egress.
    - to: [{ ipBlock: { cidr: 0.0.0.0/0, except: [<vpc-cidr>, 169.254.169.254/32] } }]
      ports: [{ port: 443, protocol: TCP }]
```

Excluding `169.254.169.254/32` is belt-and-braces alongside the §6 IMDS hop
limit: two independent controls stopping agent code from reaching instance
credentials.

### Metrics scrape sidecar

`/metrics` is on the orchestrator's loopback listener and is admin-key gated
(§8). Rather than binding that listener to the pod IP — which would break the
impersonation boundary — add a tiny proxy that exposes only `GET /metrics` on the
pod IP and injects the key, then point a `ServiceMonitor`/`PodMonitor` (Prometheus
Operator) or your scrape config at port 9090:

```yaml
        - name: metrics-proxy
          image: nginx:1.30-alpine
          ports: [{ name: metrics, containerPort: 9090 }]
          # nginx.conf (mount from a ConfigMap):
          #   server { listen 9090;
          #     location = /metrics {
          #       proxy_pass http://127.0.0.1:8000/metrics;
          #       proxy_set_header Authorization "Bearer <ADMIN_API_KEY>";
          #     }
          #     location / { return 404; } }
          # Render the key in via envsubst on an nginx.conf.template at startup —
          # don't bake it into the ConfigMap.
          securityContext:
            allowPrivilegeEscalation: false
            capabilities: { drop: ["ALL"] }
          resources:
            requests: { cpu: "50m", memory: "64Mi" }
            limits:   { cpu: "200m", memory: "128Mi" }
```

The orchestrator authenticates admin reads with `Authorization: Bearer
<ADMIN_API_KEY>`. Keep the proxy's `location /` a 404 so the sidecar cannot become
a general-purpose hole into the orchestrator, and keep it off the Service that
backs the Ingress.

### Packaging these manifests for GitOps

Nothing above needs templating to be managed declaratively. A Kustomize base with
per-environment overlays is the smaller-footprint option; a thin Helm chart is
fine if charts are your standard:

```
deploy/k8s/
  base/           namespace.yaml serviceaccount.yaml statefulset.yaml service.yaml
                  ingress.yaml networkpolicy.yaml storageclass.yaml kustomization.yaml
  overlays/prod/  kustomization.yaml (images, replicas:1, resources, host, ARNs)
```

Two things bite in **Argo CD** specifically:

1. **`volumeClaimTemplates` are immutable.** Any change to them makes the
   StatefulSet un-patchable, and Argo reports a permanently `OutOfSync` app. To
   resize, patch the **PVC** directly (`allowVolumeExpansion: true`, §5) and leave
   the template alone; for a template change, delete the StatefulSet with
   `--cascade=orphan` and re-apply.
2. **Set `Replace=false` and avoid auto-prune on the PVC.** An automated sync that
   prunes the volume claim destroys workspaces, uploads, and the audit dir. Add
   `argocd.argoproj.io/sync-options: Prune=false` on the PVC, or exclude
   PersistentVolumeClaims from the app's prune scope.

Also keep **automated sync from being an upgrade mechanism you didn't intend**:
this workload restarts (with downtime, §6) on every pod-spec change, so pin
digests in the overlay and let a human promote them.

## 8. Observability and cluster integration

- **Logs** go to stdout/stderr → your CloudWatch/Fluent Bit pipeline. Leave
  `FLEET_LOG_FILE` unset; the file sink exists for hosts without a log collector
  and would only duplicate lines onto the PVC.
- **Metrics:** `/metrics` (Prometheus text format) is served by the
  **orchestrator** on `127.0.0.1:8000` and is **admin-API-key gated** — cost and
  token data must not be public. Because that listener must stay loopback, give
  the pod a tiny reverse-proxy sidecar that listens on the pod IP, forwards only
  `GET /metrics` to `127.0.0.1:8000`, and injects the admin key; point your
  scraper at the sidecar. Do not "solve" this by binding the orchestrator to the
  pod IP.
- **Useful series** for this deployment: `fleet_sandbox_memory_usage_bytes` /
  `fleet_sandbox_memory_limit_bytes` (right-size the per-sandbox caps),
  `fleet_sandbox_pids_peak`, and the sandbox pool gauge. Per-run peaks come from
  read-only `podman stats` sampling — observability only, never affecting
  isolation ([`docs/DEPLOYMENT.md`](DEPLOYMENT.md)).
- **Tracing:** `FLEET_OTEL_ENDPOINT` + `FLEET_OTEL_SAMPLE_RATIO` if you run a
  collector.

### Cluster integration gotchas

Each of these is something a Kubernetes-native environment does by default that
either breaks this pod or silently misleads you about it.

- **The sandboxes are invisible to the Kubernetes API.** They are Podman
  containers inside the pod: no entry in `kubectl get pods`, no cAdvisor
  container metrics, no kubelet events, no k8s audit records for them. Where to
  look instead: `kubectl exec … -- podman ps`, the `fleet_sandbox_*` metrics, the
  per-task resource telemetry, and the per-run logs / audit dir. Say this out
  loud in review — a platform team that expects pod-level visibility into agent
  workloads will otherwise assume it exists.
- **NodeLocal DNSCache breaks DNS inside sandboxes.** If the node's
  `/etc/resolv.conf` points at a link-local or loopback address (`169.254.20.10`,
  `127.0.0.1`), that address means something different inside the sandbox's
  network namespace under either helper, and name resolution fails for every
  outbound HTTP tool — while the fleet process itself resolves fine, so it looks
  like a model problem, not a DNS problem. Pin explicit resolvers for Podman in
  the image's `containers.conf`:

  ```ini
  [containers]
  dns_servers = ["172.20.0.10"]   # your cluster's kube-dns Service IP, or a VPC resolver
  ```

- **`ResourceQuota` / `LimitRange` in the namespace will reject the pod.** A
  34-vCPU/70-GiB request trips inherited defaults, and a `LimitRange` with a
  low `max` silently caps it. Give the namespace its own quota sized to the node,
  or none.
- **VPA in `Auto` mode is destructive here** — it restarts the pod to resize it.
  If you run VPA cluster-wide, exclude this workload or set `updateMode: "Off"`
  and use its recommendations to hand-tune §6.
- **`automountServiceAccountToken: false`** is safe and recommended: fleet makes
  no API calls, and the pod runs code the model wrote. IRSA/Pod Identity project
  their own token separately and keep working.
- **Runtime security tooling** (Falco, GuardDuty Runtime Monitoring, Aqua/Sysdig)
  will see nested container creation, user-namespace clones, and `newuidmap` from
  a privileged pod, and will alert on all of it. Baseline those signatures for
  this namespace *before* go-live — otherwise fleet's normal operation reads as an
  ongoing container-escape attempt, and the noise trains everyone to ignore the
  detector.
- **Node AMI upgrades are planned outages** (§6), so exclude this node group from
  any automatic AMI-refresh schedule and drain it deliberately.

## 9. Day-2 operations (what replaces bootstrap/update/doctor)

| Single-host | On EKS |
|---|---|
| `scripts/bootstrap.sh` | build images (§3) + `kubectl apply` |
| `fleet update` | build a new image tag, `kubectl set image` / re-apply, pod restarts |
| `fleet restart` | `kubectl rollout restart statefulset/fleet` |
| `scripts/doctor.sh` (systemd-specific) | `kubectl exec … -- fleet validate-config` plus §10 — and the in-process Doctor panel still works, see below |
| `fleet admin add <email>` | `kubectl exec -it sts/fleet -c fleet -- fleet admin add <email>` |
| `fleet mcp account set …` | same, via `kubectl exec` |
| journald | `kubectl logs sts/fleet -c fleet` |

`fleet validate-config` is the portable check — it verifies the bundle, podman
reachability, the sandbox image's presence, and the runtime preflight.

**Settings → Admin → Doctor works here too**, and degrades honestly: its
container-portable checks (chat and sched databases, model API key,
subuid/subgid ranges, rootless podman, sandbox image) all run normally, while the
systemd-dependent ones (sibling unit state, the scheduled-backup timer) report
`skip` with "systemctl not on PATH (no systemd)" rather than inventing advisories
about units that were never meant to exist here. Note the consequence, though: a
`skip` on scheduled backups is *not* reassurance — it is the gap you closed by
hand above.

**Config and bundle changes.** With the bundle baked into the image, a bundle
change is an image rebuild + pod restart. If you instead mount the bundle from a
PVC or clone it in an init container, MCP server definitions can be reloaded live
with `fleet mcp reload` / SIGHUP / the admin endpoint
([`docs/MCP-RELOAD.md`](MCP-RELOAD.md)). Reloadable env ceilings need the env-file
setup described in §3b; otherwise change them by editing the manifest and
restarting.

**Backups — read this one carefully.** Two things are stateful: the databases and
the PVC (EBS snapshots via the CSI `VolumeSnapshot` API — workspaces, uploads,
audit). The Podman image store on the PVC is reconstructible; don't optimize
backups for it.

The trap: fleet now ships `deploy/fleet-backup.service` + `fleet-backup.timer`,
which `bootstrap.sh --enable-service` installs and enables **by default**, and
`fleet doctor` reports on. **None of that exists here** — those are systemd units,
`bootstrap.sh` never runs on this deployment, and nothing in the pod will tell you
backups aren't happening. That gap is precisely the failure the timer was added to
fix (#966: a box reporting "38 ok, 0 advisories" while holding no backups at all,
for five days, with live client data). So pick one deliberately and write it down:

- **RDS automated backups + snapshots** (simplest, and what this guide assumes) —
  covers exactly the loss of a host or volume that a same-host `pg_dump` does not.
- **A Kubernetes `CronJob`** running `fleet backup` or `pg_dump` on a schedule, if
  you want the logical dump the timer would have produced (recoverable from a bad
  migration or an accidental delete). Give it its own ServiceAccount and write to
  S3, not to the PVC — a dump beside the data it protects is not a backup.

Either way, note that neither captures attachment/upload files, which live on the
PVC — those need the `VolumeSnapshot` schedule. See
[`docs/BACKUP_RESTORE.md`](BACKUP_RESTORE.md) for what a dump does and does not
cover.

**Upgrades and node patching** are downtime windows. Sequence: cordon nothing,
just `kubectl delete pod` / `rollout restart` and let the drain budget run —
fleet flips `/readyz` to 503, the ALB stops sending traffic, in-flight turns and
scheduled tasks drain within `FLEET_SHUTDOWN_GRACE_SECONDS`, and the new pod
re-attaches the same PVC.

## 10. Verification checklist

Run these in the fleet container (`kubectl exec -it sts/fleet -c fleet -- bash`)
before you call the deployment done. Each maps to a row in §2 that fails
*silently* if you skip it.

```sh
# 1. Rootless podman works at all, with the expected driver.
podman info --format '{{.Host.CgroupsVersion}} {{.Store.GraphDriverName}} {{.Host.Security.Rootless}}'
#    want: v2 <overlay|fuse-overlayfs> true       (NOT vfs)

# 2. The uid mapping fleet actually uses.
podman run --rm --userns=keep-id:uid=1000,gid=1000 "$FLEET_SANDBOX_IMAGE" id
#    want: uid=1000 gid=1000

# 3. Memory limits BIND. If this prints "max", --memory is being ignored and
#    every per-sandbox and per-task cap is fiction.
podman run --rm --memory=64m "$FLEET_SANDBOX_IMAGE" cat /sys/fs/cgroup/memory.max
#    want: 67108864

# 4. All three network postures (each needs /dev/net/tun for its helper).
#    a) normal turns — podman's rootless default (pasta on >= 5.0):
podman run --rm "$FLEET_SANDBOX_IMAGE" \
  python3 -c 'import socket;socket.create_connection(("1.1.1.1",443),5);print("default egress ok")'
#    b) allowlisted-egress posture — needs the slirp4netns binary specifically.
#       A missing binary now aborts BOOT with a fail-closed preflight, so check it
#       here if you plan to enable that mode:
podman run --rm --network=slirp4netns:allow_host_loopback=true \
  "$FLEET_SANDBOX_IMAGE" python3 -c 'import socket;socket.create_connection(("1.1.1.1",443),5);print("slirp egress ok")'
#    c) lockdown / scheduled runs — the hard seal:
podman run --rm --network=none "$FLEET_SANDBOX_IMAGE" true && echo "sealed mode ok"

# 5. The TOTAL writable-layer cap (the per-file ulimit applies either way, §5).
podman run --rm --storage-opt size=1g "$FLEET_SANDBOX_IMAGE" true \
  && echo "total-size cap available" || echo "per-file cap only — total layer size unbounded"

# 6. DNS inside a sandbox — the NodeLocal DNSCache trap (§8). Resolution can
#    fail here while the fleet process itself resolves fine.
podman run --rm --network=slirp4netns:allow_host_loopback=true \
  "$FLEET_SANDBOX_IMAGE" python3 -c 'import socket;print(socket.gethostbyname("api.openai.com"))'

# 7. The volume is actually writable by uid 1000 (fsGroup, §7).
touch /var/lib/fleet/.write-probe && rm /var/lib/fleet/.write-probe && echo "volume writable"

# 8. fleet's own preflight: bundle, podman, image, runtime.
fleet validate-config

# 9. Health + drain semantics.
curl -fsS http://127.0.0.1:8080/readyz; curl -fsS http://127.0.0.1:8080/livez
```

From outside the pod, confirm the cluster-side wiring:

```sh
# Admission actually permits the pod (fails at admission, with no pod to debug).
kubectl -n fleet get statefulset fleet -o jsonpath='{.status.readyReplicas}'
kubectl -n fleet describe statefulset fleet | grep -iA3 'FailedCreate\|forbidden'

# The volume landed in the AZ the node group lives in (§6).
kubectl -n fleet get pvc state-fleet-0 -o jsonpath='{.spec.volumeName}' \
  | xargs -I{} kubectl get pv {} -o jsonpath='{.spec.nodeAffinity}'

# SSE survives the ALB: this must stream for minutes, not cut off at 60s.
curl -N https://fleet.example.com/…   # any streaming chat turn

# Graceful drain: /readyz flips to 503 and in-flight work finishes, no SIGKILL.
kubectl -n fleet delete pod fleet-0 --wait=true
```

Then, from the UI, run one interactive turn that executes `run_python` and one
scheduled task, and confirm in `kubectl logs` that no line reports the
`--storage-opt` fallback or a warm-pool cold-start failure.

## Appendix: the complete manifest set

The sections above explain each piece; this is all of it assembled in apply
order, so nothing gets missed in transcription. It is the same content — if the
two ever disagree, the numbered sections are the explanation and this is the
transcription.

**Fill these in first.** Every placeholder appears in angle brackets:

| Placeholder | Where it comes from |
|---|---|
| `<ACCT>`, `<REGION>` | your AWS account ID and region |
| `<SHA>` | the image tags you built in §3 |
| `<SANDBOX_DIGEST>` | `sha256:…` of the sandbox image pushed in §3a — pin by digest |
| `<ROLE_ARN>` | the IRSA/Pod Identity role from §7 |
| `<ACM_ARN>` | the ACM certificate for your hostname |
| `<RDS_HOST>`, `<RDS_SUBNET_CIDR>` | the RDS endpoint and its subnet range (§4) |
| `<VPC_CIDR>` | the cluster VPC range, for the ALB ingress rule (§7) |
| `<KUBE_DNS_IP>` | `kubectl -n kube-system get svc kube-dns -o jsonpath='{.spec.clusterIP}'` |
| `<HOSTNAME>` | the public hostname, e.g. `fleet.example.com` |
| secret values | §7; prefer External Secrets over literals |

Sizing below is the worked 32-concurrent-agent example (`m7i.12xlarge`): raise
`FLEET_MAX_CONCURRENT_AGENTS`, the per-sandbox caps, the pod resources, and the
instance type **together** — see [§6](#resource-requests-count-the-sandboxes).

```yaml
# 1 ── Namespace. The PSA labels are what let the privileged pod be admitted (§7).
apiVersion: v1
kind: Namespace
metadata:
  name: fleet
  labels:
    pod-security.kubernetes.io/enforce: privileged
    pod-security.kubernetes.io/enforce-version: latest
    pod-security.kubernetes.io/audit: baseline
    pod-security.kubernetes.io/warn: baseline
    elbv2.k8s.aws/pod-readiness-gate-inject: enabled
---
# 2 ── StorageClass. xfs + prjquota is what makes the sandbox disk quota a HARD
#      cap on top of the per-file ulimit that applies regardless (§5).
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: fleet-gp3-xfs
provisioner: ebs.csi.aws.com
parameters:
  type: gp3
  iops: "6000"
  throughput: "500"
  fsType: xfs
mountOptions: ["prjquota"]
allowVolumeExpansion: true
volumeBindingMode: WaitForFirstConsumer
---
# 3 ── Identity. No Role/RoleBinding: fleet makes zero Kubernetes API calls (§7).
apiVersion: v1
kind: ServiceAccount
metadata:
  name: fleet
  namespace: fleet
  annotations:
    eks.amazonaws.com/role-arn: <ROLE_ARN>
automountServiceAccountToken: false
---
# 4 ── Secrets. Replace with an ExternalSecret / SecretProviderClass in a GitOps
#      repo — the pod spec below is identical either way (§7).
apiVersion: v1
kind: Secret
metadata:
  name: fleet-env
  namespace: fleet
stringData:
  OPENROUTER_API_KEY: "<OPENROUTER_API_KEY>"
  FLEET_CHAT_DATABASE_URL: "postgres://chat:<CHAT_PW>@<RDS_HOST>:5432/chat?sslmode=require"
  FLEET_SCHED_DATABASE_URL: "postgres://sched:<SCHED_PW>@<RDS_HOST>:5432/sched?sslmode=require"
  FLEET_SERVER_TOKEN: "<SHARED_CHAT_TOKEN>"
  ADMIN_API_KEY: "<ORCHESTRATOR_ADMIN_KEY>"
  APP_SESSION_SECRET: "<WEB_SESSION_SECRET>"
  # plus every MCP connector credential the bundle's manifest.yaml names
---
# 5 ── The workload.
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: fleet
  namespace: fleet
spec:
  replicas: 1                       # NEVER raise: single-owner leases + per-process semaphore
  serviceName: fleet
  podManagementPolicy: OrderedReady
  updateStrategy: { type: RollingUpdate }
  selector:
    matchLabels: { app: fleet }
  template:
    metadata:
      labels: { app: fleet }
      annotations:
        karpenter.sh/do-not-disrupt: "true"
    spec:
      serviceAccountName: fleet
      # Without fsGroup, uid 1000 cannot write the fresh EBS volume and the pod
      # crash-loops before it ever starts podman (§7).
      securityContext:
        fsGroup: 1000
        fsGroupChangePolicy: OnRootMismatch
      nodeSelector: { workload: fleet }
      tolerations:
        - { key: dedicated, value: fleet, effect: NoSchedule }
      terminationGracePeriodSeconds: 90   # > FLEET_SHUTDOWN_GRACE_SECONDS

      initContainers:
        - name: pull-sandbox
          image: <ACCT>.dkr.ecr.<REGION>.amazonaws.com/fleet:<SHA>
          command: ["/bin/sh", "-c"]
          args:
            - |
              set -e
              aws ecr get-login-password --region "$AWS_REGION" \
                | podman login --username AWS --password-stdin "$ECR_REGISTRY"
              podman pull "$FLEET_SANDBOX_IMAGE"
          env:
            - { name: AWS_REGION,   value: "<REGION>" }
            - { name: ECR_REGISTRY, value: "<ACCT>.dkr.ecr.<REGION>.amazonaws.com" }
            - { name: FLEET_SANDBOX_IMAGE, value: "<ACCT>.dkr.ecr.<REGION>.amazonaws.com/fleet-sandbox@<SANDBOX_DIGEST>" }
            - { name: HOME,            value: "/var/lib/fleet" }
            - { name: XDG_RUNTIME_DIR, value: "/var/lib/fleet/run" }
          securityContext: { privileged: true, runAsUser: 1000 }
          volumeMounts:
            - { name: state, mountPath: /var/lib/fleet }

      containers:
        - name: fleet
          image: <ACCT>.dkr.ecr.<REGION>.amazonaws.com/fleet:<SHA>
          envFrom:
            - secretRef: { name: fleet-env }
          env:
            - { name: FLEET_SERVER_ADDR,       value: "127.0.0.1:8080" }
            - { name: FLEET_ORCHESTRATOR_ADDR, value: "127.0.0.1:8000" }  # must stay loopback
            - { name: FLEET_CLIENT_CONFIG_DIR, value: "/opt/fleet/client" }
            - { name: FLEET_DATA_DIR,          value: "/var/lib/fleet/data" }
            - { name: FLEET_WORKSPACE_ROOT,    value: "/var/lib/fleet/workspace" }
            - { name: HOME,                    value: "/var/lib/fleet" }
            - { name: XDG_RUNTIME_DIR,         value: "/var/lib/fleet/run" }
            - { name: FLEET_SANDBOX_IMAGE,     value: "<ACCT>.dkr.ecr.<REGION>.amazonaws.com/fleet-sandbox@<SANDBOX_DIGEST>" }
            - { name: FLEET_PUBLIC_URL,        value: "https://<HOSTNAME>" }
            - { name: FLEET_MAX_CONCURRENT_AGENTS,  value: "32" }
            - { name: FLEET_SANDBOX_MEMORY,         value: "2g" }
            - { name: FLEET_SANDBOX_CPUS,           value: "1.0" }
            - { name: FLEET_SANDBOX_WARM_SIZE,      value: "4" }
            - { name: FLEET_SHUTDOWN_GRACE_SECONDS, value: "60" }
            - { name: FLEET_TIMEZONE,          value: "UTC" }
            - { name: FLEET_TRUSTED_PROXIES,   value: "127.0.0.1,::1" }
          securityContext:
            privileged: true              # see §2 for what this buys and costs
            allowPrivilegeEscalation: true # newuidmap/newgidmap file caps
            runAsUser: 1000                # NOT root — rootful podman ignores keep-id
            runAsGroup: 1000
          resources:
            requests: { cpu: "34", memory: "70Gi" }   # base + 32 × per-sandbox cap
            limits:   { cpu: "34", memory: "70Gi" }
          # exec, not httpGet: kubelet dials the pod IP and cannot reach loopback.
          startupProbe:
            exec: { command: ["curl", "-fsS", "http://127.0.0.1:8080/readyz"] }
            periodSeconds: 10
            failureThreshold: 30
          livenessProbe:
            exec: { command: ["curl", "-fsS", "http://127.0.0.1:8080/livez"] }
            periodSeconds: 30
            failureThreshold: 4
          readinessProbe:
            exec: { command: ["curl", "-fsS", "http://127.0.0.1:8080/readyz"] }
            periodSeconds: 10
          volumeMounts:
            - { name: state, mountPath: /var/lib/fleet }

        - name: web
          image: <ACCT>.dkr.ecr.<REGION>.amazonaws.com/fleet-web:<SHA>
          ports:
            - { name: http, containerPort: 3000 }
          env:
            - { name: CHAT_SERVER_URL,         value: "http://127.0.0.1:8080" }
            - { name: ORCHESTRATOR_SERVER_URL, value: "http://127.0.0.1:8000" }
            - { name: CHAT_SERVER_TOKEN,         valueFrom: { secretKeyRef: { name: fleet-env, key: FLEET_SERVER_TOKEN } } }
            - { name: ORCHESTRATOR_SERVER_TOKEN, valueFrom: { secretKeyRef: { name: fleet-env, key: ADMIN_API_KEY } } }
            - { name: APP_SESSION_SECRET,        valueFrom: { secretKeyRef: { name: fleet-env, key: APP_SESSION_SECRET } } }
          securityContext:
            allowPrivilegeEscalation: false
            capabilities: { drop: ["ALL"] }
          resources:
            requests: { cpu: "500m", memory: "1Gi" }
            limits:   { cpu: "2",    memory: "2Gi" }
          readinessProbe:
            httpGet: { path: /, port: 3000 }
            periodSeconds: 10
          lifecycle:
            preStop:
              exec: { command: ["sleep", "20"] }   # outlive ALB deregistration

  volumeClaimTemplates:                  # immutable — see the Argo notes in §7
    - metadata: { name: state }
      spec:
        accessModes: ["ReadWriteOnce"]
        storageClassName: fleet-gp3-xfs
        resources: { requests: { storage: 400Gi } }
---
# 6 ── Service (the web tier is the only exposed port).
apiVersion: v1
kind: Service
metadata: { name: fleet, namespace: fleet }
spec:
  selector: { app: fleet }
  ports: [{ name: http, port: 3000, targetPort: 3000 }]
---
# 7 ── Ingress. The idle timeout is what keeps SSE turns from being severed.
apiVersion: networking.k8s.io/v1
kind: Ingress
metadata:
  name: fleet
  namespace: fleet
  annotations:
    alb.ingress.kubernetes.io/scheme: internet-facing
    alb.ingress.kubernetes.io/target-type: ip
    alb.ingress.kubernetes.io/listen-ports: '[{"HTTPS":443}]'
    alb.ingress.kubernetes.io/certificate-arn: <ACM_ARN>
    alb.ingress.kubernetes.io/ssl-redirect: "443"
    alb.ingress.kubernetes.io/load-balancer-attributes: idle_timeout.timeout_seconds=1800
    alb.ingress.kubernetes.io/healthcheck-path: /
spec:
  ingressClassName: alb
  rules:
    - host: <HOSTNAME>
      http:
        paths:
          - path: /
            pathType: Prefix
            backend: { service: { name: fleet, port: { number: 3000 } } }
---
# 8 ── NetworkPolicy. This governs agent-executed code too: sandbox egress NATs
#      through the pod's netns (§7). Needs a policy-enforcing CNI.
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: { name: fleet, namespace: fleet }
spec:
  podSelector: { matchLabels: { app: fleet } }
  policyTypes: ["Ingress", "Egress"]
  ingress:
    - from: [{ ipBlock: { cidr: <VPC_CIDR> } }]
      ports: [{ port: 3000, protocol: TCP }]
  egress:
    - to:
        - namespaceSelector: { matchLabels: { kubernetes.io/metadata.name: kube-system } }
          podSelector: { matchLabels: { k8s-app: kube-dns } }
      ports: [{ port: 53, protocol: UDP }, { port: 53, protocol: TCP }]
    - to: [{ ipBlock: { cidr: <RDS_SUBNET_CIDR> } }]
      ports: [{ port: 5432, protocol: TCP }]
    - to:
        - ipBlock:
            cidr: 0.0.0.0/0
            except: [<VPC_CIDR>, 169.254.169.254/32]
      ports: [{ port: 443, protocol: TCP }]
```

Then, in order: create the node group (§6), apply the above, `kubectl exec` in and
run the §10 checklist, add an admin (`fleet admin add <email>`), and log in.

Not included above, deliberately: the metrics scrape sidecar and its ConfigMap
(optional, §7 — add it once Prometheus is wired up), and the `containers.conf`
`dns_servers` pin, which belongs in the **image** rather than the manifest (§8).

## What this deployment does not change

- One governed run loop (`agentcore.Run`) — policy, cost/token ceilings, audit
  ([ADR-0001](adr/0001-one-governed-run-loop.md)).
- The mandatory sandbox for every tool call's data plane, with no host-execution
  fallback ([ADR-0002](adr/0002-mandatory-rootless-podman-sandbox.md),
  [ADR-0036](adr/0036-sandboxed-file-tools-and-host-io-exceptions.md)).
- Host-side MCP credential brokering — secrets never enter a sandbox
  ([ADR-0003](adr/0003-host-side-mcp-credential-brokering.md)).
- Client content stays in an out-of-repo bundle
  ([ADR-0006](adr/0006-external-client-config-bundle.md)).

What it *does* change is the outer boundary: on the single box the fleet process
is an unprivileged system user, and here it is a privileged container on a
dedicated node. Treat the node as the trust boundary and size the isolation you
add around it (§2) accordingly.
