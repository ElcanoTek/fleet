package agent

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/ElcanoTek/fleet/internal/clientconfig"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/sandbox"
)

// ApplyBundleSandbox resolves the bundle manifest's sandbox block into cfg,
// with env (already loaded into cfg) winning field by field. It is the ONE
// resolver for every entrypoint that builds a sandbox pool from a bundle —
// `fleet serve`, `fleet task run` and `fleet validate-config` — so they cannot
// drift in which image, runtime or backend they mean. Pure: it logs nothing
// (callers log what they want around it) and touches only cfg.
//
//   - Image: an explicit FLEET_SANDBOX_IMAGE wins, else the manifest's
//     sandbox.image (the opt-in prebuilt/registry path), else sandbox.tag (the
//     build-on-box default). fleet does NOT build the image here — bootstrap /
//     scripts/build-sandbox-image.sh does; this only feeds the resolved ref to
//     the consuming pool.
//   - OCI runtime (#217): FLEET_SANDBOX_RUNTIME wins, else sandbox.runtime,
//     normalized once (sandbox.ResolveRuntime: "libkrun" → "krun") so every
//     consumer (pool, preflight, readiness probe, validate-config) keys off the
//     same value. Both sources are operator-authored deployment config, so the
//     runtime is a trusted operator choice, not an untrusted downgrade.
//   - Backend (#989): FLEET_SANDBOX_BACKEND wins, else sandbox.backend, else
//     podman. An unrecognized value is an error — never a silent fallback to
//     podman. Under kubernetes each FLEET_SANDBOX_K8S_* setting is filled from
//     sandbox.kubernetes when its env var is empty; under podman that block is
//     not consulted at all.
//   - Egress allowlist (#211): the manifest's network_allowlist, which
//     FLEET_DEFAULT_NETWORK_MODE=allowlisted draws from.
//
// A nil bundle is treated as an empty manifest.
func ApplyBundleSandbox(cfg *config.Config, bundle *clientconfig.Bundle) error {
	var sb clientconfig.Sandbox
	if bundle != nil {
		sb = bundle.Sandbox()
	}
	if strings.TrimSpace(cfg.SandboxImage) == "" {
		if ref := sb.ResolvedImageRef(); ref != "" {
			cfg.SandboxImage = ref
		}
	}
	// Always write back the normalized value so cfg carries the canonical name
	// (also collapses whitespace-padded env values the loader didn't trim).
	cfg.SandboxRuntime = sandbox.ResolveRuntime(cfg.SandboxRuntime, sb.Runtime)
	cfg.SandboxNetworkAllowlist = append([]string(nil), sb.NetworkAllowlist...)

	resolved, err := sandbox.ResolveBackend(cfg.SandboxBackend, sb.Backend)
	if err != nil {
		return err
	}
	cfg.SandboxBackend = resolved
	if resolved != sandbox.BackendKubernetes {
		return nil
	}
	return applyBundleKubernetes(cfg, sb.Kubernetes)
}

// applyBundleKubernetes fills each kubernetes setting from the manifest's
// sandbox.kubernetes block when the corresponding FLEET_SANDBOX_K8S_* env var
// is empty. Structured manifest values are canonicalized into the env vars'
// string forms so the pool build has ONE source to parse.
func applyBundleKubernetes(cfg *config.Config, k clientconfig.KubernetesSandbox) error {
	fill := func(dst *string, bundleVal string) {
		if strings.TrimSpace(*dst) == "" {
			*dst = strings.TrimSpace(bundleVal)
		}
	}
	fill(&cfg.SandboxK8sNamespace, k.Namespace)
	fill(&cfg.SandboxK8sWorkspaceClaim, k.WorkspaceClaim)
	fill(&cfg.SandboxK8sServiceAccount, k.ServiceAccount)
	fill(&cfg.SandboxK8sImagePullSecret, k.ImagePullSecret)
	fill(&cfg.SandboxK8sRuntimeClass, k.RuntimeClass)
	fill(&cfg.SandboxK8sSeccompProfile, k.SeccompProfile)
	fill(&cfg.SandboxK8sKubeconfig, k.Kubeconfig)
	fill(&cfg.SandboxK8sNetworkPolicy, k.NetworkPolicy)
	fill(&cfg.SandboxK8sOpenEgressPolicy, k.OpenEgressPolicy)
	// bundle_docs_in_image is a manifest bool and an env string. Only a
	// manifest TRUE needs carrying: false is already the empty-string default,
	// and writing "false" here would make an unset env look explicitly disabled.
	if strings.TrimSpace(cfg.SandboxK8sBundleDocsInImage) == "" && k.BundleDocsInImage {
		cfg.SandboxK8sBundleDocsInImage = "true"
	}
	if strings.TrimSpace(cfg.SandboxK8sNodeSelector) == "" && len(k.NodeSelector) > 0 {
		keys := make([]string, 0, len(k.NodeSelector))
		for key := range k.NodeSelector {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		pairs := make([]string, 0, len(keys))
		for _, key := range keys {
			pairs = append(pairs, key+"="+k.NodeSelector[key])
		}
		cfg.SandboxK8sNodeSelector = strings.Join(pairs, ",")
	}
	if strings.TrimSpace(cfg.SandboxK8sTolerations) == "" && len(k.Tolerations) > 0 {
		raw, err := json.Marshal(k.Tolerations)
		if err != nil {
			return fmt.Errorf("encode sandbox.kubernetes.tolerations: %w", err)
		}
		cfg.SandboxK8sTolerations = string(raw)
	}
	return nil
}
