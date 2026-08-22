package sandbox

// Tests for the kubernetes sandbox backend (#989): pod lifecycle, exec
// transport, the #796 poison-and-retire containment, fileops through the real
// executor, the pool routing, and the orphan sweep — all against the fake
// apiserver in k8s_fake_test.go.

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func testContainerConfig(t *testing.T) ContainerConfig {
	t.Helper()
	return ContainerConfig{
		Image:            "registry.example/fleet-sandbox:test",
		WorkspaceHostDir: t.TempDir(),
		BridgeScript:     []byte("# fake bridge\n"),
	}
}

func TestK8sResolveBackend(t *testing.T) {
	cases := []struct {
		env, bundle, want string
		wantErr           bool
	}{
		{"", "", BackendPodman, false},
		{"podman", "kubernetes", BackendPodman, false},
		{"", "kubernetes", BackendKubernetes, false},
		{"KUBERNETES", "", BackendKubernetes, false},
		{" kubernetes ", "", BackendKubernetes, false},
		{"", "docker", "", true},
		{"k8s", "", "", true},
	}
	for _, tc := range cases {
		got, err := ResolveBackend(tc.env, tc.bundle)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ResolveBackend(%q,%q): want error, got %q", tc.env, tc.bundle, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("ResolveBackend(%q,%q) = %q, %v; want %q", tc.env, tc.bundle, got, err, tc.want)
		}
	}
}

func TestK8sPodSpecHardening(t *testing.T) {
	cfg := testContainerConfig(t)
	cfg.NoNetwork = true
	kcfg := KubernetesConfig{
		Namespace:       "fleet-sandboxes",
		WorkspaceClaim:  "fleet-workspace",
		ServiceAccount:  "fleet-sandbox",
		ImagePullSecret: "regcred",
	}
	pod, err := buildSandboxPod(applyContainerDefaults(cfg), kcfg, "fleet-sandbox-abc")
	if err != nil {
		t.Fatalf("buildSandboxPod: %v", err)
	}

	// These pins are the k8s counterpart of podman_args_test.go: a change that
	// weakens the pod hardening must show up as a failing assertion, not slip
	// through a spec refactor.
	spec := pod.Spec
	if spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken {
		t.Error("automountServiceAccountToken must be explicitly false — a sandbox must never hold apiserver credentials")
	}
	if spec.SecurityContext == nil || spec.SecurityContext.RunAsNonRoot == nil || !*spec.SecurityContext.RunAsNonRoot {
		t.Error("runAsNonRoot must be true")
	}
	if spec.SecurityContext.SeccompProfile == nil || spec.SecurityContext.SeccompProfile.Type != "RuntimeDefault" {
		t.Error("seccompProfile must default to RuntimeDefault")
	}
	c := spec.Containers[0]
	if c.SecurityContext == nil || c.SecurityContext.ReadOnlyRootFilesystem == nil || !*c.SecurityContext.ReadOnlyRootFilesystem {
		t.Error("readOnlyRootFilesystem must be true")
	}
	if c.SecurityContext.AllowPrivilegeEscalation == nil || *c.SecurityContext.AllowPrivilegeEscalation {
		t.Error("allowPrivilegeEscalation must be false")
	}
	if c.SecurityContext.Capabilities == nil || len(c.SecurityContext.Capabilities.Drop) != 1 || c.SecurityContext.Capabilities.Drop[0] != "ALL" {
		t.Error("capabilities must drop ALL")
	}
	if c.WorkingDir != cfg.WorkspaceHostDir {
		t.Errorf("workingDir = %q, want the workspace root %q", c.WorkingDir, cfg.WorkspaceHostDir)
	}
	// Same-path workspace mount — the invariant that keeps MCP paths valid.
	foundWorkspace := false
	for _, m := range c.VolumeMounts {
		if m.Name == "workspace" {
			foundWorkspace = true
			if m.MountPath != cfg.WorkspaceHostDir {
				t.Errorf("workspace mounted at %q, want same-path %q", m.MountPath, cfg.WorkspaceHostDir)
			}
		}
	}
	if !foundWorkspace {
		t.Error("workspace volume mount missing")
	}
	// Limit conversions: 512m podman → bytes; 1.0 cpus → 1000m; disk 5 → 5Gi.
	if got := c.Resources.Limits["memory"]; got != "536870912" {
		t.Errorf("memory limit = %q, want 536870912 (512 MiB in bytes)", got)
	}
	if got := c.Resources.Limits["cpu"]; got != "1000m" {
		t.Errorf("cpu limit = %q, want 1000m", got)
	}
	if got := c.Resources.Limits["ephemeral-storage"]; got != "5Gi" {
		t.Errorf("ephemeral-storage limit = %q, want 5Gi", got)
	}
	// Sealed posture label + ownership labels.
	if got := pod.Metadata.Labels[k8sLabelEgress]; got != "none" {
		t.Errorf("egress label = %q, want none for NoNetwork", got)
	}
	if got := pod.Metadata.Labels[k8sLabelInstance]; got != k8sInstanceLabel {
		t.Errorf("instance label = %q, want %q", got, k8sInstanceLabel)
	}
	if len(spec.ImagePullSecrets) != 1 || spec.ImagePullSecrets[0].Name != "regcred" {
		t.Error("imagePullSecrets not carried")
	}
	if spec.ServiceAccountName != "fleet-sandbox" {
		t.Error("serviceAccountName not carried")
	}
}

func TestK8sPodSpecVariants(t *testing.T) {
	cfg := applyContainerDefaults(testContainerConfig(t))
	kcfg := KubernetesConfig{Namespace: "ns", WorkspaceClaim: "ws", RuntimeClassName: "kata", SeccompLocalhostProfile: "profiles/fleet.json"}
	pod, err := buildSandboxPod(cfg, kcfg, "fleet-sandbox-x")
	if err != nil {
		t.Fatalf("buildSandboxPod: %v", err)
	}
	if got := pod.Metadata.Labels[k8sLabelEgress]; got != "open" {
		t.Errorf("egress label = %q, want open without NoNetwork", got)
	}
	if pod.Spec.RuntimeClassName == nil || *pod.Spec.RuntimeClassName != "kata" {
		t.Error("runtimeClassName not carried")
	}
	sp := pod.Spec.SecurityContext.SeccompProfile
	if sp.Type != "Localhost" || sp.LocalhostProfile == nil || *sp.LocalhostProfile != "profiles/fleet.json" {
		t.Errorf("seccomp profile = %+v, want Localhost profiles/fleet.json", sp)
	}

	// A negative disk limit disables the ephemeral-storage cap.
	cfg.DiskLimitGB = -1
	pod, err = buildSandboxPod(cfg, kcfg, "fleet-sandbox-y")
	if err != nil {
		t.Fatalf("buildSandboxPod: %v", err)
	}
	if _, ok := pod.Spec.Containers[0].Resources.Limits["ephemeral-storage"]; ok {
		t.Error("negative DiskLimitGB must not emit an ephemeral-storage limit")
	}
}

func TestK8sQuantityConversions(t *testing.T) {
	if _, err := k8sQuantityFromPodmanMemory("512x"); err == nil {
		t.Error("bad memory suffix must error")
	}
	if got, _ := k8sQuantityFromPodmanMemory("2g"); got != "2147483648" {
		t.Errorf("2g = %q", got)
	}
	if got, _ := k8sQuantityFromPodmanCPU("2.50"); got != "2500m" {
		t.Errorf("2.50 cpus = %q", got)
	}
	if _, err := k8sQuantityFromPodmanCPU("zero"); err == nil {
		t.Error("bad cpu must error")
	}
}

func TestK8sInstanceLabelRoundTrip(t *testing.T) {
	pid, start, ok := parseK8sInstanceLabel(k8sInstanceLabel)
	if !ok || pid != os.Getpid() || start <= 0 {
		t.Fatalf("parseK8sInstanceLabel(%q) = %d, %d, %v", k8sInstanceLabel, pid, start, ok)
	}
	if _, _, ok := parseK8sInstanceLabel("garbage"); ok {
		t.Error("garbage label must not parse")
	}
}

func TestK8sBashExec(t *testing.T) {
	fake := newFakeKube(t)
	fake.bashBehaviors["echo hi"] = func(_ string, stdout, _ io.Writer, _ *websocket.Conn) int {
		_, _ = stdout.Write([]byte("hi\n"))
		return 0
	}
	fake.bashBehaviors["exit 3"] = func(_ string, _, stderr io.Writer, _ *websocket.Conn) int {
		_, _ = stderr.Write([]byte("boom"))
		return 3
	}
	backend := fake.backend(t, KubernetesConfig{Namespace: "fleet-sandboxes"})
	sb, err := backend.newSandbox(context.Background(), testContainerConfig(t))
	if err != nil {
		t.Fatalf("newSandbox: %v", err)
	}
	defer sb.Close()
	if got := sb.ModeName(); got != "kubernetes" {
		t.Errorf("ModeName = %q", got)
	}

	res, err := sb.RunBash(context.Background(), BashRequest{Command: "echo hi"})
	if err != nil {
		t.Fatalf("RunBash: %v", err)
	}
	if res.ExitCode != 0 || string(res.Stdout) != "hi\n" {
		t.Errorf("RunBash = exit %d stdout %q", res.ExitCode, res.Stdout)
	}

	res, err = sb.RunBash(context.Background(), BashRequest{Command: "exit 3", WorkingDir: "/some/dir"})
	if err != nil {
		t.Fatalf("RunBash exit 3: %v", err)
	}
	if res.ExitCode != 3 || string(res.Stderr) != "boom" {
		t.Errorf("RunBash exit3 = exit %d stderr %q", res.ExitCode, res.Stderr)
	}
	fake.mu.Lock()
	workdir := fake.lastBashWorkdir
	fake.mu.Unlock()
	if workdir != "/some/dir" {
		t.Errorf("workdir wrapper carried %q, want /some/dir", workdir)
	}
}

func TestK8sBashCancelPoisonsAndDeletesPod(t *testing.T) {
	fake := newFakeKube(t)
	started := make(chan struct{}, 1)
	fake.bashBehaviors["block"] = func(_ string, _, _ io.Writer, conn *websocket.Conn) int {
		started <- struct{}{}
		// Simulate a process that never exits: hold until the client closes.
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return 0
			}
		}
	}
	backend := fake.backend(t, KubernetesConfig{Namespace: "fleet-sandboxes"})
	sb, err := backend.newSandbox(context.Background(), testContainerConfig(t))
	if err != nil {
		t.Fatalf("newSandbox: %v", err)
	}
	defer sb.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		cancel()
	}()
	res, err := sb.RunBash(ctx, BashRequest{Command: "block", Timeout: time.Minute})
	if err != nil {
		t.Fatalf("RunBash: %v", err)
	}
	if !res.Cancelled || res.TimedOut {
		t.Errorf("want Cancelled, got %+v", res)
	}
	if !res.SandboxRetired || !res.CleanupConfirmed {
		t.Errorf("want SandboxRetired+CleanupConfirmed, got %+v", res)
	}
	if !sb.Poisoned() {
		t.Error("sandbox must be poisoned after a cancelled bash call")
	}
	fake.mu.Lock()
	deleted := len(fake.deleted)
	fake.mu.Unlock()
	if deleted == 0 {
		t.Error("cancelled bash must delete the pod (the #796 containment)")
	}
	if _, err := sb.RunBash(context.Background(), BashRequest{Command: "echo hi"}); !errors.Is(err, ErrPoisoned) {
		t.Errorf("post-poison RunBash err = %v, want ErrPoisoned", err)
	}
}

func TestK8sRunPythonBridge(t *testing.T) {
	fake := newFakeKube(t)
	backend := fake.backend(t, KubernetesConfig{Namespace: "fleet-sandboxes"})
	sb, err := backend.newSandbox(context.Background(), testContainerConfig(t))
	if err != nil {
		t.Fatalf("newSandbox: %v", err)
	}
	defer sb.Close()

	res, err := sb.RunPython(context.Background(), PythonRequest{Code: "1+1"})
	if err != nil {
		t.Fatalf("RunPython: %v", err)
	}
	if res.Status != "ok" || res.Result != "ran: 1+1" {
		t.Errorf("RunPython = %+v", res)
	}
	// Second call reuses the session.
	res, err = sb.RunPython(context.Background(), PythonRequest{Code: "2+2"})
	if err != nil {
		t.Fatalf("RunPython second: %v", err)
	}
	if res.Result != "ran: 2+2" {
		t.Errorf("RunPython second = %+v", res)
	}
}

func TestK8sFileOpsThroughRealExecutor(t *testing.T) {
	if !pythonAvailable() {
		t.Skip("python3 not available on the test host")
	}
	fake := newFakeKube(t)
	backend := fake.backend(t, KubernetesConfig{Namespace: "fleet-sandboxes"})
	cfg := testContainerConfig(t)
	cfg.BridgeScript = []byte("# unused\n")
	sb, err := backend.newSandbox(context.Background(), cfg)
	if err != nil {
		t.Fatalf("newSandbox: %v", err)
	}
	defer sb.Close()

	// The fake runs the UPLOADED fileops.py on the host, so the workspace dir
	// is a real host directory here.
	root := filepath.Join(cfg.WorkspaceHostDir, "conv1")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := sb.BindFileOpRoot(context.Background(), root); err != nil {
		t.Fatalf("BindFileOpRoot: %v", err)
	}
	target := filepath.Join(root, "hello.txt")
	if _, err := sb.RunFileOp(context.Background(), FileOpRequest{Op: FileOpWrite, Path: target, Root: root, Data: []byte("hello k8s\n")}); err != nil {
		t.Fatalf("write: %v", err)
	}
	res, err := sb.RunFileOp(context.Background(), FileOpRequest{Op: FileOpRead, Path: target, Root: root, Limit: 1024})
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(res.Data) != "hello k8s\n" {
		t.Errorf("read back %q", res.Data)
	}
	// A root outside every mount is refused before any exec happens.
	if _, err := sb.RunFileOp(context.Background(), FileOpRequest{Op: FileOpRead, Path: "/etc/passwd", Root: "/etc"}); !errors.Is(err, ErrFileOpUnsafePath) {
		t.Errorf("outside-mount fileop err = %v, want ErrFileOpUnsafePath", err)
	}
}

func TestK8sPoolRouting(t *testing.T) {
	fake := newFakeKube(t)
	fake.bashBehaviors["echo pool"] = func(_ string, stdout, _ io.Writer, _ *websocket.Conn) int {
		_, _ = stdout.Write([]byte("pool\n"))
		return 0
	}
	backend := fake.backend(t, KubernetesConfig{Namespace: "fleet-sandboxes"})
	pool := NewPool(PoolConfig{
		Mode:              ModeKubernetes,
		KubernetesBackend: backend,
		BridgeScript:      []byte("# bridge\n"),
		Container:         testContainerConfig(t),
	})
	defer pool.Close()

	sb, cleanup, err := pool.Take(context.Background())
	if err != nil {
		t.Fatalf("Take: %v", err)
	}
	res, err := sb.RunBash(context.Background(), BashRequest{Command: "echo pool"})
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("RunBash via pool: %v, %+v", err, res)
	}
	cleanup()

	// Lockdown take: sealed pods come from the same backend.
	sb, cleanup, err = pool.TakeContainer(context.Background())
	if err != nil {
		t.Fatalf("TakeContainer: %v", err)
	}
	_ = sb
	cleanup()

	// Allowlisted egress is refused, fail-closed.
	if _, _, err := pool.TakeContainerWithEgress(context.Background(), ResourceOverride{}, []string{"example.com"}); err == nil {
		t.Error("TakeContainerWithEgress must fail closed under the kubernetes backend")
	}
}

func TestK8sResourceOverridesReachPodSpec(t *testing.T) {
	fake := newFakeKube(t)
	backend := fake.backend(t, KubernetesConfig{Namespace: "fleet-sandboxes"})
	pool := NewPool(PoolConfig{
		Mode:              ModeKubernetes,
		KubernetesBackend: backend,
		BridgeScript:      []byte("# bridge\n"),
		Container:         testContainerConfig(t),
	})
	defer pool.Close()

	sb, cleanup, err := pool.TakeContainerWithOverrides(context.Background(), ResourceOverride{MemoryLimit: "1024m", CPULimit: "2.00"}, true)
	if err != nil {
		t.Fatalf("TakeContainerWithOverrides: %v", err)
	}
	defer cleanup()
	_ = sb

	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.pods) != 1 {
		t.Fatalf("want 1 pod, have %d", len(fake.pods))
	}
	for _, pod := range fake.pods {
		limits := pod.Spec.Containers[0].Resources.Limits
		if limits["memory"] != "1073741824" {
			t.Errorf("override memory = %q, want 1073741824", limits["memory"])
		}
		if limits["cpu"] != "2000m" {
			t.Errorf("override cpu = %q, want 2000m", limits["cpu"])
		}
		if pod.Metadata.Labels[k8sLabelEgress] != "none" {
			t.Errorf("sealed take must label egress=none, got %q", pod.Metadata.Labels[k8sLabelEgress])
		}
	}
}

func TestK8sCloseDeletesPod(t *testing.T) {
	fake := newFakeKube(t)
	backend := fake.backend(t, KubernetesConfig{Namespace: "fleet-sandboxes"})
	sb, err := backend.newSandbox(context.Background(), testContainerConfig(t))
	if err != nil {
		t.Fatalf("newSandbox: %v", err)
	}
	sb.Close()
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.pods) != 0 || len(fake.deleted) != 1 {
		t.Errorf("Close must delete the pod: %d live, %d deleted", len(fake.pods), len(fake.deleted))
	}
}

func TestK8sPruneOrphanedPods(t *testing.T) {
	fake := newFakeKube(t)
	backend := fake.backend(t, KubernetesConfig{Namespace: "fleet-sandboxes"})

	addPod := func(name, instance string) {
		fake.mu.Lock()
		fake.pods[name] = &k8sPod{Metadata: k8sObjectMeta{
			Name: name,
			Labels: map[string]string{
				k8sLabelName:      k8sLabelNameValue,
				k8sLabelManagedBy: k8sLabelManagedVal,
				k8sLabelInstance:  instance,
			},
		}}
		fake.mu.Unlock()
	}
	addPod("fleet-sandbox-own", k8sInstanceLabel)     // this process — never touched
	addPod("fleet-sandbox-dead", "p999999999-t12345") // dead owner — pruned
	// Unlabeled/unparseable ownership: fails "still running" (unparseable pid),
	// so it is pruned — in k8s a pod without a live owner in THIS process tree
	// is a leftover by construction (single-replica invariant).
	addPod("fleet-sandbox-mystery", "not-a-label")

	n, err := backend.PruneOrphanedPods(context.Background())
	if err != nil {
		t.Fatalf("PruneOrphanedPods: %v", err)
	}
	if n != 2 {
		t.Errorf("pruned %d, want 2", n)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if _, ok := fake.pods["fleet-sandbox-own"]; !ok {
		t.Error("own pod must never be pruned")
	}
	if _, ok := fake.pods["fleet-sandbox-dead"]; ok {
		t.Error("dead-owner pod must be pruned")
	}
}

func TestK8sProxyURLRefused(t *testing.T) {
	fake := newFakeKube(t)
	backend := fake.backend(t, KubernetesConfig{Namespace: "fleet-sandboxes"})
	cfg := testContainerConfig(t)
	cfg.ProxyURL = "http://127.0.0.1:9999"
	if _, err := backend.newSandbox(context.Background(), cfg); err == nil {
		t.Error("a ProxyURL (allowlisted egress) must be refused by the kubernetes backend")
	}
}

func TestK8sBridgeUploadVerified(t *testing.T) {
	fake := newFakeKube(t)
	backend := fake.backend(t, KubernetesConfig{Namespace: "fleet-sandboxes"})
	cfg := testContainerConfig(t)
	sb, err := backend.newSandbox(context.Background(), cfg)
	if err != nil {
		t.Fatalf("newSandbox: %v", err)
	}
	defer sb.Close()
	fake.mu.Lock()
	defer fake.mu.Unlock()
	var podName string
	for name := range fake.pods {
		podName = name
	}
	if got := fake.files[podName+":"+k8sBridgePath]; string(got) != string(cfg.BridgeScript) {
		t.Errorf("bridge upload = %q, want %q", got, cfg.BridgeScript)
	}
	if got := fake.files[podName+":"+k8sFileOpsPath]; len(got) == 0 || !strings.Contains(string(got), "fileops.py") {
		t.Errorf("fileops upload missing or wrong (%d bytes)", len(got))
	}
}
