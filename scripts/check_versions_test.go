// Package scripts holds test-only coverage for the shell linters in this
// directory. There are deliberately no non-test Go files here — `go build`
// ignores the package; `go test ./scripts` (part of `make test`) runs it.
package scripts

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// A version that must be identical in two places is a version that will
// eventually differ. These tests make that drift a test failure instead of a
// hope, because hope already failed once: CI pinned node '22' across six jobs
// in four workflow files while scripts/doctor.sh's floor said 20 and the box
// ran whatever `dnf install nodejs` resolved to. Nothing reconciled the three,
// and no bot could — Dependabot updates action refs, not the inputs passed to
// them, and nothing it watches covers an OS package (see .github/dependabot.yml).
//
// The rule these encode: every version gets ONE declaration point, and anything
// that must agree with it is asserted here rather than remembered.

func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("..")
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	return root
}

func readFile(t *testing.T, root, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(root, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

// nvmrcMajor is the single declared node major — web/.nvmrc. Everything else
// that names a node version is checked against this.
func nvmrcMajor(t *testing.T, root string) int {
	t.Helper()
	raw := strings.TrimSpace(readFile(t, root, "web/.nvmrc"))
	raw = strings.TrimPrefix(raw, "v")
	if i := strings.IndexByte(raw, '.'); i >= 0 {
		raw = raw[:i]
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		t.Fatalf("web/.nvmrc is %q, want a major version like \"24\": %v", raw, err)
	}
	return n
}

// majorFromRange pulls the leading integer out of a semver range like ">=24",
// "^24" or "24.x".
func majorFromRange(spec string) (int, bool) {
	m := regexp.MustCompile(`(\d+)`).FindStringSubmatch(spec)
	if m == nil {
		return 0, false
	}
	n, err := strconv.Atoi(m[1])
	return n, err == nil
}

func pkgJSON(t *testing.T, root, rel string) map[string]any {
	t.Helper()
	var d map[string]any
	if err := json.Unmarshal([]byte(readFile(t, root, rel)), &d); err != nil {
		t.Fatalf("parse %s: %v", rel, err)
	}
	return d
}

// TestNodeMajorAgreesEverywhere: web/.nvmrc is the declaration point; every
// other node version in the tree must agree with it.
func TestNodeMajorAgreesEverywhere(t *testing.T) {
	root := repoRoot(t)
	want := nvmrcMajor(t, root)

	// engines.node — the constraint npm reports on. Nothing reads .nvmrc to
	// produce it, so without this assertion bumping .nvmrc leaves it stale and
	// silent (engines is a warning npm never fails on).
	for _, rel := range []string{"web/package.json", "scripts/rampart-service/package.json"} {
		eng, ok := pkgJSON(t, root, rel)["engines"].(map[string]any)
		if !ok {
			t.Errorf("%s has no engines block; expected engines.node >= %d", rel, want)
			continue
		}
		spec, _ := eng["node"].(string)
		got, ok := majorFromRange(spec)
		if !ok {
			t.Errorf("%s engines.node = %q, cannot read a major from it", rel, spec)
			continue
		}
		if got != want {
			t.Errorf("%s engines.node = %q (major %d), but web/.nvmrc says %d — bump them together", rel, spec, got, want)
		}
	}

	// @types/node's major tracks the Node RUNTIME major. A mismatch type-checks
	// the app against API surface the runtime does not have, which is a real
	// build-passes/runtime-fails gap and the kind of soft coupling nobody thinks
	// of as "the node version". It drifted to ^26 while the runtime was 24.
	dev, ok := pkgJSON(t, root, "web/package.json")["devDependencies"].(map[string]any)
	if !ok {
		t.Fatal("web/package.json has no devDependencies")
	}
	spec, _ := dev["@types/node"].(string)
	if got, ok := majorFromRange(spec); !ok {
		t.Errorf("@types/node = %q, cannot read a major", spec)
	} else if got != want {
		t.Errorf("@types/node = %q (major %d) but web/.nvmrc says %d — @types/node's major tracks the runtime major", spec, got, want)
	}

	// The rampart container should sit on the same node line as everything else.
	cf := readFile(t, root, "scripts/rampart-service/Containerfile")
	if !strings.Contains(cf, "node:"+strconv.Itoa(want)+"-slim") {
		t.Errorf("scripts/rampart-service/Containerfile does not use node:%d-slim (web/.nvmrc says %d)", want, want)
	}
}

// TestWorkflowsDeclareVersionsByFile: a literal version in a workflow is a
// copy that drifts. setup-node and setup-go both accept a *-version-file input,
// so the declaration lives in web/.nvmrc and go.mod respectively.
func TestWorkflowsDeclareVersionsByFile(t *testing.T) {
	root := repoRoot(t)
	dir := filepath.Join(root, ".github", "workflows")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read workflows: %v", err)
	}
	// Matches `node-version: '22'` / `go-version: "1.26.6"` but not the
	// *-version-file forms, and not a version-file value.
	literal := regexp.MustCompile(`(?m)^\s*(node|go)-version:\s*['"]?[\d.]`)
	found := 0
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yml") {
			continue
		}
		body := readFile(t, root, filepath.Join(".github", "workflows", e.Name()))
		for _, m := range literal.FindAllString(body, -1) {
			t.Errorf("%s pins a literal version (%q) — use `node-version-file: web/.nvmrc` or `go-version-file: go.mod` so the version has one declaration point",
				e.Name(), strings.TrimSpace(m))
			found++
		}
	}
	if found == 0 {
		t.Log("no literal node/go version pins in any workflow")
	}
}

// TestDuplicatedToolPinsAgree: some tool versions genuinely must appear twice
// (a pinned tarball needs its checksum next to it in each job that installs
// it). Where that is unavoidable, assert the copies match rather than trusting
// a "keep these in sync" comment to be obeyed.
func TestDuplicatedToolPinsAgree(t *testing.T) {
	root := repoRoot(t)
	ci := readFile(t, root, ".github/workflows/ci.yml")

	for _, tc := range []struct {
		name  string
		other string
		re    *regexp.Regexp
	}{
		{"GRYPE_VERSION", ".github/workflows/grype-scheduled.yml", regexp.MustCompile(`GRYPE_VERSION:\s*'([^']+)'`)},
		{"GRYPE_SHA256", ".github/workflows/grype-scheduled.yml", regexp.MustCompile(`GRYPE_SHA256:\s*'([^']+)'`)},
		{"GITLEAKS_VERSION", ".github/workflows/dev-ci.yml", regexp.MustCompile(`GITLEAKS_VERSION:\s*'([^']+)'`)},
		{"golangci-lint version", ".github/workflows/dev-ci.yml", regexp.MustCompile(`golangci-lint-action@v\d+\s+with:\s+(?:#[^\n]*\n\s+)*version:\s*(v[\d.]+)`)},
	} {
		a := tc.re.FindStringSubmatch(ci)
		b := tc.re.FindStringSubmatch(readFile(t, root, tc.other))
		if a == nil || b == nil {
			// A pin that moved or was consolidated away is fine; a silently
			// half-matched pair is not, so say which side went missing.
			t.Logf("%s: not found in both ci.yml (%v) and %s (%v) — skipping", tc.name, a != nil, tc.other, b != nil)
			continue
		}
		if a[1] != b[1] {
			t.Errorf("%s disagrees: ci.yml has %q, %s has %q — these install the same tool and must match", tc.name, a[1], tc.other, b[1])
		}
	}
}

// TestPostgresMajorAgreesAcrossCI: the Postgres major appears as a service
// image in several workflows AND as the pg_dump client major that ci.yml
// installs. Those two MUST match — pg_dump refuses to dump a newer server —
// and the failure mode is the dangerous kind: ci.yml's own comment says the
// backup/restore round-trip test "is written to skip, never fail, when client
// and server majors disagree". So bumping the service image and forgetting the
// client does not go red; it silently switches the round-trip test OFF behind a
// green check. Assert it instead of trusting a comment.
func TestPostgresMajorAgreesAcrossCI(t *testing.T) {
	root := repoRoot(t)
	imageRe := regexp.MustCompile(`image:\s*postgres:(\d+)`)
	clientRe := regexp.MustCompile(`postgresql-client-(\d+)`)

	majors := map[string][]string{} // major -> where it was seen
	note := func(major, where string) { majors[major] = append(majors[major], where) }

	dir := filepath.Join(root, ".github", "workflows")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read workflows: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yml") {
			continue
		}
		body := readFile(t, root, filepath.Join(".github", "workflows", e.Name()))
		for _, m := range imageRe.FindAllStringSubmatch(body, -1) {
			note(m[1], e.Name()+" (service image)")
		}
		for _, m := range clientRe.FindAllStringSubmatch(body, -1) {
			note(m[1], e.Name()+" (pg_dump client)")
		}
	}

	if len(majors) == 0 {
		t.Skip("no postgres pins found in workflows")
	}
	if len(majors) > 1 {
		for major, where := range majors {
			t.Errorf("postgres major %s appears in: %s", major, strings.Join(where, ", "))
		}
		t.Error("postgres majors disagree across CI — a server/client mismatch makes the backup/restore round-trip test SKIP rather than fail, so this would ship as a green check with the test silently off")
	}
}

// goMinor pulls "1.27" out of a go.mod `go` directive or a `golang:1.27` image
// tag, discarding any patch component. Comparing at major.minor is deliberate:
// web/go.mod says in its own comment that pinning a PATCH there just created a
// second copy of the root pin to bump in lockstep, and the golang base image
// has no patch tag worth chasing either. The MINOR is the part that has to
// agree.
func goMinor(spec string) (string, bool) {
	m := regexp.MustCompile(`(\d+)\.(\d+)`).FindStringSubmatch(spec)
	if m == nil {
		return "", false
	}
	return m[1] + "." + m[2], true
}

// TestGoMinorAgreesEverywhere: go.mod is the declaration point for the Go
// toolchain — CI reads it via `go-version-file`, and the Makefile's
// GOTOOLCHAIN=auto makes every build path fetch exactly what it names. But two
// copies of that version exist outside it, and NOTHING reconciled them:
//
//   - web/go.mod — a no-package boundary module, so nothing compiles against it
//     and a stale `go` line there is completely silent.
//   - docs/EKS-DEPLOYMENT.md — a `FROM golang:<minor>` build stage an operator
//     copies verbatim. Too old and their image cannot build the module at all.
//
// This is the same blind spot the node major had, and it bit the same way: the
// pin sat at 1.26 after 1.27 shipped, with nothing to say so. Dependabot cannot
// help — it does not update the `go` directive (dependabot-core#9527), so the
// toolchain version is ours to move, which makes moving all of it at once a
// thing to assert rather than remember.
func TestGoMinorAgreesEverywhere(t *testing.T) {
	root := repoRoot(t)

	rootSpec := regexp.MustCompile(`(?m)^go\s+(\S+)`).FindStringSubmatch(readFile(t, root, "go.mod"))
	if rootSpec == nil {
		t.Fatal("go.mod has no `go` directive")
	}
	want, ok := goMinor(rootSpec[1])
	if !ok {
		t.Fatalf("go.mod `go %s` — cannot read a major.minor from it", rootSpec[1])
	}

	webSpec := regexp.MustCompile(`(?m)^go\s+(\S+)`).FindStringSubmatch(readFile(t, root, "web/go.mod"))
	if webSpec == nil {
		t.Error("web/go.mod has no `go` directive")
	} else if got, ok := goMinor(webSpec[1]); !ok {
		t.Errorf("web/go.mod `go %s` — cannot read a major.minor from it", webSpec[1])
	} else if got != want {
		t.Errorf("web/go.mod says go %s but go.mod says %s — bump them together (web/go.mod is major.minor only by design, but the minor still has to agree)", got, want)
	}

	const eksDoc = "docs/EKS-DEPLOYMENT.md"
	img := regexp.MustCompile(`FROM golang:(\S+?)(?:\s|$)`).FindAllStringSubmatch(readFile(t, root, eksDoc), -1)
	if len(img) == 0 {
		t.Logf("%s has no `FROM golang:` stage — nothing to check", eksDoc)
	}
	for _, m := range img {
		got, ok := goMinor(m[1])
		if !ok {
			t.Errorf("%s has `FROM golang:%s` — cannot read a major.minor from it", eksDoc, m[1])
			continue
		}
		if got != want {
			t.Errorf("%s builds on `golang:%s` but go.mod says %s — an operator copying that stage gets an image too old to build the module", eksDoc, m[1], want)
		}
	}
}

// TestGolangciLintPinAgreesWithDocs: the golangci-lint binary version is
// declared in ci.yml (TestDuplicatedToolPinsAgree already holds dev-ci.yml to
// it) and then RESTATED in prose in two docs that tell a contributor which
// version to install. Those copies are invisible to every other check, and they
// drift exactly the way you would expect: the v2.12.2 -> v2.13.1 bump that Go
// 1.27 forced updated ONBOARDING.md and missed docs/TESTING.md, which went on
// telling contributors to install the version that cannot lint the tree.
//
// Asserting them is the same rule this file already applies to the node major
// and the Go minor: one declaration point, and anything that must agree with it
// is a test failure rather than something to remember.
func TestGolangciLintPinAgreesWithDocs(t *testing.T) {
	root := repoRoot(t)

	pin := regexp.MustCompile(`golangci-lint-action@v\d+\s+with:\s+(?:#[^\n]*\n\s+)*version:\s*(v[\d.]+)`).
		FindStringSubmatch(readFile(t, root, ".github/workflows/ci.yml"))
	if pin == nil {
		t.Fatal("ci.yml: could not find the golangci-lint-action version pin")
	}
	want := pin[1]

	// Each doc names the version in prose. Match any golangci-lint-adjacent
	// vN.N.N so a stale copy is caught rather than skipped for not matching.
	for _, rel := range []string{"ONBOARDING.md", "docs/TESTING.md"} {
		body := readFile(t, root, rel)
		found := regexp.MustCompile(`golangci-lint\D{0,40}?(v\d+\.\d+\.\d+)`).FindAllStringSubmatch(body, -1)
		if len(found) == 0 {
			t.Errorf("%s names no golangci-lint version; ci.yml pins %s — keep the doc honest or drop the mention", rel, want)
			continue
		}
		for _, m := range found {
			if m[1] != want {
				t.Errorf("%s says golangci-lint %s but ci.yml pins %s — a contributor following the doc installs a binary that cannot lint the tree", rel, m[1], want)
			}
		}
	}
}
