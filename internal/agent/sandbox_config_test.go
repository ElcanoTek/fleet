package agent

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ElcanoTek/fleet/internal/clientconfig"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/sandbox"
)

// k8sBundle writes a minimal bundle whose sandbox block carries the given
// kubernetes: YAML, so the manifest→config fill can be exercised without a
// cluster.
func k8sBundle(t *testing.T, kubernetesYAML string) *clientconfig.Bundle {
	t.Helper()
	dir := t.TempDir()
	// sandbox.image spares the fixture a Containerfile: the loader insists on
	// one only for the build-on-box path.
	manifest := "sandbox:\n  image: registry.example/fleet-sandbox:test\n  backend: kubernetes\n  kubernetes:\n" + kubernetesYAML
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	bundle, err := clientconfig.Load(dir)
	if err != nil {
		t.Fatalf("load bundle: %v", err)
	}
	return bundle
}

// bundle_docs_in_image is a manifest bool and an env string. It must follow the
// same env-wins-else-bundle precedence as every other kubernetes setting —
// including the case that motivated the field: an operator turning it OFF from
// the chart for a bundle whose manifest turns it on.
func TestApplyBundleSandboxBundleDocsInImage(t *testing.T) {
	tests := []struct {
		name     string
		manifest string
		env      string
		want     string
	}{
		{name: "manifest on, env unset", manifest: "    bundle_docs_in_image: true\n", want: "true"},
		{name: "manifest on, env off wins", manifest: "    bundle_docs_in_image: true\n", env: "false", want: "false"},
		{name: "manifest off, env unset stays empty", manifest: "    bundle_docs_in_image: false\n", want: ""},
		{name: "manifest silent, env on wins", manifest: "    workspace_claim: ws\n", env: "true", want: "true"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			bundle := k8sBundle(t, tc.manifest)
			cfg := &config.Config{SandboxK8sBundleDocsInImage: tc.env}
			if err := ApplyBundleSandbox(cfg, bundle); err != nil {
				t.Fatalf("ApplyBundleSandbox: %v", err)
			}
			if cfg.SandboxK8sBundleDocsInImage != tc.want {
				t.Errorf("SandboxK8sBundleDocsInImage = %q, want %q", cfg.SandboxK8sBundleDocsInImage, tc.want)
			}
			// Whatever the resolved string, it must parse — the pool build
			// treats a parse error as a boot failure.
			if _, err := sandbox.ParseK8sBundleDocsInImage(cfg.SandboxK8sBundleDocsInImage); err != nil {
				t.Errorf("resolved value does not parse: %v", err)
			}
		})
	}
}

// Under the podman backend the kubernetes block is not consulted at all: a
// bundle that declares baked-in docs for its cluster deployment must not have
// that leak into a single-box install, where the real bind mounts apply.
func TestApplyBundleSandboxPodmanIgnoresKubernetesBlock(t *testing.T) {
	dir := t.TempDir()
	manifest := "sandbox:\n  image: registry.example/fleet-sandbox:test\n  kubernetes:\n    bundle_docs_in_image: true\n    workspace_claim: ws\n"
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	bundle, err := clientconfig.Load(dir)
	if err != nil {
		t.Fatalf("load bundle: %v", err)
	}
	cfg := &config.Config{}
	if err := ApplyBundleSandbox(cfg, bundle); err != nil {
		t.Fatalf("ApplyBundleSandbox: %v", err)
	}
	if cfg.SandboxBackend != sandbox.BackendPodman {
		t.Fatalf("SandboxBackend = %q, want podman (the default)", cfg.SandboxBackend)
	}
	if cfg.SandboxK8sBundleDocsInImage != "" || cfg.SandboxK8sWorkspaceClaim != "" {
		t.Errorf("kubernetes settings leaked into the podman path: docs=%q claim=%q",
			cfg.SandboxK8sBundleDocsInImage, cfg.SandboxK8sWorkspaceClaim)
	}
}

// A bundle that selects the kubernetes backend with no env override must reach
// cfg: every entrypoint (serve, task run, validate-config) reads the backend
// from here.
func TestApplyBundleSandboxBundleOnlyKubernetesBackend(t *testing.T) {
	bundle := k8sBundle(t, "    workspace_claim: ws\n    namespace: sandboxes\n")
	cfg := &config.Config{}
	if err := ApplyBundleSandbox(cfg, bundle); err != nil {
		t.Fatalf("ApplyBundleSandbox: %v", err)
	}
	if cfg.SandboxBackend != sandbox.BackendKubernetes {
		t.Fatalf("SandboxBackend = %q, want kubernetes from the bundle", cfg.SandboxBackend)
	}
	if cfg.SandboxK8sWorkspaceClaim != "ws" || cfg.SandboxK8sNamespace != "sandboxes" {
		t.Errorf("kubernetes settings not filled from the bundle: claim=%q namespace=%q",
			cfg.SandboxK8sWorkspaceClaim, cfg.SandboxK8sNamespace)
	}
	if cfg.SandboxImage != "registry.example/fleet-sandbox:test" {
		t.Errorf("SandboxImage = %q, want the bundle's sandbox.image", cfg.SandboxImage)
	}
}

// sandbox.kubernetes.open_egress_policy is documented as the manifest
// spelling of FLEET_SANDBOX_K8S_OPEN_EGRESS_POLICY; boot used to drop it, so
// the preflight demanded the default-named policy whatever the bundle said.
func TestApplyBundleSandboxOpenEgressPolicy(t *testing.T) {
	bundle := k8sBundle(t, "    open_egress_policy: bundle-open-egress\n")
	cfg := &config.Config{}
	if err := ApplyBundleSandbox(cfg, bundle); err != nil {
		t.Fatalf("ApplyBundleSandbox: %v", err)
	}
	if cfg.SandboxK8sOpenEgressPolicy != "bundle-open-egress" {
		t.Errorf("SandboxK8sOpenEgressPolicy = %q, want the bundle's open_egress_policy", cfg.SandboxK8sOpenEgressPolicy)
	}
}

// Env (already loaded into cfg) wins over the bundle, field by field —
// including the backend itself.
func TestApplyBundleSandboxEnvWins(t *testing.T) {
	bundle := k8sBundle(t, "    open_egress_policy: bundle-open-egress\n    namespace: bundle-ns\n    network_policy: bundle-deny\n")
	cfg := &config.Config{
		SandboxImage:               "env.example/sandbox:env",
		SandboxK8sOpenEgressPolicy: "env-open-egress",
		SandboxK8sNamespace:        "env-ns",
	}
	if err := ApplyBundleSandbox(cfg, bundle); err != nil {
		t.Fatalf("ApplyBundleSandbox: %v", err)
	}
	if cfg.SandboxImage != "env.example/sandbox:env" {
		t.Errorf("SandboxImage = %q, want the env value", cfg.SandboxImage)
	}
	if cfg.SandboxK8sOpenEgressPolicy != "env-open-egress" || cfg.SandboxK8sNamespace != "env-ns" {
		t.Errorf("env lost to the bundle: open-egress=%q namespace=%q", cfg.SandboxK8sOpenEgressPolicy, cfg.SandboxK8sNamespace)
	}
	if cfg.SandboxK8sNetworkPolicy != "bundle-deny" {
		t.Errorf("SandboxK8sNetworkPolicy = %q, want the bundle's (no env set)", cfg.SandboxK8sNetworkPolicy)
	}

	// An env backend overrides the bundle's kubernetes selection, and then the
	// kubernetes block is not consulted at all.
	podman := &config.Config{SandboxBackend: sandbox.BackendPodman}
	if err := ApplyBundleSandbox(podman, bundle); err != nil {
		t.Fatalf("ApplyBundleSandbox: %v", err)
	}
	if podman.SandboxBackend != sandbox.BackendPodman || podman.SandboxK8sOpenEgressPolicy != "" {
		t.Errorf("env podman backend: backend=%q open-egress=%q", podman.SandboxBackend, podman.SandboxK8sOpenEgressPolicy)
	}
}

// An unrecognized backend fails closed rather than falling back to podman.
func TestApplyBundleSandboxUnknownBackendFails(t *testing.T) {
	if err := ApplyBundleSandbox(&config.Config{SandboxBackend: "nomad"}, nil); err == nil {
		t.Fatal("an unrecognized backend must be an error")
	}
}

// A one-shot run in allowlisted network mode draws its egress allowlist from
// the bundle manifest, as the server does; it used to get an empty list, so
// the proxy and web_fetch refused every host.
func TestApplyBundleSandboxCarriesNetworkAllowlist(t *testing.T) {
	cfg := &config.Config{}
	bundle := &clientconfig.Bundle{SandboxConfig: clientconfig.Sandbox{NetworkAllowlist: []string{"api.example.com", "*.example.org"}}}
	if err := ApplyBundleSandbox(cfg, bundle); err != nil {
		t.Fatalf("ApplyBundleSandbox: %v", err)
	}
	if len(cfg.SandboxNetworkAllowlist) != 2 || cfg.SandboxNetworkAllowlist[0] != "api.example.com" ||
		cfg.SandboxNetworkAllowlist[1] != "*.example.org" {
		t.Fatalf("SandboxNetworkAllowlist = %v, want the manifest's list", cfg.SandboxNetworkAllowlist)
	}
}
