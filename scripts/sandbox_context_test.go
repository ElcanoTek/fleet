// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package scripts

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// genericSandboxContext copies config/default/sandbox into dir/rel and returns
// the copied Containerfile's path.
func genericSandboxContext(t *testing.T, dir, rel string) string {
	t.Helper()
	src := filepath.Join(repoRoot(t), "config/default/sandbox")
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dir, rel)
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, filepath.Join(dst, e.Name()), string(b))
	}
	return filepath.Join(dst, "Containerfile")
}

func writeTestFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// libBash runs script with scripts/lib/sandbox-bundle.sh sourced.
func libBash(t *testing.T, script string, env ...string) string {
	t.Helper()
	cmd := exec.Command("bash", "-c", `set -euo pipefail; . "$ROOT/scripts/lib/sandbox-bundle.sh"; `+script)
	cmd.Env = append(append(os.Environ(), "ROOT="+repoRoot(t)), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("bash: %v\n%s", err, out)
	}
	return string(out)
}

func contextHash(t *testing.T, containerfile string) string {
	t.Helper()
	return libBash(t, `fleet_sandbox_context_hash "$CF"`, "CF="+containerfile)
}

// TestSandboxContextHash — update.sh's rebuild stamp covers the whole build
// context `podman build` sends, not only the Containerfile: a custom bundle
// that edits only its sandbox/requirements.txt must rebuild. The hash is
// deterministic, independent of where the context lives, blind to VCS
// metadata and to what .containerignore keeps out of the build.
func TestSandboxContextHash(t *testing.T) {
	root := repoRoot(t)
	generic := contextHash(t, filepath.Join(root, "config/default/sandbox/Containerfile"))
	if !regexp.MustCompile(`^ctx1-[0-9a-f]{64}$`).MatchString(generic) {
		t.Fatalf("context hash = %q, want ctx1-<sha256>", generic)
	}
	if got := contextHash(t, filepath.Join(t.TempDir(), "no/such/Containerfile")); got != "absent" {
		t.Errorf("missing Containerfile hashes to %q, want absent", got)
	}

	dir := t.TempDir()
	cf := genericSandboxContext(t, dir, "images/agent")
	ctx := filepath.Dir(cf)
	base := contextHash(t, cf)
	if base != generic {
		t.Errorf("the generic context at another path hashes %s, want %s (the generic's)", base, generic)
	}
	if again := contextHash(t, cf); again != base {
		t.Errorf("unchanged context: hash %s then %s", base, again)
	}

	// VCS metadata never counts.
	writeTestFile(t, filepath.Join(ctx, ".git", "HEAD"), "ref: refs/heads/main\n")
	writeTestFile(t, filepath.Join(ctx, "sub", ".svn", "entries"), "x\n")
	if got := contextHash(t, cf); got != base {
		t.Errorf("VCS metadata changed the hash: %s → %s", base, got)
	}

	// Only requirements.txt changes: the hash does.
	writeTestFile(t, filepath.Join(ctx, "requirements.txt"), "placeholder==1.0\n")
	edited := contextHash(t, cf)
	if edited == base {
		t.Fatal("editing only requirements.txt left the context hash unchanged")
	}
	if again := contextHash(t, cf); again != edited {
		t.Errorf("unchanged edited context: hash %s then %s", edited, again)
	}

	// A file the generic context does not have changes it too.
	cf2 := genericSandboxContext(t, t.TempDir(), "sandbox")
	writeTestFile(t, filepath.Join(filepath.Dir(cf2), "extra", "setup.sh"), "#!/bin/sh\n")
	if got := contextHash(t, cf2); got == generic {
		t.Error("an extra file in the context left the hash unchanged")
	}
}

// TestSandboxContextHashHonoursContainerignore — a path the context's
// .containerignore (else .dockerignore) excludes is never sent to the build,
// so it must not trigger one; the ignore file itself, and a `!` re-include,
// do count.
func TestSandboxContextHashHonoursContainerignore(t *testing.T) {
	for _, name := range []string{".containerignore", ".dockerignore"} {
		t.Run(name, func(t *testing.T) {
			cf := genericSandboxContext(t, t.TempDir(), "sandbox")
			ctx := filepath.Dir(cf)
			writeTestFile(t, filepath.Join(ctx, name), "# build scratch\n/cache/\n**/*.pyc\n!cache/keep.txt\n")
			withIgnore := contextHash(t, cf)

			writeTestFile(t, filepath.Join(ctx, "cache", "sub", "blob"), "1\n")
			writeTestFile(t, filepath.Join(ctx, "deep", "x.pyc"), "1\n")
			if got := contextHash(t, cf); got != withIgnore {
				t.Errorf("ignored files changed the hash: %s → %s", withIgnore, got)
			}
			writeTestFile(t, filepath.Join(ctx, "cache", "keep.txt"), "1\n")
			if got := contextHash(t, cf); got == withIgnore {
				t.Error("a file re-included by !pattern did not change the hash")
			}
		})
	}

	cf := genericSandboxContext(t, t.TempDir(), "sandbox")
	before := contextHash(t, cf)
	writeTestFile(t, filepath.Join(filepath.Dir(cf), ".containerignore"), "nothing-matches\n")
	if got := contextHash(t, cf); got == before {
		t.Error("adding a .containerignore did not change the hash")
	}

	// The matcher keeps `*` within one segment and lets `**` span segments,
	// as the build does; a pattern naming a directory covers its contents.
	for _, c := range []struct {
		pat, rel string
		want     bool
	}{
		{"*.pyc", "x.pyc", true},
		{"*.pyc", "a/x.pyc", false},
		{"**/*.pyc", "a/b/x.pyc", true},
		{"**/*.pyc", "x.pyc", true},
		{"a/*/c", "a/b/c", true},
		{"a/*/c", "a/b/b/c", false},
		{"cache", "cache/sub/blob", true},
		{"cach", "cache/sub/blob", false},
		{"[ab]?", "bx/q", true},
	} {
		got := libBash(t, `fleet__ignore_match "$P" "$R" && echo y || echo n`, "P="+c.pat, "R="+c.rel)
		if (strings.TrimSpace(got) == "y") != c.want {
			t.Errorf("pattern %q vs %q: match=%s, want %v", c.pat, c.rel, strings.TrimSpace(got), c.want)
		}
	}
}

// TestSandboxUsesReleaseImageComparesTheWholeContext — the release image is
// this bundle's image only when the bundle's whole build context matches the
// generic one: a file the generic context lacks makes a custom image.
func TestSandboxUsesReleaseImageComparesTheWholeContext(t *testing.T) {
	root := repoRoot(t)
	generic := func(name string) string {
		b, err := os.ReadFile(filepath.Join(root, "config/default/sandbox", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	same := map[string]string{
		"sandbox/Containerfile": generic("Containerfile"), "sandbox/requirements.txt": generic("requirements.txt"), "sandbox/requirements.in": generic("requirements.in"),
	}
	if !usesReleaseImage(t, sandboxBundle(t, "", same)) {
		t.Fatal("the generic context does not count as the release image")
	}
	extra := map[string]string{"sandbox/entrypoint.sh": "#!/bin/sh\n"}
	for k, v := range same {
		extra[k] = v
	}
	if usesReleaseImage(t, sandboxBundle(t, "", extra)) {
		t.Error("a context with an extra file still counts as the release image")
	}
	vcs := map[string]string{"sandbox/.git/HEAD": "ref: refs/heads/main\n"}
	for k, v := range same {
		vcs[k] = v
	}
	if !usesReleaseImage(t, sandboxBundle(t, "", vcs)) {
		t.Error("VCS metadata in the context made it a custom image")
	}
}

// updateStampSnippet returns update.sh's cf_now/cf_prev computation, run
// verbatim by TestUpdateSandboxStampGate.
func updateStampSnippet(t *testing.T, update string) string {
	t.Helper()
	const from, to = `cf_now="$(fleet_sandbox_context_hash "$sandbox_cf")"`, `[[ -n "$cf_prev" ]] || cf_prev="absent"`
	i := strings.Index(update, from)
	j := strings.Index(update, to)
	if i < 0 || j < i {
		t.Fatalf("update.sh no longer computes cf_now with fleet_sandbox_context_hash (%q … %q)", from, to)
	}
	return update[i:j+len(to)] + "\n"
}

// TestUpdateSandboxStampGate — update.sh's rebuild gate, fed the stamp
// file: an edit to any file in a custom context rebuilds, an unchanged
// context does not, and a stamp an earlier release wrote (the bare
// Containerfile sha256) reads as changed — one rebuild, never an error.
func TestUpdateSandboxStampGate(t *testing.T) {
	root := repoRoot(t)
	update := readFile(t, root, "scripts/update.sh")
	snippet := shellFunc(t, update, "hash_file") + updateStampSnippet(t, update)
	for _, want := range []string{
		`elif [[ "$cf_prev" == "absent" ]]; then`,
		`elif [[ "$cf_prev" =~ ^[0-9a-f]{64}$ ]]; then`,
		`elif [[ "$cf_now" != "$cf_prev" ]]; then`,
		`printf '%s\n' "$cf_now" > "$STAMP_FILE"`,
	} {
		if !strings.Contains(update, want) {
			t.Errorf("update.sh's sandbox gate must contain %q", want)
		}
	}

	cf := genericSandboxContext(t, t.TempDir(), "sandbox")
	writeTestFile(t, cf, "FROM registry.invalid/base\nCOPY requirements.txt /tmp/\n")
	stamp := filepath.Join(t.TempDir(), "sandbox-containerfile.sha256")
	gate := func() (now, prev string) {
		t.Helper()
		out := libBash(t, `sandbox_cf="$CF"; STAMP_FILE="$ST"; `+snippet+` printf '%s %s' "$cf_now" "$cf_prev"`, "CF="+cf, "ST="+stamp)
		f := strings.Fields(out)
		if len(f) != 2 {
			t.Fatalf("gate output %q", out)
		}
		return f[0], f[1]
	}
	record := func(v string) { writeTestFile(t, stamp, v+"\n") }

	now, prev := gate()
	if prev != "absent" || now == "absent" {
		t.Fatalf("no stamp: cf_now=%s cf_prev=%s", now, prev)
	}

	// The stamp an earlier release recorded: a bare sha256 of the
	// Containerfile. The gate sees a different value (and the legacy branch
	// names it); nothing fails.
	cfBody, err := os.ReadFile(cf)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(cfBody)
	record(hex.EncodeToString(sum[:]))
	if now2, prev2 := gate(); now2 == prev2 || !regexp.MustCompile(`^[0-9a-f]{64}$`).MatchString(prev2) {
		t.Errorf("legacy stamp: cf_now=%s cf_prev=%s, want a mismatch on a bare sha256", now2, prev2)
	}

	// After the rebuild records cf_now: unchanged context, no rebuild.
	record(now)
	if now2, prev2 := gate(); now2 != prev2 {
		t.Errorf("unchanged context: cf_now=%s cf_prev=%s, want equal", now2, prev2)
	}
	// Only requirements.txt edited: rebuild.
	writeTestFile(t, filepath.Join(filepath.Dir(cf), "requirements.txt"), "placeholder==2.0\n")
	if now2, prev2 := gate(); now2 == prev2 {
		t.Error("editing only requirements.txt did not change cf_now against the stamp")
	}
	// The published path's stamp also reads as a change.
	record("release-image")
	if now2, prev2 := gate(); now2 == prev2 {
		t.Error("release-image stamp matched a context hash")
	}
}

// TestReclaimReleaseImage — after a digest pull, the previous release image
// keeps its @sha256 name and `image prune` never removes it. update.sh rmi's
// it by that name, only when it differs from the image just tagged, and
// never with --force (sandbox_podman stubbed).
func TestReclaimReleaseImage(t *testing.T) {
	const (
		repo = "ghcr.io/elcanotek/fleet-sandbox@sha256:"
		prev = repo + "1111111111111111111111111111111111111111111111111111111111111111"
		cur  = repo + "2222222222222222222222222222222222222222222222222222222222222222"
		tag  = "localhost/fleet-sandbox:latest"
	)
	stub := `sandbox_podman() {
  printf '%s\n' "$*" >> "$LOG"
  if [[ "$1" == rmi ]]; then return "${RMI_RC:-0}"; fi
  if [[ "$1 $2" == "image inspect" ]]; then
    case "${*: -1}" in
      "$PREV") [[ -n "$PREV_ID" ]] && { echo "$PREV_ID"; return 0; }; return 125 ;;
      "$CUR"|"$TAG") echo "$CUR_ID" ;;
      *) return 125 ;;
    esac
  fi
}
`
	run := func(prevRef, prevID string, rmiRC string) []string {
		t.Helper()
		log := filepath.Join(t.TempDir(), "podman.log")
		writeTestFile(t, log, "")
		libBash(t, stub+`fleet_sandbox_reclaim_release_image "$PREV" "$CUR" "$TAG"; echo "rc=$?" >> "$LOG"`,
			"LOG="+log, "PREV="+prevRef, "PREV_ID="+prevID, "CUR="+cur, "TAG="+tag, "CUR_ID=cur-id", "RMI_RC="+rmiRC)
		b, err := os.ReadFile(log)
		if err != nil {
			t.Fatal(err)
		}
		return strings.Split(strings.TrimSpace(string(b)), "\n")
	}
	rmis := func(calls []string) []string {
		var out []string
		for _, c := range calls {
			if strings.HasPrefix(c, "rmi") {
				out = append(out, c)
			}
		}
		return out
	}

	if got := rmis(run(prev, "prev-id", "0")); len(got) != 1 || got[0] != "rmi "+prev {
		t.Errorf("superseded image: rmi calls %q, want exactly [rmi %s] (no --force)", got, prev)
	}
	for _, c := range []struct{ name, prev, prevID string }{
		{"no previous digest", "", "prev-id"},
		{"same digest", cur, "cur-id"},
		{"same image under another digest name", prev, "cur-id"},
		{"previous image no longer in the store", prev, ""},
		{"stamp is not a digest reference", tag, "prev-id"},
		{"stamp names some other tag", "localhost/other:1", "prev-id"},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := rmis(run(c.prev, c.prevID, "0")); len(got) != 0 {
				t.Errorf("rmi ran: %q", got)
			}
		})
	}
	// An image a container still uses: rmi refuses, and that is not an error.
	if calls := run(prev, "prev-id", "2"); calls[len(calls)-1] != "rc=0" {
		t.Errorf("a refused rmi must not fail the update: %q", calls)
	}

	// update.sh reclaims before the prune, only when the digest moved.
	update := readFile(t, repoRoot(t), "scripts/update.sh")
	guard := `if [[ -n "$pub_prev" && "$pub_prev" != "$pub_ref" ]]; then
        fleet_sandbox_reclaim_release_image "$pub_prev" "$pub_ref" "$ref_now"
      fi
      sandbox_podman image prune -f >/dev/null 2>&1 || true`
	if !strings.Contains(update, guard) {
		t.Errorf("update.sh must reclaim the superseded release image before its prune:\n%s", guard)
	}
	if strings.Contains(update, "rmi -f") || strings.Contains(update, "rmi --force") {
		t.Error("update.sh must never force-remove an image")
	}
}
