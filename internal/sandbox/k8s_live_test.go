package sandbox_test

// This opt-in suite uses a real Kubernetes apiserver and CNI. Unlike the fake
// apiserver tests, it verifies exec upgrades, PVC bytes, and egress enforcement.
// See docs/KUBERNETES-LIVE-TEST.md for the required disposable-cluster setup.

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/sandbox"
	"github.com/ElcanoTek/fleet/internal/tools"
)

func TestKubernetesLiveSandbox(t *testing.T) {
	kubeconfig := os.Getenv("FLEET_TEST_K8S_KUBECONFIG")
	if kubeconfig == "" {
		t.Skip("FLEET_TEST_K8S_KUBECONFIG is unset — skipping real-cluster integration")
	}
	required := func(name string) string {
		t.Helper()
		value := os.Getenv(name)
		if value == "" {
			t.Fatalf("%s is required for the real-cluster integration", name)
		}
		return value
	}
	workspace := required("FLEET_TEST_K8S_WORKSPACE")
	if !filepath.IsAbs(workspace) {
		t.Fatal("FLEET_TEST_K8S_WORKSPACE must be an absolute, shared PVC path")
	}
	image := required("FLEET_TEST_K8S_IMAGE")
	target := required("FLEET_TEST_K8S_EGRESS_TARGET") // controlled TCP service, host:port
	cfg := sandbox.KubernetesConfig{
		KubeconfigPath: kubeconfig,
		Namespace:      required("FLEET_TEST_K8S_NAMESPACE"),
		WorkspaceClaim: required("FLEET_TEST_K8S_WORKSPACE_CLAIM"),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	backend, err := sandbox.NewKubernetesBackend(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.Preflight(ctx); err != nil {
		t.Fatalf("live preflight: %v", err)
	}
	// A live API returning 404 for the required seal must fail closed.
	missing := cfg
	missing.NetworkPolicyName = "fleet-live-deliberately-missing-policy"
	badBackend, err := sandbox.NewKubernetesBackend(missing)
	if err != nil {
		t.Fatal(err)
	}
	if err := badBackend.Preflight(ctx); err == nil || !strings.Contains(err.Error(), "sealed-egress") {
		t.Fatalf("missing policy preflight = %v, want sealed-egress rejection", err)
	}

	pool := sandbox.NewPool(sandbox.PoolConfig{
		Mode:              sandbox.ModeKubernetes,
		KubernetesBackend: backend,
		BridgeScript:      tools.PythonBridgeScript(),
		Container:         sandbox.ContainerConfig{Image: image, WorkspaceHostDir: workspace},
	})
	defer pool.Close()
	for _, sealed := range []bool{false, true} {
		name := "open"
		if sealed {
			name = "sealed"
		}
		t.Run(name, func(t *testing.T) {
			var sb *sandbox.Sandbox
			var release func()
			if sealed {
				sb, release, err = pool.TakeContainer(ctx)
			} else {
				sb, release, err = pool.Take(ctx)
			}
			if err != nil {
				t.Fatal(err)
			}
			defer release()
			bash, err := sb.RunBash(ctx, sandbox.BashRequest{
				Command: "test \"$(id -u)\" = 1000 && test ! -e /var/run/secrets/kubernetes.io/serviceaccount/token && ! touch /fleet-live-root-write && printf 'ISOLATION_OK'",
				Timeout: 20 * time.Second,
			})
			if err != nil || bash.ExitCode != 0 || !strings.Contains(string(bash.Stdout), "ISOLATION_OK") {
				t.Fatalf("bash isolation: %+v, %v", bash, err)
			}
			py, err := sb.RunPython(ctx, sandbox.PythonRequest{Code: "print('PYTHON_OK', sum(range(10)))", Timeout: 30 * time.Second})
			if err != nil || py.Status != "success" || !strings.Contains(py.Stdout, "PYTHON_OK 45") {
				t.Fatalf("Python exec: %+v, %v", py, err)
			}

			file := filepath.Join(workspace, "fleet-live-"+name+".txt")
			t.Cleanup(func() { _ = os.Remove(file) })
			if err := sb.BindFileOpRoot(ctx, workspace); err != nil {
				t.Fatal(err)
			}
			written, err := sb.RunFileOp(ctx, sandbox.FileOpRequest{Op: sandbox.FileOpWrite, Root: workspace, Path: file, Data: []byte("before")})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := sb.RunFileOp(ctx, sandbox.FileOpRequest{Op: sandbox.FileOpEdit, Root: workspace, Path: file, OldText: "before", NewText: "after", ExpectedSHA256: written.SHA256}); err != nil {
				t.Fatal(err)
			}
			read, err := sb.RunFileOp(ctx, sandbox.FileOpRequest{Op: sandbox.FileOpRead, Root: workspace, Path: file})
			if err != nil || string(read.Data) != "after" {
				t.Fatalf("FileOp read: %q, %v", read.Data, err)
			}
			if host, err := os.ReadFile(file); err != nil || string(host) != "after" {
				t.Fatalf("PVC same-path host read: %q, %v", host, err)
			}
			if _, err := sb.RunFileOp(ctx, sandbox.FileOpRequest{Op: sandbox.FileOpRead, Root: workspace, Path: "/etc/passwd"}); err == nil {
				t.Fatal("FileOp accepted a path outside its policy root")
			}
			// Same reachable target for both modes: a sealed timeout only proves
			// isolation when the open control can connect to that exact service.
			quoted, _ := json.Marshal(target)
			code := "import socket\nhost, port = " + string(quoted) + ".rsplit(':', 1)\ntry:\n s = socket.create_connection((host, int(port)), timeout=3)\n s.close()\n print('EGRESS_OPEN')\nexcept OSError:\n print('EGRESS_BLOCKED')\n"
			check, err := sb.RunPython(ctx, sandbox.PythonRequest{Code: code, Timeout: 10 * time.Second})
			want := "EGRESS_OPEN"
			if sealed {
				want = "EGRESS_BLOCKED"
			}
			if err != nil || check.Status != "success" || !strings.Contains(check.Stdout, want) {
				t.Fatalf("CNI enforcement: got %+v, %v, want %s", check, err, want)
			}
		})
	}
}
