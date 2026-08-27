package sandbox

// Preflight + kubeconfig fail-closed tests for the kubernetes backend (#989).

import (
	"context"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestKubernetesBackendApiserverVersion pins the /readyz seam: the exported
// accessor must surface the same /version answer the preflight sees, so the
// readiness probe under this backend reports on the apiserver rather than on
// a podman binary the control-plane host does not have.
func TestKubernetesBackendApiserverVersion(t *testing.T) {
	fake := newFakeKube(t)
	backend := fake.backend(t, KubernetesConfig{Namespace: "fleet-sandboxes"})
	version, err := backend.ApiserverVersion(context.Background())
	if err != nil {
		t.Fatalf("ApiserverVersion: %v", err)
	}
	if version == "" {
		t.Fatal("ApiserverVersion returned an empty version string")
	}
}

func TestK8sPreflightHappyPath(t *testing.T) {
	fake := newFakeKube(t)
	backend := fake.backend(t, KubernetesConfig{Namespace: "fleet-sandboxes", RuntimeClassName: "kata"})
	if err := backend.Preflight(context.Background()); err != nil {
		t.Fatalf("Preflight: %v", err)
	}
}

func TestK8sPreflightFailClosed(t *testing.T) {
	t.Run("missing workspace claim", func(t *testing.T) {
		fake := newFakeKube(t)
		b, err := NewKubernetesBackend(KubernetesConfig{KubeconfigPath: fake.kubeconfigPath(t)})
		if err != nil {
			t.Fatal(err)
		}
		if err := b.Preflight(context.Background()); err == nil || !strings.Contains(err.Error(), "workspace PVC") {
			t.Errorf("want workspace-claim error, got %v", err)
		}
	})
	t.Run("rbac denial names the verb", func(t *testing.T) {
		fake := newFakeKube(t)
		fake.denied["create pods/exec"] = true
		backend := fake.backend(t, KubernetesConfig{})
		err := backend.Preflight(context.Background())
		if err == nil || !strings.Contains(err.Error(), "create pods/exec") {
			t.Errorf("want create pods/exec denial, got %v", err)
		}
	})
	t.Run("websocket exec verb is checked", func(t *testing.T) {
		// The regression this guards: the exec stream is a WebSocket upgrade,
		// i.e. an HTTP GET, so the apiserver authorizes `get pods/exec` — but
		// the preflight only ever asked for `create pods/exec`. A Role granting
		// just `create` sailed through boot and then 403'd on the very first
		// bash/run_python/fileop call, long after the operator was told the
		// cluster checked out.
		fake := newFakeKube(t)
		fake.denied["get pods/exec"] = true
		backend := fake.backend(t, KubernetesConfig{})
		err := backend.Preflight(context.Background())
		if err == nil || !strings.Contains(err.Error(), "get pods/exec") {
			t.Errorf("want get pods/exec denial, got %v", err)
		}
	})
	t.Run("missing pvc", func(t *testing.T) {
		fake := newFakeKube(t)
		fake.noPVC = true
		backend := fake.backend(t, KubernetesConfig{})
		if err := backend.Preflight(context.Background()); err == nil || !strings.Contains(err.Error(), "workspace PVC") {
			t.Errorf("want PVC error, got %v", err)
		}
	})
	t.Run("missing networkpolicy", func(t *testing.T) {
		fake := newFakeKube(t)
		fake.noNetpol = true
		backend := fake.backend(t, KubernetesConfig{})
		if err := backend.Preflight(context.Background()); err == nil || !strings.Contains(err.Error(), "NetworkPolicy") {
			t.Errorf("want NetworkPolicy error, got %v", err)
		}
	})
	t.Run("missing runtimeclass", func(t *testing.T) {
		fake := newFakeKube(t)
		fake.noRuntimeClass = true
		backend := fake.backend(t, KubernetesConfig{RuntimeClassName: "kata"})
		if err := backend.Preflight(context.Background()); err == nil || !strings.Contains(err.Error(), "RuntimeClass") {
			t.Errorf("want RuntimeClass error, got %v", err)
		}
	})
	t.Run("bad credentials", func(t *testing.T) {
		fake := newFakeKube(t)
		path := fake.kubeconfigPath(t)
		raw, _ := os.ReadFile(path)
		bad := strings.ReplaceAll(string(raw), fakeKubeToken, "wrong-token")
		if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
			t.Fatal(err)
		}
		b, err := NewKubernetesBackend(KubernetesConfig{KubeconfigPath: path, WorkspaceClaim: "ws"})
		if err != nil {
			t.Fatal(err)
		}
		if err := b.Preflight(context.Background()); err == nil || !strings.Contains(err.Error(), "unreachable or credentials rejected") {
			t.Errorf("want credentials error, got %v", err)
		}
	})
}

func TestK8sBackendDefaults(t *testing.T) {
	fake := newFakeKube(t)
	b, err := NewKubernetesBackend(KubernetesConfig{KubeconfigPath: fake.kubeconfigPath(t), WorkspaceClaim: "ws"})
	if err != nil {
		t.Fatal(err)
	}
	if got := b.Namespace(); got != defaultK8sNamespace {
		t.Errorf("default namespace = %q, want %q", got, defaultK8sNamespace)
	}
	if b.cfg.NetworkPolicyName != defaultK8sNetworkPolicy {
		t.Errorf("default networkpolicy = %q", b.cfg.NetworkPolicyName)
	}
	if b.StartTimeout() != defaultK8sStartTimeout {
		t.Errorf("default start timeout = %v", b.StartTimeout())
	}
}

func writeKubeconfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "kubeconfig")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestKubeconfigFailClosed(t *testing.T) {
	fake := newFakeKube(t)
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: fake.srv.Certificate().Raw})
	caB64 := base64.StdEncoding.EncodeToString(caPEM)

	t.Run("insecure-skip-tls-verify refused", func(t *testing.T) {
		path := writeKubeconfig(t, fmt.Sprintf(`current-context: c
contexts: [{name: c, context: {cluster: cl, user: u}}]
clusters: [{name: cl, cluster: {server: %s, insecure-skip-tls-verify: true}}]
users: [{name: u, user: {token: t}}]
`, fake.srv.URL))
		if _, _, err := newKubeconfigClient(path); err == nil || !strings.Contains(err.Error(), "insecure-skip-tls-verify") {
			t.Errorf("want insecure refusal, got %v", err)
		}
	})
	t.Run("exec plugin refused", func(t *testing.T) {
		path := writeKubeconfig(t, fmt.Sprintf(`current-context: c
contexts: [{name: c, context: {cluster: cl, user: u}}]
clusters: [{name: cl, cluster: {server: %s, certificate-authority-data: %s}}]
users: [{name: u, user: {exec: {command: aws}}}]
`, fake.srv.URL, caB64))
		if _, _, err := newKubeconfigClient(path); err == nil || !strings.Contains(err.Error(), "exec plugin") {
			t.Errorf("want exec-plugin refusal, got %v", err)
		}
	})
	t.Run("http server refused", func(t *testing.T) {
		path := writeKubeconfig(t, `current-context: c
contexts: [{name: c, context: {cluster: cl, user: u}}]
clusters: [{name: cl, cluster: {server: http://127.0.0.1:8080}}]
users: [{name: u, user: {token: t}}]
`)
		if _, _, err := newKubeconfigClient(path); err == nil || !strings.Contains(err.Error(), "requires TLS") {
			t.Errorf("want TLS refusal, got %v", err)
		}
	})
	t.Run("no credentials refused", func(t *testing.T) {
		path := writeKubeconfig(t, fmt.Sprintf(`current-context: c
contexts: [{name: c, context: {cluster: cl, user: u}}]
clusters: [{name: cl, cluster: {server: %s, certificate-authority-data: %s}}]
users: [{name: u, user: {}}]
`, fake.srv.URL, caB64))
		if _, _, err := newKubeconfigClient(path); err == nil || !strings.Contains(err.Error(), "no supported credentials") {
			t.Errorf("want no-credentials refusal, got %v", err)
		}
	})
	t.Run("context namespace surfaces", func(t *testing.T) {
		path := writeKubeconfig(t, fmt.Sprintf(`current-context: c
contexts: [{name: c, context: {cluster: cl, user: u, namespace: my-ns}}]
clusters: [{name: cl, cluster: {server: %s, certificate-authority-data: %s}}]
users: [{name: u, user: {token: t}}]
`, fake.srv.URL, caB64))
		_, ns, err := newKubeconfigClient(path)
		if err != nil || ns != "my-ns" {
			t.Errorf("namespace = %q, %v; want my-ns", ns, err)
		}
	})
	t.Run("missing current-context refused", func(t *testing.T) {
		path := writeKubeconfig(t, "contexts: []\n")
		if _, _, err := newKubeconfigClient(path); err == nil || !strings.Contains(err.Error(), "current-context") {
			t.Errorf("want current-context error, got %v", err)
		}
	})
}
