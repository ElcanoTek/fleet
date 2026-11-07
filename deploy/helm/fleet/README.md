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

# The chart attached to a release (fleet-<YYYY>.<MMDD>.<N>.tgz on the release
# page) installs that release's images. Verify it first, like any release file:
TAG=v2026.11.07.3 CHART=fleet-2026.1107.3.tgz   # https://github.com/ElcanoTek/fleet/releases
for f in SHA256SUMS SHA256SUMS.sigstore.json "$CHART"; do
  curl -fsSLO "https://github.com/ElcanoTek/fleet/releases/download/${TAG}/$f"
done
cosign verify-blob --bundle SHA256SUMS.sigstore.json \
  --certificate-identity https://github.com/ElcanoTek/fleet/.github/workflows/release.yml@refs/heads/main \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com SHA256SUMS
sha256sum -c --ignore-missing SHA256SUMS
helm install fleet "./$CHART" \
  --namespace fleet \
  --set postgres.enabled=true \
  --set config.existingSecret=fleet-secrets

# …or the chart from a checkout, told which release to run:
helm install fleet deploy/helm/fleet --namespace fleet --set image.tag="${TAG}" \
  --set postgres.enabled=true --set config.existingSecret=fleet-secrets
```

Both default to the images every release publishes, signed with keyless cosign
and carrying an SBOM and SLSA provenance (ADR-0100):
`ghcr.io/elcanotek/fleet` (the control plane, with the generic bundle) and
`ghcr.io/elcanotek/fleet-sandbox`, at the same release tag. `image.repository`
/ `image.tag` and `sandbox.image` still override them. The chart attached to a
release deploys both images BY DIGEST (`image.digest` and `sandbox.imageDigest`
are stamped in from the signed release); a chart from a checkout has no digests
and falls back to the tag — clear the digests to deploy by tag on purpose. For an image with your
client bundle baked in, build it `FROM` the published one. Verify the images
before you deploy them; the deployment guide shows the `cosign verify` line.

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

1. Use the published `ghcr.io/elcanotek/fleet` image (the default), or build
   yours `FROM` it — the binary in it embeds the web bundle (`fleet version
   --web` prints a build id). There is no web image any more.
2. Move the keys your web Secret carried into `config.existingSecret`:
   `APP_SESSION_SECRET` (required for sign-in; keep the same value and existing
   sessions survive the upgrade), and any `FLEET_OIDC_*` / `AUTH_*` keys.
   `CHAT_SERVER_TOKEN`, `CHAT_SERVER_URL` and `ORCHESTRATOR_SERVER_URL` are no
   longer read.
3. Delete the `web:` block from your values. A values file that still sets
   `web.enabled: true` fails `helm upgrade` with these steps.

