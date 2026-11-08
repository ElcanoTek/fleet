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
	"strconv"
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
// deterministic and independent of where the context lives. It excludes
// nothing — not .git, not ignore files or what they name: an over-broad hash
// costs at most an extra rebuild, a missed change a stale image.
func TestSandboxContextHash(t *testing.T) {
	root := repoRoot(t)
	generic := contextHash(t, filepath.Join(root, "config/default/sandbox/Containerfile"))
	if !regexp.MustCompile(`^ctx4-[0-9a-f]{64}$`).MatchString(generic) {
		t.Fatalf("context hash = %q, want ctx4-<sha256>", generic)
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

	// The Containerfile counts at its real relative path: renaming it changes
	// the hash, though only relative paths count — the copy above at
	// images/agent/ matched the generic one.
	cfR := genericSandboxContext(t, t.TempDir(), "sandbox")
	renamed := filepath.Join(filepath.Dir(cfR), "Dockerfile")
	if err := os.Rename(cfR, renamed); err != nil {
		t.Fatal(err)
	}
	if got := contextHash(t, renamed); got == generic {
		t.Error("renaming the Containerfile left the hash unchanged")
	}
	// Same files, but the build uses a different entry as its Containerfile.
	both := genericSandboxContext(t, t.TempDir(), "sandbox")
	writeTestFile(t, filepath.Join(filepath.Dir(both), "Alt.Containerfile"), "FROM scratch\n")
	if contextHash(t, both) == contextHash(t, filepath.Join(filepath.Dir(both), "Alt.Containerfile")) {
		t.Error("choosing a different Containerfile in the same context left the hash unchanged")
	}

	// Nothing is excluded: VCS metadata and ignore files count, and so does
	// a change to what an ignore file names.
	cf3 := genericSandboxContext(t, t.TempDir(), "sandbox")
	ctx3 := filepath.Dir(cf3)
	writeTestFile(t, filepath.Join(ctx3, ".git", "HEAD"), "ref: refs/heads/main\n")
	withGit := contextHash(t, cf3)
	if withGit == generic {
		t.Error("a .git directory in the context left the hash unchanged")
	}
	writeTestFile(t, filepath.Join(ctx3, ".git", "HEAD"), "ref: refs/heads/other\n")
	withIgnore := contextHash(t, cf3)
	if withIgnore == withGit {
		t.Error("a change inside .git left the hash unchanged")
	}
	writeTestFile(t, filepath.Join(ctx3, ".containerignore"), ".\n")
	if got := contextHash(t, cf3); got == withIgnore {
		t.Error("adding a .containerignore left the hash unchanged")
	}
	withIgnore = contextHash(t, cf3)
	writeTestFile(t, filepath.Join(ctx3, "scratch.txt"), "1\n") // what `.` would exclude
	if got := contextHash(t, cf3); got == withIgnore {
		t.Error("a file a .containerignore names left the hash unchanged")
	}
}

// TestSandboxContextHashCoversModesAndDirectories — COPY preserves
// permissions and creates directories, so a chmod +x, a directory mode change
// or an added empty directory is a new image; an unchanged tree keeps its
// hash. Group/other write bits (what the umask decides) do not count.
func TestSandboxContextHashCoversModesAndDirectories(t *testing.T) {
	cf := genericSandboxContext(t, t.TempDir(), "sandbox")
	ctx := filepath.Dir(cf)
	sub := filepath.Join(ctx, "bin")
	writeTestFile(t, filepath.Join(sub, "entry.sh"), "#!/bin/sh\n")
	chmod := func(p string, m os.FileMode) {
		t.Helper()
		if err := os.Chmod(p, m); err != nil {
			t.Fatal(err)
		}
	}
	chmod(sub, 0o755)
	chmod(filepath.Join(sub, "entry.sh"), 0o644)
	chmod(cf, 0o644)
	base := contextHash(t, cf)
	if again := contextHash(t, cf); again != base {
		t.Fatalf("unchanged tree: hash %s then %s", base, again)
	}

	for _, c := range []struct {
		name string
		path string
		mode os.FileMode
	}{
		{"file made executable", filepath.Join(sub, "entry.sh"), 0o755},
		{"requirements.txt made executable", filepath.Join(ctx, "requirements.txt"), 0o744},
		{"Containerfile mode", cf, 0o600},
		{"directory mode", sub, 0o700},
	} {
		t.Run(c.name, func(t *testing.T) {
			old, err := os.Stat(c.path)
			if err != nil {
				t.Fatal(err)
			}
			chmod(c.path, c.mode)
			if got := contextHash(t, cf); got == base {
				t.Errorf("mode-only change to %s (→ %o) left the hash unchanged", c.path, c.mode)
			}
			chmod(c.path, old.Mode().Perm())
			if got := contextHash(t, cf); got != base {
				t.Errorf("restoring the mode did not restore the hash: %s, want %s", got, base)
			}
		})
	}

	// Umask-only differences are not a different image.
	chmod(filepath.Join(sub, "entry.sh"), 0o664)
	chmod(sub, 0o775)
	if got := contextHash(t, cf); got != base {
		t.Errorf("group-write bits changed the hash: %s, want %s", got, base)
	}
	chmod(filepath.Join(sub, "entry.sh"), 0o644)
	chmod(sub, 0o755)

	// An added empty directory counts.
	if err := os.Mkdir(filepath.Join(ctx, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := contextHash(t, cf); got == base {
		t.Error("an added empty directory left the hash unchanged")
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
	// The generic Containerfile under another name is a different context
	// (a `COPY .` would carry the name); at another directory it is not.
	renamed := map[string]string{
		"sandbox/Dockerfile": generic("Containerfile"), "sandbox/requirements.txt": generic("requirements.txt"), "sandbox/requirements.in": generic("requirements.in"),
	}
	if usesReleaseImage(t, sandboxBundle(t, "sandbox/Dockerfile", renamed)) {
		t.Error("the generic context with a renamed Containerfile still counts as the release image")
	}
	elsewhere := map[string]string{
		"images/agent/Containerfile": generic("Containerfile"), "images/agent/requirements.txt": generic("requirements.txt"), "images/agent/requirements.in": generic("requirements.in"),
	}
	if !usesReleaseImage(t, sandboxBundle(t, "images/agent/Containerfile", elsewhere)) {
		t.Error("the generic context at another directory no longer counts as the release image")
	}
	if usesReleaseImage(t, sandboxBundle(t, "", vcs)) {
		t.Error("a context holding .git still counts as the release image (the hash excludes nothing)")
	}
}

// updateStampSnippet returns update.sh's cf_now/cf_prev computation, run
// verbatim by TestUpdateSandboxStampGate.
func updateStampSnippet(t *testing.T, update string) string {
	t.Helper()
	const from, to = "if [[ -n \"$sandbox_image_ref\" ]]; then\n  cf_now=\"prebuilt\"", `[[ -n "$cf_prev" ]] || cf_prev="absent"`
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
		out := libBash(t, `sandbox_cf="$CF"; STAMP_FILE="$ST"; sandbox_image_ref=""; `+snippet+` printf '%s %s' "$cf_now" "$cf_prev"`, "CF="+cf, "ST="+stamp)
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
	// Stamps of the earlier context formats (ctx1: contents only; ctx2:
	// modes, but VCS metadata and ignored paths excluded; ctx3: the
	// Containerfile under a fixed name) read as a change
	// too: one rebuild, no error.
	for _, old := range []string{"ctx1-", "ctx2-", "ctx3-"} {
		record(old + strings.Repeat("a", 64))
		if now2, prev2 := gate(); now2 == prev2 || !strings.HasPrefix(now2, "ctx4-") {
			t.Errorf("%s stamp: cf_now=%s cf_prev=%s, want a ctx4 mismatch", old, now2, prev2)
		}
	}
}

// reclaimStore is a fake podman image store for
// fleet_sandbox_reclaim_release_images: STORE holds "ref id" lines, INUSE the
// refs a container still uses (rmi refuses them, as podman does), and
// STORE_BROKEN makes every query fail the way an unreadable store does.
type reclaimStore struct {
	t                       *testing.T
	dir, store, inUse, list string
	log                     string
	broken                  bool
}

const reclaimStub = `sandbox_podman() {
  printf '%s\n' "$*" >> "$LOG"
  [[ -n "${STORE_BROKEN:-}" ]] && return 125
  local ref="${*: -1}" id
  id="$(awk -v r="$ref" '$1 == r { print $2 }' "$STORE")"
  case "$1" in
    rmi)
      [[ -n "$id" ]] || return 1
      grep -qxF -- "$ref" "$INUSE" && return 2
      awk -v r="$ref" '$1 != r' "$STORE" > "$STORE.n"
      mv "$STORE.n" "$STORE" ;;
    image)
      case "$2" in
        exists) [[ -n "$id" ]] ;;
        inspect) [[ -n "$id" ]] || return 125; printf '%s\n' "$id" ;;
      esac ;;
  esac
}
`

func newReclaimStore(t *testing.T, images map[string]string) *reclaimStore {
	t.Helper()
	dir := t.TempDir()
	s := &reclaimStore{t: t, dir: dir, store: filepath.Join(dir, "store"), inUse: filepath.Join(dir, "inuse"),
		list: filepath.Join(dir, "state", "sandbox-image.reclaim"), log: filepath.Join(dir, "podman.log")}
	var b strings.Builder
	for ref, id := range images {
		b.WriteString(ref + " " + id + "\n")
	}
	writeTestFile(t, s.store, b.String())
	writeTestFile(t, s.inUse, "")
	writeTestFile(t, s.log, "")
	return s
}

// run calls the reclaim with NEW_REF and the protected refs.
func (s *reclaimStore) run(newRef string, protect ...string) {
	s.t.Helper()
	args := ""
	env := []string{"LOG=" + s.log, "STORE=" + s.store, "INUSE=" + s.inUse, "LIST=" + s.list, "NEW=" + newRef}
	for i, p := range protect {
		args += ` "$P` + strconv.Itoa(i) + `"`
		env = append(env, "P"+strconv.Itoa(i)+"="+p)
	}
	if s.broken {
		env = append(env, "STORE_BROKEN=1")
	}
	libBash(s.t, reclaimStub+`fleet_sandbox_reclaim_release_images "$LIST" "$NEW"`+args+`; echo "rc=$?" >> "$LOG"`, env...)
	if calls := s.calls(); calls[len(calls)-1] != "rc=0" {
		s.t.Fatalf("reclaim did not return 0: %q", calls)
	}
}

func (s *reclaimStore) calls() []string {
	b, err := os.ReadFile(s.log)
	if err != nil {
		s.t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func (s *reclaimStore) rmis() []string {
	var out []string
	for _, c := range s.calls() {
		if strings.HasPrefix(c, "rmi") {
			out = append(out, c)
		}
	}
	return out
}

func (s *reclaimStore) pending() []string {
	b, err := os.ReadFile(s.list)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		s.t.Fatal(err)
	}
	return strings.Fields(string(b))
}

func (s *reclaimStore) has(ref string) bool {
	b, err := os.ReadFile(s.store)
	if err != nil {
		s.t.Fatal(err)
	}
	return strings.Contains(string(b), ref+" ")
}

// TestReclaimReleaseImages — after a digest pull the previous release image
// keeps its @sha256 name, so `image prune` never removes it. update.sh rmi's
// it by that name; a refusal (a container still uses it) keeps it in the
// pending list, which every later run — and the retry after the restart —
// tries again. The current ref, the tag and their image are never touched,
// and nothing is ever forced (sandbox_podman stubbed with a fake store).
func TestReclaimReleaseImages(t *testing.T) {
	const (
		repo  = "ghcr.io/elcanotek/fleet-sandbox@sha256:"
		old   = repo + "0000000000000000000000000000000000000000000000000000000000000000"
		prev  = repo + "1111111111111111111111111111111111111111111111111111111111111111"
		cur   = repo + "2222222222222222222222222222222222222222222222222222222222222222"
		alias = repo + "3333333333333333333333333333333333333333333333333333333333333333"
		tag   = "localhost/fleet-sandbox:latest"
	)
	images := map[string]string{old: "id-old", prev: "id-prev", cur: "id-cur", tag: "id-cur", alias: "id-cur"}

	t.Run("refused rmi stays pending and the next run removes it", func(t *testing.T) {
		s := newReclaimStore(t, images)
		writeTestFile(t, s.inUse, prev+"\n")
		s.run(prev, cur, tag)
		if got := s.pending(); len(got) != 1 || got[0] != prev {
			t.Fatalf("after a refused rmi, pending = %q, want [%s]", got, prev)
		}
		if !s.has(prev) {
			t.Fatal("the in-use image was removed")
		}
		// Still in use on the next try (the restart's retry): still pending.
		s.run("", cur, tag)
		if got := s.pending(); len(got) != 1 || got[0] != prev {
			t.Fatalf("still in use: pending = %q, want [%s]", got, prev)
		}
		// Freed: the next run removes it and empties the list.
		writeTestFile(t, s.inUse, "")
		s.run("", cur, tag)
		if s.has(prev) {
			t.Error("the freed image was not removed")
		}
		if got := s.pending(); got != nil {
			t.Errorf("pending after removal = %q, want none (file removed)", got)
		}
		for _, c := range s.rmis() {
			if c != "rmi "+prev {
				t.Errorf("unexpected rmi call %q (only %s, never forced)", c, prev)
			}
		}
	})

	t.Run("pending and newly superseded refs are both tried", func(t *testing.T) {
		s := newReclaimStore(t, images)
		writeTestFile(t, s.list, old+"\n")
		s.run(prev, cur, tag)
		if s.has(old) || s.has(prev) {
			t.Errorf("both superseded images should be gone; rmi calls %q", s.rmis())
		}
		if got := s.pending(); got != nil {
			t.Errorf("pending = %q, want none", got)
		}
	})

	t.Run("an unreadable store keeps the list and removes nothing", func(t *testing.T) {
		s := newReclaimStore(t, images)
		s.broken = true
		s.run(prev, cur, tag)
		if got := s.rmis(); len(got) != 0 {
			t.Errorf("rmi ran against an unreadable store: %q", got)
		}
		if got := s.pending(); len(got) != 1 || got[0] != prev {
			t.Errorf("pending = %q, want [%s]", got, prev)
		}
	})

	t.Run("nothing pending, nothing superseded: no podman calls", func(t *testing.T) {
		s := newReclaimStore(t, images)
		s.run("", cur, tag)
		if got := s.calls(); len(got) != 1 {
			t.Errorf("podman calls %q, want none", got[:len(got)-1])
		}
	})

	// update.sh reclaims before the prune (the newly superseded ref only when
	// the digest moved), and again after the restart, never forced.
	update := readFile(t, repoRoot(t), "scripts/update.sh")
	for _, want := range []string{
		`[[ -n "$pub_prev" && "$pub_prev" != "$pub_ref" ]] && sandbox_superseded="$pub_prev"`,
		`fleet_sandbox_reclaim_release_images "$SANDBOX_RECLAIM_FILE" "$sandbox_superseded" "$pub_ref" "$ref_now" "$sandbox_image_ref"
      sandbox_podman image prune -f >/dev/null 2>&1 || true`,
		`fleet_sandbox_reclaim_release_images "$SANDBOX_RECLAIM_FILE" "" "$sandbox_current" "$sandbox_current_tag" "$sandbox_base" "$sandbox_image_ref"`,
	} {
		if !strings.Contains(update, want) {
			t.Errorf("update.sh must contain:\n%s", want)
		}
	}
	if i, j := strings.Index(update, `"$sandbox_current" "$sandbox_current_tag" "$sandbox_base"`), strings.Index(update, "INSTALL_STATE=restarted"); i < j {
		t.Error("the post-restart reclaim must run after the restart succeeded")
	}
	if strings.Contains(update, "rmi -f") || strings.Contains(update, "rmi --force") {
		t.Error("update.sh must never force-remove an image")
	}
}

// TestReclaimKeepsProtectedRefsListed — a pending ref that is the current
// release ref, the tag, the prebuilt sandbox.image, or the same image under
// another name gets no rmi but stays listed; once it is no longer protected,
// the next run removes it.
func TestReclaimKeepsProtectedRefsListed(t *testing.T) {
	const (
		repo  = "ghcr.io/elcanotek/fleet-sandbox@sha256:"
		prev  = repo + "1111111111111111111111111111111111111111111111111111111111111111"
		cur   = repo + "2222222222222222222222222222222222222222222222222222222222222222"
		alias = repo + "3333333333333333333333333333333333333333333333333333333333333333"
		tag   = "localhost/fleet-sandbox:latest"
	)
	images := map[string]string{prev: "id-prev", cur: "id-cur", tag: "id-cur", alias: "id-cur"}

	t.Run("current refs and the current image are never removed", func(t *testing.T) {
		s := newReclaimStore(t, images)
		// cur and tag are protected refs; alias is the current image under
		// another digest name; a non-digest ref and a gone one are dropped;
		// a duplicate is tried once.
		writeTestFile(t, s.list, strings.Join([]string{cur, tag, alias, "localhost/other:1", repo + "9999", prev, prev}, "\n")+"\n")
		s.run("", cur, tag)
		if got := s.rmis(); len(got) != 1 || got[0] != "rmi "+prev {
			t.Errorf("rmi calls %q, want exactly [rmi %s]", got, prev)
		}
		for _, ref := range []string{cur, tag, alias} {
			if !s.has(ref) {
				t.Errorf("%s was removed", ref)
			}
		}
		// Refs equal to a protected ref, or naming its image, get no rmi but
		// stay listed; the non-digest, gone and removed ones leave.
		if got := s.pending(); strings.Join(got, " ") != cur+" "+alias {
			t.Errorf("pending = %q, want [%s %s]", got, cur, alias)
		}

		// A later release moves the tag on: cur and alias are superseded now,
		// and the next run removes both.
		const next = repo + "8888888888888888888888888888888888888888888888888888888888888888"
		writeTestFile(t, s.store, cur+" id-cur\n"+alias+" id-cur\n"+next+" id-next\n"+tag+" id-next\n")
		writeTestFile(t, s.log, "")
		s.run("", next, tag)
		if s.has(cur) || s.has(alias) || !s.has(next) || !s.has(tag) {
			t.Errorf("after the tag moved: rmi calls %q", s.rmis())
		}
		if got := s.pending(); got != nil {
			t.Errorf("pending = %q, want none", got)
		}
	})

	t.Run("the manifest's prebuilt sandbox.image is never removed", func(t *testing.T) {
		const prebuilt = "registry.example/sandbox@sha256:4444444444444444444444444444444444444444444444444444444444444444"
		const prebuiltAlias = "registry.example/sandbox@sha256:5555555555555555555555555555555555555555555555555555555555555555"
		s := newReclaimStore(t, map[string]string{
			prebuilt: "id-prebuilt", prebuiltAlias: "id-prebuilt", prev: "id-prev", cur: "id-cur", tag: "id-cur",
		})
		writeTestFile(t, s.list, prebuilt+"\n"+prebuiltAlias+"\n")
		// As update.sh's post-restart retry passes it: digest-file ref, tag,
		// (empty) custom base, sandbox_image_ref.
		s.run(prev, cur, tag, "", prebuilt)
		if got := s.rmis(); len(got) != 1 || got[0] != "rmi "+prev {
			t.Errorf("rmi calls %q, want exactly [rmi %s]", got, prev)
		}
		if !s.has(prebuilt) || !s.has(prebuiltAlias) {
			t.Error("the configured sandbox.image was removed")
		}
		if got := s.pending(); strings.Join(got, " ") != prebuilt+" "+prebuiltAlias {
			t.Errorf("pending = %q, want both prebuilt refs kept (skipped, not dropped)", got)
		}

		// The manifest stops naming that image: the next run reclaims both.
		writeTestFile(t, s.log, "")
		s.run("", cur, tag, "", "")
		if s.has(prebuilt) || s.has(prebuiltAlias) {
			t.Errorf("no longer protected, still present; rmi calls %q", s.rmis())
		}
		if got := s.pending(); got != nil {
			t.Errorf("pending = %q, want none", got)
		}
	})
}
