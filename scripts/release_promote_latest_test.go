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

	"github.com/goccy/go-yaml"
)

const (
	promoteFleet   = "ghcr.io/elcanotek/fleet"
	promoteSandbox = "ghcr.io/elcanotek/fleet-sandbox"
)

func sha(b string) string {
	s := sha256.Sum256([]byte(b))
	return "sha256:" + hex.EncodeToString(s[:])
}

// promoteFixture is the GitHub and registry state the promote script sees.
type promoteFixture struct {
	listed      []string          // non-draft releases
	complete    map[string]bool   // which carry SHA256SUMS
	releaseJSON map[string]string // tag → release.json
	tamper      bool              // release.json differs from what SHA256SUMS signed
	cosignFails bool
	latest      map[string]string // repo → the raw manifest :latest names now
}

func runPromote(t *testing.T, f promoteFixture) (string, []string, error) {
	t.Helper()
	bin, state := t.TempDir(), t.TempDir()
	for tag, ok := range f.complete {
		v := "false"
		if ok {
			v = "true"
		}
		_ = os.WriteFile(filepath.Join(state, "complete-"+tag), []byte(v), 0o644)
	}
	for tag, rj := range f.releaseJSON {
		d := filepath.Join(state, "assets-"+tag)
		_ = os.MkdirAll(d, 0o755)
		signed := rj
		if f.tamper {
			rj = strings.Replace(rj, "sha256:", "sha256:0", 1)
		}
		_ = os.WriteFile(filepath.Join(d, "release.json"), []byte(rj), 0o644)
		_ = os.WriteFile(filepath.Join(d, "SHA256SUMS"), []byte(strings.TrimPrefix(sha(signed), "sha256:")+"  release.json\n"), 0o644)
		_ = os.WriteFile(filepath.Join(d, "SHA256SUMS.sigstore.json"), []byte("{}"), 0o644)
	}
	for repo, m := range f.latest {
		_ = os.WriteFile(filepath.Join(state, "latest-"+strings.ReplaceAll(repo, "/", "_")), []byte(m), 0o644)
	}
	log := filepath.Join(state, "copies")
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/bash\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	write("gh", `S="`+state+`"
case "$1 $2" in
  "release list") printf '%s\n' `+strings.Join(f.listed, " ")+` ;;
  "release view") [ -f "$S/complete-$3" ] && cat "$S/complete-$3" || echo false ;;
  "release download")
    tag="$3"; shift 3; dir=""
    while [ $# -gt 0 ]; do [ "$1" = --dir ] && dir="$2"; shift; done
    cp "$S/assets-$tag/"* "$dir/" ;;
  *) exit 1 ;;
esac
`)
	cosign := "exit 0\n"
	if f.cosignFails {
		cosign = "echo 'signature mismatch' >&2; exit 1\n"
	}
	write("cosign", cosign)
	write("skopeo", `S="`+state+`"
if [ "$1 $2" = "inspect --raw" ]; then
  ref="${3#docker://}"; f="$S/latest-$(printf '%s' "${ref%:latest}" | tr / _)"
  [ -f "$f" ] || exit 1
  cat "$f"; exit 0
fi
if [ "$1" = copy ]; then echo "$*" >> "`+log+`"; exit 0; fi
exit 1
`)
	cmd := exec.Command(filepath.Join(repoRoot(t), "scripts/release-promote-latest.sh"))
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"),
		"FLEET_IMAGE="+promoteFleet, "SANDBOX_IMAGE="+promoteSandbox,
		"SIGNER_IDENTITY=https://example.invalid/release.yml@refs/heads/main", "OIDC_ISSUER=https://example.invalid")
	out, err := cmd.CombinedOutput()
	copies, _ := os.ReadFile(log)
	var lines []string
	for _, l := range strings.Split(strings.TrimSpace(string(copies)), "\n") {
		if l != "" {
			lines = append(lines, l)
		}
	}
	return string(out), lines, err
}

func releaseJSON(tag, fleetDigest, sandboxDigest string) string {
	return `{"schema":1,"tag":"` + tag + `","commit":"1111111111111111111111111111111111111111","images":{"` +
		promoteFleet + `":"` + promoteFleet + `@` + fleetDigest + `","` + promoteSandbox + `":"` + promoteSandbox + `@` + sandboxDigest + `"}}`
}

// TestPromoteLatestPicksTheNewestCompleteRelease — :latest follows the newest
// COMPLETE published release (verified), fleet first then the sandbox, and a
// re-run that finds both already there copies nothing.
func TestPromoteLatestPicksTheNewestCompleteRelease(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not on PATH")
	}
	f2, s2 := sha("fleet-index-2"), sha("sandbox-index-2")
	base := promoteFixture{
		// .3 is published but incomplete (no SHA256SUMS yet): .2 is the
		// newest complete release.
		listed:   []string{"v2026.11.07.3", "v2026.11.07.2", "v2026.11.07.1"},
		complete: map[string]bool{"v2026.11.07.3": false, "v2026.11.07.2": true, "v2026.11.07.1": true},
		releaseJSON: map[string]string{
			"v2026.11.07.2": releaseJSON("v2026.11.07.2", f2, s2),
			"v2026.11.07.1": releaseJSON("v2026.11.07.1", sha("fleet-index-1"), sha("sandbox-index-1")),
		},
		latest: map[string]string{promoteFleet: "fleet-index-1", promoteSandbox: "sandbox-index-1"},
	}

	out, copies, err := runPromote(t, base)
	if err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	want := []string{
		"copy --all --preserve-digests docker://" + promoteFleet + "@" + f2 + " docker://" + promoteFleet + ":latest",
		"copy --all --preserve-digests docker://" + promoteSandbox + "@" + s2 + " docker://" + promoteSandbox + ":latest",
	}
	if strings.Join(copies, "\n") != strings.Join(want, "\n") {
		t.Errorf("copies:\n%s\nwant (fleet first, then the sandbox):\n%s\n%s", strings.Join(copies, "\n"), strings.Join(want, "\n"), out)
	}

	// Idempotent: both already at .2.
	done := base
	done.latest = map[string]string{promoteFleet: "fleet-index-2", promoteSandbox: "sandbox-index-2"}
	if out, copies, err := runPromote(t, done); err != nil || len(copies) != 0 {
		t.Errorf("a re-run copied %v (err %v)\n%s", copies, err, out)
	}

	// Only one behind: only that one moves.
	half := base
	half.latest = map[string]string{promoteFleet: "fleet-index-2", promoteSandbox: "sandbox-index-1"}
	if _, copies, err := runPromote(t, half); err != nil || len(copies) != 1 || !strings.Contains(copies[0], promoteSandbox+"@"+s2) {
		t.Errorf("want only the sandbox moved, got %v (err %v)", copies, err)
	}

	// A signature that does not verify, or a release.json that is not the
	// signed one, moves nothing.
	bad := base
	bad.cosignFails = true
	if out, copies, err := runPromote(t, bad); err == nil || len(copies) != 0 {
		t.Errorf("an unverified SHA256SUMS still promoted %v\n%s", copies, out)
	}
	tampered := base
	tampered.tamper = true
	if out, copies, err := runPromote(t, tampered); err == nil || len(copies) != 0 {
		t.Errorf("a release.json that is not the signed one still promoted %v\n%s", copies, out)
	}

	// Nothing complete: nothing moves, and that is not an error.
	none := base
	none.complete = map[string]bool{"v2026.11.07.3": false}
	if out, copies, err := runPromote(t, none); err != nil || len(copies) != 0 {
		t.Errorf("want no promotion and no error, got %v (err %v)\n%s", copies, err, out)
	}
}

// TestOnlyPromoteLatestWritesLatest — :latest is written by one serialized
// job and nowhere else in release.yml.
func TestOnlyPromoteLatestWritesLatest(t *testing.T) {
	raw, jobs := releaseWorkflow(t)
	var wf struct {
		Jobs map[string]struct {
			Concurrency struct {
				Group            string `yaml:"group"`
				CancelInProgress bool   `yaml:"cancel-in-progress"`
			} `yaml:"concurrency"`
			Needs any `yaml:"needs"`
		} `yaml:"jobs"`
	}
	if err := yaml.Unmarshal([]byte(raw), &wf); err != nil {
		t.Fatal(err)
	}
	pl, ok := wf.Jobs["promote-latest"]
	if !ok {
		t.Fatal("release.yml has no promote-latest job")
	}
	if pl.Concurrency.Group != "release-promote-latest" || pl.Concurrency.CancelInProgress {
		t.Errorf("promote-latest concurrency = %+v, want group release-promote-latest without cancel-in-progress", pl.Concurrency)
	}
	if !strings.Contains(allRuns(jobs["promote-latest"]), "scripts/release-promote-latest.sh") {
		t.Error("promote-latest does not run scripts/release-promote-latest.sh")
	}
	for name, j := range jobs {
		if name == "promote-latest" {
			continue
		}
		for _, s := range j.Steps {
			for _, l := range strings.Split(s.Run, "\n") {
				if l = strings.TrimSpace(l); !strings.HasPrefix(l, "#") && strings.Contains(l, ":latest") {
					t.Errorf("job %s step %q writes :latest (%s) — only promote-latest may", name, s.Name, l)
				}
			}
		}
	}
}
