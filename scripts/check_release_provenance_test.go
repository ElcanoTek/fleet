// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package scripts

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const (
	releaseCommit = "1111111111111111111111111111111111111111"
	mainHead      = "2222222222222222222222222222222222222222"
)

func ghEnv() []string {
	return append(os.Environ(),
		"GITHUB_SERVER_URL=https://github.com", "GITHUB_REPOSITORY=ElcanoTek/fleet",
		"GITHUB_REF=refs/heads/main", "GITHUB_SHA="+mainHead,
		"GITHUB_WORKFLOW_REF=ElcanoTek/fleet/.github/workflows/release.yml@refs/heads/main",
		"GITHUB_EVENT_NAME=workflow_run", "GITHUB_REPOSITORY_ID=1", "GITHUB_REPOSITORY_OWNER_ID=2",
		"RUNNER_ENVIRONMENT=github-hosted", "GITHUB_RUN_ID=3", "GITHUB_RUN_ATTEMPT=1")
}

// TestSLSAPredicateNamesTheReleaseCommit — under workflow_run, GITHUB_SHA is
// the default-branch head when the release ran; the predicate's source must
// be the release commit regardless.
func TestSLSAPredicateNamesTheReleaseCommit(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not on PATH")
	}
	root := repoRoot(t)
	cmd := exec.Command(filepath.Join(root, "scripts/slsa-predicate.sh"), "v2026.11.07.3", releaseCommit)
	cmd.Env = ghEnv()
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	var p struct {
		BuildDefinition struct {
			BuildType            string `json:"buildType"`
			ResolvedDependencies []struct {
				URI    string            `json:"uri"`
				Digest map[string]string `json:"digest"`
			} `json:"resolvedDependencies"`
			ExternalParameters struct {
				Workflow struct{ Path string } `json:"workflow"`
			} `json:"externalParameters"`
		} `json:"buildDefinition"`
		RunDetails struct {
			Builder struct{ ID string } `json:"builder"`
		} `json:"runDetails"`
	}
	if err := json.Unmarshal(out, &p); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	deps := p.BuildDefinition.ResolvedDependencies
	if len(deps) == 0 || deps[0].Digest["gitCommit"] != releaseCommit || deps[0].URI != "git+https://github.com/ElcanoTek/fleet@refs/tags/v2026.11.07.3" {
		t.Errorf("the source dependency is not the release tag at the release commit: %+v", deps)
	}
	if p.BuildDefinition.BuildType != "https://actions.github.io/buildtypes/workflow/v1" ||
		p.BuildDefinition.ExternalParameters.Workflow.Path != ".github/workflows/release.yml" ||
		p.RunDetails.Builder.ID != "https://github.com/ElcanoTek/fleet/.github/workflows/release.yml@refs/heads/main" {
		t.Errorf("predicate shape drifted from the GitHub workflow build type:\n%s", out)
	}
	for _, bad := range [][]string{{"v2026.11.07.3", "HEAD"}, {"latest", releaseCommit}, {"v2026.11.07.3", releaseCommit + `"; rm -rf /`}} {
		c := exec.Command(filepath.Join(root, "scripts/slsa-predicate.sh"), bad...)
		c.Env = ghEnv()
		if err := c.Run(); err == nil {
			t.Errorf("slsa-predicate.sh accepted %q", bad)
		}
	}
}

// TestCheckAttestedCommit — the publish job's self-check accepts only
// verified provenance whose source is the release commit.
func TestCheckAttestedCommit(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not on PATH")
	}
	root := repoRoot(t)
	statement := func(commit string) string {
		return `{"verificationResult":{"statement":{"predicateType":"https://slsa.dev/provenance/v1","predicate":{"buildDefinition":{"resolvedDependencies":[{"uri":"x","digest":{"gitCommit":"` + commit + `"}}]}}}}}`
	}
	for _, c := range []struct {
		name, report string
		ok           bool
	}{
		{"release commit", "[" + statement(releaseCommit) + "]", true},
		{"main head instead", "[" + statement(mainHead) + "]", false},
		{"one of two names another commit", "[" + statement(releaseCommit) + "," + statement(mainHead) + "]", false},
		{"no provenance at all", "[]", false},
		{"not provenance", `[{"verificationResult":{"statement":{"predicateType":"https://spdx.dev/Document","predicate":{}}}}]`, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "verify.json")
			if err := os.WriteFile(p, []byte(c.report), 0o600); err != nil {
				t.Fatal(err)
			}
			out, err := exec.Command(filepath.Join(root, "scripts/check-attested-commit.sh"), p, releaseCommit).CombinedOutput()
			if (err == nil) != c.ok {
				t.Errorf("ok = %v, want %v\n%s", err == nil, c.ok, out)
			}
		})
	}
}

// TestReleaseProvenanceAndScanOrder — in the publish job: the image archives
// are scanned with CI's pinned Grype and policy before anything is pushed or
// signed; every provenance attestation uses the explicit predicate; and the
// attestations are verified against the release commit before release.json
// and SHA256SUMS are written.
func TestReleaseProvenanceAndScanOrder(t *testing.T) {
	raw, jobs := releaseWorkflow(t)
	pub := jobs["publish"]
	idx := func(pred func(name, uses, run string) bool) int {
		for i, s := range pub.Steps {
			if pred(s.Name, s.Uses, s.Run) {
				return i
			}
		}
		return -1
	}
	scan := idx(func(_, _, run string) bool {
		return strings.Contains(run, `grype "oci-archive:${archive}"`) && strings.Contains(run, "scripts/check-grype-policy.sh")
	})
	push := idx(func(_, _, run string) bool { return strings.Contains(run, "skopeo copy") })
	sign := idx(func(_, _, run string) bool { return strings.Contains(run, "cosign sign") })
	verify := idx(func(_, _, run string) bool { return strings.Contains(run, "scripts/check-attested-commit.sh") })
	sums := idx(func(_, _, run string) bool { return strings.Contains(run, "> SHA256SUMS") })
	if scan < 0 || push < 0 || sign < 0 || scan > push || scan > sign {
		t.Errorf("the Grype scan of the archives (step %d) must come before the first push (%d) and signature (%d)", scan, push, sign)
	}
	if scanStep := pub.Steps[max(scan, 0)].Run; !strings.Contains(scanStep, "for name in fleet-sandbox fleet") || !strings.Contains(scanStep, "for arch in amd64 arm64") {
		t.Error("the scan must cover both images on both architectures")
	}
	if verify < 0 || sums < 0 || verify > sums {
		t.Errorf("the provenance check (step %d) must run before SHA256SUMS is written (%d)", verify, sums)
	}
	provenance := 0
	for i, s := range pub.Steps {
		if !strings.HasPrefix(s.Uses, "actions/attest-build-provenance@") {
			continue
		}
		provenance++
		if s.With["predicate-path"] == nil || s.With["predicate-type"] != "https://slsa.dev/provenance/v1" {
			t.Errorf("provenance step %q uses the action's own predicate, whose source commit is the workflow_run's default-branch head", s.Name)
		}
		if i > verify {
			t.Errorf("provenance step %q runs after the verification", s.Name)
		}
	}
	if provenance != 3 {
		t.Errorf("want 3 provenance attestations (two images, the binaries), got %d", provenance)
	}
	// One Grype pin for CI and the release.
	pin := regexp.MustCompile(`GRYPE_(VERSION|SHA256):\s*'([^']+)'`)
	ci := readFile(t, repoRoot(t), ".github/workflows/ci.yml")
	if a, b := pin.FindAllStringSubmatch(raw, -1), pin.FindAllStringSubmatch(ci, -1); len(a) != 2 || len(b) != 2 || a[0][2] != b[0][2] || a[1][2] != b[1][2] {
		t.Errorf("release.yml's Grype pin %v differs from ci.yml's %v", a, b)
	}
}
