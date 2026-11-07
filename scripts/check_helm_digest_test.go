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
	for _, want := range []string{`.Values.image.signedDigest`, `.Values.sandbox.signedImageDigest`, `(eq $repo .signedRepo)`, `printf "%s@%s" $repo $digest`} {
		if !strings.Contains(helpers, want) {
			t.Errorf("_helpers.tpl no longer contains %q", want)
		}
	}
	values := readFile(t, root, "deploy/helm/fleet/values.yaml")
	if strings.Count(values, "\n  signedDigest: \"\"\n") != 1 || strings.Count(values, "\n  signedImageDigest: \"\"\n") != 1 {
		t.Error(`values.yaml must hold exactly one '  signedDigest: ""' and one '  signedImageDigest: ""' placeholder — what helm-stamp-digests.sh stamps`)
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

	// An overridden repository never inherits the official signed digest: it
	// uses its own digest when one is set, else the tag — flagged as unpinned.
	own := "sha256:" + strings.Repeat("c", 64)
	render := func(args ...string) string {
		t.Helper()
		out, err := exec.Command("helm", append([]string{"template", "ci", tgz}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("%v\n%s", err, out)
		}
		return string(out)
	}
	for _, c := range []struct {
		args          []string
		want, notWant []string
	}{
		{[]string{"--set", "image.repository=registry.invalid/custom"},
			[]string{`image: "registry.invalid/custom:v2026.11.07.3"`, `unpinned-images: "registry.invalid/custom:v2026.11.07.3"`, `value: "ghcr.io/elcanotek/fleet-sandbox@` + sd + `"`},
			[]string{"registry.invalid/custom@" + fd}},
		{[]string{"--set", "image.repository=registry.invalid/custom", "--set", "image.digest=" + own},
			[]string{`image: "registry.invalid/custom@` + own + `"`},
			[]string{"unpinned-images"}},
		{[]string{"--set", "sandbox.imageRepository=registry.invalid/sbx"},
			[]string{`value: "registry.invalid/sbx:v2026.11.07.3"`, `unpinned-images: "registry.invalid/sbx:v2026.11.07.3"`},
			[]string{"registry.invalid/sbx@" + sd}},
		{[]string{"--set", "sandbox.imageRepository=registry.invalid/sbx", "--set", "sandbox.imageDigest=" + own},
			[]string{`value: "registry.invalid/sbx@` + own + `"`},
			[]string{"unpinned-images"}},
		{[]string{"--set", "sandbox.image=registry.invalid/sbx:v1"},
			[]string{`unpinned-images: "registry.invalid/sbx:v1"`}, nil},
	} {
		out := render(c.args...)
		for _, w := range c.want {
			if !strings.Contains(out, w) {
				t.Errorf("%v: render lacks %s", c.args, w)
			}
		}
		for _, w := range c.notWant {
			if strings.Contains(out, w) {
				t.Errorf("%v: render contains %s", c.args, w)
			}
		}
	}

	// NOTES.txt (which `helm template` does not render) warns about each
	// unpinned image: render it through a copy of the chart.
	notes := filepath.Join(dir, "notes")
	if out, err := exec.Command("tar", "-xzf", tgz, "-C", dir).CombinedOutput(); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if err := os.Rename(filepath.Join(dir, "fleet"), notes); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(notes, "templates", "NOTES.txt"))
	if err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(notes, "templates", "_notes_copy.tpl"), append(append([]byte(`{{- define "notes.copy" -}}`), body...), []byte(`{{- end -}}`)...), 0o644)
	_ = os.WriteFile(filepath.Join(notes, "templates", "notes-render.yaml"), []byte("apiVersion: v1\nkind: ConfigMap\nmetadata:\n  name: notes\ndata:\n  notes: |\n    {{- include \"notes.copy\" . | nindent 4 }}\n"), 0o644)
	out, err = exec.Command("helm", "template", "ci", notes, "--show-only", "templates/notes-render.yaml", "--set", "image.repository=registry.invalid/custom").CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if !strings.Contains(string(out), "WARNING: registry.invalid/custom:v2026.11.07.3 is NOT digest-pinned") {
		t.Errorf("NOTES does not warn about the unpinned override:\n%s", out)
	}
	out, err = exec.Command("helm", "template", "ci", notes, "--show-only", "templates/notes-render.yaml").CombinedOutput()
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if strings.Contains(string(out), "WARNING") {
		t.Errorf("NOTES warns although both images are digest-pinned:\n%s", out)
	}
	_ = os.Remove(tgz)
}
