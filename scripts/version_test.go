// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package scripts

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// scripts/version.sh is the ONE place that knows what a fleet release number
// looks like (ADR-0059, docs/VERSIONING.md). Nothing else parses or authors one:
// the Makefile stamps whatever `describe` says, and .github/workflows/release.yml
// tags whatever `next` says. That concentration is the point — and it makes the
// script's behaviour load-bearing enough to test directly rather than by reading
// it.
//
// These tests drive the real script against SYNTHETIC git histories in t.TempDir
// (never the fleet checkout, which has its own tags and would make the
// assertions depend on when they run). They cover the two properties the scheme
// exists for and the one the format fights for:
//
//   - several releases in one day get distinct, increasing ordinals;
//   - a build between releases says which release it is past, and by how far;
//   - the SemVer rendering Helm/npm require never grows a leading zero or a
//     fourth component.

// gitBin resolves git once; every test here needs a real one.
func gitBin(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("git")
	if err != nil {
		t.Skipf("git not on PATH: %v", err)
	}
	return path
}

// versionScript returns the absolute path to the script under test.
func versionScript(t *testing.T) string {
	t.Helper()
	p := filepath.Join(repoRoot(t), "scripts", "version.sh")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("scripts/version.sh missing: %v", err)
	}
	return p
}

// testRepo is a throwaway git repo the tests tag by hand.
type testRepo struct {
	t      *testing.T
	dir    string
	script string
}

func newTestRepo(t *testing.T) *testRepo {
	t.Helper()
	git := gitBin(t)
	r := &testRepo{t: t, dir: t.TempDir()}
	r.git(git, "init", "--quiet", "--initial-branch=main")
	// Identity + signing off: a repo-local config so the test never depends on
	// (or touches) the machine's git identity.
	r.git(git, "config", "user.email", "test@example.invalid")
	r.git(git, "config", "user.name", "fleet version test")
	r.git(git, "config", "commit.gpgsign", "false")
	r.git(git, "config", "tag.gpgsign", "false")

	// The script under test is COPIED IN at its shipped path rather than run
	// from the fleet checkout. That is not tidiness: the script derives identity
	// only when git discovery lands on its own checkout (otherwise a source
	// archive unpacked inside an unrelated repo would stamp that repo's tags —
	// see TestIdentityIsNotBorrowedFromAnEnclosingRepo). Running fleet's own
	// copy with cwd set to a synthetic repo is exactly the case it refuses, so
	// the copy is what makes these tests exercise the real path.
	//
	// It is committed, not left untracked, because an untracked file now makes
	// the tree dirty (TestUntrackedBuildInputsMarkTheTreeDirty) and every
	// clean-tree assertion below would inherit that.
	body, err := os.ReadFile(versionScript(t))
	if err != nil {
		t.Fatalf("read version.sh: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(r.dir, "scripts"), 0o750); err != nil {
		t.Fatalf("mkdir scripts: %v", err)
	}
	r.script = filepath.Join(r.dir, "scripts", "version.sh")
	if err := os.WriteFile(r.script, body, 0o700); err != nil { //nolint:gosec // executable test fixture
		t.Fatalf("write version.sh: %v", err)
	}
	r.git(git, "add", "scripts/version.sh")
	r.git(git, "commit", "--quiet", "-m", "add version.sh")
	return r
}

// hermeticEnv strips the ambient git wiring — GIT_DIR and friends, set by any
// caller that itself runs under git — so every command below acts on the
// synthetic repo in r.dir and not on whatever repo invoked `go test`. The
// variables are REMOVED rather than set empty: git reads an empty GIT_DIR as the
// path "" and fails with exit 128 rather than falling back to discovery.
func (r *testRepo) hermeticEnv() []string {
	strip := map[string]bool{
		"GIT_DIR": true, "GIT_WORK_TREE": true, "GIT_INDEX_FILE": true,
		"GIT_COMMON_DIR": true, "GIT_CEILING_DIRECTORIES": true,
	}
	env := make([]string, 0, len(os.Environ())+2)
	for _, kv := range os.Environ() {
		if k, _, ok := strings.Cut(kv, "="); ok && strip[k] {
			continue
		}
		env = append(env, kv)
	}
	// Repo-local config only: the machine's global/system git config must not
	// change what these assertions see (a global tag.gpgsign, say).
	return append(env, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
}

func (r *testRepo) git(git string, args ...string) {
	r.t.Helper()
	cmd := exec.Command(git, args...)
	cmd.Dir = r.dir
	cmd.Env = r.hermeticEnv()
	if out, err := cmd.CombinedOutput(); err != nil {
		r.t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
}

// commit adds one file and commits it, so each call advances the graph.
func (r *testRepo) commit(name string) {
	r.t.Helper()
	git := gitBin(r.t)
	if err := os.WriteFile(filepath.Join(r.dir, name), []byte(name+"\n"), 0o600); err != nil {
		r.t.Fatalf("write %s: %v", name, err)
	}
	r.git(git, "add", name)
	r.git(git, "commit", "--quiet", "-m", "add "+name)
}

func (r *testRepo) tag(name string) {
	r.t.Helper()
	r.git(gitBin(r.t), "tag", "-a", name, "-m", name)
}

// run invokes the script inside the synthetic repo and returns its trimmed
// stdout.
func (r *testRepo) run(args ...string) string {
	r.t.Helper()
	cmd := exec.Command(r.script, args...)
	cmd.Dir = r.dir
	cmd.Env = r.hermeticEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.t.Fatalf("version.sh %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// TestNextOpensAndAdvancesTheDaysOrdinal is the scheme's headline property:
// "there will be multiple releases in a day" is the normal case, so the ordinal
// must open at 1 and advance without a human choosing it.
func TestNextOpensAndAdvancesTheDaysOrdinal(t *testing.T) {
	r := newTestRepo(t)
	r.commit("a")

	if got, want := r.run("next", "2026.09.04"), "v2026.09.04.1"; got != want {
		t.Errorf("first release of the day = %q, want %q", got, want)
	}

	// Three releases in one day, each taking the next ordinal.
	for i, want := range []string{"v2026.09.04.1", "v2026.09.04.2", "v2026.09.04.3"} {
		got := r.run("next", "2026.09.04")
		if got != want {
			t.Fatalf("release %d of the day = %q, want %q", i+1, got, want)
		}
		r.commit(fmt.Sprintf("c%d", i))
		r.tag(got)
	}

	// A new day resets to 1 even though the repo already carries tags — the
	// ordinal counts THAT DAY's releases, not all releases.
	if got, want := r.run("next", "2026.09.05"), "v2026.09.05.1"; got != want {
		t.Errorf("next day = %q, want %q", got, want)
	}

	// Double digits: the ordinal must compare numerically, not lexically, or the
	// 10th release of a busy day collides with the 2nd.
	r.tag("v2026.09.05.9")
	if got, want := r.run("next", "2026.09.05"), "v2026.09.05.10"; got != want {
		t.Errorf("after .9 = %q, want %q", got, want)
	}
	r.tag("v2026.09.05.10")
	if got, want := r.run("next", "2026.09.05"), "v2026.09.05.11"; got != want {
		t.Errorf("after .10 = %q, want %q (lexical sort would say .10)", got, want)
	}
}

// TestNextDefaultsToTodayUTC — release.yml calls `next` with no argument, so the
// no-arg path is the one that actually ships. It must be UTC (see ADR-0059) and
// shaped like a tag.
func TestNextDefaultsToTodayUTC(t *testing.T) {
	r := newTestRepo(t)
	r.commit("a")

	got := r.run("next")
	want := "v" + time.Now().UTC().Format("2006.01.02") + ".1"
	if got != want {
		t.Errorf("next (no date) = %q, want %q — the default must be today in UTC", got, want)
	}
}

// TestNextRejectsAMalformedDate — the ordinal is derived by matching tags
// against the date, so a date in the wrong shape would silently produce
// "v2026-09-04.1" and poison the tag namespace. It must fail instead.
func TestNextRejectsAMalformedDate(t *testing.T) {
	r := newTestRepo(t)
	r.commit("a")

	cmd := exec.Command(r.script, "next", "2026-09-04")
	cmd.Dir = r.dir
	cmd.Env = r.hermeticEnv()
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("next with a dashed date succeeded (%q); it must refuse", out)
	}
	if !strings.Contains(string(out), "YYYY.MM.DD") {
		t.Errorf("refusal did not say the expected shape: %q", out)
	}
}

// TestDescribeReportsDistanceFromTheRelease is what makes a date-based scheme
// usable on boxes that track main: between releases the stamp must say WHICH
// release it is past and by how many commits, not just "dev".
func TestDescribeReportsDistanceFromTheRelease(t *testing.T) {
	r := newTestRepo(t)
	r.commit("a")

	// Before the first release ever: the honest sentinel plus a revision.
	pre := r.run("describe")
	if !strings.HasPrefix(pre, "dev+g") {
		t.Errorf("describe with no tags = %q, want a dev+g<sha> sentinel", pre)
	}

	r.tag("v2026.09.04.1")
	if got, want := r.run("describe"), "2026.09.04.1"; got != want {
		t.Errorf("describe at the release = %q, want exactly %q (no build metadata)", got, want)
	}

	r.commit("b")
	r.commit("c")
	got := r.run("describe")
	distance := regexp.MustCompile(`^2026\.09\.04\.1\+2\.g[0-9a-f]{12}$`)
	if !distance.MatchString(got) {
		t.Errorf("describe 2 commits past the release = %q, want 2026.09.04.1+2.g<12 hex>", got)
	}

	// A modified tree marks itself, and must not grow a second '+' — SemVer
	// allows exactly one build-metadata separator.
	if err := os.WriteFile(filepath.Join(r.dir, "b"), []byte("modified\n"), 0o600); err != nil {
		t.Fatalf("dirty the tree: %v", err)
	}
	dirty := r.run("describe")
	if !strings.HasSuffix(dirty, ".dirty") {
		t.Errorf("describe of a modified tree = %q, want a .dirty suffix", dirty)
	}
	if strings.Count(dirty, "+") != 1 {
		t.Errorf("describe of a modified tree = %q, want exactly one '+' (SemVer build metadata)", dirty)
	}
}

// TestCurrentTracksTheNewestRelease — `current` is what `make helm-package`
// stamps as the app version, so it must follow the newest tag and compare
// numerically across a year boundary.
func TestCurrentTracksTheNewestRelease(t *testing.T) {
	r := newTestRepo(t)
	r.commit("a")

	if got, want := r.run("current"), "dev"; got != want {
		t.Errorf("current with no releases = %q, want %q", got, want)
	}

	for _, tag := range []string{"v2026.09.04.1", "v2026.09.04.2", "v2026.12.25.1", "v2027.01.05.3"} {
		r.commit("c" + tag)
		r.tag(tag)
	}
	if got, want := r.run("current"), "2027.01.05.3"; got != want {
		t.Errorf("current = %q, want %q", got, want)
	}
}

// TestSemverRenderingIsStrictAndOrdered guards the one place the format bends
// for someone else: Helm and npm PARSE their version fields and reject both a
// fourth component and a leading zero, so `semver` re-renders the identity as
// YYYY.<MM*100+DD>.<N>. If this drifts, `helm lint --strict` fails in CI.
func TestSemverRenderingIsStrictAndOrdered(t *testing.T) {
	strictSemVer := regexp.MustCompile(`^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$`)

	for _, tc := range []struct{ tag, want string }{
		{"v2026.09.04.1", "2026.904.1"},   // single-digit month AND day
		{"v2026.09.04.12", "2026.904.12"}, // a busy day
		{"v2026.12.25.3", "2026.1225.3"},  // two-digit month
		{"v2027.01.05.2", "2027.105.2"},   // January, the leading-zero trap
		{"v2026.10.01.1", "2026.1001.1"},  // day 01
	} {
		r := newTestRepo(t)
		r.commit("a")
		r.tag(tc.tag)

		got := r.run("semver")
		if got != tc.want {
			t.Errorf("semver of %s = %q, want %q", tc.tag, got, tc.want)
		}
		if !strictSemVer.MatchString(got) {
			t.Errorf("semver of %s = %q, which is not strict 3-field SemVer (Helm/npm would reject it)", tc.tag, got)
		}
	}

	// Pre-first-release: still parseable, because both consumers validate the
	// field's schema even in a checkout nobody has released.
	r := newTestRepo(t)
	r.commit("a")
	if got, want := r.run("semver"), "0.0.0"; got != want {
		t.Errorf("semver with no releases = %q, want %q", got, want)
	}
}

// TestReleaseTagGlobIgnoresForeignTags — `next` counts matching tags and
// `describe` parses them, so the glob must not pick up a hand-cut semver tag or
// an image tag that shares the repo's tag namespace.
func TestReleaseTagGlobIgnoresForeignTags(t *testing.T) {
	r := newTestRepo(t)
	r.commit("a")
	for _, foreign := range []string{
		"v1.2.3",
		"sandbox-image-2026.09.04",
		"release-2026.09.04.7",
		"v26.9.4.1",
		// The one git's glob cannot exclude: in fnmatch, the trailing `[0-9]*`
		// means "one digit then ANYTHING", so this matches --match and only the
		// anchored regex rejects it. Left unfiltered it reached `semver` as
		// `10#1oops` (an arithmetic abort) and, worse, read as "already
		// released" in release.yml — which would skip cutting the real tag.
		"v2026.09.04.1oops",
	} {
		r.tag(foreign)
	}

	if got, want := r.run("current"), "dev"; got != want {
		t.Errorf("current with only foreign tags = %q, want %q", got, want)
	}
	if got := r.run("describe"); !strings.HasPrefix(got, "dev+g") {
		t.Errorf("describe with only foreign tags = %q, want a dev+g<sha> sentinel", got)
	}
	if got, want := r.run("next", "2026.09.04"), "v2026.09.04.1"; got != want {
		t.Errorf("next with only foreign tags = %q, want %q", got, want)
	}
}

// TestUnknownSubcommandFails — the Makefile and release.yml both interpolate
// this script's stdout directly into a build stamp and a `git tag` argument. A
// typo must fail the build loudly rather than stamping an empty version.
func TestUnknownSubcommandFails(t *testing.T) {
	r := newTestRepo(t)
	r.commit("a")

	cmd := exec.Command(r.script, "bump")
	cmd.Dir = r.dir
	cmd.Env = r.hermeticEnv()
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("an unknown subcommand succeeded (%q); it must refuse", out)
	}
	if !strings.Contains(string(out), "unknown subcommand") {
		t.Errorf("refusal was not explanatory: %q", out)
	}
}

// TestSemverIsNotDerailedByAForeignTag — the failure mode that made the strict
// regex necessary rather than merely tidy: `semver` parses the ordinal with
// base-10 arithmetic, so a glob-matching tag with a nonnumeric ordinal did not
// produce a wrong answer, it aborted the command that the Makefile and
// `make helm-package` interpolate.
func TestSemverIsNotDerailedByAForeignTag(t *testing.T) {
	r := newTestRepo(t)
	r.commit("a")
	r.tag("v2026.09.04.1oops")

	if got, want := r.run("semver"), "0.0.0"; got != want {
		t.Errorf("semver with only a malformed tag = %q, want %q (and it must not abort)", got, want)
	}
	if got := r.run("released-at"); got != "" {
		t.Errorf("released-at on a commit carrying only a malformed tag = %q, want empty — release.yml would read this as \"already released\" and never cut the real tag", got)
	}
}

// TestCurrentIsReachableFromHead — `make helm-package` stamps `current` as the
// chart's app version. Answering with "the newest tag anywhere in the repo"
// would label a chart built from an older checkout with a release it does not
// contain, whenever the clone has since fetched newer tags.
func TestCurrentIsReachableFromHead(t *testing.T) {
	r := newTestRepo(t)
	r.commit("a")
	r.tag("v2026.09.04.1")
	r.commit("b")
	r.tag("v2026.09.05.1")

	// Check out the older release while the newer tag is still in the repo.
	r.git(gitBin(t), "checkout", "--quiet", "v2026.09.04.1")

	if got, want := r.run("current"), "2026.09.04.1"; got != want {
		t.Errorf("current at the older release = %q, want %q — a chart packaged here must not claim a release it does not contain", got, want)
	}
	if got, want := r.run("semver"), "2026.904.1"; got != want {
		t.Errorf("semver at the older release = %q, want %q", got, want)
	}
}

// TestUntrackedBuildInputsMarkTheTreeDirty — `git diff HEAD` sees only tracked
// changes, so an untracked .go file (or an embedded asset) changed what
// `make build` produced while the stamp still claimed the pristine release.
func TestUntrackedBuildInputsMarkTheTreeDirty(t *testing.T) {
	r := newTestRepo(t)
	r.commit("a")
	r.tag("v2026.09.04.1")

	if got, want := r.run("describe"), "2026.09.04.1"; got != want {
		t.Fatalf("describe on a clean tree at the release = %q, want %q", got, want)
	}

	if err := os.WriteFile(filepath.Join(r.dir, "extra.go"), []byte("package x\n"), 0o600); err != nil {
		t.Fatalf("write untracked file: %v", err)
	}
	if got := r.run("describe"); !strings.HasSuffix(got, "dirty") {
		t.Errorf("describe with an untracked build input = %q, want a dirty marker — the binary differs from the release it would otherwise claim", got)
	}
}

// TestIdentityIsNotBorrowedFromAnEnclosingRepo — a fleet source archive unpacked
// inside somebody else's git checkout must report the `dev` sentinel, not that
// repo's tags. Git discovery walks upward, so "am I in a git repo?" is not the
// same question as "am I in THIS repo?".
func TestIdentityIsNotBorrowedFromAnEnclosingRepo(t *testing.T) {
	git := gitBin(t)
	outer := t.TempDir()

	run := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command(git, args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run(outer, "init", "--quiet", "--initial-branch=main")
	run(outer, "config", "user.email", "test@example.invalid")
	run(outer, "config", "user.name", "outer repo")
	if err := os.WriteFile(filepath.Join(outer, "z"), []byte("z\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	run(outer, "add", "z")
	run(outer, "commit", "--quiet", "-m", "outer")
	// A tag that IS a valid fleet release name — the point is that matching the
	// format is not enough; it has to be this repo's.
	run(outer, "tag", "v2099.01.01.9")

	// Unpack a "source archive" of fleet (just the script, at its real path)
	// inside that checkout.
	unpacked := filepath.Join(outer, "fleet-src")
	if err := os.MkdirAll(filepath.Join(unpacked, "scripts"), 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body, err := os.ReadFile(versionScript(t))
	if err != nil {
		t.Fatalf("read version.sh: %v", err)
	}
	script := filepath.Join(unpacked, "scripts", "version.sh")
	if err := os.WriteFile(script, body, 0o700); err != nil { //nolint:gosec // executable test fixture
		t.Fatalf("write version.sh: %v", err)
	}

	for _, sub := range []string{"describe", "current"} {
		cmd := exec.Command(script, sub)
		cmd.Dir = unpacked
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("version.sh %s: %v\n%s", sub, err, out)
		}
		if got := strings.TrimSpace(string(out)); got != "dev" {
			t.Errorf("version.sh %s inside an unrelated git checkout = %q, want \"dev\" — it must not stamp a stranger's release into a fleet binary", sub, got)
		}
	}
}
