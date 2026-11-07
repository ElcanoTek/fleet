// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package scripts

import (
	"regexp"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"

	"github.com/ElcanoTek/fleet/internal/release"
)

// The release pipeline and the box-side verifier are two halves of one
// contract (ADR-0100, docs/RELEASE-ARTIFACTS.md). Nothing in CI runs
// release.yml before it ships, and a box only meets a broken contract on its
// next update — so the joins are tests here.

type wfJob struct {
	Permissions map[string]string `yaml:"permissions"`
	Needs       any               `yaml:"needs"`
	Env         map[string]string `yaml:"env"`
	Steps       []struct {
		Name string            `yaml:"name"`
		Uses string            `yaml:"uses"`
		With map[string]any    `yaml:"with"`
		Run  string            `yaml:"run"`
		Env  map[string]string `yaml:"env"`
	} `yaml:"steps"`
}

func releaseWorkflow(t *testing.T) (string, map[string]wfJob) {
	t.Helper()
	raw := readFile(t, repoRoot(t), ".github/workflows/release.yml")
	var wf struct {
		Jobs map[string]wfJob `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(raw), &wf); err != nil {
		t.Fatalf("parse release.yml: %v", err)
	}
	for _, j := range []string{"tag", "build", "publish"} {
		if _, ok := wf.Jobs[j]; !ok {
			t.Fatalf("release.yml has no %q job", j)
		}
	}
	return raw, wf.Jobs
}

func allRuns(j wfJob) string {
	var b strings.Builder
	for _, s := range j.Steps {
		b.WriteString(s.Run)
		b.WriteString("\n")
	}
	return b.String()
}

// TestReleaseSignerIsTheIdentityBoxesVerify — the publish job signs, then
// verifies its own signatures against SIGNER_IDENTITY. That env value must be
// exactly internal/release.SignerIdentity, or every box refuses every release.
func TestReleaseSignerIsTheIdentityBoxesVerify(t *testing.T) {
	_, jobs := releaseWorkflow(t)
	pub := jobs["publish"]
	if got := pub.Env["SIGNER_IDENTITY"]; got != release.SignerIdentity {
		t.Errorf("release.yml publish SIGNER_IDENTITY = %q, internal/release.SignerIdentity = %q", got, release.SignerIdentity)
	}
	if got := pub.Env["OIDC_ISSUER"]; got != release.OIDCIssuer {
		t.Errorf("release.yml publish OIDC_ISSUER = %q, internal/release.OIDCIssuer = %q", got, release.OIDCIssuer)
	}
	if !strings.HasSuffix(release.SignerIdentity, "/.github/workflows/release.yml@refs/heads/main") {
		t.Errorf("SignerIdentity %q must name this workflow file on main (workflow_run runs from the default branch)", release.SignerIdentity)
	}
	if got := pub.Env["FLEET_IMAGE"]; got != release.FleetImage {
		t.Errorf("FLEET_IMAGE %q != release.FleetImage %q", got, release.FleetImage)
	}
	if got := pub.Env["SANDBOX_IMAGE"]; got != release.SandboxImage {
		t.Errorf("SANDBOX_IMAGE %q != release.SandboxImage %q", got, release.SandboxImage)
	}
	runs := allRuns(pub)
	for _, want := range []string{
		"cosign sign-blob --yes --bundle \"${f}.sigstore.json\"",
		"cosign verify-blob --bundle",
		"cosign sign --yes",
		"cosign attest --yes --type spdxjson",
		"internal/release/cosign.lock",
		"sha256sum -c",
		"> SHA256SUMS",
		"> release.json",
		"fleet-linux-amd64 fleet-linux-arm64",
	} {
		if !strings.Contains(runs, want) {
			t.Errorf("the publish job no longer runs %q", want)
		}
	}
	// The file names the box asks for.
	for _, name := range []string{release.SumsFile, release.ManifestFile, release.BinaryAsset("amd64"), release.BinaryAsset("arm64"), release.BundleSuffix} {
		if !strings.Contains(runs, name) {
			t.Errorf("the publish job never produces %q, which `fleet release fetch` downloads", name)
		}
	}
	var provenance int
	for _, s := range pub.Steps {
		if strings.HasPrefix(s.Uses, "actions/attest-build-provenance@") {
			provenance++
		}
	}
	if provenance != 3 {
		t.Errorf("want SLSA provenance for the binaries and both images (3 steps), got %d", provenance)
	}
}

// TestReleaseJobsAreSplitByPrivilege — the build job runs the release's code
// and must hold nothing but a read token; the publish job holds the write
// scopes and must not run the release's code (no checkout of the tag or SHA).
func TestReleaseJobsAreSplitByPrivilege(t *testing.T) {
	_, jobs := releaseWorkflow(t)
	build := jobs["build"]
	if len(build.Permissions) != 1 || build.Permissions["contents"] != "read" {
		t.Errorf("build job permissions = %v, want exactly contents: read — it executes the released commit's code", build.Permissions)
	}
	for _, s := range build.Steps {
		if strings.HasPrefix(s.Uses, "actions/checkout@") && s.With["persist-credentials"] != false {
			t.Errorf("build job checkout must set persist-credentials: false")
		}
	}
	pub := jobs["publish"]
	for scope, want := range map[string]string{"contents": "write", "packages": "write", "id-token": "write", "attestations": "write"} {
		if pub.Permissions[scope] != want {
			t.Errorf("publish job %s = %q, want %q", scope, pub.Permissions[scope], want)
		}
	}
	for _, s := range pub.Steps {
		if strings.HasPrefix(s.Uses, "actions/checkout@") {
			if ref, ok := s.With["ref"]; ok {
				t.Errorf("publish job checks out %v; it holds write scopes and must run only the default branch's code", ref)
			}
		}
	}
	if strings.Contains(allRuns(pub), "make ") || strings.Contains(allRuns(pub), "go build") || strings.Contains(allRuns(pub), "npm ") {
		t.Error("the publish job builds something; building belongs to the unprivileged build job")
	}
}

// TestReleaseAssertsNoHostExecutor — the #159 invariant, as a property of the
// published artifact: the build job refuses a binary whose build settings
// carry the fleet_host_executor tag, or whose compile-time marker says so.
func TestReleaseAssertsNoHostExecutor(t *testing.T) {
	_, jobs := releaseWorkflow(t)
	runs := allRuns(jobs["build"])
	for _, want := range []string{
		`go version -m "${bin}"`,
		"-tags=.*fleet_host_executor",
		"version --build-info",
		".host_executor",
		"CGO_ENABLED=0 GOOS=linux",
		"version --web",
	} {
		if !strings.Contains(runs, want) {
			t.Errorf("the build job no longer checks %q", want)
		}
	}
	if regexp.MustCompile(`go build[^\n]*-tags`).MatchString(runs) {
		t.Error("the release build passes -tags; the host executor fence (#159) depends on it not doing so")
	}
	raw, _ := releaseWorkflow(t)
	for _, want := range []string{"arch: amd64\n            runner: ubuntu-24.04\n", "arch: arm64\n            runner: ubuntu-24.04-arm\n"} {
		if !strings.Contains(raw, want) {
			t.Errorf("release.yml no longer builds %q natively", strings.TrimSpace(want))
		}
	}
}

// TestFleetImageIsBuiltFromTheCheckedBinary — the published image must carry
// the same bytes as the signed download, not a second build.
func TestFleetImageIsBuiltFromTheCheckedBinary(t *testing.T) {
	cf := readFile(t, repoRoot(t), "deploy/fleet.Containerfile")
	if !strings.Contains(cf, "COPY dist/fleet-linux-${FLEET_ARCH} /usr/local/bin/fleet") {
		t.Error("deploy/fleet.Containerfile must COPY the release's dist/fleet-linux-<arch> binary")
	}
	if strings.Contains(cf, "go build") || strings.Contains(cf, "npm ") {
		t.Error("deploy/fleet.Containerfile builds fleet itself; it must reuse the checked release binary")
	}
}

// TestShellVerifierPinsTheSameTrust — scripts/lib/release.sh verifies the
// first binary on a box that has no capable fleet yet. It must pin the same
// identity and issuer, and read the same cosign.lock, as the Go verifier.
func TestShellVerifierPinsTheSameTrust(t *testing.T) {
	sh := readFile(t, repoRoot(t), "scripts/lib/release.sh")
	for _, want := range []string{
		`FLEET_RELEASE_IDENTITY="` + release.SignerIdentity + `"`,
		`FLEET_RELEASE_ISSUER="` + release.OIDCIssuer + `"`,
		"internal/release/cosign.lock",
		"--certificate-identity",
		release.SumsFile + release.BundleSuffix,
	} {
		if !strings.Contains(sh, want) {
			t.Errorf("scripts/lib/release.sh no longer contains %q", want)
		}
	}
	if strings.Contains(sh, "certificate-identity-regexp") {
		t.Error("scripts/lib/release.sh must match the signer identity exactly, not by regexp")
	}
}
