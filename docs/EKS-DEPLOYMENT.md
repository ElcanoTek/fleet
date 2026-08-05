# Deploying fleet on Amazon EKS (one pod, one big node)

> Operator recipe for organizations whose platform standard is Kubernetes. It
> keeps fleet's single-process, single-node model intact — one pod on one large
> node, scaled vertically — and does **not** try to spread the Podman sandboxes
> across worker nodes. For the supported single-host install see
> [`docs/DEPLOYMENT.md`](DEPLOYMENT.md).

## Read this first (scope, honesty, and what is not shipped)

- **fleet's shipped deployment target is a single VM under systemd**
  ([ADR-0004](adr/0004-single-box-vm-native-deployment.md)). That ADR stands.
  There is no Helm chart, no operator, and no k8s manifest in this repo, and
  **CI does not exercise this path** — the CI matrix builds and tests the
  systemd/single-host model. Everything below is a hand-verified recipe you own
  and must validate on your own cluster.
- **No fleet container image ships either.** `deploy/` contains systemd units, not
  images. You build two images yourself (§3): the fleet runtime image (Go binary
  **+ Podman inside it**) and the Next.js web image. The sandbox image stays what
  it already is — a per-client *bundle* artifact.
- **One pod. One replica. Forever.** Scheduled-task crash recovery uses
  single-owner database leases and the concurrency cap is a per-process
  semaphore. Two fleet pods against one pair of databases is a **correctness
  bug**, not a capacity increase. No `Deployment` with rolling updates, no HPA,
  no `replicas: 2`.
- **The sandbox stays local to the process.** Every agent tool call's data plane
  runs in a rootless-Podman container that `agentcore` starts and `podman exec`s
  into on the same host as the run loop
  ([ADR-0002](adr/0002-mandatory-rootless-podman-sandbox.md)); the remote worker
  registry was deliberately removed
  ([ADR-0011](adr/0011-remove-worker-node-registry.md)). There is no seam that
  dispatches a sandbox to another node, so "put the runners on their own node
  group" is not a configuration — it would be a rewrite. This guide runs Podman
  **inside the fleet pod**, which is why the pod needs the privileges in §2.
- **What you gain** by doing this at all: your existing ECR/IRSA/ALB/Secrets
  Manager/observability plumbing, one node group to patch, and node-failure
  rescheduling. **What you give up** versus the systemd path: `bootstrap.sh`,
  `fleet update`, and `scripts/doctor.sh` all assume a systemd host — you replace
  them with image rebuilds and `kubectl exec` (§9).

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
--pids-limit=… --network=slirp4netns:allow_host_loopback=true …` and then
`podman exec`s each tool call into it. That needs, inside the fleet container:

| Requirement | Why | How |
|---|---|---|
| `/etc/subuid` + `/etc/subgid` ranges for the container's user | `--userns=keep-id` maps uids into the range; without it Podman fails with a `newuidmap` mapping error | baked into the image (§3) |
| `newuidmap`/`newgidmap` with their file capabilities intact | performs the uid/gid mapping | `shadow-utils` in the image **and** `allowPrivilegeEscalation: true` (file caps are neutralized by `NoNewPrivileges` — the same reason `deploy/fleet.service` sets `NoNewPrivileges=no`) |
| a writable, **persistent** graph root (`$HOME/.local/share/containers`) | holds the ~1.5 GB sandbox image + per-container writable layers | the PVC mounted at `/var/lib/fleet` (§5) |
| an overlay-capable storage driver | `vfs` copies the whole ~1.5 GB image per container start — fatal for a per-turn warm pool | native `overlay` (privileged) or `fuse-overlayfs` + `/dev/fuse` |
| `/dev/net/tun` | `slirp4netns` egress for normal turns and the allowlisted-egress proxy mode | privileged, or a device plugin |
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
which builds with `scripts/build-sandbox-image.sh`, pushes an immutable
`{git-sha}` tag, and opens a PR pinning `sandbox.image` in the client repo.
Point it at ECR instead of GHCR, or mirror the GHCR tag into ECR.

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
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN make build            # → ./fleet and ./fleet-admin

FROM fedora:41
# podman + the rootless stack fleet actually invokes; curl for the exec probes (§7).
RUN dnf install -y --setopt=install_weak_deps=False \
      podman crun conmon slirp4netns fuse-overlayfs containers-common \
      shadow-utils catatonit iptables-nft git curl ca-certificates \
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
FROM node:22 AS build
WORKDIR /app
COPY web/package*.json ./
RUN npm ci
COPY web/ .
RUN npm run build
FROM node:22-slim
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

**Why `xfs` + `prjquota`:** fleet caps each sandbox's writable layer with
`--storage-opt size=…`, a hard total cap, but Podman only accepts that on a
quota-capable driver (overlay+xfs with pquota, btrfs, zfs — **not** overlay+ext4
and not vfs). fleet probes this once at boot and, when unsupported, falls back to
`--ulimit fsize`, which caps any single file but **not total disk use** — an
agent can still fill the volume with many files. The fallback is logged at
startup; the guide's recommendation is to make the probe succeed instead.

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
  --node-type r7i.24xlarge --nodes 1 --nodes-min 1 --nodes-max 1 \
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
  `c7i`. `r7i.24xlarge` (96 vCPU / 768 GB) comfortably hosts
  `FLEET_MAX_CONCURRENT_AGENTS=64`; `m7i.8xlarge` (32/128) suits 24–32.
- **`maxPods`:** the sandboxes are Podman containers *inside* the pod, so they
  don't consume pod IPs or count against `maxPods`. Only fleet's own pod does.
- **Karpenter:** annotate the pod `karpenter.sh/do-not-disrupt: "true"`. Node
  consolidation on a single stateful pod means unplanned restarts.
- **Cluster Autoscaler / HPA:** neither applies. Do not attach an HPA.
- **PodDisruptionBudget:** don't set a blocking one (`minAvailable: 1` on a
  single-replica workload blocks node drains indefinitely). A single-pod
  deployment means node replacement is a **planned downtime window** — the honest
  consequence of the single-writer design, same as rebooting the single box.

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

Namespace, secret, and IRSA service account first (use External Secrets or the
Secrets Store CSI driver in place of a literal `Secret` if that's your standard):

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

## 8. Observability

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

## 9. Day-2 operations (what replaces bootstrap/update/doctor)

| Single-host | On EKS |
|---|---|
| `scripts/bootstrap.sh` | build images (§3) + `kubectl apply` |
| `fleet update` | build a new image tag, `kubectl set image` / re-apply, pod restarts |
| `fleet restart` | `kubectl rollout restart statefulset/fleet` |
| `scripts/doctor.sh` (systemd-specific) | `kubectl exec … -- fleet validate-config` plus §10 |
| `fleet admin add <email>` | `kubectl exec -it sts/fleet -c fleet -- fleet admin add <email>` |
| `fleet mcp account set …` | same, via `kubectl exec` |
| journald | `kubectl logs sts/fleet -c fleet` |

`fleet validate-config` is the portable check — it verifies the bundle, podman
reachability, the sandbox image's presence, and the runtime preflight.

**Config and bundle changes.** With the bundle baked into the image, a bundle
change is an image rebuild + pod restart. If you instead mount the bundle from a
PVC or clone it in an init container, MCP server definitions can be reloaded live
with `fleet mcp reload` / SIGHUP / the admin endpoint
([`docs/MCP-RELOAD.md`](MCP-RELOAD.md)). Reloadable env ceilings need the env-file
setup described in §3b; otherwise change them by editing the manifest and
restarting.

**Backups.** Two things are stateful: the databases (RDS automated backups +
snapshots, or `pg_dump` per [`docs/BACKUP_RESTORE.md`](BACKUP_RESTORE.md)) and
the PVC (EBS snapshots via the CSI `VolumeSnapshot` API — workspaces, uploads,
audit). The Podman image store on the PVC is reconstructible; don't optimize
backups for it.

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

# 4. slirp4netns egress (needs /dev/net/tun) and the hard seal.
podman run --rm --network=slirp4netns:allow_host_loopback=true \
  "$FLEET_SANDBOX_IMAGE" python3 -c 'import socket;socket.create_connection(("1.1.1.1",443),5);print("egress ok")'
podman run --rm --network=none "$FLEET_SANDBOX_IMAGE" true && echo "sealed mode ok"

# 5. Disk quota — does --storage-opt work, or will fleet fall back to ulimit fsize?
podman run --rm --storage-opt size=1g "$FLEET_SANDBOX_IMAGE" true \
  && echo "hard disk cap available" || echo "will fall back to per-file ulimit only"

# 6. fleet's own preflight: bundle, podman, image, runtime.
fleet validate-config

# 7. Health + drain semantics.
curl -fsS http://127.0.0.1:8080/readyz; curl -fsS http://127.0.0.1:8080/livez
```

Then, from the UI, run one interactive turn that executes `run_python` and one
scheduled task, and confirm in `kubectl logs` that no line reports the
`--storage-opt` fallback or a warm-pool cold-start failure.

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
