// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package scripts

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestReleaseChartDeploysByDigest — the chart a release attaches is stamped
// with the image digests the publish job pushed and signed, before
// SHA256SUMS covers it; the templates render repository@digest when a digest
// is set and fall back to the tag only for an unstamped chart.
func TestReleaseChartDeploysByDigest(t *testing.T) {
	_, jobs := releaseWorkflow(t)
	runs := allRuns(jobs["publish"])
	stamp := strings.Index(runs, "scripts/helm-stamp-digests.sh")
	sums := strings.Index(runs, "> SHA256SUMS")
	if stamp < 0 || sums < 0 || stamp > sums {
		t.Errorf("the publish job must stamp the chart's digests (at %d) before writing SHA256SUMS (at %d)", stamp, sums)
	}
	root := repoRoot(t)
	helpers := readFile(t, root, "deploy/helm/fleet/templates/_helpers.tpl")
	for _, want := range []string{`.Values.image.digest`, `.Values.sandbox.imageDigest`, `printf "%s@%s" $repo $digest`} {
		if !strings.Contains(helpers, want) {
			t.Errorf("_helpers.tpl no longer contains %q", want)
		}
	}
	values := readFile(t, root, "deploy/helm/fleet/values.yaml")
	if strings.Count(values, "\n  digest: \"\"\n") != 1 || strings.Count(values, "\n  imageDigest: \"\"\n") != 1 {
		t.Error(`values.yaml must hold exactly one '  digest: ""' and one '  imageDigest: ""' placeholder — what helm-stamp-digests.sh stamps`)
	}
	if !strings.Contains(readFile(t, root, ".github/workflows/ci.yml"), "scripts/helm-stamp-digests.sh") {
		t.Error("CI's helm job must render a digest-stamped chart")
	}
}

// TestHelmStampDigests runs the stamping end to end when helm is available.
func TestHelmStampDigests(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	root := repoRoot(t)
	dir := t.TempDir()
	if out, err := exec.Command("helm", "package", filepath.Join(root, "deploy/helm/fleet"), "--destination", dir, "--version", "2026.1107.3", "--app-version", "2026.11.07.3").CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	tgz := filepath.Join(dir, "fleet-2026.1107.3.tgz")
	fd, sd := "sha256:"+strings.Repeat("a", 64), "sha256:"+strings.Repeat("b", 64)
	stamp := func(a, b string) (string, error) {
		out, err := exec.Command("bash", filepath.Join(root, "scripts/helm-stamp-digests.sh"), tgz, a, b).CombinedOutput()
		return string(out), err
	}
	if out, err := stamp("sha256:abc", sd); err == nil {
		t.Fatalf("a malformed digest was stamped\n%s", out)
	}
	if out, err := stamp(fd, `sha256:x"; rm -rf /`); err == nil {
		t.Fatalf("a hostile digest was stamped\n%s", out)
	}
	if out, err := stamp(fd, sd); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	out, err := exec.Command("helm", "template", "ci", tgz).CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	for _, want := range []string{`image: "ghcr.io/elcanotek/fleet@` + fd + `"`, `value: "ghcr.io/elcanotek/fleet-sandbox@` + sd + `"`} {
		if !strings.Contains(string(out), want) {
			t.Errorf("stamped chart does not render %s", want)
		}
	}
	if out, err := stamp(fd, sd); err == nil {
		t.Errorf("an already-stamped chart was stamped again\n%s", out)
	}
	_ = os.Remove(tgz)
}
