# fleet Helm chart

> The enterprise deployment path (issue #989 / [ADR-0049](../../../docs/adr/0049-kubernetes-backend-split-control-plane.md)):
> the fleet control plane as a single-replica Deployment, with agent sandboxes
> running as ephemeral pods via `FLEET_SANDBOX_BACKEND=kubernetes`. The
> single-box podman install ([`docs/DEPLOYMENT.md`](../../../docs/DEPLOYMENT.md))
> remains the default fleet deployment; use this chart when Kubernetes is your
> platform standard.

Full walkthrough — a 15-minute kind path and the production checklist — lives
in [`docs/DEPLOYMENT-KUBERNETES.md`](../../../docs/DEPLOYMENT-KUBERNETES.md).

This chart renders a control plane; it does not supply the *bundle* that plane
loads. For a worked one — both Containerfiles, a documented values overlay for
this chart, and an empty-cluster-to-working-fleet guide — fork
[`ElcanoTek/example-kubernetes-config`](https://github.com/ElcanoTek/example-kubernetes-config).

## What it installs

| Piece | Object | Notes |
| --- | --- | --- |
| Control plane | Deployment (1 replica, Recreate) | chat :8080 + orchestrator :8000 + the web UI :3000; no replica knob — the scheduler is single-owner |
| Sandbox RBAC | Role/RoleBinding | exactly the verbs the boot preflight checks: pods create/get/list/delete, pods/exec create, PVC + NetworkPolicy get |
| Workspace | RWX PVC | mounted at the SAME path in the control plane and every sandbox pod |
| Sealed egress | NetworkPolicy `fleet-sandbox-deny-all` | selects pods labeled `fleet.elcanotek.com/egress=none`; the preflight requires it to exist |
| Postgres | optional StatefulSet | evaluation only — production points at a managed database |
| Ingress | optional | routes to the control plane's web port (3000): the web UI and its `/api`, served by the fleet binary |

## Minimum install

The Secret has to exist before the install, not alongside it: the Deployment
mounts it with `envFrom`, so a pod created ahead of it sits in
`CreateContainerConfigError`. That means the namespace comes first too — which
is why this is three commands and not one with `--create-namespace`.

```sh
kubectl create namespace fleet
kubectl -n fleet create secret generic fleet-secrets \
  --from-literal=OPENROUTER_API_KEY=sk-or-...

helm install fleet deploy/helm/fleet \
  --namespace fleet \
  --set image.repository=REGISTRY/fleet --set image.tag=v1 \
  --set sandbox.image=REGISTRY/fleet-sandbox:v1 \
  --set postgres.enabled=true \
  --set config.existingSecret=fleet-secrets
```

You build both images yourself — fleet publishes none. See the deployment
guide for the two Containerfiles.

## Honest scope

- **NetworkPolicy enforcement is the CNI's job.** The chart ships the deny-all
  object and fleet's preflight verifies it exists; on a CNI without
  NetworkPolicy support it seals nothing. Verify enforcement (the guide's
  checklist shows how).
- **One control-plane replica.** No HPA, no `replicas`. Scale work with more
  sandbox capacity, the control plane with bigger requests.
- The chart is linted in CI but not exercised against a live cluster there;
  the kind walkthrough in the guide is the verified path.

## Upgrading from a chart with a `web:` block

The separate Next.js web Deployment is gone (ADR-0091,
`docs/adr/0091-go-serves-the-browser-tier.md`): the fleet binary serves the web
UI itself on port 3000, the chart's Service exposes it as `web`, and the
Ingress routes there. On upgrade:

1. Build the fleet image from a checkout with `make build` (it builds the web
   bundle and embeds it; `fleet version --web` inside the image must print a
   build id, not `missing`). There is no web image to build any more.
2. Move the keys your web Secret carried into `config.existingSecret`:
   `APP_SESSION_SECRET` (required for sign-in; keep the same value and existing
   sessions survive the upgrade), and any `FLEET_OIDC_*` / `AUTH_*` keys.
   `CHAT_SERVER_TOKEN`, `CHAT_SERVER_URL` and `ORCHESTRATOR_SERVER_URL` are no
   longer read.
3. Delete the `web:` block from your values. A values file that still sets
   `web.enabled: true` fails `helm upgrade` with these steps.

