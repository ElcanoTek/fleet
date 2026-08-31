package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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
func TestResolveSandboxBackendBundleDocsInImage(t *testing.T) {
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
			if err := resolveSandboxBackendInto(cfg, bundle); err != nil {
				t.Fatalf("resolveSandboxBackendInto: %v", err)
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
func TestResolveSandboxBackendPodmanIgnoresKubernetesBlock(t *testing.T) {
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
	if err := resolveSandboxBackendInto(cfg, bundle); err != nil {
		t.Fatalf("resolveSandboxBackendInto: %v", err)
	}
	if cfg.SandboxBackend != sandbox.BackendPodman {
		t.Fatalf("SandboxBackend = %q, want podman (the default)", cfg.SandboxBackend)
	}
	if cfg.SandboxK8sBundleDocsInImage != "" || cfg.SandboxK8sWorkspaceClaim != "" {
		t.Errorf("kubernetes settings leaked into the podman path: docs=%q claim=%q",
			cfg.SandboxK8sBundleDocsInImage, cfg.SandboxK8sWorkspaceClaim)
	}
}

// validate-config must surface a knob that boot will refuse, so an operator
// sees it BEFORE the upgrade that starts refusing it rather than after the
// control plane fails to come back. FLEET_SANDBOX_PIDS was the gap (#1264):
// boot ignored it and validate-config reported the backend healthy, so a
// configured process ceiling that imposed nothing looked fine from both sides.
func TestValidateConfigFlagsPidsKnobUnderKubernetes(t *testing.T) {
	if _, ok := os.LookupEnv("FLEET_SANDBOX_SECCOMP_PROFILE"); ok {
		t.Setenv("FLEET_SANDBOX_SECCOMP_PROFILE", "")
	}
	res := checkKubernetesSandbox(
		context.Background(),
		checkResult{Name: "sandbox", Blocking: true},
		&config.Config{SandboxPids: 64},
		nil,
	)
	if res.Status != statusFail {
		t.Fatalf("a pids ceiling the backend cannot impose must fail the check, got %v (%s)", res.Status, res.Detail)
	}
	for _, want := range []string{"FLEET_SANDBOX_PIDS=64", "podPidsLimit"} {
		if !strings.Contains(res.Detail, want) {
			t.Errorf("detail must name %q so the operator can act on it; got: %s", want, res.Detail)
		}
	}
}

// The same check must stay quiet when the knob is unset — zero means "pool
// default", not "configured".
func TestValidateConfigAcceptsUnsetPidsKnobUnderKubernetes(t *testing.T) {
	if _, ok := os.LookupEnv("FLEET_SANDBOX_SECCOMP_PROFILE"); ok {
		t.Setenv("FLEET_SANDBOX_SECCOMP_PROFILE", "")
	}
	res := checkKubernetesSandbox(
		context.Background(),
		checkResult{Name: "sandbox", Blocking: true},
		&config.Config{},
		nil,
	)
	if res.Status == statusFail && strings.Contains(res.Detail, "FLEET_SANDBOX_PIDS") {
		t.Errorf("an unset pids knob must not fail the check; got: %s", res.Detail)
	}
}
