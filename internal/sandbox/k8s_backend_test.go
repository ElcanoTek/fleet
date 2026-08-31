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

func TestK8sSchedulingKnobs(t *testing.T) {
	// Parse helpers fail closed on malformed input.
	if _, err := ParseK8sNodeSelector("pool"); err == nil {
		t.Error("bare key without =value must error")
	}
	if _, err := ParseK8sNodeSelector("=v"); err == nil {
		t.Error("empty key must error")
	}
	sel, err := ParseK8sNodeSelector(" pool=sandboxes, arch=amd64 ")
	if err != nil || sel["pool"] != "sandboxes" || sel["arch"] != "amd64" {
		t.Errorf("ParseK8sNodeSelector = %v, %v", sel, err)
	}
	if _, err := ParseK8sTolerations(`[{"unknown":"field"}]`); err == nil {
		t.Error("unknown toleration field must error (strict decoding)")
	}
	tols, err := ParseK8sTolerations(`[{"key":"fleet.elcanotek.com/sandbox","operator":"Exists","effect":"NoSchedule"}]`)
	if err != nil || len(tols) != 1 || tols[0].Key != "fleet.elcanotek.com/sandbox" {
		t.Errorf("ParseK8sTolerations = %+v, %v", tols, err)
	}

	// They reach the pod spec, and the pull policy is explicit (the API
	// default for a :latest tag is Always, which breaks side-loaded images).
	cfg := applyContainerDefaults(testContainerConfig(t))
	pod, err := buildSandboxPod(cfg, KubernetesConfig{
		Namespace: "ns", WorkspaceClaim: "ws",
		NodeSelector: sel, Tolerations: tols,
	}, "fleet-sandbox-sched")
	if err != nil {
		t.Fatalf("buildSandboxPod: %v", err)
	}
	if pod.Spec.NodeSelector["pool"] != "sandboxes" {
		t.Errorf("nodeSelector not carried: %v", pod.Spec.NodeSelector)
	}
	if len(pod.Spec.Tolerations) != 1 || pod.Spec.Tolerations[0].Effect != "NoSchedule" {
		t.Errorf("tolerations not carried: %+v", pod.Spec.Tolerations)
	}
	if got := pod.Spec.Containers[0].ImagePullPolicy; got != "IfNotPresent" {
		t.Errorf("imagePullPolicy = %q, want explicit IfNotPresent", got)
	}
}

func TestK8sSanitizeClusterText(t *testing.T) {
	// Cluster/pod-derived text is newline-stripped before it can enter a
	// logged error — a pod printing "\nFAKE LOG LINE" to stderr must not be
	// able to forge journal entries (go/log-injection).
	got := sanitizeClusterText("line one\r\nFAKE LOG LINE\ntail")
	if strings.ContainsAny(got, "\r\n") {
		t.Errorf("sanitizeClusterText left line breaks in %q", got)
	}
	if got != "line one  FAKE LOG LINE tail" {
		t.Errorf("sanitizeClusterText = %q", got)
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

// End to end through the fake apiserver + the real fileops.py: when the
// operator declares the bundle's doc dirs are in the sandbox image (the agent
// layer then keeps them in ReadOnlyMounts), a read of one resolves and a write
// beneath it is refused. This is the whole point of
// bundle_docs_in_image — view_file on `protocols/…` working in a pod — and it
// must not come with a write path.
func TestK8sFileOpsReadBundleDocsWhenDeclaredInImage(t *testing.T) {
	if !pythonAvailable() {
		t.Skip("python3 not available on the test host")
	}
	// Stands in for the path the sandbox image carries the bundle doc dir at;
	// the fake execs on the host, so the same path must exist here.
	docs := filepath.Join(t.TempDir(), "client", "protocols")
	if err := os.MkdirAll(docs, 0o755); err != nil {
		t.Fatal(err)
	}
	protocol := filepath.Join(docs, "deal-creation.yaml")
	if err := os.WriteFile(protocol, []byte("steps: [prepare, confirm, create]\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	fake := newFakeKube(t)
	backend := fake.backend(t, KubernetesConfig{Namespace: "fleet-sandboxes"})
	cfg := testContainerConfig(t)
	cfg.BridgeScript = []byte("# unused\n")
	cfg.ReadOnlyMounts = []string{docs}
	sb, err := backend.newSandbox(context.Background(), cfg)
	if err != nil {
		t.Fatalf("newSandbox: %v", err)
	}
	defer sb.Close()

	res, err := sb.RunFileOp(context.Background(), FileOpRequest{Op: FileOpRead, Path: protocol, Root: docs, Limit: 1024})
	if err != nil {
		t.Fatalf("read declared bundle doc: %v", err)
	}
	if !strings.Contains(string(res.Data), "prepare") {
		t.Errorf("read back %q", res.Data)
	}
	// Read-only means read-only: the declaration re-admits reads, never writes.
	if _, err := sb.RunFileOp(context.Background(), FileOpRequest{Op: FileOpWrite, Path: protocol, Root: docs, Data: []byte("tampered\n")}); !errors.Is(err, ErrFileOpUnsafePath) {
		t.Errorf("write beneath a declared doc root err = %v, want ErrFileOpUnsafePath", err)
	}
	if err := sb.BindFileOpRoot(context.Background(), docs); !errors.Is(err, ErrFileOpUnsafePath) {
		t.Errorf("binding a declared doc root as writable err = %v, want ErrFileOpUnsafePath", err)
	}
	// Without the declaration the agent layer passes no mounts, and the same
	// read is refused by the anchor before any exec.
	bare := testContainerConfig(t)
	bare.BridgeScript = []byte("# unused\n")
	sb2, err := backend.newSandbox(context.Background(), bare)
	if err != nil {
		t.Fatalf("newSandbox (no mounts): %v", err)
	}
	defer sb2.Close()
	if _, err := sb2.RunFileOp(context.Background(), FileOpRequest{Op: FileOpRead, Path: protocol, Root: docs, Limit: 1024}); !errors.Is(err, ErrFileOpUnsafePath) {
		t.Errorf("undeclared doc read err = %v, want ErrFileOpUnsafePath", err)
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

// TestK8sInstanceLabelPerProcess pins the property the orphan sweep rests on:
// two incarnations inside the SAME pod — what kubelet produces when it
// restarts a crashed container in place — must not share an instance label.
// They did when the label was the pod UID alone, and the sweep then skipped
// every pod the crashed process had left running.
func TestK8sInstanceLabelPerProcess(t *testing.T) {
	const uid = "6c1e3aac-7749-443d-a50d-0060b4bfb69f"
	first := k8sInstanceLabelFor(uid, "aaaaaaaa")
	second := k8sInstanceLabelFor(uid, "bbbbbbbb")
	if first == second {
		t.Fatalf("same pod UID must still yield distinct incarnation labels, got %q twice", first)
	}
	for _, label := range []string{first, second} {
		if len(label) > 63 {
			t.Errorf("instance label %q is %d chars, over the 63-char label limit", label, len(label))
		}
		if !strings.Contains(label, uid) {
			t.Errorf("instance label %q should carry the pod UID for diagnosis", label)
		}
	}
	// An over-long UID is trimmed, never the nonce: the nonce is what makes
	// the value unique, and the value must stay a legal label.
	long := k8sInstanceLabelFor(strings.Repeat("f", 120), "aaaaaaaa")
	if len(long) > 63 {
		t.Errorf("over-long UID must be trimmed to fit, got %d chars", len(long))
	}
	if !strings.HasSuffix(long, "aaaaaaaa") {
		t.Errorf("the nonce must survive trimming, got %q", long)
	}
	// Out of cluster there is no pod identity, so the pid form is unchanged.
	if _, _, ok := parseK8sInstanceLabel(k8sInstanceLabelFor("", "aaaaaaaa")); !ok {
		t.Error("empty uid must keep the parseable out-of-cluster pid form")
	}
}

// TestK8sPruneSweepsPredecessorInSamePod is the regression for the in-place
// container restart: same pod, same UID, previous process. Those pods are
// Guaranteed QoS, so leaving them running strands their full CPU/memory
// reservation with nothing left to reclaim it.
func TestK8sPruneSweepsPredecessorInSamePod(t *testing.T) {
	const uid = "6c1e3aac-7749-443d-a50d-0060b4bfb69f"
	prevOwner, prevLabel := k8sControlPlaneOwner, k8sInstanceLabel
	k8sControlPlaneOwner = "larkspur"
	k8sInstanceLabel = k8sInstanceLabelFor(uid, "bbbbbbbb")
	t.Cleanup(func() {
		k8sControlPlaneOwner, k8sInstanceLabel = prevOwner, prevLabel
	})

	fake := newFakeKube(t)
	backend := fake.backend(t, KubernetesConfig{Namespace: "fleet-sandboxes"})
	addPod := func(name, instance, owner string) {
		fake.mu.Lock()
		fake.pods[name] = &k8sPod{Metadata: k8sObjectMeta{
			Name: name,
			Labels: map[string]string{
				k8sLabelName:      k8sLabelNameValue,
				k8sLabelManagedBy: k8sLabelManagedVal,
				k8sLabelInstance:  instance,
				k8sLabelOwner:     owner,
			},
		}}
		fake.mu.Unlock()
	}
	addPod("fleet-sandbox-mine", k8sInstanceLabel, "larkspur")
	// The predecessor container in THIS pod: same UID, earlier nonce.
	addPod("fleet-sandbox-predecessor", k8sInstanceLabelFor(uid, "aaaaaaaa"), "larkspur")
	// A neighbouring release's live sandbox: never ours to reclaim.
	addPod("fleet-sandbox-neighbour", k8sInstanceLabelFor("9f1d0f6e-0000-0000-0000-000000000000", "cccccccc"), "other-release")

	n, err := backend.PruneOrphanedPods(context.Background())
	if err != nil {
		t.Fatalf("PruneOrphanedPods: %v", err)
	}
	if n != 1 {
		t.Errorf("pruned %d, want 1 (the predecessor only)", n)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if _, ok := fake.pods["fleet-sandbox-predecessor"]; ok {
		t.Error("a pod from a previous process in the same pod must be pruned")
	}
	if _, ok := fake.pods["fleet-sandbox-mine"]; !ok {
		t.Error("this incarnation's own pod must never be pruned")
	}
	if _, ok := fake.pods["fleet-sandbox-neighbour"]; !ok {
		t.Error("another release's pod must never be pruned")
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

// TestParseK8sBundleDocsInImage pins the fail-closed boolean: unset is false
// (fleet assumes nothing about a sandbox image's contents), and a typo must
// refuse to boot rather than read as "keep the anchors".
func TestParseK8sBundleDocsInImage(t *testing.T) {
	for _, raw := range []string{"", "   "} {
		v, err := ParseK8sBundleDocsInImage(raw)
		if err != nil || v {
			t.Errorf("ParseK8sBundleDocsInImage(%q) = %v, %v; want false, nil", raw, v, err)
		}
	}
	for _, raw := range []string{"true", "TRUE", "1", " true "} {
		v, err := ParseK8sBundleDocsInImage(raw)
		if err != nil || !v {
			t.Errorf("ParseK8sBundleDocsInImage(%q) = %v, %v; want true, nil", raw, v, err)
		}
	}
	for _, raw := range []string{"false", "0"} {
		v, err := ParseK8sBundleDocsInImage(raw)
		if err != nil || v {
			t.Errorf("ParseK8sBundleDocsInImage(%q) = %v, %v; want false, nil", raw, v, err)
		}
	}
	for _, raw := range []string{"ture", "yes-please", "on"} {
		if _, err := ParseK8sBundleDocsInImage(raw); err == nil {
			t.Errorf("ParseK8sBundleDocsInImage(%q) must error (fail closed)", raw)
		}
	}
}

// TestFileOpAnchorSupportingDocsByBackend pins the behavior the
// bundle_docs_in_image declaration exists for: with the supporting-doc mounts
// retained, a bundle doc root anchors a READ-ONLY fileop; with them dropped
// (the kubernetes default, where a pod mounts only the workspace claim), the
// same root is refused before the file is looked for.
func TestFileOpAnchorSupportingDocsByBackend(t *testing.T) {
	const ws = "/var/lib/fleet/workspace"
	docs := []string{"/opt/fleet/client/protocols", "/opt/fleet/client/skills"}

	for _, root := range docs {
		anchor, readOnly, err := fileOpAnchorFor(ws, docs, root)
		if err != nil || anchor != root || !readOnly {
			t.Errorf("mounts retained: fileOpAnchorFor(%q) = %q, ro=%v, %v; want the root itself, read-only, nil", root, anchor, readOnly, err)
		}
		if _, _, err := fileOpAnchorFor(ws, nil, root); !errors.Is(err, ErrFileOpUnsafePath) {
			t.Errorf("mounts dropped: fileOpAnchorFor(%q) err = %v; want ErrFileOpUnsafePath", root, err)
		}
	}

	// The workspace claim itself is unaffected either way — it is the one
	// mount a sandbox pod always has.
	if anchor, readOnly, err := fileOpAnchorFor(ws, nil, ws+"/conv-1"); err != nil || anchor != ws || readOnly {
		t.Errorf("workspace anchor = %q, ro=%v, %v; want %q, writable, nil", anchor, readOnly, err, ws)
	}
}

// TestK8sCloseIsBoundedWithUnreadBridgeStdout pins the teardown deadlock fixed
// alongside this test.
//
// The bridge's stdout reaches fleet through an io.Pipe whose writer is the exec
// demux loop and whose only reader stops at the first newline of a response.
// Anything the pod writes past that line leaves the loop parked in pw.Write —
// io.Pipe is unbuffered, so the write blocks until someone reads. Teardown then
// deadlocked: terminateBridgeLocked joined on session.done, which only fires
// when the loop exits, which it could not do. It ran under the sandbox mutex,
// so the pod was never deleted and Pool.Close hung behind it.
func TestK8sCloseIsBoundedWithUnreadBridgeStdout(t *testing.T) {
	fake := newFakeKube(t)
	// Comfortably more than the pipe can hand off with nobody reading.
	fake.bridgeTrailingStdout = []byte(strings.Repeat("noise on fd 1\n", 512))
	backend := fake.backend(t, KubernetesConfig{Namespace: "fleet-sandboxes"})

	sb, err := backend.newSandbox(context.Background(), testContainerConfig(t))
	if err != nil {
		t.Fatalf("newSandbox: %v", err)
	}
	if _, err := sb.RunPython(context.Background(), PythonRequest{Code: "1+1"}); err != nil {
		t.Fatalf("RunPython: %v", err)
	}

	done := make(chan struct{})
	go func() {
		sb.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("Sandbox.Close() blocked on the unread bridge pipe — the demux loop is parked in pw.Write and nothing closes the read half")
	}

	// Teardown must also have actually deleted the pod; the old deadlock
	// stalled before reaching the delete.
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.deleted) == 0 {
		t.Error("Close() returned but deleted no pod")
	}
}

// TestK8sPodSpecSharedLibrarySubPathMount pins the shared-file-library posture
// (docs/SHARED-FILES.md): a read-only root nested inside the workspace claim
// is re-mounted from the SAME claim as a read-only subPath mount — the k8s
// counterpart of podman's nested `--volume …:ro` overlay — while host-path
// roots (bundle docs the image itself carries) still get anchors only, never
// a mount.
func TestK8sPodSpecSharedLibrarySubPathMount(t *testing.T) {
	cfg := testContainerConfig(t)
	shared := filepath.Join(cfg.WorkspaceHostDir, "shared")
	cfg.ReadOnlyMounts = []string{shared, "/opt/fleet/client/protocols", ""}
	kcfg := KubernetesConfig{Namespace: "fleet-sandbox", WorkspaceClaim: "fleet-workspace"}

	pod, err := buildSandboxPod(applyContainerDefaults(cfg), kcfg, "fleet-sandbox-shared")
	if err != nil {
		t.Fatalf("buildSandboxPod: %v", err)
	}
	var found *k8sVolumeMount
	for i, m := range pod.Spec.Containers[0].VolumeMounts {
		if m.MountPath == shared {
			found = &pod.Spec.Containers[0].VolumeMounts[i]
			continue
		}
		// Nothing else may be read-only or subPath'd, and the host doc dir
		// must not become a mount at all.
		if m.ReadOnly || m.SubPath != "" {
			t.Errorf("unexpected read-only/subPath mount %+v", m)
		}
		if m.MountPath == "/opt/fleet/client/protocols" {
			t.Errorf("host doc dir got a pod mount %+v — pods have no host filesystem to bind", m)
		}
	}
	if found == nil {
		t.Fatalf("no volume mount for the shared library at %q; mounts = %+v", shared, pod.Spec.Containers[0].VolumeMounts)
	}
	if found.Name != "workspace" {
		t.Errorf("shared mount volume = %q, want the workspace claim volume", found.Name)
	}
	if !found.ReadOnly {
		t.Errorf("shared mount is not read-only — a turn could rewrite what every chat reads")
	}
	if found.SubPath != "shared" {
		t.Errorf("shared mount subPath = %q, want %q", found.SubPath, "shared")
	}
}
