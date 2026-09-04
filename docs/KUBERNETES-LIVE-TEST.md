# Real-cluster sandbox integration test

`TestKubernetesLiveSandbox` complements the fake-apiserver suite with real
Kubernetes exec streams, shared PVC file operations, pod isolation settings,
and an enforced egress policy. It uses no model or model credentials. Normal
`make test` skips it unless `FLEET_TEST_K8S_KUBECONFIG` is set.

The test requires a **disposable namespace and workspace**, a built sandbox
image, and a CNI that enforces NetworkPolicy. It creates and removes sandbox
pods. Its controlled TCP target must be reachable from an open sandbox and
unreachable from a sealed sandbox; a cluster that merely accepts policy
objects cannot pass. It also verifies that a missing sealed-egress policy
fails preflight.

## Local kind rehearsal

Install Podman, kind, kubectl and Go. Build `localhost/fleet-sandbox:latest`
with `scripts/build-sandbox-image.sh`. The example below uses rootless Podman
and Calico; kind's default CNI does not enforce the required policies.

Create a fresh directory and cluster, keeping all credentials outside the repo:

```sh
live_root=$(mktemp -d)
mkdir "$live_root/workspace"
# This disposable leaf must be writable by the nested pod's mapped UID 1000.
chmod 0777 "$live_root/workspace"
cat > "$live_root/kind.yaml" <<EOF_KIND
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
networking:
  disableDefaultCNI: true
  podSubnet: 192.168.0.0/16
nodes:
- role: control-plane
  extraMounts:
  - hostPath: $live_root/workspace
    containerPath: /fleet-review-workspace
EOF_KIND
export KIND_EXPERIMENTAL_PROVIDER=podman
systemd-run --user --scope -p Delegate=yes kind create cluster \
  --name fleet-review --config "$live_root/kind.yaml" \
  --kubeconfig "$live_root/kubeconfig"
export KUBECONFIG="$live_root/kubeconfig"
kubectl apply --server-side -f \
  https://raw.githubusercontent.com/projectcalico/calico/v3.32.2/manifests/calico.yaml
kubectl -n kube-system rollout status daemonset/calico-node --timeout=180s
podman save --format docker-archive -o "$live_root/sandbox.tar" localhost/fleet-sandbox:latest
kind load image-archive "$live_root/sandbox.tar" --name fleet-review
kubectl apply -f internal/sandbox/testdata/k8s-live.yaml
kubectl -n fleet-sandbox-test wait pod/egress-target --for=condition=Ready --timeout=120s
```

The fixture contains a static hostPath PV for this single-node rehearsal, a
PVC, narrow runner RBAC, both required NetworkPolicies, and a TCP target. The
open-egress policy allows only that target. It is test infrastructure, **not a
production storage or egress configuration**.

Create a separate, short-lived runner kubeconfig. This preserves the cluster
CA and endpoint while removing the administrator credentials. The token is
written to a host-only file, never a sandbox environment or log:

```sh
export FLEET_TEST_K8S_KUBECONFIG="$live_root/runner-kubeconfig"
python3 - <<'PY'
import json, os, subprocess
config = json.loads(subprocess.check_output(["kubectl", "config", "view", "--raw", "-o", "json"]))
token = subprocess.check_output(["kubectl", "-n", "fleet-sandbox-test", "create", "token", "fleet-runner", "--duration=1h"], text=True).strip()
config["users"] = [{"name": "runner", "user": {"token": token}}]
for item in config["contexts"]:
    item["context"]["user"] = "runner"
    item["context"]["namespace"] = "fleet-sandbox-test"
fd = os.open(os.environ["FLEET_TEST_K8S_KUBECONFIG"], os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
with os.fdopen(fd, "w") as output:
    json.dump(config, output)
PY
export FLEET_TEST_K8S_NAMESPACE=fleet-sandbox-test
export FLEET_TEST_K8S_WORKSPACE_CLAIM=workspace
export FLEET_TEST_K8S_WORKSPACE="$live_root/workspace"
export FLEET_TEST_K8S_IMAGE=localhost/fleet-sandbox:latest
export FLEET_TEST_K8S_EGRESS_TARGET="$(kubectl -n fleet-sandbox-test get service egress-target -o jsonpath='{.spec.clusterIP}'):8080"
podman unshare go test -p 1 -tags fleet_host_executor ./internal/sandbox \
  -run '^TestKubernetesLiveSandbox$' -count=1 -v
kubectl -n fleet-sandbox-test get pods -l app.kubernetes.io/name=fleet-sandbox
```

`podman unshare` aligns the test process with the rootless node's UID mapping:
FileOp creates private files owned by pod UID 1000, which is a subordinate UID
on the outer host. A regular outer-host process cannot read those private
files. On an ordinary cluster, run the test from a control-plane environment
with the same PVC and ownership as the sandbox pods and omit `podman unshare`.

After saving the test output, delete only this disposable cluster and its
fixture files:

```sh
kind delete cluster --name fleet-review
podman unshare rm -rf -- "$live_root"
```

This covers the sandbox backend and policy enforcement, not Helm installation,
Ingress, the complete fleet control plane, multi-node RWX storage, or a managed
cluster's identity integration. Rootless prerequisites vary; see the
[kind rootless guide](https://kind.sigs.k8s.io/docs/user/rootless/) and
[Calico kind guide](https://docs.tigera.io/calico/latest/getting-started/kubernetes/kind).
