// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package scripts

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// gateRegistry is what the stub skopeo serves: ref → raw manifest. A ref
// starting oci-archive: is the build's archive.
func runGate(t *testing.T, refs map[string]string) (string, error) {
	t.Helper()
	bin, store := t.TempDir(), t.TempDir()
	for ref, raw := range refs {
		name := strings.NewReplacer("/", "_", ":", "_", "@", "_").Replace(ref)
		if err := os.WriteFile(filepath.Join(store, name), []byte(raw), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	stub := `#!/bin/bash
[ "$1 $2" = "inspect --raw" ] || exit 1
f="` + store + `/$(printf '%s' "$3" | tr '/:@' '___')"
[ -f "$f" ] || { echo "manifest unknown" >&2; exit 1; }
cat "$f"
`
	if err := os.WriteFile(filepath.Join(bin, "skopeo"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(filepath.Join(repoRoot(t), "scripts/release-ghcr-gate.sh"), "ghcr.io/elcanotek/fleet", "v2026.11.07.17",
		"dist/fleet-image-linux-amd64.oci.tar", "dist/fleet-image-linux-arm64.oci.tar")
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
	out, err := cmd.Output()
	return string(out), err
}

func manifest(config string) string {
	return `{"schemaVersion":2,"mediaType":"application/vnd.oci.image.manifest.v1+json","config":{"digest":"` + config + `"},"layers":[]}`
}

func digestOf(raw string) string {
	s := sha256.Sum256([]byte(raw))
	return "sha256:" + hex.EncodeToString(s[:])
}

func index(digests ...string) string {
	m := make([]string, 0, len(digests))
	for _, d := range digests {
		m = append(m, `{"digest":"`+d+`"}`)
	}
	return `{"schemaVersion":2,"manifests":[` + strings.Join(m, ",") + `]}`
}

// TestReleaseGHCRGate — the publish job's immutability gate: a first publish
// pushes; a re-run on the same build artifacts resumes, reusing what is
// there; another build's bytes under the tag refuse.
func TestReleaseGHCRGate(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not on PATH")
	}
	cfgA := "sha256:" + strings.Repeat("a", 64)
	cfgB := "sha256:" + strings.Repeat("b", 64)
	archives := map[string]string{
		"oci-archive:dist/fleet-image-linux-amd64.oci.tar": manifest(cfgA),
		"oci-archive:dist/fleet-image-linux-arm64.oci.tar": manifest(cfgB),
	}
	with := func(extra map[string]string) map[string]string {
		m := map[string]string{}
		for k, v := range archives {
			m[k] = v
		}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}
	amd, arm := manifest(cfgA), manifest(cfgB)
	reg := "docker://ghcr.io/elcanotek/fleet:v2026.11.07.17"

	t.Run("no tag: push", func(t *testing.T) {
		out, err := runGate(t, with(nil))
		if err != nil || !strings.HasPrefix(out, "state=push\n") || !strings.Contains(out, "amd64=\narm64=\nindex=\n") {
			t.Fatalf("err=%v out=%q", err, out)
		}
	})
	t.Run("same build already pushed: resume, reusing every digest", func(t *testing.T) {
		idx := index(digestOf(amd), digestOf(arm))
		out, err := runGate(t, with(map[string]string{reg + "-amd64": amd, reg + "-arm64": arm, reg: idx}))
		want := "state=resume\namd64=" + digestOf(amd) + "\narm64=" + digestOf(arm) + "\nindex=" + digestOf(idx) + "\n"
		if err != nil || out != want {
			t.Fatalf("err=%v\nout=%q\nwant=%q", err, out, want)
		}
	})
	t.Run("same build, interrupted after one architecture: resume, push the rest", func(t *testing.T) {
		out, err := runGate(t, with(map[string]string{reg + "-amd64": amd}))
		if err != nil || out != "state=resume\namd64="+digestOf(amd)+"\narm64=\nindex=\n" {
			t.Fatalf("err=%v out=%q", err, out)
		}
	})
	t.Run("another build under the tag: refuse", func(t *testing.T) {
		other := manifest("sha256:" + strings.Repeat("c", 64))
		if out, err := runGate(t, with(map[string]string{reg + "-amd64": other, reg + "-arm64": arm})); err == nil {
			t.Fatalf("a different build was accepted: %q", out)
		}
	})
	t.Run("an index of other manifests: refuse", func(t *testing.T) {
		idx := index(digestOf(amd), "sha256:"+strings.Repeat("d", 64))
		if out, err := runGate(t, with(map[string]string{reg + "-amd64": amd, reg + "-arm64": arm, reg: idx})); err == nil {
			t.Fatalf("a foreign index was accepted: %q", out)
		}
	})
	t.Run("an index with no architecture tags: refuse", func(t *testing.T) {
		if out, err := runGate(t, with(map[string]string{reg: index(digestOf(amd), digestOf(arm))})); err == nil {
			t.Fatalf("an index without its architecture tags was accepted: %q", out)
		}
	})
}

// TestRetryTransient — cosign calls retry only on transient failures.
func TestRetryTransient(t *testing.T) {
	bin := t.TempDir()
	count := filepath.Join(t.TempDir(), "n")
	_ = os.WriteFile(count, []byte("0"), 0o644)
	_ = os.WriteFile(filepath.Join(bin, "sleep"), []byte("#!/bin/sh\nexit 0\n"), 0o755)
	// fake-cosign fails FAILS times with MSG, then succeeds.
	_ = os.WriteFile(filepath.Join(bin, "fake-cosign"), []byte(`#!/bin/sh
n=$(cat `+count+`); echo $((n+1)) > `+count+`
[ "$n" -ge "$FAILS" ] && { echo signed; exit 0; }
echo "$MSG" >&2; exit 1
`), 0o755)
	run := func(fails, msg string) (string, int, error) {
		_ = os.WriteFile(count, []byte("0"), 0o644)
		cmd := exec.Command(filepath.Join(repoRoot(t), "scripts/retry-transient.sh"), "fake-cosign", "sign", "--yes", "x")
		cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "FAILS="+fails, "MSG="+msg)
		out, err := cmd.CombinedOutput()
		n, _ := os.ReadFile(count)
		var calls int
		for _, c := range strings.TrimSpace(string(n)) {
			calls = calls*10 + int(c-'0')
		}
		return string(out), calls, err
	}
	rekor502 := `Error: signing x: Post "https://rekor.sigstore.dev/api/v1/log/entries": giving up after 2 attempt(s): status 502 Bad Gateway`
	if out, calls, err := run("3", rekor502); err != nil || calls != 4 || !strings.Contains(out, "signed") {
		t.Errorf("a Rekor 502 three times then success: err=%v calls=%d\n%s", err, calls, out)
	}
	if out, calls, err := run("100", rekor502); err == nil || calls != 5 {
		t.Errorf("a persistent 502 must stop after 5 attempts: err=%v calls=%d\n%s", err, calls, out)
	}
	if out, calls, err := run("100", "Error: getting signer: reading certificate: 403 Forbidden"); err == nil || calls != 1 {
		t.Errorf("a non-transient failure must not be retried: err=%v calls=%d\n%s", err, calls, out)
	}
	if out, calls, err := run("1", "dial tcp: i/o timeout"); err != nil || calls != 2 {
		t.Errorf("a timeout is transient: err=%v calls=%d\n%s", err, calls, out)
	}

	// Every cosign call in release.yml that signs or attests goes through it.
	raw, _ := releaseWorkflow(t)
	for _, l := range strings.Split(raw, "\n") {
		l = strings.TrimSpace(l)
		if strings.HasPrefix(l, "#") {
			continue
		}
		for _, verb := range []string{"cosign sign ", "cosign attest ", "cosign sign-blob "} {
			if strings.Contains(l, verb) && !strings.Contains(l, "scripts/retry-transient.sh "+verb) {
				t.Errorf("release.yml runs %q without scripts/retry-transient.sh: %s", strings.TrimSpace(verb), l)
			}
		}
	}
	// Each provenance action has a retry step.
	_, jobs := releaseWorkflow(t)
	var first, retries int
	for _, s := range jobs["publish"].Steps {
		if strings.HasPrefix(s.Uses, "actions/attest-build-provenance@") {
			if strings.HasSuffix(s.Name, "(retry)") {
				retries++
			} else {
				first++
			}
		}
	}
	if first != 3 || retries != 3 {
		t.Errorf("want 3 provenance steps each with a retry, got %d and %d retries", first, retries)
	}
}

// TestSBOMAttestationsAreByReference — the image SBOM attestations name the
// SBOM release asset and its hash instead of embedding it (the sandbox SBOM
// is too large for Rekor), and nothing attests an SBOM inline.
func TestSBOMAttestationsAreByReference(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not on PATH")
	}
	sbom := filepath.Join(t.TempDir(), "fleet-sandbox-image-linux-amd64.spdx.json")
	body := `{"spdxVersion":"SPDX-2.3","name":"placeholder"}`
	if err := os.WriteFile(sbom, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command(filepath.Join(repoRoot(t), "scripts/sbom-reference.sh"), "v2026.11.07.18", sbom).Output()
	if err != nil {
		t.Fatal(err)
	}
	pred, err := os.ReadFile(strings.TrimSpace(string(out)))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"name": "fleet-sandbox-image-linux-amd64.spdx.json"`,
		`"sha256": "` + strings.TrimPrefix(digestOf(body), "sha256:") + `"`,
		`"uri": "https://github.com/ElcanoTek/fleet/releases/download/v2026.11.07.18/fleet-sandbox-image-linux-amd64.spdx.json"`,
	} {
		if !strings.Contains(string(pred), want) {
			t.Errorf("predicate lacks %s:\n%s", want, pred)
		}
	}
	if strings.Contains(string(pred), "spdxVersion") {
		t.Error("the predicate embeds the SBOM")
	}
	if _, err := exec.Command(filepath.Join(repoRoot(t), "scripts/sbom-reference.sh"), "latest", sbom).Output(); err == nil {
		t.Error("a non-release tag was accepted")
	}
	raw, _ := releaseWorkflow(t)
	if strings.Contains(raw, "--type spdxjson") {
		t.Error("release.yml still attests an SBOM inline (--type spdxjson); Rekor refuses the sandbox's")
	}
}
