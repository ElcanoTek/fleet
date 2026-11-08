// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package scripts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// failingStat makes `stat` fail for any path ending in its FAIL_SUFFIX, and
// failingSha makes `sha256sum` fail on input containing UNREADABLE — the two
// ways an entry of a real context fails to hash (permission denied, EIO on a
// root-squashed NFS mount), simulated so the test also runs as root.
const failingStat = `stat() {
  if [[ -n "${FAIL_SUFFIX:-}" && "${*: -1}" == *"$FAIL_SUFFIX" ]]; then
    echo "stat: cannot statx '${*: -1}': Permission denied" >&2
    return 1
  fi
  command stat "$@"
}
`

const failingSha = `sha256sum() {
  local t
  t="$(mktemp)"
  cat > "$t"
  if grep -q UNREADABLE "$t"; then
    rm -f "$t"
    echo "sha256sum: -: Input/output error" >&2
    return 1
  fi
  command sha256sum < "$t"
  local rc=$?
  rm -f "$t"
  return "$rc"
}
`

// tryContextHash runs fleet_sandbox_context_hash with the failure stubs
// loaded; it returns the hash (empty on failure), whether it succeeded, and
// the combined output.
func tryContextHash(t *testing.T, containerfile string, env ...string) (string, bool, string) {
	t.Helper()
	out := libBash(t, failingStat+failingSha+`if h="$(fleet_sandbox_context_hash "$CF")"; then printf '\nHASH=%s\n' "$h"; else printf '\nFAILED stdout=[%s]\n' "$h"; fi`,
		append(env, "CF="+containerfile)...)
	for _, ln := range strings.Split(out, "\n") {
		if h, ok := strings.CutPrefix(ln, "HASH="); ok {
			return h, true, out
		}
	}
	return "", false, out
}

// TestSandboxContextHashFailsClosed — any entry the hash cannot stat, read or
// list makes it fail (non-zero, the path on stderr, nothing on stdout), never
// a ctx4- hash of the part of the tree it could read.
func TestSandboxContextHashFailsClosed(t *testing.T) {
	cf := genericSandboxContext(t, t.TempDir(), "sandbox")
	ctx := filepath.Dir(cf)
	if h, ok, out := tryContextHash(t, cf); !ok || !strings.HasPrefix(h, "ctx4-") {
		t.Fatalf("readable context did not hash: %s", out)
	}

	unreadable := filepath.Join(ctx, "data", "blob.bin")
	writeTestFile(t, unreadable, "UNREADABLE\n")
	h, ok, out := tryContextHash(t, cf)
	if ok || h != "" || !strings.Contains(out, "FAILED stdout=[]") {
		t.Errorf("a file sha256sum cannot read still produced a hash %q:\n%s", h, out)
	}
	if !strings.Contains(out, unreadable) {
		t.Errorf("the failure does not name %s:\n%s", unreadable, out)
	}
	if err := os.Remove(unreadable); err != nil {
		t.Fatal(err)
	}

	// A file whose mode cannot be read.
	if _, ok, out := tryContextHash(t, cf, "FAIL_SUFFIX=/requirements.in"); ok || !strings.Contains(out, filepath.Join(ctx, "requirements.in")) {
		t.Errorf("a stat failure still produced a hash, or did not name the file:\n%s", out)
	}
	// The Containerfile itself.
	if _, ok, out := tryContextHash(t, cf, "FAIL_SUFFIX=/Containerfile"); ok {
		t.Errorf("a Containerfile stat failure still produced a hash:\n%s", out)
	}

	// The real thing, where the test user can be denied (root reads anything).
	if os.Geteuid() != 0 {
		secret := filepath.Join(ctx, "secret.txt")
		writeTestFile(t, secret, "x\n")
		if err := os.Chmod(secret, 0); err != nil {
			t.Fatal(err)
		}
		if _, ok, out := tryContextHash(t, cf); ok || !strings.Contains(out, secret) {
			t.Errorf("an unreadable (mode 000) file still produced a hash:\n%s", out)
		}
		_ = os.Chmod(secret, 0o644)

		locked := filepath.Join(ctx, "locked")
		writeTestFile(t, filepath.Join(locked, "inner.txt"), "x\n")
		if err := os.Chmod(locked, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
		if _, ok, out := tryContextHash(t, cf); ok {
			t.Errorf("a directory find cannot list still produced a hash:\n%s", out)
		}
	}
}

// TestSandboxUsesReleaseImageFailsClosed — a context that cannot be hashed
// completely is never "the release image": the box builds instead.
func TestSandboxUsesReleaseImageFailsClosed(t *testing.T) {
	root := repoRoot(t)
	b, err := os.ReadFile(filepath.Join(root, "config/default/sandbox", "requirements.in"))
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"sandbox/requirements.in": string(b)}
	for _, n := range []string{"Containerfile", "requirements.txt"} {
		c, err := os.ReadFile(filepath.Join(root, "config/default/sandbox", n))
		if err != nil {
			t.Fatal(err)
		}
		files["sandbox/"+n] = string(c)
	}
	bundle := sandboxBundle(t, "", files)
	check := `if fleet_sandbox_uses_release_image "$B" "$ROOT/config/default"; then echo RELEASE; else echo CUSTOM; fi`
	if out := libBash(t, failingStat+check, "B="+bundle); !strings.Contains(out, "RELEASE") {
		t.Fatalf("the generic context should be the release image:\n%s", out)
	}
	if out := libBash(t, failingStat+check, "B="+bundle, "FAIL_SUFFIX=/requirements.in"); !strings.Contains(out, "CUSTOM") {
		t.Errorf("a context that could not be hashed counted as the release image:\n%s", out)
	}
}

// updateBlock returns update.sh's text from the line starting with from
// through the line containing through.
func updateBlock(t *testing.T, update, from, through string) string {
	t.Helper()
	i := strings.Index(update, from)
	if i < 0 {
		t.Fatalf("update.sh no longer contains %q", from)
	}
	j := strings.Index(update[i:], through)
	if j < 0 {
		t.Fatalf("update.sh no longer contains %q after %q", through, from)
	}
	end := i + j + len(through)
	if k := strings.IndexByte(update[end:], '\n'); k >= 0 {
		end += k
	}
	return update[i:end] + "\n"
}

// TestUpdateUnhashableContextRebuildsWithoutStamp — update.sh's own lines:
// a context that cannot be hashed makes cf_now "unknown", the gate takes the
// rebuild path, and the post-build bookkeeping records no stamp (the one
// already there stays).
func TestUpdateUnhashableContextRebuildsWithoutStamp(t *testing.T) {
	root := repoRoot(t)
	update := readFile(t, root, "scripts/update.sh")
	if strings.Contains(update, `cf_now="cf-$(hash_file`) {
		t.Error("update.sh still falls back to a Containerfile-only hash")
	}
	stampCalc := shellFunc(t, update, "hash_file") + updateStampSnippet(t, update)
	gate := updateBlock(t, update, "  if [[ \"$cf_now\" == \"unknown\" ]]; then\n    # Fail closed", `sandbox_unverified="the image tag could not be resolved`) + "  fi\n"
	record := updateBlock(t, update, "      mkdir -p \"$STATE_DIR\"\n      printf '%s\\n' \"$sandbox_base\" > \"$SANDBOX_BASE_FILE\"",
		`fleet_sandbox_reclaim_release_images "$SANDBOX_RECLAIM_FILE" "$sandbox_superseded" "$sandbox_base" "$ref_now" "$sandbox_image_ref"`)

	cf := genericSandboxContext(t, t.TempDir(), "sandbox")
	writeTestFile(t, filepath.Join(filepath.Dir(cf), "data.bin"), "UNREADABLE\n")
	state := t.TempDir()
	stamp := filepath.Join(state, "sandbox-containerfile.sha256")
	writeTestFile(t, stamp, "ctx3-previous\n")
	prelude := failingSha + reclaimStub + `
warn() { printf 'WARN %s\n' "$*"; }
ok() { printf 'OK %s\n' "$*"; }
sandbox_image_state() { echo present; }
sandbox_image_age_days() { echo 0; }
SANDBOX_MAX_AGE_DAYS=7
sandbox_cf="$CF"; STATE_DIR="$ST"; STAMP_FILE="$ST/sandbox-containerfile.sha256"
SANDBOX_BASE_FILE="$ST/sandbox-base.ref"; REF_FILE="$ST/sandbox-image.ref"; SANDBOX_RECLAIM_FILE="$ST/sandbox-image.reclaim"
ref_now=localhost/fleet-sandbox:latest; ref_prev="$ref_now"; ref_err=""; service_user=""
sandbox_base=""; base_prev=""; sandbox_image_ref=""; sandbox_unverified=""; sandbox_build_no_cache=0; build_reason=""
`
	dir := t.TempDir()
	env := []string{"CF=" + cf, "ST=" + state, "LOG=" + filepath.Join(dir, "log"), "STORE=" + filepath.Join(dir, "store"), "INUSE=" + filepath.Join(dir, "inuse")}
	writeTestFile(t, filepath.Join(dir, "store"), "")
	writeTestFile(t, filepath.Join(dir, "inuse"), "")
	out := libBash(t, prelude+stampCalc+gate+`printf 'CF_NOW=%s\nREASON=%s\n' "$cf_now" "$build_reason"
`+record, env...)
	if !strings.Contains(out, "CF_NOW=unknown\n") {
		t.Fatalf("an unhashable context did not make cf_now unknown:\n%s", out)
	}
	if !strings.Contains(out, "REASON=the sandbox build context could not be hashed completely") {
		t.Errorf("the gate did not take the rebuild path for an unknown hash:\n%s", out)
	}
	if !strings.Contains(out, "no build-context hash was recorded") {
		t.Errorf("no warning that the stamp was not recorded:\n%s", out)
	}
	if got := strings.TrimSpace(readFile(t, state, "sandbox-containerfile.sha256")); got != "ctx3-previous" {
		t.Errorf("the stamp was rewritten from an incomplete hash: %q", got)
	}

	// The same lines with a readable context record its hash.
	if err := os.Remove(filepath.Join(filepath.Dir(cf), "data.bin")); err != nil {
		t.Fatal(err)
	}
	out = libBash(t, prelude+stampCalc+gate+`printf 'REASON=%s\n' "$build_reason"
`+record, env...)
	if got := strings.TrimSpace(readFile(t, state, "sandbox-containerfile.sha256")); !strings.HasPrefix(got, "ctx4-") || got == "ctx3-previous" {
		t.Errorf("a readable context's rebuild did not record its hash: %q\n%s", got, out)
	}
}

// TestUpdateReclaimsSupersededBase — when a custom build's FLEET_SANDBOX_BASE
// digest moves, the previous base goes onto the pending reclaim list (an
// rmi refused while it is in use keeps it there) and a later run removes
// it; the new base and the tag are never touched.
func TestUpdateReclaimsSupersededBase(t *testing.T) {
	const (
		repo    = "ghcr.io/elcanotek/fleet-sandbox@sha256:"
		oldBase = repo + "6666666666666666666666666666666666666666666666666666666666666666"
		newBase = repo + "7777777777777777777777777777777777777777777777777777777777777777"
		tag     = "localhost/fleet-sandbox:latest"
	)
	update := readFile(t, repoRoot(t), "scripts/update.sh")
	record := updateBlock(t, update, "      mkdir -p \"$STATE_DIR\"\n      printf '%s\\n' \"$sandbox_base\" > \"$SANDBOX_BASE_FILE\"",
		`fleet_sandbox_reclaim_release_images "$SANDBOX_RECLAIM_FILE" "$sandbox_superseded" "$sandbox_base" "$ref_now" "$sandbox_image_ref"`)
	s := newReclaimStore(t, map[string]string{oldBase: "id-old", newBase: "id-new", tag: "id-bundle"})
	state := filepath.Dir(s.list)
	prelude := reclaimStub + `
warn() { printf 'WARN %s\n' "$*"; }
ok() { printf 'OK %s\n' "$*"; }
STATE_DIR="$ST"; STAMP_FILE="$ST/sandbox-containerfile.sha256"; SANDBOX_BASE_FILE="$ST/sandbox-base.ref"
REF_FILE="$ST/sandbox-image.ref"; SANDBOX_RECLAIM_FILE="$LIST"
cf_now=ctx3-x; ref_now="$TAG"; sandbox_image_ref=""
`
	env := []string{"ST=" + state, "LIST=" + s.list, "LOG=" + s.log, "STORE=" + s.store, "INUSE=" + s.inUse, "TAG=" + tag}

	// Base unchanged: nothing is listed or removed.
	libBash(t, prelude+`base_prev="$NB"; sandbox_base="$NB"
`+record, append(env, "NB="+newBase)...)
	if got := s.rmis(); len(got) != 0 || s.pending() != nil {
		t.Fatalf("unchanged base: rmi %q, pending %q", got, s.pending())
	}

	// Base moved while a container still uses the old one: refused, listed.
	writeTestFile(t, s.inUse, oldBase+"\n")
	libBash(t, prelude+`base_prev="$OB"; sandbox_base="$NB"
`+record, append(env, "OB="+oldBase, "NB="+newBase)...)
	if got := s.pending(); len(got) != 1 || got[0] != oldBase {
		t.Fatalf("superseded base not listed: pending %q, rmi %q", got, s.rmis())
	}
	if !s.has(oldBase) {
		t.Fatal("the in-use old base was removed")
	}

	// Next run (old base free now): removed, list emptied.
	writeTestFile(t, s.inUse, "")
	s.run("", newBase, tag, "")
	if s.has(oldBase) || s.pending() != nil {
		t.Errorf("old base not reclaimed on the next run: pending %q, rmi %q", s.pending(), s.rmis())
	}
	for _, ref := range []string{newBase, tag} {
		if !s.has(ref) {
			t.Errorf("%s was removed", ref)
		}
	}
	for _, c := range s.rmis() {
		if c != "rmi "+oldBase {
			t.Errorf("unexpected rmi %q", c)
		}
	}

	// The post-restart retry protects the base on a custom build, and the
	// digest file only on the published path.
	for _, want := range []string{
		`if [[ "$sandbox_published" == "1" && -z "$sandbox_image_ref" && -f "$SANDBOX_DIGEST_FILE" ]]; then`,
		`fleet_sandbox_reclaim_release_images "$SANDBOX_RECLAIM_FILE" "" "$sandbox_current" "$ref_now" "$sandbox_base" "$sandbox_image_ref"`,
	} {
		if !strings.Contains(update, want) {
			t.Errorf("update.sh must contain %q", want)
		}
	}
}
