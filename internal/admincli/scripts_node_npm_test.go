// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package admincli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// ── the npm interpreter pin ─────────────────────────────────────────────────
//
// scripts/lib/node-version.sh claims the web tier "will build+run" on the
// interpreter it resolves. The build half of that was false on Fedora TWICE, for
// the same reason each time: a PATH edit that looks like it pins the interpreter
// and does not.
//
//  1. Prefixing the resolved binary's DIRECTORY — /usr/bin holds both `node-24`
//     and the default stream's `node`, so the bare name still resolved to 22.
//     Fixed by a private shim directory holding a `node` symlink.
//
//  2. The `node` symlink alone — Fedora's spec rewrites npm's shebang to an
//     ABSOLUTE `#!/usr/bin/node-<major>` (it has to: the streams are
//     parallel-installable, so a relative `env node` shebang would make npm-22
//     run under whichever stream is the default). So `npm ci` stayed on 22 while
//     `fleet update` reported 24, and the only symptom was npm's own
//     EBADENGINE warning against web/package.json's `"node": ">=24"`:
//
//     npm warn EBADENGINE required: { node: '>=24' }
//     npm warn EBADENGINE current:  { node: 'v22.23.1', npm: '10.9.8' }
//
// fedoraNodeLayout builds a stand-in for that exact layout, because these tests
// cannot assume the runner has parallel node streams (or dnf) — and the previous
// round of this bug shipped precisely because the layout was taken from a
// package list rather than reproduced.
//
// It returns the directory holding the fake bindir. The fake `node-NN` runs
// /bin/sh on whatever script it is handed, which is enough for the fake
// npm-cli.js files to report which interpreter their shebang selected — the one
// fact under test.
func fedoraNodeLayout(t *testing.T, majors []string, defaultMajor string, withVersionedNpm map[string]bool) string {
	t.Helper()
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, m := range majors {
		nodeBin := filepath.Join(bin, "node-"+m)
		node := "#!/bin/sh\n" +
			"if [ \"$1\" = \"-v\" ]; then echo v" + m + ".9.9; exit 0; fi\n" +
			"exec /bin/sh \"$@\"\n"
		if err := os.WriteFile(nodeBin, []byte(node), 0o755); err != nil {
			t.Fatal(err)
		}
		if !withVersionedNpm[m] {
			continue
		}
		// Fedora gives each stream its own private sitelib and points a
		// versioned bindir symlink at the npm-cli.js inside it.
		cliDir := filepath.Join(root, "lib", "node_modules_"+m, "npm", "bin")
		if err := os.MkdirAll(cliDir, 0o755); err != nil {
			t.Fatal(err)
		}
		cli := filepath.Join(cliDir, "npm-cli.js")
		// The absolute shebang is the whole point of the fixture: it is what no
		// PATH edit can override.
		body := "#!" + nodeBin + "\n" +
			"echo '{ \"fleet-web\": \"0.1.0\", \"npm\": \"" + m + ".0.0\", \"node\": \"" + m + ".9.9\" }'\n"
		if err := os.WriteFile(cli, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(cli, filepath.Join(bin, "npm-"+m)); err != nil {
			t.Fatal(err)
		}
	}
	// The unversioned names point at the DEFAULT stream, as Fedora's
	// alternatives do — this is the trap both bugs fell into.
	if err := os.Symlink(filepath.Join(bin, "node-"+defaultMajor), filepath.Join(bin, "node")); err != nil {
		t.Fatal(err)
	}
	if withVersionedNpm[defaultMajor] {
		if err := os.Symlink(filepath.Join(bin, "npm-"+defaultMajor), filepath.Join(bin, "npm")); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// sourceNodeLib runs a bash snippet with scripts/lib/node-version.sh sourced,
// returning combined output. The snippet gets the fixture root as $1.
func sourceNodeLib(t *testing.T, fixtureRoot, snippet string) (string, error) {
	t.Helper()
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("bash not available; skipping node-version.sh unit test")
	}
	lib := filepath.Join(repoRootFromTest(t), "scripts", "lib", "node-version.sh")
	cmd := exec.Command("bash", "-c", ". "+lib+"\nset -u\n"+snippet, "_", fixtureRoot)
	cmd.Env = append(os.Environ(), "TERM=dumb")
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// TestNodeBuildPathPinsNpmToTheResolvedInterpreter — the regression test for the
// EBADENGINE build. Under the PATH fleet_node_build_path returns, `npm` must run
// under the RESOLVED interpreter, not the one its shebang names. The same
// fixture is first used to prove the old behaviour still fails, so a fixture
// that quietly stopped reproducing the layout cannot make this test vacuous.
func TestNodeBuildPathPinsNpmToTheResolvedInterpreter(t *testing.T) {
	root := fedoraNodeLayout(t, []string{"22", "24"}, "22", map[string]bool{"22": true, "24": true})
	out, err := sourceNodeLib(t, root, `
bin="$1/bin"
nb="$bin/node-24"
echo "cli=$(fleet_resolve_npm_cli "$nb")"
# The pre-fix behaviour: prefixing the bindir (which holds BOTH streams) and
# relying on npm's shebang. Must still land on 22, or the fixture is not
# reproducing the layout this test exists for.
echo "olddir=$(PATH="$bin:$PATH" npm)"
p="$(fleet_node_build_path "$nb")"
echo "shim=${p%%:*}"
echo "node=$(PATH="$p" node -v)"
echo "npm=$(PATH="$p" npm)"
echo "readback=$(fleet_npm_node_version "$p")"
echo "major=$(fleet_node_version_major "$(fleet_npm_node_version "$p")")"
fleet_node_build_path_cleanup "$p" "$nb"
[ -d "${p%%:*}" ] && echo "cleanup=LEAKED" || echo "cleanup=gone"
`)
	if err != nil {
		t.Fatalf("snippet failed: %v\n--- output ---\n%s", err, out)
	}
	if !strings.Contains(out, "node_modules_24/npm/bin/npm-cli.js") {
		t.Errorf("fleet_resolve_npm_cli must pick the npm belonging to node-24\n--- output ---\n%s", out)
	}
	// The fixture must still reproduce the bug via the old mechanism.
	if !strings.Contains(out, `olddir={ "fleet-web": "0.1.0", "npm": "22.0.0", "node": "22.9.9" }`) {
		t.Fatalf("fixture no longer reproduces the parallel-stream trap (a bindir prefix must still land on 22)\n--- output ---\n%s", out)
	}
	for _, want := range []string{
		"node=v24.9.9",
		`"node": "24.9.9"`,
		"readback=v24.9.9",
		"major=24",
		"cleanup=gone",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("build PATH must pin npm to node-24: missing %q\n--- output ---\n%s", want, out)
		}
	}
	if strings.Contains(out, `"npm": "22.0.0", "node": "22.9.9" }`+"\nreadback") {
		t.Errorf("npm still ran under node 22 under the build PATH\n--- output ---\n%s", out)
	}
}

// TestResolveNpmCliRefusesTheDefaultStreamsNpm — a box with nodejs24 and no
// nodejs24-npm is a real state (Fedora's `nodejs<major>` does not pull npm).
// Falling back to the unversioned `npm` there would return the DEFAULT stream's
// npm, which is exactly the wrong answer: its shebang pins it to the old major.
// The refusal is what lets update/doctor name the one package that is missing.
func TestResolveNpmCliRefusesTheDefaultStreamsNpm(t *testing.T) {
	root := fedoraNodeLayout(t, []string{"22", "24"}, "22", map[string]bool{"22": true})
	out, err := sourceNodeLib(t, root, `
nb="$1/bin/node-24"
if cli="$(fleet_resolve_npm_cli "$nb")"; then
  echo "resolved=$cli"
else
  echo "refused"
fi
`)
	if err != nil {
		t.Fatalf("snippet failed: %v\n--- output ---\n%s", err, out)
	}
	if !strings.Contains(out, "refused") {
		t.Errorf("fleet_resolve_npm_cli must refuse rather than return the default stream's npm\n--- output ---\n%s", out)
	}
	if strings.Contains(out, "node_modules_22") {
		t.Errorf("fleet_resolve_npm_cli returned the node-22 npm for node-24\n--- output ---\n%s", out)
	}
}

// TestNodeBuildPathCleanupOnlyRemovesItsOwnShim — the shim sits at the head of
// root's PATH during a build, so its removal path has to be as narrow as its
// creation. A first PATH entry that is not one of ours must survive.
func TestNodeBuildPathCleanupOnlyRemovesItsOwnShim(t *testing.T) {
	root := fedoraNodeLayout(t, []string{"24"}, "24", map[string]bool{"24": true})
	out, err := sourceNodeLib(t, root, `
nb="$1/bin/node-24"
# A directory that merely looks like a PATH entry, with a file in it.
foreign="$(mktemp -d)"
: >"$foreign/keep-me"
fleet_node_build_path_cleanup "$foreign:$PATH" "$nb"
[ -f "$foreign/keep-me" ] && echo "foreign=kept" || echo "foreign=DELETED"
# A real shim with an extra file dropped in must also be left alone.
p="$(fleet_node_build_path "$nb")"
shim="${p%%:*}"
: >"$shim/unexpected"
fleet_node_build_path_cleanup "$p" "$nb"
[ -d "$shim" ] && echo "dirty=kept" || echo "dirty=DELETED"
rm -rf "$shim" "$foreign"
`)
	if err != nil {
		t.Fatalf("snippet failed: %v\n--- output ---\n%s", err, out)
	}
	for _, want := range []string{"foreign=kept", "dirty=kept"} {
		if !strings.Contains(out, want) {
			t.Errorf("cleanup removed a directory it did not create: missing %q\n--- output ---\n%s", want, out)
		}
	}
}

// TestUpdateGatesNpmSeparatelyFromNode — "a node 24 is installed" does not imply
// "npm will build on node 24", so the gate has to probe both and the build has to
// read back which interpreter npm actually used. Asserted against the script text
// because the failure needs parallel node streams to reproduce, which a CI runner
// does not have.
func TestUpdateGatesNpmSeparatelyFromNode(t *testing.T) {
	root := repoRootFromTest(t)
	body, err := os.ReadFile(filepath.Join(root, "scripts", "update.sh"))
	if err != nil {
		t.Fatal(err)
	}
	script := string(body)
	for _, want := range []string{
		// The probe answers both questions, with its own exit code for npm so
		// the refusal can name the right missing package.
		`npm_cli_resolved="$(fleet_resolve_npm_cli "$node_bin_resolved")"`,
		"· 3 a versioned node resolved but its npm did not",
		`if (( _np_rc == 3 )); then`,
		"nodejs${node_major_want}-npm",
		// …and only for a versioned interpreter. A single-node layout whose npm
		// this probe cannot read is not a blocker: there npm's shebang really is
		// the relative `env node`, so the `node` pin covers it, and refusing
		// would invent a blocker out of an unread file.
		`if [[ -z "$npm_cli_resolved" && "${node_bin_resolved##*/}" =~ ^node-[0-9]+$ ]]; then`,
		// The gate's promise, stated as two claims because it is two claims.
		`ok "web tier will build with ${npm_cli_resolved}`,
		// The build reads the interpreter back from npm, not from the symlink
		// we ourselves put on PATH.
		`_build_node="$(fleet_npm_node_version "$_build_path" "$SRC_DIR/web" || true)"`,
		`refusing to build the web tier with an npm pinned to an unsupported node`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("update.sh must contain %q", want)
		}
	}
	// The old read-back asked the one component that was already correct.
	if strings.Contains(script, `_build_node="$(PATH="$_build_path" node -v`) {
		t.Error("update.sh still reports the build interpreter from `node -v` under the build PATH, which cannot see npm's shebang")
	}
}

// TestBootstrapAndDoctorPinNpmToo — one implementation of "which node?" is the
// whole point of scripts/lib/node-version.sh, and the same applies to "which
// npm?": a fix that reached only update.sh would leave a fresh provision and
// every doctor run on the old behaviour.
func TestBootstrapAndDoctorPinNpmToo(t *testing.T) {
	root := repoRootFromTest(t)
	for script, wants := range map[string][]string{
		"bootstrap.sh": {
			`if npm_cli="$(fleet_resolve_npm_cli "$node_bin")"; then`,
			`elif [[ "${node_bin##*/}" =~ ^node-[0-9]+$ ]]; then`,
			`build_node="$(fleet_npm_node_version "$build_path" "$web_src" || true)"`,
			`ok "web app built on ${build_node}"`,
			"skipping the web tier rather than building it with an npm pinned to an unsupported node.",
		},
		"doctor.sh": {
			`npm_cli="$(fleet_resolve_npm_cli "$node_bin" || true)"`,
			`fail "no npm belongs to ${node_bin} — install nodejs${NODE_FLOOR}-npm`,
			`fixed "installed the npm that belongs to ${node_bin}`,
			// The scoped --node exit accounts for it too: `fleet update --check`
			// turns that exit code into its own.
			`printf '%s✗ doctor --node: node %s at %s, but no npm belongs to it`,
		},
	} {
		body, err := os.ReadFile(filepath.Join(root, "scripts", script))
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range wants {
			if !strings.Contains(string(body), want) {
				t.Errorf("%s must contain %q", script, want)
			}
		}
	}
	// bootstrap's old success line reported the symlink it had just made.
	body, err := os.ReadFile(filepath.Join(root, "scripts", "bootstrap.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), `ok "web app built on $(PATH="$build_path" node -v`) {
		t.Error("bootstrap.sh still reports the build interpreter from `node -v` under the build PATH")
	}
}
