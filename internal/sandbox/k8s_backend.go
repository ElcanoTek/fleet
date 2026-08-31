// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package sandbox

// k8s_backend.go is the Kubernetes sandbox backend (#989): the same per-turn
// execution boundary as the rootless-Podman backend, delivered as an
// ephemeral Pod per sandbox instead of a local container. One Sandbox = one
// Pod running `sleep infinity`; bash is a one-shot exec, the python bridge is
// a held exec session, and file operations run the same embedded fileops.py —
// all over the apiserver's exec subresource, so the fleet control plane never
// shares a kernel with model-authored execution.
//
// What carries over from the podman backend unchanged: the workspace is
// mounted at the SAME absolute path as the control plane sees it (a shared
// RWX PersistentVolumeClaim replaces the bind mount), the rootfs is
// read-only with tmpfs-equivalent emptyDirs for scratch, all capabilities are
// dropped, and a cancelled/timed-out call poisons the sandbox and destroys
// the whole PID namespace — here by deleting the Pod with zero grace (#796).
//
// What is honestly different (documented in docs/DEPLOYMENT-KUBERNETES.md and
// ADR-0049): egress sealing is delegated to a NetworkPolicy the chart ships
// (verified to exist at boot, but ENFORCED by the cluster CNI, not by fleet);
// the per-pod pids limit is not expressible in a Pod spec; the "allowlisted"
// egress mode is unsupported (fail-closed at boot); resource telemetry (#263)
// is not collected; and seccomp is RuntimeDefault or an operator-installed
// Localhost profile rather than the bundled JSON.
//
// MCP credentials keep their ADR-0003 posture automatically: the broker runs
// in the control-plane process, and nothing in a sandbox Pod's spec, env, or
// mounts carries a credential — automountServiceAccountToken is explicitly
// false so a sandbox cannot even talk to the apiserver that created it.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/ElcanoTek/fleet/internal/safe"
)

// Sandbox backend names (#989): where every sandbox runs. The knob mirrors
// sandbox.runtime's precedence (FLEET_SANDBOX_BACKEND env wins, else the
// bundle manifest's sandbox.backend, else podman).
const (
	// BackendPodman is the co-located rootless-Podman backend — the
	// single-box default and the only backend before #989.
	BackendPodman = "podman"
	// BackendKubernetes runs each sandbox as an ephemeral pod in a cluster:
	// the split control-plane/runner enterprise deployment.
	BackendKubernetes = "kubernetes"
)

// ResolveBackend applies the sandbox-backend precedence in ONE place so every
// entrypoint (fleet boot, `fleet validate-config`) resolves identically: an
// explicit env value (FLEET_SANDBOX_BACKEND) wins, else the bundle manifest's
// sandbox.backend, else podman. An unrecognized value is an ERROR, never a
// silent fallback to podman — the #1119 posture: a typo'd security-relevant
// knob must refuse to boot rather than quietly mean something else.
func ResolveBackend(envBackend, bundleBackend string) (string, error) {
	raw := strings.ToLower(strings.TrimSpace(envBackend))
	if raw == "" {
		raw = strings.ToLower(strings.TrimSpace(bundleBackend))
	}
	switch raw {
	case "", BackendPodman:
		return BackendPodman, nil
	case BackendKubernetes:
		return BackendKubernetes, nil
	default:
		return "", fmt.Errorf("unrecognized sandbox backend %q (FLEET_SANDBOX_BACKEND / manifest sandbox.backend): want %q or %q — refusing to guess (fail-closed)", raw, BackendPodman, BackendKubernetes)
	}
}

// KubernetesConfig configures the kubernetes sandbox backend. It is trusted
// operator config, same authority tier as ContainerConfig / sandbox.runtime.
type KubernetesConfig struct {
	// Namespace is where sandbox Pods are created. Defaults to
	// "fleet-sandboxes"; in-cluster deployments may point it at any namespace
	// the RBAC grant covers. Keeping it SEPARATE from the control plane's own
	// namespace is what lets the RBAC grant be pod-scoped and narrow.
	Namespace string

	// WorkspaceClaim is the name of the ReadWriteMany PersistentVolumeClaim
	// (in Namespace) that holds the workspace root. It is mounted into every
	// sandbox Pod at ContainerConfig.WorkspaceHostDir — the same absolute path
	// the control plane mounts it at — preserving the same-path invariant that
	// keeps MCP-returned paths usable inside bash/run_python. Required.
	WorkspaceClaim string

	// ServiceAccount, when set, is stamped as the Pod's serviceAccountName.
	// The token is never mounted either way (automountServiceAccountToken is
	// forced false); this exists so admission policies can key on identity.
	ServiceAccount string

	// ImagePullSecret, when set, is attached for pulling the sandbox image
	// from a private registry.
	ImagePullSecret string

	// RuntimeClassName, when set, selects a hypervisor-isolated runtime class
	// (e.g. kata) for sandbox Pods — the k8s counterpart of sandbox.runtime.
	// Preflighted to exist, fail-closed, mirroring ADR-0010.
	RuntimeClassName string

	// SeccompLocalhostProfile, when set, is a node-local profile path
	// (relative to the kubelet's seccomp root) applied as a Localhost seccomp
	// profile. Empty means RuntimeDefault.
	SeccompLocalhostProfile string

	// KubeconfigPath selects out-of-cluster auth. Empty means in-cluster
	// (the standard service-account mount).
	KubeconfigPath string

	// NetworkPolicyName is the deny-all NetworkPolicy the boot preflight
	// requires to exist in Namespace — the object that seals egress for Pods
	// labeled fleet.elcanotek.com/egress=none. Defaults to
	// "fleet-sandbox-deny-all". The preflight verifies the OBJECT exists;
	// enforcement is the CNI's job and the docs say so plainly.
	//
	// Required in EVERY mode, because a sealed turn can happen in any of them:
	// a scheduled run with AllowNetwork off gets a sealed sandbox even on a
	// deployment whose default posture is open.
	NetworkPolicyName string

	// OpenEgressPolicyName is its counterpart for Pods labeled
	// fleet.elcanotek.com/egress=open, defaulting to
	// "fleet-sandbox-open-egress". The preflight requires this object too
	// whenever DefaultNetworkMode is open, because an open Pod that NO policy
	// selects is a full citizen of the pod network: it can reach the fleet
	// Service, the in-cluster database, the apiserver, and the node's metadata
	// endpoint, all from model-authored code. Podman's open mode is bounded by
	// construction (rootless pasta/slirp4netns is outbound-only and cannot
	// reach the fleet process); here nothing is bounded unless a policy says
	// so, which is why the docs called this policy required long before the
	// preflight enforced it.
	OpenEgressPolicyName string

	// DefaultNetworkMode is the fleet-wide egress posture sandboxes are
	// created with (NetworkModeOpen — including its historical "" spelling —
	// or NetworkModeLockdown; allowlisted is refused before a backend is
	// built). The preflight reads it to decide whether the open-egress policy
	// is required.
	DefaultNetworkMode string

	// UnrestrictedEgressAcknowledged lets an operator boot in open mode with
	// no open-egress policy of fleet's, for the one case fleet genuinely
	// cannot verify: egress shaped by tooling it cannot see — a Cilium
	// CiliumNetworkPolicy, a Calico GlobalNetworkPolicy, a service mesh, a
	// cluster-wide default-deny. It is an explicit statement, never a default,
	// and it is logged every boot rather than silently honoured.
	UnrestrictedEgressAcknowledged bool

	// NodeSelector pins sandbox pods to labeled nodes — the standard way to
	// give runners a DEDICATED node pool, which is the issue's scaling story
	// (more runner capacity = a bigger pool, never more fleet replicas).
	NodeSelector map[string]string

	// Tolerations let sandbox pods schedule onto a tainted runner pool, the
	// usual companion to NodeSelector for a pool nothing else may land on.
	Tolerations []K8sToleration

	// StartTimeout caps pod schedule+pull+start. Zero defaults to 2 minutes
	// (image pulls make the podman default of 30s unrealistic).
	StartTimeout time.Duration
}

// K8sToleration mirrors the four core/v1 Toleration fields sandbox pods need
// (tolerationSeconds is a drain concern that does not apply to pods fleet
// deletes itself).
type K8sToleration struct {
	Key      string `json:"key,omitempty"`
	Operator string `json:"operator,omitempty"`
	Value    string `json:"value,omitempty"`
	Effect   string `json:"effect,omitempty"`
}

// ParseK8sNodeSelector parses the FLEET_SANDBOX_K8S_NODE_SELECTOR form —
// comma-separated key=value pairs ("pool=fleet-sandboxes,arch=amd64") — into
// the map the pod spec takes. Empty input is a nil map; a malformed pair is
// an error so a typo'd selector refuses to boot instead of silently
// scheduling sandboxes onto the wrong nodes.
func ParseK8sNodeSelector(s string) (map[string]string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	out := make(map[string]string)
	for _, pair := range strings.Split(s, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(pair), "=")
		k, v = strings.TrimSpace(k), strings.TrimSpace(v)
		if !ok || k == "" || v == "" {
			return nil, fmt.Errorf("invalid node selector pair %q (want key=value, comma-separated)", pair)
		}
		out[k] = v
	}
	return out, nil
}

// ParseK8sTolerations parses the FLEET_SANDBOX_K8S_TOLERATIONS form — a JSON
// array of {key, operator, value, effect} objects. Empty input is nil;
// malformed JSON or an unknown field is an error (fail-closed, strict
// decoding, matching the additive-first schema posture).
func ParseK8sTolerations(s string) ([]K8sToleration, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	dec := json.NewDecoder(strings.NewReader(s))
	dec.DisallowUnknownFields()
	var out []K8sToleration
	if err := dec.Decode(&out); err != nil {
		return nil, fmt.Errorf("invalid tolerations JSON (want an array of {key,operator,value,effect}): %w", err)
	}
	return out, nil
}

// ParseK8sBundleDocsInImage parses the
// FLEET_SANDBOX_K8S_BUNDLE_DOCS_IN_IMAGE form — a boolean declaring that the
// sandbox IMAGE carries the bundle's supporting-doc dirs at the same absolute
// paths the control plane reads them from, so the fileop path anchors for
// those roots stay valid inside a pod (see the ReadOnlyMounts handling in
// internal/agent). Empty input is false (the safe default: fleet assumes
// nothing about a sandbox image's contents). A malformed value is an error —
// a typo'd "ture" must not read as "keep the anchors".
func ParseK8sBundleDocsInImage(s string) (bool, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return false, nil
	}
	v, err := strconv.ParseBool(s)
	if err != nil {
		return false, fmt.Errorf("invalid boolean %q (want true or false)", s)
	}
	return v, nil
}

// defaultK8sNamespace / defaultK8sNetworkPolicy are the conventions the Helm
// chart ships; the backend defaults match so a chart install needs no extra
// wiring.
const (
	defaultK8sNamespace     = "fleet-sandboxes"
	defaultK8sNetworkPolicy = "fleet-sandbox-deny-all"
	// defaultK8sOpenEgressPolicy names the chart's companion policy for
	// egress=open pods. Symmetric with the deny-all name on purpose: an
	// operator who reads one knob should be able to guess the other.
	defaultK8sOpenEgressPolicy = "fleet-sandbox-open-egress"
	// k8sUnrestrictedEgressAckEnv is named in the refusal and in the warning
	// the acknowledgement produces, so both messages carry the exact string an
	// operator has to set (or unset) rather than a description of it.
	k8sUnrestrictedEgressAckEnv = "FLEET_SANDBOX_K8S_OPEN_EGRESS_ACKNOWLEDGED"
	defaultK8sStartTimeout      = 2 * time.Minute
)

// sandboxContainerName is the single container in every sandbox Pod.
const sandboxContainerName = "sandbox"

// k8sBridgeDir is the writable emptyDir where the bridge + fileops scripts
// are uploaded at pod start (the k8s counterpart of the /opt/bridge bind
// mount — there is no host filesystem to bind from).
const (
	k8sBridgeDir     = "/opt/fleet-bridge"
	k8sBridgePath    = k8sBridgeDir + "/bridge.py"
	k8sFileOpsPath   = k8sBridgeDir + "/fileops.py"
	k8sPodNamePrefix = "fleet-sandbox-"
)

// Pod labels. app.kubernetes.io/* follow the k8s recommended-label
// convention; the fleet.elcanotek.com/* pair carries the ownership identity
// the orphan sweep keys on (the k8s counterpart of the podman
// fleet.instance label) and the egress posture the chart's NetworkPolicies
// select on.
const (
	k8sLabelName       = "app.kubernetes.io/name"
	k8sLabelManagedBy  = "app.kubernetes.io/managed-by"
	k8sLabelInstance   = "fleet.elcanotek.com/instance"
	k8sLabelOwner      = "fleet.elcanotek.com/owner"
	k8sLabelEgress     = "fleet.elcanotek.com/egress"
	k8sLabelNameValue  = "fleet-sandbox"
	k8sLabelManagedVal = "fleet"
)

// k8sControlPlaneUID and k8sControlPlaneOwner are the control plane's own
// cluster identity, supplied by the chart through the downward API:
//
//	FLEET_POD_UID   ← metadata.uid                              (this POD, not
//	                                                             this incarnation:
//	                                                             see k8sBootNonce)
//	FLEET_OWNER_ID  ← metadata.labels['app.kubernetes.io/instance'] (this release)
//
// Both are empty when fleet talks to a cluster from outside it (a kubeconfig
// dev box), which is why every consumer keeps a fallback.
var (
	k8sControlPlaneUID   = sanitizeK8sLabelValue(os.Getenv("FLEET_POD_UID"))
	k8sControlPlaneOwner = sanitizeK8sLabelValue(os.Getenv("FLEET_OWNER_ID"))
)

// k8sBootNonce is unique to this PROCESS, and that is the point: the pod UID
// is not. Kubelet restarts a crashed container IN PLACE — same pod, same UID,
// same downward-API value — so an incarnation identified by UID alone cannot
// tell its own sandbox pods from the ones the process it replaced left behind,
// and the sweep below skips exactly the pods it exists to reclaim. The nonce
// changes on every start, container restart included.
//
// crypto/rand failing is not a reason to refuse to boot. The fallback only has
// to differ from the previous process in this pod, and a start timestamp does.
var k8sBootNonce = func() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano()&0xffffffff, 16)
	}
	return hex.EncodeToString(b[:])
}()

// k8sInstanceLabel identifies the control-plane incarnation that owns a
// sandbox pod.
//
// In cluster that is the pod UID plus this process's boot nonce. The UID is
// diagnostic — it maps a sandbox back to the control-plane pod that made it —
// and the nonce is what makes the value name an INCARNATION rather than a pod.
// A SIGKILL, an OOM kill, a panic, or a failed liveness probe (the chart ships
// one) each restart the container inside the same pod, and each is a new
// process that must treat its predecessor's pods as orphans.
//
// The pid-based form this used to carry is meaningless across containers: the
// pid came from a namespace that no longer exists and fleet is pid 1 in its
// own container, so a restarted control plane asked whether IT was running and
// concluded "yes". Out of cluster there is no pod identity at all, so that
// form stays as the fallback — there it is still true.
var k8sInstanceLabel = k8sInstanceLabelFor(k8sControlPlaneUID, k8sBootNonce)

// k8sInstanceLabelFor builds the instance label. It is kept separate from the
// package var so a test can construct ANOTHER incarnation's label — including
// the one that matters: a different nonce under the same pod UID. An empty uid
// means out of cluster, where the pid form is unchanged.
func k8sInstanceLabelFor(uid, nonce string) string {
	if uid == "" {
		pid, start, _ := instanceLabelOwner(thisInstanceLabel)
		return fmt.Sprintf("p%d-t%d", pid, start)
	}
	// Keep the whole value inside the 63-char label limit, trimming the UID
	// rather than the nonce: a shortened UID is still a usable hint, while a
	// shortened nonce could collide with the very incarnation it has to differ
	// from. The value ends in the nonce, so it still ends alphanumeric as a
	// label value must.
	if limit := 63 - len(nonce) - 2; len(uid) > limit {
		uid = uid[:limit]
	}
	return "u" + uid + "-" + nonce
}

// sanitizeK8sLabelValue reduces a value to something legal as a label value
// (alphanumeric, '-', '_', '.', at most 63 chars, must start and end
// alphanumeric). A value that cannot be made legal comes back empty, and the
// caller falls back rather than emitting a pod spec the apiserver rejects.
func sanitizeK8sLabelValue(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range v {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-_.")
	if len(out) > 63 {
		out = strings.Trim(out[:63], "-_.")
	}
	return out
}

// parseK8sInstanceLabel inverts k8sInstanceLabel's encoding. ok is false for
// anything unparseable — callers treat that as "ownership unknown".
func parseK8sInstanceLabel(label string) (pid int, startedAt int64, ok bool) {
	rest, found := strings.CutPrefix(label, "p")
	if !found {
		return 0, 0, false
	}
	pidStr, startStr, found := strings.Cut(rest, "-t")
	if !found {
		return 0, 0, false
	}
	pid, err := strconv.Atoi(pidStr)
	if err != nil || pid <= 0 {
		return 0, 0, false
	}
	startedAt, err = strconv.ParseInt(startStr, 10, 64)
	if err != nil || startedAt <= 0 {
		return pid, 0, true
	}
	return pid, startedAt, true
}

// KubernetesBackend is the boot-built handle for the kubernetes sandbox
// backend: one API client plus the resolved config, shared by the pool, the
// preflight, and the orphan sweep. Construct with NewKubernetesBackend.
type KubernetesBackend struct {
	cfg    KubernetesConfig
	client *k8sClient

	// reapMu guards the retry set for pod deletes that failed. A delete can
	// fail for reasons that have nothing to do with the pod — an apiserver
	// restart, a network blip, throttling — and the pod then keeps its
	// Guaranteed reservation until something reclaims it. Nothing else will:
	// the boot sweep deliberately skips pods carrying THIS incarnation's
	// label, so it cannot reclaim fleet's own leftovers, and a Pod owned by a
	// Pod gets no help from the garbage collector while its owner is alive.
	// Only the process that created the pod knows it is finished with it.
	reapMu      sync.Mutex
	reapPending map[string]int // pod name → attempts so far
	reapRunning bool           // a drain goroutine is live
}

// NewKubernetesBackend resolves credentials (in-cluster unless a kubeconfig
// is configured) and defaults, returning the backend handle. It performs no
// network I/O — Preflight does the fail-closed cluster checks.
func NewKubernetesBackend(cfg KubernetesConfig) (*KubernetesBackend, error) {
	var (
		client       *k8sClient
		kubeconfigNS string
		err          error
	)
	if cfg.KubeconfigPath != "" {
		client, kubeconfigNS, err = newKubeconfigClient(cfg.KubeconfigPath)
	} else {
		client, err = newInClusterClient()
	}
	if err != nil {
		return nil, fmt.Errorf("kubernetes sandbox backend: %w", err)
	}
	if cfg.Namespace == "" {
		// Precedence: explicit config, else the kubeconfig context's
		// namespace, else (in-cluster) the control plane's own namespace —
		// the Helm chart's default topology, because a PersistentVolumeClaim
		// cannot be mounted across namespaces and the workspace claim is
		// shared with the control plane — else the shipped default name.
		cfg.Namespace = kubeconfigNS
		if cfg.Namespace == "" && cfg.KubeconfigPath == "" {
			cfg.Namespace = inClusterNamespace()
		}
		if cfg.Namespace == "" {
			cfg.Namespace = defaultK8sNamespace
		}
	}
	if cfg.NetworkPolicyName == "" {
		cfg.NetworkPolicyName = defaultK8sNetworkPolicy
	}
	if cfg.OpenEgressPolicyName == "" {
		cfg.OpenEgressPolicyName = defaultK8sOpenEgressPolicy
	}
	if cfg.StartTimeout <= 0 {
		cfg.StartTimeout = defaultK8sStartTimeout
	}
	return &KubernetesBackend{cfg: cfg, client: client}, nil
}

// Namespace reports the resolved sandbox namespace (for logs and doctor
// output).
func (b *KubernetesBackend) Namespace() string { return b.cfg.Namespace }

// StartTimeout reports the resolved pod start ceiling; the pool derives its
// outer construction contexts from it (mirroring resolveStartTimeout).
func (b *KubernetesBackend) StartTimeout() time.Duration { return b.cfg.StartTimeout }

// ApiserverVersion reports the cluster's version string via GET /version —
// the cheapest authenticated "is the apiserver reachable and are my
// credentials valid" call, the same one the boot preflight opens with.
// Exported for the /readyz sandbox check: under this backend the sandbox
// runtime IS the apiserver, so readiness probes it instead of a local
// `podman --version` that reports on a binary this deployment never execs.
func (b *KubernetesBackend) ApiserverVersion(ctx context.Context) (string, error) {
	return b.client.serverVersion(ctx)
}

// newSandbox starts one sandbox Pod and returns the wrapping handle. cfg
// carries the backend-shared knobs (image, workspace path, limits, network
// posture); the pool routes here from the same take paths that call
// NewContainer for podman.
func (b *KubernetesBackend) newSandbox(ctx context.Context, cfg ContainerConfig) (*Sandbox, error) {
	if cfg.Image == "" {
		return nil, fmt.Errorf("sandbox: ContainerConfig.Image required")
	}
	if cfg.WorkspaceHostDir == "" {
		return nil, fmt.Errorf("sandbox: ContainerConfig.WorkspaceHostDir required")
	}
	if cfg.BridgeScript == nil {
		return nil, fmt.Errorf("sandbox: ContainerConfig.BridgeScript required")
	}
	if cfg.ProxyURL != "" {
		// The allowlisted egress proxy binds to the control-plane host's
		// loopback; a pod on another node cannot reach it, and pretending
		// otherwise would grant open egress under an "allowlisted" banner.
		return nil, errors.New("sandbox: allowlisted egress mode is not supported by the kubernetes backend (fail-closed)")
	}
	cfg = applyContainerDefaults(cfg)
	k := &k8sImpl{backend: b, cfg: cfg}
	if err := k.start(ctx); err != nil {
		k.close()
		return nil, err
	}
	return &Sandbox{mode: ModeKubernetes, impl: k}, nil
}

// k8sImpl is the kubernetes impl: one Pod, exec'd into for every operation.
// The struct mirrors containerImpl's locking discipline — podMu guards the
// pod name (cleared by close, snapshotted by cross-goroutine readers), mu
// guards the bridge session and is held for a whole run_python cell.
type k8sImpl struct {
	backend *KubernetesBackend
	cfg     ContainerConfig

	podMu   sync.Mutex
	podName string

	mu           sync.Mutex
	bridge       *k8sExecSession
	bridgeStdout *bufio.Reader
	// bridgeStdoutPipe is the READ half behind bridgeStdout. Held so teardown
	// can unblock a demux loop parked in pw.Write — see terminateBridgeLocked.
	bridgeStdoutPipe *io.PipeReader
	bridgeStderr     *syncBuffer
	bridgeStarted    bool

	execPoisoned atomic.Bool
}

// generatePodName mirrors generateContainerName with the pod prefix.
func generatePodName() string {
	var buf [8]byte
	_, _ = rand.Read(buf[:])
	return k8sPodNamePrefix + hex.EncodeToString(buf[:])
}

// k8sQuantityFromPodmanMemory converts a podman --memory value ("512m",
// "2g", bare bytes) into a Kubernetes resource quantity (plain bytes —
// unambiguous and accepted everywhere a quantity is).
func k8sQuantityFromPodmanMemory(limit string) (string, error) {
	b, err := parseMemoryToBytes(limit)
	if err != nil {
		return "", err
	}
	return strconv.FormatInt(b, 10), nil
}

// k8sQuantityFromPodmanCPU converts a podman --cpus value ("1.0", "2.50")
// into a Kubernetes CPU quantity in millicores.
func k8sQuantityFromPodmanCPU(limit string) (string, error) {
	f, err := strconv.ParseFloat(strings.TrimSpace(limit), 64)
	if err != nil || f <= 0 {
		return "", fmt.Errorf("invalid cpu limit %q", limit)
	}
	return strconv.FormatInt(int64(f*1000), 10) + "m", nil
}

// buildSandboxPod is the pure pod-spec builder — the k8s counterpart of the
// `podman run` argument list in containerImpl.start, kept side-effect-free so
// the hardening posture is pinned by unit tests the way podman_args_test.go
// pins the flag list.
func buildSandboxPod(cfg ContainerConfig, kcfg KubernetesConfig, name string) (*k8sPod, error) {
	memory, err := k8sQuantityFromPodmanMemory(cfg.MemoryLimit)
	if err != nil {
		return nil, fmt.Errorf("sandbox pod memory limit: %w", err)
	}
	cpu, err := k8sQuantityFromPodmanCPU(cfg.CPULimit)
	if err != nil {
		return nil, fmt.Errorf("sandbox pod cpu limit: %w", err)
	}

	egress := "open"
	if cfg.NoNetwork {
		// Sealed posture: selected by the deny-all NetworkPolicy the preflight
		// verified. The label is the contract; enforcement is the CNI's.
		egress = "none"
	}

	boolPtr := func(v bool) *bool { return &v }
	int64Ptr := func(v int64) *int64 { return &v }

	limits := map[string]string{"memory": memory, "cpu": cpu}
	requests := map[string]string{"memory": memory, "cpu": cpu}
	if cfg.DiskLimitGB > 0 {
		// ephemeral-storage caps the pod's writable layer AND its emptyDirs —
		// a strictly stronger surface than podman's per-file ulimit + layer
		// quota. The workspace PVC is still outside it, exactly like the bind
		// mount is outside podman's quota; the docs carry the same honest
		// limit statement.
		limits["ephemeral-storage"] = fmt.Sprintf("%dGi", cfg.DiskLimitGB)
	}

	seccomp := &k8sSeccompProfile{Type: "RuntimeDefault"}
	if kcfg.SeccompLocalhostProfile != "" {
		profile := kcfg.SeccompLocalhostProfile
		seccomp = &k8sSeccompProfile{Type: "Localhost", LocalhostProfile: &profile}
	}

	// emptyDir scratch mounts mirror the podman --tmpfs set (sizes included)
	// so a --read-only-rootfs image behaves identically in both backends,
	// plus the bridge dir the scripts are uploaded into.
	mounts := []k8sVolumeMount{
		{Name: "workspace", MountPath: cfg.WorkspaceHostDir},
		{Name: "bridge", MountPath: k8sBridgeDir},
		{Name: "tmp", MountPath: "/tmp"},
		{Name: "ipython", MountPath: "/home/sandbox/.ipython"},
		{Name: "cache", MountPath: "/home/sandbox/.cache"},
		{Name: "config", MountPath: "/home/sandbox/.config"},
	}
	volumes := []k8sVolume{
		{Name: "workspace", PersistentVolumeClaim: &k8sPVCVolSource{ClaimName: kcfg.WorkspaceClaim}},
		{Name: "bridge", EmptyDir: &k8sEmptyDir{SizeLimit: "8Mi"}},
		{Name: "tmp", EmptyDir: &k8sEmptyDir{SizeLimit: "128Mi"}},
		{Name: "ipython", EmptyDir: &k8sEmptyDir{SizeLimit: "32Mi"}},
		{Name: "cache", EmptyDir: &k8sEmptyDir{SizeLimit: "32Mi"}},
		{Name: "config", EmptyDir: &k8sEmptyDir{SizeLimit: "8Mi"}},
	}
	// Read-only roots nested inside the workspace claim (today: the shared
	// file library's staged tree, docs/SHARED-FILES.md) are re-mounted from
	// the SAME claim as read-only subPath mounts — the k8s counterpart of the
	// podman backend's nested `--volume …:ro` overlay, so a sandbox can read
	// the library but no turn can rewrite what every other chat reads. Roots
	// NOT nested in the claim carry fileop anchors only (bundle docs the
	// image itself provides) and get no mount, exactly as before.
	for _, dir := range cfg.ReadOnlyMounts {
		if dir == "" {
			continue
		}
		rel, err := filepath.Rel(cfg.WorkspaceHostDir, filepath.Clean(dir))
		if err != nil || rel == "." || !filepath.IsLocal(rel) {
			continue
		}
		mounts = append(mounts, k8sVolumeMount{
			Name:      "workspace",
			MountPath: filepath.Join(cfg.WorkspaceHostDir, rel),
			SubPath:   filepath.ToSlash(rel),
			ReadOnly:  true,
		})
	}

	spec := k8sPodSpec{
		RestartPolicy: "Never",
		// The sandbox must not be able to reach the apiserver that made it:
		// no token, no service links, ever.
		AutomountServiceAccountToken:  boolPtr(false),
		EnableServiceLinks:            boolPtr(false),
		TerminationGracePeriodSeconds: int64Ptr(5),
		ServiceAccountName:            kcfg.ServiceAccount,
		SecurityContext: &k8sPodSecurityCtx{
			RunAsNonRoot: boolPtr(true),
			// uid/gid 1000 matches the image's USER sandbox and the podman
			// keep-id mapping, so workspace files are owned consistently across
			// backends. fsGroup makes the PVC group-writable for that gid.
			RunAsUser:      int64Ptr(1000),
			RunAsGroup:     int64Ptr(1000),
			FSGroup:        int64Ptr(1000),
			SeccompProfile: seccomp,
		},
		Containers: []k8sContainer{{
			Name:  sandboxContainerName,
			Image: cfg.Image,
			// Explicit IfNotPresent: the API default for a :latest tag is
			// Always, which breaks side-loaded images (kind) and re-pulls a
			// mutable tag mid-fleet-run — sandbox image freshness is a deploy
			// concern, not a per-pod one.
			ImagePullPolicy: "IfNotPresent",
			// PID 1: a do-nothing process to keep the pod alive; every real
			// operation execs into it — same shape as the podman backend.
			Command:    []string{"sleep", "infinity"},
			WorkingDir: cfg.WorkspaceHostDir,
			SecurityContext: &k8sContainerSecCtx{
				AllowPrivilegeEscalation: boolPtr(false),
				ReadOnlyRootFilesystem:   boolPtr(true),
				Capabilities:             &k8sCapabilities{Drop: []string{"ALL"}},
			},
			Resources:    &k8sResources{Limits: limits, Requests: requests},
			VolumeMounts: mounts,
		}},
		Volumes: volumes,
	}
	if kcfg.RuntimeClassName != "" {
		rc := kcfg.RuntimeClassName
		spec.RuntimeClassName = &rc
	}
	if kcfg.ImagePullSecret != "" {
		spec.ImagePullSecrets = []k8sLocalObjRef{{Name: kcfg.ImagePullSecret}}
	}
	// Dedicated runner pool: selector + taints, when configured.
	if len(kcfg.NodeSelector) > 0 {
		spec.NodeSelector = kcfg.NodeSelector
	}
	for _, tol := range kcfg.Tolerations {
		spec.Tolerations = append(spec.Tolerations, k8sToleration(tol))
	}

	labels := map[string]string{
		k8sLabelName:      k8sLabelNameValue,
		k8sLabelManagedBy: k8sLabelManagedVal,
		k8sLabelInstance:  k8sInstanceLabel,
		k8sLabelEgress:    egress,
	}
	// The owning RELEASE, distinct from the owning incarnation above. Two fleet
	// installs may legitimately share one sandbox namespace, and without this
	// the orphan sweep cannot tell "a pod my predecessor left behind" from "a
	// pod my neighbour is using right now" — it would delete the neighbour's
	// sandboxes mid-turn.
	if k8sControlPlaneOwner != "" {
		labels[k8sLabelOwner] = k8sControlPlaneOwner
	}

	return &k8sPod{
		Metadata: k8sObjectMeta{
			Name:      name,
			Namespace: kcfg.Namespace,
			Labels:    labels,
		},
		Spec: spec,
	}, nil
}

// k8sPodPollInterval is how often start() re-reads the pod while waiting for
// Running.
const k8sPodPollInterval = 500 * time.Millisecond

func (k *k8sImpl) start(ctx context.Context) error {
	name := generatePodName()
	k.podMu.Lock()
	k.podName = name
	k.podMu.Unlock()

	pod, err := buildSandboxPod(k.cfg, k.backend.cfg, name)
	if err != nil {
		return err
	}
	startCtx, cancel := context.WithTimeout(ctx, k.backend.cfg.StartTimeout)
	defer cancel()
	if err := k.backend.client.createPod(startCtx, k.backend.cfg.Namespace, pod); err != nil {
		return fmt.Errorf("create sandbox pod: %w", err)
	}
	if err := k.waitForRunning(startCtx, name); err != nil {
		return err
	}
	// Upload the bridge + fileops scripts into the pod's writable bridge
	// emptyDir. There is no host filesystem to bind-mount them from; the
	// upload verifies byte counts so a truncated transfer fails loudly here
	// rather than as an opaque bridge error mid-turn.
	if err := k.uploadFile(startCtx, k8sBridgePath, k.cfg.BridgeScript); err != nil {
		return fmt.Errorf("upload bridge script: %w", err)
	}
	if err := k.uploadFile(startCtx, k8sFileOpsPath, fileOpsScript); err != nil {
		return fmt.Errorf("upload fileops script: %w", err)
	}
	return nil
}

func (k *k8sImpl) waitForRunning(ctx context.Context, name string) error {
	ticker := time.NewTicker(k8sPodPollInterval)
	defer ticker.Stop()
	var lastState, lastSchedule string
	for {
		pod, err := k.backend.client.getPod(ctx, k.backend.cfg.Namespace, name)
		if err == nil {
			switch pod.Status.Phase {
			case "Running":
				for _, cs := range pod.Status.ContainerStatuses {
					if cs.Name == sandboxContainerName && cs.Ready {
						return nil
					}
				}
			case "Failed", "Succeeded":
				// Status text is cluster-derived — sanitized before it enters
				// an error that upstream code logs (go/log-injection).
				return fmt.Errorf("sandbox pod %s entered terminal phase %s before becoming ready: %s", name, sanitizeClusterText(pod.Status.Phase), sanitizeClusterText(pod.Status.Message))
			}
			for _, cs := range pod.Status.ContainerStatuses {
				if cs.State.Waiting != nil {
					lastState = sanitizeClusterText(cs.State.Waiting.Reason)
					// Pull failures never self-heal within a start timeout —
					// fail fast with the reason instead of burning the window.
					if lastState == "ErrImagePull" || lastState == "ImagePullBackOff" || lastState == "InvalidImageName" {
						return fmt.Errorf("sandbox pod %s cannot pull image %s (%s): %s", name, k.cfg.Image, lastState, sanitizeClusterText(cs.State.Waiting.Message))
					}
				}
			}
			// A pod that cannot be SCHEDULED has no container status at all —
			// nothing has been assigned to a kubelet yet — so the loop above
			// sees nothing and the timeout below would say only "not ready".
			// The scheduler has already written why, and on this backend it is
			// the likeliest way a start fails: a nodeSelector or toleration
			// that matches no node, a node-pinned PersistentVolume the sandbox
			// cannot reach, or a cluster with no room left.
			//
			// Recorded, not fatal. Unlike a pull failure this often self-heals
			// inside the start window — a warm pod retires and frees its
			// Guaranteed reservation, a drained node comes back, the autoscaler
			// lands a node — so it stays worth waiting out the budget.
			if sched := podSchedulingBlocker(pod); sched != "" {
				lastSchedule = sched
			}
		}
		select {
		case <-ctx.Done():
			switch {
			case lastSchedule != "":
				// The scheduler's answer first: a pod that never got a node
				// explains the whole failure, and a container state (if any)
				// would be stale detail beside it.
				return fmt.Errorf("sandbox pod %s was never scheduled before the start timeout (%s): %w", name, lastSchedule, ctx.Err())
			case lastState != "":
				return fmt.Errorf("sandbox pod %s not ready before start timeout (last container state: %s): %w", name, lastState, ctx.Err())
			}
			return fmt.Errorf("sandbox pod %s not ready before start timeout: %w", name, ctx.Err())
		case <-ticker.C:
		}
	}
}

// podSchedulingBlocker renders the scheduler's own explanation for a pod that
// has not been placed, or "" when scheduling is not what is holding it up.
//
// Kubernetes reports this as a PodScheduled condition with status False, whose
// message is the familiar "0/N nodes are available: …" breakdown. That text is
// the single most useful line an operator can be handed here, so it is passed
// through nearly whole — sanitized like all cluster-derived text, and capped
// so a large cluster's per-node enumeration cannot turn one error into a wall.
func podSchedulingBlocker(pod *k8sPod) string {
	for _, c := range pod.Status.Conditions {
		if c.Type != "PodScheduled" || c.Status != "False" {
			continue
		}
		reason := sanitizeClusterText(c.Reason)
		msg := sanitizeClusterText(c.Message)
		const maxLen = 512
		if len(msg) > maxLen {
			cut := maxLen
			// Back up to a rune boundary so the cap cannot split a UTF-8
			// sequence and emit an invalid byte into the error text.
			for cut > 0 && !utf8.RuneStart(msg[cut]) {
				cut--
			}
			msg = msg[:cut] + "…"
		}
		switch {
		case reason != "" && msg != "":
			return reason + ": " + msg
		case reason != "":
			return reason
		case msg != "":
			return msg
		}
		return "PodScheduled=False"
	}
	return ""
}

// uploadFile writes data to path inside the pod via a one-shot exec. The v4
// exec protocol cannot half-close stdin, so the reader is bounded with
// `head -c <len>`; the write is verified by byte count.
func (k *k8sImpl) uploadFile(ctx context.Context, path string, data []byte) error {
	podName := k.currentPodName()
	if podName == "" {
		return ErrClosed
	}
	// path is one of the two fixed k8sBridgeDir constants — never
	// model-supplied — so embedding it in the shell line is safe.
	script := fmt.Sprintf("head -c %d > %s && wc -c < %s", len(data), path, path)
	var stdout, stderr bytes.Buffer
	code, err := k.backend.client.runOneShotExec(ctx, k.backend.cfg.Namespace, podName, sandboxContainerName,
		[]string{"/bin/sh", "-c", script}, data, &stdout, &stderr)
	if err != nil {
		return fmt.Errorf("upload %s: %w", path, err)
	}
	if code != 0 {
		// stderr is pod output — sanitized like all cluster-derived text.
		return fmt.Errorf("upload %s: exit %d (%.200s)", path, code, sanitizeClusterText(stderr.String()))
	}
	if got := sanitizeClusterText(strings.TrimSpace(stdout.String())); got != strconv.Itoa(len(data)) {
		return fmt.Errorf("upload %s: wrote %s of %d bytes", path, got, len(data))
	}
	return nil
}

// currentPodName snapshots the pod name under podMu — same discipline as
// containerImpl.currentContainerID; "" means already torn down.
func (k *k8sImpl) currentPodName() string {
	k.podMu.Lock()
	defer k.podMu.Unlock()
	return k.podName
}

func (k *k8sImpl) poisoned() bool { return k.execPoisoned.Load() }

// deletePodNow removes the pod immediately on a fresh context — the #796
// containment: destroying the pod destroys its PID namespace and every
// straggler in it. Reports whether the deletion (or prior disappearance) was
// confirmed. Mirrors killContainerNow, including taking the name as a
// parameter so it never touches k.mu.
func (k *k8sImpl) deletePodNow(podName string) bool {
	if podName == "" {
		return true // already torn down by close()
	}
	delCtx, cancel := context.WithTimeout(context.Background(), execReapTimeout)
	defer cancel()
	if err := k.backend.client.deletePod(delCtx, k.backend.cfg.Namespace, podName); err != nil {
		if isK8sNotFound(err) {
			return true
		}
		// The cancel path's containment (#796) is destroying the PID
		// namespace, so a delete that did not land leaves model-authored
		// stragglers running. The caller reports that honestly to the user
		// ("could not be confirmed killed"); queue a retry so it also stops
		// being true as soon as the apiserver answers again.
		log.Printf("sandbox: cancelled-exec pod delete unconfirmed (%s): %v — retrying in the background", podName, err)
		k.backend.scheduleReap(podName)
		return false
	}
	return true
}

// Retry budget for a failed pod delete. The window is sized for a control-plane
// blip an operator would call routine — a managed apiserver upgrade, an etcd
// hiccup, a throttled burst — not for a cluster that is down for the day: past
// that, giving up loudly is better than a goroutine retrying into the void,
// and a restart's boot sweep reclaims what is left because a restart makes
// every leftover somebody else's incarnation.
//
// The two intervals are atomics, not for the drain goroutines' sake (each
// computes its own wait) but for the tests': a drain goroutine legitimately
// outlives the test that spawned it — that is the feature — so the next
// test's shortened pacing must not be a data race against a live loop's read.
var (
	podReapInterval    = durationKnob(10 * time.Second) // overridden in tests
	podReapMaxInterval = durationKnob(2 * time.Minute)
	podReapMaxAttempts = 25 // ~30 minutes with the backoff below
	podReapMaxPending  = 256
)

// durationKnob is an atomically read/written duration package knob.
type reapDurationKnob struct{ ns atomic.Int64 }

func durationKnob(d time.Duration) *reapDurationKnob {
	k := &reapDurationKnob{}
	k.ns.Store(int64(d))
	return k
}

func (k *reapDurationKnob) get() time.Duration  { return time.Duration(k.ns.Load()) }
func (k *reapDurationKnob) set(d time.Duration) { k.ns.Store(int64(d)) }

// scheduleReap queues a pod whose delete failed, and makes sure a drain
// goroutine is running. The goroutine exits when the queue empties, so an
// installation that never fails a delete never carries one.
func (b *KubernetesBackend) scheduleReap(podName string) {
	if podName == "" {
		return
	}
	b.reapMu.Lock()
	defer b.reapMu.Unlock()
	if b.reapPending == nil {
		b.reapPending = make(map[string]int)
	}
	if _, known := b.reapPending[podName]; !known && len(b.reapPending) >= podReapMaxPending {
		// Refusing to grow without bound is the point of a bound; say so
		// rather than dropping the pod silently.
		log.Printf("sandbox: pod-delete retry queue is full (%d) — %s will be reclaimed by the boot-time orphan sweep instead", podReapMaxPending, sanitizeClusterText(podName))
		return
	}
	b.reapPending[podName] = 0
	if !b.reapRunning {
		b.reapRunning = true
		go b.reapLoop()
	}
}

// reapLoop retries queued deletes until they succeed, the budget runs out, or
// the process exits. Backoff is global rather than per-pod on purpose: these
// failures share one cause (the apiserver is unreachable), so pacing the whole
// queue together is what avoids hammering a cluster that is already unwell.
func (b *KubernetesBackend) reapLoop() {
	wait := podReapInterval.get()
	for {
		time.Sleep(wait)
		wait *= 2
		if ceiling := podReapMaxInterval.get(); wait > ceiling {
			wait = ceiling
		}

		b.reapMu.Lock()
		names := make([]string, 0, len(b.reapPending))
		for name := range b.reapPending {
			names = append(names, name)
		}
		b.reapMu.Unlock()

		for _, name := range names {
			ctx, cancel := context.WithTimeout(context.Background(), execReapTimeout)
			err := b.client.deletePod(ctx, b.cfg.Namespace, name)
			cancel()
			if err == nil || isK8sNotFound(err) {
				b.reapMu.Lock()
				delete(b.reapPending, name)
				b.reapMu.Unlock()
				log.Printf("sandbox: reclaimed sandbox pod %s on retry (its delete had failed while the apiserver was unreachable)", sanitizeClusterText(name))
				continue
			}
			b.reapMu.Lock()
			b.reapPending[name]++
			giveUp := b.reapPending[name] >= podReapMaxAttempts
			if giveUp {
				delete(b.reapPending, name)
			}
			b.reapMu.Unlock()
			if giveUp {
				log.Printf("sandbox: gave up deleting sandbox pod %s after %d attempts — it holds its resource reservation until the boot-time orphan sweep on the next control-plane start: %v",
					sanitizeClusterText(name), podReapMaxAttempts, err)
			}
		}

		b.reapMu.Lock()
		if len(b.reapPending) == 0 {
			b.reapRunning = false
			b.reapMu.Unlock()
			return
		}
		b.reapMu.Unlock()
	}
}

func (k *k8sImpl) runBash(ctx context.Context, req BashRequest) (BashResult, error) {
	podName := k.currentPodName()
	if podName == "" {
		return BashResult{}, fmt.Errorf("run bash: %w", ErrClosed)
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// The exec API has no --workdir; a positional-parameter wrapper applies
	// the cwd without any quoting of the user command or the directory.
	command := []string{"bash", "-c", req.Command}
	if req.WorkingDir != "" {
		command = []string{"/bin/sh", "-c", `cd -- "$1" || exit 126; shift; exec bash -c "$1"`, "fleet-bash", req.WorkingDir, req.Command}
	}

	stdoutBuf := &cappedBuffer{cap: BashOutputCaptureCap}
	stderrBuf := &cappedBuffer{cap: BashOutputCaptureCap}
	code, execErr := k.backend.client.runOneShotExec(cmdCtx, k.backend.cfg.Namespace, podName, sandboxContainerName,
		command, nil, stdoutBuf, stderrBuf)

	// snapshot(), not the live buffers: a cancelled StreamWithContext returns
	// without joining its copy goroutines, so an abandoned copier may still be
	// writing while this result is assembled (the PR #1285 -race finding).
	stdoutBytes, stdoutDiscarded := stdoutBuf.snapshot()
	stderrBytes, stderrDiscarded := stderrBuf.snapshot()
	res := BashResult{
		ExitCode:        code,
		Stdout:          stdoutBytes,
		Stderr:          stderrBytes,
		StdoutDiscarded: stdoutDiscarded,
		StderrDiscarded: stderrDiscarded,
	}
	if cmdCtx.Err() != nil {
		// Cancellation/timeout only tore down the exec CONNECTION; the shell
		// and its descendants keep running in the pod (#796). Delete the pod
		// synchronously — destroying the PID namespace is the one guaranteed
		// containment — and poison the sandbox so it is retired, exactly like
		// the podman backend.
		res.TimedOut = errors.Is(cmdCtx.Err(), context.DeadlineExceeded)
		res.Cancelled = !res.TimedOut
		k.execPoisoned.Store(true)
		res.CleanupConfirmed = k.deletePodNow(podName)
		res.SandboxRetired = true
		return res, nil
	}
	if execErr != nil {
		// Same reasoning as the bridge EOF above: an exec against an evicted
		// or OOM-killed pod fails with a transport error that names no cause.
		return res, fmt.Errorf("pod exec bash: %w%s", execErr, k.podFailureSuffix())
	}
	return res, nil
}

func (k *k8sImpl) runFileOp(ctx context.Context, req FileOpRequest) (FileOpResult, error) {
	anchor, readOnly, err := fileOpAnchorFor(k.cfg.WorkspaceHostDir, k.cfg.ReadOnlyMounts, req.Root)
	if err != nil {
		return FileOpResult{}, err
	}
	if readOnly && req.Op != FileOpRead {
		return FileOpResult{}, fmt.Errorf("fileop write requested beneath a read-only mount: %w", ErrFileOpUnsafePath)
	}
	if !readOnly && !req.rootBound {
		return FileOpResult{}, fmt.Errorf("writable fileop root was not bound before the turn: %w", ErrFileOpUnsafePath)
	}
	return k.executeFileOp(ctx, req, anchor)
}

func (k *k8sImpl) bindFileOpRoot(ctx context.Context, root string) (FileOpRootIdentity, error) {
	anchor, readOnly, err := fileOpAnchorFor(k.cfg.WorkspaceHostDir, k.cfg.ReadOnlyMounts, root)
	if err != nil {
		return FileOpRootIdentity{}, err
	}
	if readOnly {
		return FileOpRootIdentity{}, fmt.Errorf("cannot bind a read-only mount as the writable fileop root: %w", ErrFileOpUnsafePath)
	}
	res, err := k.executeFileOp(ctx, FileOpRequest{Op: fileOpBindRoot, Path: root, Root: root}, anchor)
	if err != nil {
		return FileOpRootIdentity{}, err
	}
	return res.rootIdentity, nil
}

func (k *k8sImpl) executeFileOp(ctx context.Context, req FileOpRequest, anchor string) (FileOpResult, error) {
	podName := k.currentPodName()
	if podName == "" {
		return FileOpResult{}, fmt.Errorf("fileop %s: %w", req.Op, ErrClosed)
	}
	cmdCtx, cancel := context.WithTimeout(ctx, fileOpTimeout)
	defer cancel()

	reqJSON, err := encodeFileOpWire(req, anchor)
	if err != nil {
		return FileOpResult{}, err
	}

	// fileops.py reads stdin to EOF; v4 exec cannot half-close stdin, so the
	// read is bounded with `head -c <len>` — EOF arrives when head exits.
	script := fmt.Sprintf("head -c %d | python3 %s", len(reqJSON), k8sFileOpsPath)
	var stdout, stderr bytes.Buffer
	code, execErr := k.backend.client.runOneShotExec(cmdCtx, k.backend.cfg.Namespace, podName, sandboxContainerName,
		[]string{"/bin/sh", "-c", script}, reqJSON, &stdout, &stderr)
	if execErr != nil || cmdCtx.Err() != nil {
		if cmdCtx.Err() != nil {
			// Same containment as bash: the helper may still be alive inside a
			// persistent pod and could complete a rename after the turn stopped.
			k.execPoisoned.Store(true)
			_ = k.deletePodNow(podName)
			return FileOpResult{}, fmt.Errorf("fileop %s interrupted (%w); sandbox retired: %w", req.Op, cmdCtx.Err(), ErrPoisoned)
		}
		return FileOpResult{}, fmt.Errorf("fileop %s exec: %w (%.200s)", req.Op, execErr, sanitizeClusterText(stderr.String()))
	}
	if code != 0 {
		return FileOpResult{}, fmt.Errorf("fileop %s: helper exit %d (%.200s)", req.Op, code, sanitizeClusterText(stderr.String()))
	}
	return decodeFileOpResponse(stdout.Bytes())
}

// encodeFileOpWire builds the JSON request fileops.py reads — shared shape
// with the podman/host backends (their inline wire-building predates this
// helper; the k8s backend uses it so the three cannot drift further).
func encodeFileOpWire(req FileOpRequest, anchor string) ([]byte, error) {
	wire := map[string]any{
		"op":     string(req.Op),
		"path":   req.Path,
		"root":   req.Root,
		"anchor": anchor,
	}
	switch req.Op {
	case FileOpRead:
		wire["offset"] = req.Offset
		wire["limit"] = req.Limit
	case FileOpWrite:
		wire["data_b64"] = base64.StdEncoding.EncodeToString(req.Data)
	case FileOpEdit:
		wire["old_b64"] = base64.StdEncoding.EncodeToString([]byte(req.OldText))
		wire["new_b64"] = base64.StdEncoding.EncodeToString([]byte(req.NewText))
		wire["replace_all"] = req.ReplaceAll
		if req.ExpectedSHA256 != "" {
			wire["expected_sha256"] = req.ExpectedSHA256
		}
	case fileOpBindRoot:
		// Root + anchor are the complete request.
	default:
		return nil, fmt.Errorf("unknown fileop %q", req.Op)
	}
	if req.testPause > 0 {
		wire["test_pause_ms"] = req.testPause.Milliseconds()
		wire["test_ready_name"] = req.testReadyName
	}
	if req.rootBound {
		wire["expected_dev"] = req.expectedDev
		wire["expected_ino"] = req.expectedIno
	}
	out, err := json.Marshal(wire)
	if err != nil {
		return nil, fmt.Errorf("marshal fileop: %w", err)
	}
	return out, nil
}

func (k *k8sImpl) runPython(ctx context.Context, req PythonRequest) (PythonResult, error) {
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	if err := k.ensureBridge(); err != nil {
		return PythonResult{}, fmt.Errorf("start python bridge in pod: %w", err)
	}

	wireReq := bridgeRequest{
		Code:           req.Code,
		ReturnVars:     req.ReturnVars,
		TimeoutSeconds: int(timeout.Seconds()),
		WorkspaceDir:   req.WorkspaceDir,
		ResetKernel:    req.ResetKernel,
	}
	reqBytes, err := json.Marshal(wireReq)
	if err != nil {
		return PythonResult{}, fmt.Errorf("marshal bridge request: %w", err)
	}

	k.mu.Lock()
	defer k.mu.Unlock()

	// Re-validate under the lock (mirrors containerImpl): a concurrent close()
	// nils the bridge fields between ensureBridge and this re-acquire.
	if k.bridge == nil || k.bridgeStdout == nil {
		return PythonResult{}, fmt.Errorf("send bridge request: %w", ErrClosed)
	}
	if err := k.bridge.writeStdin(append(reqBytes, '\n')); err != nil {
		k.terminateBridgeLocked()
		return PythonResult{}, fmt.Errorf("send bridge request: %w%s", err, k.bridgeStderrSuffix())
	}

	type readResult struct {
		data      []byte
		discarded int64
		err       error
	}
	ch := make(chan readResult, 1)
	// Snapshot the reader before launching the goroutine — the cancel/timeout
	// arms nil the field via terminateBridgeLocked (#583's lesson, upheld).
	stdout := k.bridgeStdout
	go func() {
		defer safe.Recover("sandbox.k8s.bridge_read", func(any) {
			ch <- readResult{err: fmt.Errorf("bridge reader panicked")}
		})
		data, discarded, err := readCappedLine(stdout, bridgeResponseCaptureCap)
		ch <- readResult{data: data, discarded: discarded, err: err}
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		// The cell keeps executing in the pod until its PID namespace goes
		// away — delete the pod and poison, exactly the podman #796 handling.
		k.execPoisoned.Store(true)
		_ = k.deletePodNow(k.currentPodName())
		k.terminateBridgeLocked()
		return PythonResult{}, fmt.Errorf("python execution cancelled (%w); sandbox retired: %w", ctx.Err(), ErrPoisoned)
	case <-timer.C:
		k.execPoisoned.Store(true)
		_ = k.deletePodNow(k.currentPodName())
		k.terminateBridgeLocked()
		return PythonResult{}, fmt.Errorf("python execution timed out after %v; sandbox retired: %w", timeout, ErrPoisoned)
	case r := <-ch:
		if r.err != nil {
			// Session dead: the pod-side bridge exited, or the connection
			// dropped — or the pod itself is gone, which reads identically
			// from here. Reset the bridge so a surviving pod boots a fresh
			// session on the next call, and ask the cluster which of those it
			// was, because "EOF" alone sends the reader guessing.
			k.terminateBridgeLocked()
			return PythonResult{}, fmt.Errorf("bridge closed unexpectedly: %w%s%s", r.err, k.podFailureSuffix(), k.bridgeStderrSuffix())
		}
		if r.discarded > 0 {
			return PythonResult{}, fmt.Errorf("bridge response exceeded %d bytes (%d bytes discarded) and was dropped — return large results by writing them to a workspace file instead", bridgeResponseCaptureCap, r.discarded)
		}
		return parseBridgeResponse(r.data)
	}
}

// ensureBridge starts the bridge exec session on first use, mirroring
// containerImpl.ensureBridge (including the post-start settle delay outside
// the lock).
func (k *k8sImpl) ensureBridge() error {
	started, err := k.startBridgeIfNeeded()
	if err != nil || !started {
		return err
	}
	time.Sleep(100 * time.Millisecond)
	return nil
}

func (k *k8sImpl) startBridgeIfNeeded() (started bool, err error) {
	k.mu.Lock()
	defer k.mu.Unlock()

	if k.bridgeStarted && k.bridge != nil {
		select {
		case <-k.bridge.done:
			// Session ended behind our back (pod-side exit) — fall through and
			// start a fresh one.
		default:
			return false, nil
		}
	}

	podName := k.currentPodName()
	if podName == "" {
		return false, fmt.Errorf("start bridge: %w", ErrClosed)
	}

	// The bridge reads JSON-per-line from stdin and writes JSON-per-line to
	// stdout; the demux loop feeds a pipe the reader side wraps in bufio.
	pr, pw := io.Pipe()
	stderrBuf := &syncBuffer{}
	// The bridge intentionally outlives any single request ctx and is torn
	// down in close() / terminateBridgeLocked — dial under a handshake-bounded
	// background context, mirroring the podman backend's noctx bridge exec.
	session, err := k.backend.client.execPod(context.Background(), k.backend.cfg.Namespace, podName, sandboxContainerName,
		[]string{"python3", k8sBridgePath}, true, pw, io.MultiWriter(os.Stderr, stderrBuf))
	if err != nil {
		_ = pw.Close()
		_ = pr.Close()
		return false, fmt.Errorf("start bridge exec: %w", err)
	}
	// Unblock any reader waiting on the pipe once the session ends — the
	// demux loop does not own the pipe writer.
	go func() {
		<-session.done
		_ = pw.CloseWithError(io.EOF)
	}()
	k.bridge = session
	k.bridgeStdout = bufio.NewReader(pr)
	k.bridgeStdoutPipe = pr
	k.bridgeStderr = stderrBuf
	k.bridgeStarted = true
	return true, nil
}

// terminateBridgeLocked tears the bridge session down and clears the state so
// the next ensureBridge starts fresh. Caller must hold k.mu. Closing the
// websocket ends the server-side exec streams; an orphaned kernel inside a
// still-live pod is reaped by the next bridge start (reap_stale_kernels in
// python_bridge.py), the same recovery story as the podman backend.
func (k *k8sImpl) terminateBridgeLocked() {
	// Order matters, and getting it wrong deadlocks teardown under k.mu.
	//
	// The demux loop is the pipe's WRITER; the only reader is the short-lived
	// goroutine inside a runPython call, which stops at the first newline. Any
	// pod-side stdout past that response line leaves the loop parked in
	// pw.Write, because io.Pipe is unbuffered and hands off byte-for-byte. The
	// goroutine that closes the writer waits on session.done — which is the
	// very thing the parked loop cannot reach. So close the READ half first:
	// that fails the pending Write with ErrClosedPipe, the loop exits, done
	// fires, and the join below returns. Without it, close() blocked forever
	// holding k.mu and the sandbox pod was never deleted.
	if k.bridgeStdoutPipe != nil {
		_ = k.bridgeStdoutPipe.CloseWithError(io.EOF)
	}
	if k.bridge != nil {
		k.bridge.close()
	}
	k.bridge = nil
	k.bridgeStdout = nil
	k.bridgeStdoutPipe = nil
	k.bridgeStarted = false
}

func (k *k8sImpl) bridgeStderrSuffix() string {
	if k.bridgeStderr == nil {
		return ""
	}
	// Pod output — sanitized like all cluster-derived text before it joins an
	// error string that upstream code logs.
	stderr := sanitizeClusterText(strings.TrimSpace(k.bridgeStderr.Snapshot()))
	if stderr == "" {
		return ""
	}
	const maxLen = 1024
	if len(stderr) > maxLen {
		stderr = stderr[len(stderr)-maxLen:]
	}
	return " (bridge stderr: " + stderr + ")"
}

// podFailureSuffix asks the apiserver why the sandbox pod stopped serving, and
// renders the answer as a clause to append to whatever error the caller is
// already returning.
//
// It exists because the two failures that end a turn most confusingly are
// invisible from inside the pod. A kubelet eviction (ephemeral-storage or an
// emptyDir sizeLimit) and an OOM kill both reach the caller as a dead exec
// stream — `bridge closed unexpectedly: EOF`, or an exec error naming nothing
// — while the reason sits in the pod's own status, one GET away on a client
// this sandbox already holds. Left unsaid, a model asked to explain the
// failure will invent a cause; observed during the #1264 validation, where an
// emptyDir eviction was confidently reported to the user as an OOM kill.
//
// Diagnosis must never make things worse, so this is best-effort in every
// direction: it runs only on an error path, on its own short-lived context
// (the turn's is usually already cancelled), and returns "" on any doubt
// rather than risk masking the real error with a lookup failure.
func (k *k8sImpl) podFailureSuffix() string {
	podName := k.currentPodName()
	if podName == "" {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), execReapTimeout)
	defer cancel()
	pod, err := k.backend.client.getPod(ctx, k.backend.cfg.Namespace, podName)
	if err != nil {
		if isK8sNotFound(err) {
			return " (the sandbox pod is gone — the cluster deleted or evicted it mid-call)"
		}
		// Saying nothing here masks nothing, but it does something worse: the
		// condition most likely to kill a sandbox IS cluster trouble, so the
		// explanation would go missing exactly when it is most needed, and a
		// model handed a bare EOF fills the silence with a guess — the failure
		// this function exists to prevent. Seen on the validation cluster: a
		// turn died during an apiserver wobble and was explained to the user as
		// "likely container resource/disk limits or an internal sandbox timeout
		// ceiling", neither of which had happened.
		//
		// Naming the unanswered question is still appended to the original
		// error, never a replacement for it.
		return " (the cluster could not be asked why — the apiserver did not answer)"
	}
	// Eviction is a POD-level verdict: kubelet sets phase=Failed with
	// reason=Evicted and a message naming the limit that was exceeded.
	if pod.Status.Phase == "Failed" {
		reason := sanitizeClusterText(pod.Status.Reason)
		msg := sanitizeClusterText(pod.Status.Message)
		switch {
		case reason != "" && msg != "":
			return " (the sandbox pod failed: " + reason + " — " + msg + ")"
		case reason != "":
			return " (the sandbox pod failed: " + reason + ")"
		case msg != "":
			return " (the sandbox pod failed: " + msg + ")"
		default:
			return " (the sandbox pod entered the Failed phase)"
		}
	}
	// An OOM kill is a CONTAINER-level verdict and leaves the pod Running, so
	// it has to be read separately — and it is the one most worth naming,
	// since it is the cause a reader guesses for every vanished sandbox.
	for _, cs := range pod.Status.ContainerStatuses {
		t := cs.State.Terminated
		if t == nil {
			continue
		}
		reason := sanitizeClusterText(t.Reason)
		if reason == "" {
			reason = "terminated"
		}
		return fmt.Sprintf(" (the sandbox container %s, exit code %d)", reason, t.ExitCode)
	}
	return ""
}

// resourceUsage reports no telemetry: the k8s backend has no `podman stats`
// counterpart wired up (kubelet metrics need a different collection path).
// Recorded as an honest deviation in docs/DEPLOYMENT-KUBERNETES.md.
func (k *k8sImpl) resourceUsage() (ResourceUsageSummary, bool) {
	return ResourceUsageSummary{}, false
}

func (k *k8sImpl) close() {
	k.mu.Lock()
	k.terminateBridgeLocked()
	k.bridgeStderr = nil
	k.mu.Unlock()

	k.podMu.Lock()
	podName := k.podName
	k.podName = ""
	k.podMu.Unlock()

	if podName != "" {
		delCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := k.backend.client.deletePod(delCtx, k.backend.cfg.Namespace, podName); err != nil && !isK8sNotFound(err) {
			log.Printf("sandbox: close-time pod delete unconfirmed (%s): %v — retrying in the background", podName, err)
			k.backend.scheduleReap(podName)
		}
	}
}

// PruneOrphanedPods removes leftover sandbox pods from a prior fleet process
// (a crash never runs close(), so in-flight + warm pods outlive it). The
// kubernetes counterpart of PruneOrphanedContainers, but the ownership test is
// deliberately NOT podman's.
//
// Podman asks "is the labeled owner pid still alive in my pid namespace?".
// That question is meaningless across containers, and answering it here was a
// bug with two edges. The label's pid came from a namespace that no longer
// exists; the fleet process is pid 1 in its own container; and
// labeledOwnerStillRunning also allows a 120s start-time tolerance. So a
// control plane that restarted quickly — every crashloop, every rolling
// update — asked whether pid 1 was running, found itself, and skipped every
// pod its predecessor leaked. Those pods are Guaranteed-QoS, so each one holds
// its full CPU and memory reservation until something else reclaims it, and
// nothing else does: there are no ownerReferences and no periodic sweep.
// The other edge cut the opposite way: a second fleet release sharing the
// namespace could look like a dead owner and have its live sandboxes deleted.
//
// In-cluster the test is now identity, not liveness: the pod UID plus this
// process's boot nonce names the incarnation (see k8sInstanceLabel — the UID
// alone would survive an in-place container restart and re-open this same
// hole) and the release name marks the owner. A pod belonging to my release
// but not to my incarnation is by definition my predecessor's, because the
// chart runs a single-replica Recreate Deployment. A pod belonging to another
// release is never touched.
// Out of cluster there is no such identity, so the pid heuristic stays — it is
// still true there.
func (b *KubernetesBackend) PruneOrphanedPods(ctx context.Context) (int, error) {
	selector := k8sLabelName + "=" + k8sLabelNameValue + "," + k8sLabelManagedBy + "=" + k8sLabelManagedVal
	list, err := b.client.listPods(ctx, b.cfg.Namespace, selector)
	if err != nil {
		return 0, fmt.Errorf("list orphaned sandbox pods: %w", err)
	}
	removed := 0
	var errs []error
	for _, pod := range list.Items {
		label := pod.Metadata.Labels[k8sLabelInstance]
		if label == k8sInstanceLabel {
			continue
		}
		if k8sControlPlaneOwner != "" {
			// Another release's pod: never ours to reclaim, however dead its
			// owner looks. Leaking a pod is recoverable; deleting a live
			// neighbour's sandbox mid-turn is not.
			if pod.Metadata.Labels[k8sLabelOwner] != k8sControlPlaneOwner {
				continue
			}
		} else {
			pid, startedAt, ok := parseK8sInstanceLabel(label)
			if ok && labeledOwnerStillRunning(pid, startedAt) {
				continue
			}
		}
		if err := b.client.deletePod(ctx, b.cfg.Namespace, pod.Metadata.Name); err != nil {
			if isK8sNotFound(err) {
				continue
			}
			// Accumulate rather than abort: one pod stuck terminating (a
			// finalizer, a NotReady node) used to leave every later orphan in
			// the list un-swept until the next boot.
			// The name came back from the API list — sanitized like all
			// cluster-derived text before it enters a logged error.
			errs = append(errs, fmt.Errorf("remove orphaned sandbox pod %s: %w", sanitizeClusterText(pod.Metadata.Name), err))
			continue
		}
		removed++
	}
	return removed, errors.Join(errs...)
}
