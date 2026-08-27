package sandbox

// Tests for #1291: under FLEET_DEFAULT_NETWORK_MODE=lockdown the warm pool
// spawns SEALED sandboxes, and a sealed, no-override take claims them instead
// of cold-starting. They run against the fake kubernetes apiserver
// (k8s_fake_test.go) because the k8s backend exposes the two observable seams
// the fix must pin: the egress pod label (the deny-all NetworkPolicy's
// selector — the thing the boot log claims is always "none" under lockdown)
// and the pod identity (warm claim vs. cold start). The podman backend routes
// through the same warmContainerConfig, pinned separately below without a
// container runtime.

import (
	"context"
	"testing"
	"time"
)

// warmedK8sPool builds a Size=1 kubernetes-backed pool with the given
// fleet-wide network mode and waits for the warm slot to fill.
func warmedK8sPool(t *testing.T, fake *fakeKube, mode string) *Pool {
	t.Helper()
	backend := fake.backend(t, KubernetesConfig{Namespace: "fleet-sandboxes"})
	pool := NewPool(PoolConfig{
		Size:               1,
		Mode:               ModeKubernetes,
		KubernetesBackend:  backend,
		BridgeScript:       []byte("# bridge\n"),
		Container:          testContainerConfig(t),
		DefaultNetworkMode: mode,
	})
	t.Cleanup(pool.Close)
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, avail := pool.Stats(); avail >= 1 {
			return pool
		}
		if time.Now().After(deadline) {
			t.Fatal("warm pool never filled")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// podEgressLabels snapshots the fake apiserver's live pods: name → egress
// label. The label is the enforcement contract on kubernetes (the deny-all
// NetworkPolicy selects egress=none), so it is the honest observable for the
// spawn posture.
func podEgressLabels(fake *fakeKube) map[string]string {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	out := make(map[string]string, len(fake.pods))
	for name, pod := range fake.pods {
		out[name] = pod.Metadata.Labels[k8sLabelEgress]
	}
	return out
}

// sandboxPodName reports which pod backs a k8s-mode sandbox, so a test can
// tell a warm claim (pre-existing pod) from a cold start (new pod).
func sandboxPodName(t *testing.T, sb *Sandbox) string {
	t.Helper()
	k, ok := sb.impl.(*k8sImpl)
	if !ok {
		t.Fatalf("sandbox impl is %T, want *k8sImpl", sb.impl)
	}
	k.podMu.Lock()
	defer k.podMu.Unlock()
	return k.podName
}

// TestLockdownPoolSpawnsSealedWarmSandboxes pins defect #1291's first half:
// warm spawns on a lockdown pool must be sealed (egress=none), not open-egress
// containers nothing under lockdown may ever claim. On kubernetes this is
// also what makes the boot log's "every pod is labeled none" true.
func TestLockdownPoolSpawnsSealedWarmSandboxes(t *testing.T) {
	fake := newFakeKube(t)
	warmedK8sPool(t, fake, NetworkModeLockdown)
	labels := podEgressLabels(fake)
	if len(labels) == 0 {
		t.Fatal("no warm pod spawned")
	}
	for name, egress := range labels {
		if egress != "none" {
			t.Errorf("lockdown warm pod %s labeled egress=%q, want none", name, egress)
		}
	}
}

// TestLockdownTakeContainerClaimsWarmSandbox pins the second half: under
// fleet-wide lockdown a sealed, no-override take rides the warm pool (the
// parked sandbox already has the exact posture and pool-default ceilings the
// caller wants) instead of cold-starting a fresh container every turn.
func TestLockdownTakeContainerClaimsWarmSandbox(t *testing.T) {
	fake := newFakeKube(t)
	pool := warmedK8sPool(t, fake, NetworkModeLockdown)
	warm := podEgressLabels(fake)

	sb, cleanup, err := pool.TakeContainer(context.Background())
	if err != nil {
		t.Fatalf("TakeContainer: %v", err)
	}
	defer cleanup()
	name := sandboxPodName(t, sb)
	egress, existed := warm[name]
	if !existed {
		t.Fatalf("TakeContainer cold-started pod %s instead of claiming a warm one %v", name, warm)
	}
	if egress != "none" {
		t.Errorf("claimed warm pod labeled egress=%q, want none", egress)
	}
}

// TestOpenPoolTakeContainerColdStartsSealed pins the posture invariant on the
// other side: on a NON-lockdown pool the warm inventory is open-egress, so a
// sealed take (a per-conversation lockdown chat on an open fleet) must NEVER
// receive it — it cold-starts a sealed container exactly as before.
func TestOpenPoolTakeContainerColdStartsSealed(t *testing.T) {
	fake := newFakeKube(t)
	pool := warmedK8sPool(t, fake, "")
	warm := podEgressLabels(fake)
	for name, egress := range warm {
		if egress != "open" {
			t.Errorf("open-pool warm pod %s labeled egress=%q, want open", name, egress)
		}
	}

	sb, cleanup, err := pool.TakeContainer(context.Background())
	if err != nil {
		t.Fatalf("TakeContainer: %v", err)
	}
	defer cleanup()
	name := sandboxPodName(t, sb)
	if _, existed := warm[name]; existed {
		t.Fatalf("sealed take on an open pool claimed warm OPEN pod %s — the lockdown boundary is broken", name)
	}
	if got := podEgressLabels(fake)[name]; got != "none" {
		t.Errorf("cold-started sealed pod labeled egress=%q, want none", got)
	}
}

// TestLockdownOverrideTakeColdStarts pins that per-task resource overrides
// (#205) still force a cold start even under lockdown: a warm pooled sandbox
// is already running with the pool-default ceilings, so an override can never
// be satisfied by claiming one.
func TestLockdownOverrideTakeColdStarts(t *testing.T) {
	fake := newFakeKube(t)
	pool := warmedK8sPool(t, fake, NetworkModeLockdown)
	warm := podEgressLabels(fake)

	sb, cleanup, err := pool.TakeContainerWithOverrides(context.Background(), ResourceOverride{MemoryLimit: "1024m"}, true)
	if err != nil {
		t.Fatalf("TakeContainerWithOverrides: %v", err)
	}
	defer cleanup()
	name := sandboxPodName(t, sb)
	if _, existed := warm[name]; existed {
		t.Fatalf("override take claimed warm pod %s; overrides require a cold start", name)
	}
	if got := podEgressLabels(fake)[name]; got != "none" {
		t.Errorf("override cold-start pod labeled egress=%q, want none", got)
	}
}

// TestWarmContainerConfigSealsUnderLockdown pins the podman side of the fix
// without a container runtime: warmContainerConfig — the config every warm
// spawn and Take-side cold start uses — seals egress ONLY when the pool's
// fleet-wide mode is lockdown on a container backend. ModeHost is excluded (a
// host sandbox has no network namespace to seal), and open/allowlisted pools
// keep spawning open warm sandboxes exactly as before.
func TestWarmContainerConfigSealsUnderLockdown(t *testing.T) {
	cases := []struct {
		name string
		mode Mode
		net  string
		want bool
	}{
		{"container lockdown", ModeContainer, NetworkModeLockdown, true},
		{"kubernetes lockdown", ModeKubernetes, NetworkModeLockdown, true},
		{"container open default", ModeContainer, "", false},
		{"container open explicit", ModeContainer, NetworkModeOpen, false},
		{"container allowlisted", ModeContainer, NetworkModeAllowlisted, false},
		{"host lockdown", ModeHost, NetworkModeLockdown, false},
	}
	for _, tc := range cases {
		p := &Pool{cfg: PoolConfig{Mode: tc.mode, DefaultNetworkMode: tc.net}}
		if got := p.warmContainerConfig().NoNetwork; got != tc.want {
			t.Errorf("%s: warm NoNetwork = %v, want %v", tc.name, got, tc.want)
		}
	}
}
