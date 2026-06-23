# Deploying fleet on Kubernetes

fleet's default deployment is **systemd on a single host** (see the README
"Deploy" section). This guide is for teams that run everything on Kubernetes and
want fleet to live in their cluster too. Read the framing first — it changes
what you should expect.

> **Status of this guide.** The manifests below are a **reference starting
> point**, not a turnkey, cluster-tested chart. fleet's CI validates the
> systemd-on-a-host path; it does **not** run a Kubernetes integration suite.
> Your platform team owns validating these against your cluster (the sysbox
> install, your storage classes, your ingress controller, your Pod Security
> posture) and the [validation checklist](#validation-checklist-do-this-before-trusting-it)
> at the end. We ship guidance + reference YAML, not a supported Helm chart —
> see [Why not a Helm chart](#why-reference-manifests-not-a-helm-chart).

## The honest framing: this is a VM-shaped workload

fleet is **one process on one host** by design:

- The concurrency cap is a **per-process semaphore**, and scheduled-task crash
  recovery uses **single-owner database leases**. Two replicas would double-run
  scheduled tasks and over-run the cap. So fleet on Kubernetes is a
  **single-replica, node-pinned StatefulSet** — you scale it **vertically** (a
  bigger node), exactly as you'd size the VM.
- Every agent tool call runs in an **ephemeral rootless-Podman container** that
  the fleet process spawns at runtime (`podman run` of the ~1.3 GB sandbox image,
  per call). So the pod must itself be able to run containers — **nested
  containers** — which is the part that needs care on Kubernetes.

The bottom line to set with the client up front:

> fleet is operationally a dedicated VM. We can deliver it as a Kubernetes
> StatefulSet on a dedicated, **sysbox-enabled node pool** so it lives in your
> cluster and integrates with your CI / observability / secrets — but it
> consumes a node pool and requires a node-level runtime install. If you'd
> rather not run a special node pool, a **dedicated VM** (the standard
> `bootstrap.sh` path) is functionally identical, simpler, and cheaper to
> operate. Making the sandbox itself cloud-native (per-tool-call Kubernetes
> Jobs) is a sandbox re-architecture we are **not** doing today.

Give the client **both** options and let them choose. A native-k8s shop usually
takes the node pool — just make the dedicated-node cost and the sysbox install
explicit so they aren't a surprise.

## Recommended architecture

```
            Ingress (TLS, SSE-aware)
                  │
                  ▼
        Service (:3000 web, :8080 chat-SSE)
                  │
   ┌──────────────▼───────────────────────────────────────┐
   │ StatefulSet  replicas: 1   (dedicated sysbox node)     │
   │  ┌─────────────────────────┐  ┌────────────────────┐   │
   │  │ fleet container          │  │ web sidecar (:3000)│   │
   │  │  fleet :8080 + :8000     │◀─┤  Next.js, proxies  │   │
   │  │  + scheduler + worker    │  │  over loopback     │   │
   │  │  + rootless podman ──────┼─▶ ephemeral sandbox    │   │
   │  │    (nested containers)   │    containers (per call)│  │
   │  └──────────┬──────────────┘  └────────────────────┘   │
   │  graphroot: node-local SSD   workspace: RWO PVC          │
   └──────────────────────────────────────────────────────┘
                  │
                  ▼
        Postgres (managed, recommended) — chat + sched DBs
```

- **Single-replica StatefulSet** (not a Deployment): stable identity,
  `OrderedReady` so the old pod is fully gone before the new one claims the DB
  leases — matching fleet's single-owner crash-recovery model.
- **Dedicated node pool**, tainted so only fleet schedules there. Nested
  containers + the per-call image store want a node you control.
- **fleet + podman in one container**; the **web app as a sidecar** in the same
  pod (keeps the loopback proxy intact — do *not* split it into a second pod).
- **Postgres**: use **managed** Postgres (RDS/Cloud SQL/etc.) and pass the two
  DSNs as secrets. Don't couple DB durability to this node's lifecycle. A
  Postgres StatefulSet works too but is one more stateful thing to operate.

### Storage tiers — get this right or it will hurt

This is the single most important section.

| Data | Where | Why |
| --- | --- | --- |
| **Podman graphroot** (image + overlay store) | **node-local SSD** (a `local` PV, or an `emptyDir` on a fast local disk) — **never a networked PVC** | rootless overlay needs `fuse-overlayfs`/kernel-overlay on a real local fs. On NFS/EBS-style PVCs podman silently falls back to the **`vfs`** driver, which **full-copies every image layer on every `podman run`** — for a 1.3 GB image, per tool call, that is catastrophic (multi-GB copy + disk blowup each call). |
| **Per-conversation workspaces** | **PVC (RWO** is fine — single replica) | must persist across pod restarts; it is the same-path bind-mount source for every sandbox container. |
| **Postgres** | **managed** (recommended) or a separate StatefulSet | independent durability + backups. |

**Pre-bake / pre-pull the sandbox image into the graphroot** (a node-init step or
an init container that `podman pull`s it once) so the per-call `podman run` is a
fast copy-on-write start, not a pull.

### Nested containers: sysbox (recommended) vs privileged (fallback)

**Recommended — `runtimeClassName: sysbox-runc`.** [Sysbox](https://github.com/nestybox/sysbox)
makes the pod behave like a VM: nested rootless Podman + user namespaces "just
work," with **no `privileged: true`**, no manual capabilities, no `procMount`
unmasking, stock seccomp. This is the clean story for a security-conscious shop.
The cost: sysbox is **not** preinstalled on stock GKE/EKS/AKS — install
`sysbox-runc` on the dedicated node pool (its DaemonSet installer + a node
reconfigure). Since nested containers already force a self-managed/custom node
pool, lean into it.

**Fallback — privileged pod.** Where you can't install sysbox: `privileged: true`
(or, minimally, `SYS_ADMIN` + `procMount: Unmasked` + a `/dev/fuse` device + an
unconfined-enough seccomp/AppArmor for the nested mounts), in a **dedicated
namespace whose Pod Security Admission is set to `privileged`**. Be blunt with
the client: a native-k8s shop almost certainly runs `restricted`/`baseline` PSA,
which will **reject** this — so the privileged fallback is a real security ask,
which is exactly why sysbox is worth the node-pool setup.

### Pod / Service specifics

- **Ports.** Service exposes `:3000` (web) and `:8080` (chat SSE). `:8000`
  (orchestrator REST) stays cluster-internal unless you need it externally.
- **Ingress.** Route to the web `:3000`. SSE on the chat path needs buffering
  **off** and a long read timeout (e.g. nginx:
  `nginx.ingress.kubernetes.io/proxy-buffering: "off"` +
  `proxy-read-timeout: "3600"`).
- **Probes.** Readiness = TCP/HTTP on both `:3000` and `:8080`. Liveness =
  `:8080` only (don't let a slow web build kill the pod). Generous
  `initialDelaySeconds` — the image pre-pull + podman init takes time.
- **Graceful drain.** A `preStop` hook must tell fleet to stop accepting new
  work, drain in-flight `podman exec`s, and **release its DB leases** before
  exit — a hard SIGKILL mid-call leaves a stale lease and orphaned `--rm`
  containers. Set `terminationGracePeriodSeconds` above your longest tool call
  (e.g. 180–300s). fleet already drains the worker pool on `SIGTERM`; the hook
  just gives it the time.
- **Sizing.** Same as the README host-sizing table — budget ~1 vCPU + ~1.5–3 GB
  RAM per concurrent agent on top of a ~2 vCPU / 6 GB base, and set
  `FLEET_MAX_CONCURRENT_AGENTS` to match the node.

## Reference manifests

Reference only — adapt to your cluster, then run the
[validation checklist](#validation-checklist-do-this-before-trusting-it). Build a
single image that contains the `fleet` binary **and** podman + the web app (or
run the web app from a sidecar image); the `image:` refs below are placeholders.

```yaml
# namespace.yaml — sysbox path keeps a stock (non-privileged) PSA posture.
apiVersion: v1
kind: Namespace
metadata:
  name: fleet
  labels:
    pod-security.kubernetes.io/enforce: baseline   # privileged fallback: set to "privileged"
---
# runtimeclass.yaml — install sysbox-runc on the node pool first.
apiVersion: node.k8s.io/v1
kind: RuntimeClass
metadata:
  name: sysbox-runc
handler: sysbox-runc
---
# service.yaml
apiVersion: v1
kind: Service
metadata:
  name: fleet
  namespace: fleet
spec:
  selector: { app: fleet }
  ports:
    - { name: web,  port: 3000, targetPort: 3000 }
    - { name: chat, port: 8080, targetPort: 8080 }   # SSE
---
# statefulset.yaml
apiVersion: apps/v1
kind: StatefulSet
metadata:
  name: fleet
  namespace: fleet
spec:
  serviceName: fleet
  replicas: 1                      # SINGLE replica — do not scale out
  podManagementPolicy: OrderedReady
  selector: { matchLabels: { app: fleet } }
  template:
    metadata:
      labels: { app: fleet }
    spec:
      runtimeClassName: sysbox-runc        # fallback: remove + use a privileged securityContext
      nodeSelector: { fleet.elcanotek.com/sandbox: "true" }
      tolerations:
        - { key: "fleet.elcanotek.com/sandbox", operator: "Exists", effect: "NoSchedule" }
      terminationGracePeriodSeconds: 240
      containers:
        - name: fleet
          image: REGISTRY/fleet:TAG          # contains the fleet binary + podman + the sandbox image pre-pulled
          command: ["/opt/fleet/fleet"]
          ports:
            - { containerPort: 8080 }        # chat SSE
            - { containerPort: 8000 }        # orchestrator (internal)
          envFrom:
            - secretRef: { name: fleet-env } # OPENROUTER_API_KEY, FLEET_*_DATABASE_URL, MCP creds, …
          env:
            - { name: FLEET_MAX_CONCURRENT_AGENTS, value: "4" }
            - { name: FLEET_CLIENT_CONFIG_DIR, value: "/opt/fleet/client" }
          readinessProbe:
            tcpSocket: { port: 8080 }
            initialDelaySeconds: 30
          livenessProbe:
            tcpSocket: { port: 8080 }
            initialDelaySeconds: 60
          lifecycle:
            preStop:
              exec: { command: ["/bin/sh", "-c", "kill -TERM 1 && sleep 200"] }  # drain + release leases
          volumeMounts:
            - { name: graphroot, mountPath: /var/lib/containers }      # NODE-LOCAL — not a networked PVC
            - { name: workspace, mountPath: /opt/fleet/data/workspace } # the same-path bind source
        - name: web
          image: REGISTRY/fleet-web:TAG
          command: ["npm", "run", "start"]
          ports: [ { containerPort: 3000 } ]
          env:
            - { name: CHAT_SERVER_URL,         value: "http://127.0.0.1:8080" }
            - { name: ORCHESTRATOR_SERVER_URL, value: "http://127.0.0.1:8000" }
          envFrom:
            - secretRef: { name: fleet-web-env }   # CHAT_SERVER_TOKEN must match the fleet binary's FLEET_SERVER_TOKEN
          readinessProbe:
            tcpSocket: { port: 3000 }
            initialDelaySeconds: 20
      volumes:
        - name: graphroot                         # NODE-LOCAL fast disk (local PV or emptyDir on local SSD)
          emptyDir: { sizeLimit: 40Gi }           #   prefer a `local` PersistentVolume in production
  volumeClaimTemplates:
    - metadata: { name: workspace }
      spec:
        accessModes: ["ReadWriteOnce"]
        resources: { requests: { storage: 50Gi } }
```

Express the two real axes — **runtime posture** (sysbox vs privileged) and **DB
source** (managed vs in-cluster) — as **Kustomize overlays** over a base, rather
than a Helm chart with conditionals (see below).

## Why reference manifests, not a Helm chart

A single-replica, node-pinned, runtime-class-dependent workload has almost no
axes worth templating. A Helm chart would imply a flexibility the architecture
doesn't have (replica counts, HPAs, multi-AZ) and invite misconfiguration. The
only genuine variables are the runtime posture and the DB source, which Kustomize
**overlays** (`overlays/sysbox`, `overlays/privileged`, `overlays/managed-pg`,
`overlays/pg-statefulset`) express more honestly than `if .Values…` branches.

## Validation checklist (do this before trusting it)

Run these on your cluster before relying on the deployment — they are the things
that actually bite:

1. **Storage driver is overlay, not vfs.** `podman info | grep -i 'graphDriverName'`
   inside the pod must report `overlay` (with `fuse-overlayfs` or kernel
   overlay), **not** `vfs`. vfs means your graphroot is on the wrong volume —
   per-call layer copies will cripple it.
2. **keep-id workspace ownership.** Run a real agent turn that writes a file to
   the workspace from inside the sandbox, then confirm the file is owned/readable
   correctly back in the pod and persists on the PVC. The outer pod userns +
   inner `--userns=keep-id` double-remap is the fragile bit; sysbox makes it
   behave like a VM, the privileged path may need a pre-chown.
3. **Same-path bind works in-pod.** A path the host-side MCP returns (e.g.
   `/opt/fleet/data/workspace/<id>/x.csv`) must open at the same absolute path
   inside the sandbox container.
4. **SELinux `z` relabel.** On a PVC filesystem that doesn't support SELinux
   labels (NFS, some CSI), the `:z` relabel is a no-op or errors — confirm your
   workspace volume's filesystem supports it (or that the cluster isn't enforcing
   SELinux).
5. **preStop drain.** Kill the pod mid-task and confirm the lease is released and
   no orphaned sandbox containers remain — the scheduled task should re-run
   cleanly, not double-run or wedge.
6. **SSE.** Confirm a streamed chat turn actually streams through the Ingress
   (buffering off), rather than buffering until the turn ends.

If any of these fail, fix it before going further — and remember the dedicated
VM is always the simpler fallback.
