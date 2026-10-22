// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package admincli

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// ── the upstream node fallback (fleet_node_tarball_install, ADR-0078) ───────
//
// When the distro has no nodejs<major> stream, bootstrap and `doctor --node`
// install node from nodejs.org as root. The whole safety argument is "nothing
// is unpacked unless node's release key signed its sha256", so these tests
// serve a file:// dist signed by a throwaway key and check that every way of
// breaking that chain installs NOTHING — not just that the happy path works.

// nodeDistArch mirrors fleet__node_dist_arch for the runner's own arch.
func nodeDistArch(t *testing.T) string {
	t.Helper()
	switch runtime.GOARCH {
	case "amd64":
		return "x64"
	case "arm64":
		return "arm64"
	default:
		t.Skipf("no upstream node build name for GOARCH=%s", runtime.GOARCH)
		return ""
	}
}

// testGPG is a throwaway signing identity in its own short-pathed GNUPGHOME
// (gpg-agent's socket path has a ~108-byte limit that t.TempDir can exceed).
type testGPG struct{ home string }

func newTestGPG(t *testing.T, uid string) testGPG {
	t.Helper()
	for _, tool := range []string{"gpg", "gpgv", "tar", "sha256sum", "curl"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available; skipping the node tarball install tests", tool)
		}
	}
	home, err := os.MkdirTemp("", "g")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	g := testGPG{home: home}
	t.Cleanup(func() {
		kill := exec.Command("gpgconf", "--kill", "gpg-agent")
		kill.Env = append(os.Environ(), "GNUPGHOME="+home)
		_ = kill.Run()
		_ = os.RemoveAll(home)
	})
	g.run(t, "--batch", "--passphrase", "", "--quick-gen-key", uid, "ed25519", "sign", "never")
	return g
}

func (g testGPG) run(t *testing.T, args ...string) []byte {
	t.Helper()
	cmd := exec.Command("gpg", args...)
	cmd.Env = append(os.Environ(), "GNUPGHOME="+g.home)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gpg %v: %v\n%s", args, err, out)
	}
	return out
}

// exportKeys writes this identity's armored public key, the shape of
// scripts/lib/node-release-keys.asc.
func (g testGPG) exportKeys(t *testing.T, dest string) {
	t.Helper()
	if err := os.WriteFile(dest, g.run(t, "--armor", "--export"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (g testGPG) clearsign(t *testing.T, src, dest string) {
	t.Helper()
	_ = os.Remove(dest)
	g.run(t, "--batch", "--yes", "--pinentry-mode", "loopback", "--passphrase", "",
		"--clearsign", "-o", dest, src)
}

// fakeNodeRelease writes node-vVER-linux-ARCH.tar.gz into dir, laid out like
// upstream's: bin/node, and bin/npm|npx as links into lib/node_modules/npm.
// Returns the tarball's file name and sha256.
func fakeNodeRelease(t *testing.T, dir, ver, arch string) (string, string) {
	t.Helper()
	name := "node-v" + ver + "-linux-" + arch
	stage := t.TempDir()
	root := filepath.Join(stage, name)
	cliDir := filepath.Join(root, "lib", "node_modules", "npm", "bin")
	for _, d := range []string{filepath.Join(root, "bin"), cliDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	node := "#!/bin/sh\nif [ \"$1\" = \"-v\" ]; then echo v" + ver + "; exit 0; fi\nexec /bin/sh \"$@\"\n"
	if err := os.WriteFile(filepath.Join(root, "bin", "node"), []byte(node), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, cli := range []string{"npm", "npx"} {
		js := filepath.Join(cliDir, cli+"-cli.js")
		if err := os.WriteFile(js, []byte("#!/usr/bin/env node\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink("../lib/node_modules/npm/bin/"+cli+"-cli.js", filepath.Join(root, "bin", cli)); err != nil {
			t.Fatal(err)
		}
	}
	file := name + ".tar.gz"
	if out, err := exec.Command("tar", "-czf", filepath.Join(dir, file), "-C", stage, name).CombinedOutput(); err != nil {
		t.Fatalf("tar: %v\n%s", err, out)
	}
	raw, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(raw)
	return file, hex.EncodeToString(sum[:])
}

// nodeDist is a file:// stand-in for https://nodejs.org/dist.
type nodeDist struct {
	root   string // what FLEET_NODE_DIST_URL points at
	latest string // root/latest-v26.x
}

func newNodeDist(t *testing.T) nodeDist {
	t.Helper()
	root := t.TempDir()
	latest := filepath.Join(root, "latest-v26.x")
	if err := os.MkdirAll(latest, 0o755); err != nil {
		t.Fatal(err)
	}
	return nodeDist{root: root, latest: latest}
}

// publish writes SHASUMS256.txt for the given lines and has g clearsign it.
func (d nodeDist) publish(t *testing.T, g testGPG, lines ...string) {
	t.Helper()
	sums := filepath.Join(d.latest, "SHASUMS256.txt")
	if err := os.WriteFile(sums, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g.clearsign(t, sums, sums+".asc")
}

type tarballEnv struct {
	keys, prefix, bindir string
}

func newTarballEnv(t *testing.T, g testGPG) tarballEnv {
	t.Helper()
	dir := t.TempDir()
	e := tarballEnv{
		keys:   filepath.Join(dir, "keys.asc"),
		prefix: filepath.Join(dir, "fleet-node"),
		bindir: filepath.Join(dir, "bin"),
	}
	g.exportKeys(t, e.keys)
	return e
}

// install runs fleet_node_tarball_install 26 against dist and returns its
// combined output.
func (e tarballEnv) install(t *testing.T, d nodeDist) (string, error) {
	t.Helper()
	lib := filepath.Join(repoRootFromTest(t), "scripts", "lib", "node-version.sh")
	cmd := exec.Command("bash", "-c", "set -euo pipefail\n. "+lib+"\nfleet_node_tarball_install 26")
	cmd.Env = append(os.Environ(),
		"FLEET_NODE_DIST_URL=file://"+d.root,
		"FLEET_NODE_RELEASE_KEYS="+e.keys,
		"FLEET_NODE_TARBALL_PREFIX="+e.prefix,
		"FLEET_NODE_TARBALL_BINDIR="+e.bindir,
	)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// assertNothingInstalled is the refusal contract: no tree, no links.
func (e tarballEnv) assertNothingInstalled(t *testing.T) {
	t.Helper()
	for _, link := range []string{"node-26", "npm-26", "npx-26"} {
		if _, err := os.Lstat(filepath.Join(e.bindir, link)); err == nil {
			t.Errorf("%s was linked despite the refusal", link)
		}
	}
	entries, _ := os.ReadDir(e.prefix)
	for _, ent := range entries {
		t.Errorf("%s/%s was unpacked despite the refusal", e.prefix, ent.Name())
	}
}

func TestNodeTarballInstallLinksTheSignedRelease(t *testing.T) {
	arch := nodeDistArch(t)
	g := newTestGPG(t, "node release (test) <release@example.invalid>")
	d := newNodeDist(t)
	file, sum := fakeNodeRelease(t, d.latest, "26.1.0", arch)
	d.publish(t, g, sum+"  "+file)
	e := newTarballEnv(t, g)

	out, err := e.install(t, d)
	if err != nil {
		t.Fatalf("install failed: %v\n%s", err, out)
	}
	if want := filepath.Join(e.bindir, "node-26"); strings.TrimSpace(out) != want {
		t.Errorf("install printed %q, want the node-26 link %q", out, want)
	}
	ver, err := exec.Command(filepath.Join(e.bindir, "node-26"), "-v").Output()
	if err != nil || strings.TrimSpace(string(ver)) != "v26.1.0" {
		t.Fatalf("node-26 -v = %q, %v; want v26.1.0", ver, err)
	}

	// The links must be what the rest of node-version.sh already understands:
	// npm-26 beside node-26 resolving to npm-cli.js, and the tree recognised
	// as ours to refresh.
	resolved, err := sourceNodeLib(t, e.bindir, `fleet_resolve_npm_cli "$1/node-26"`)
	if err != nil || !strings.HasSuffix(resolved, "/lib/node_modules/npm/bin/npm-cli.js") {
		t.Errorf("fleet_resolve_npm_cli on the tarball's node-26 = %q, %v; want its npm-cli.js", resolved, err)
	}
	if out, err := sourceNodeLib(t, e.bindir,
		`FLEET_NODE_TARBALL_PREFIX=`+e.prefix+` fleet_node_is_tarball_install "$1/node-26"`); err != nil {
		t.Errorf("fleet_node_is_tarball_install did not recognise its own install: %v\n%s", err, out)
	}
	if _, err := sourceNodeLib(t, e.bindir,
		`FLEET_NODE_TARBALL_PREFIX=`+e.prefix+` fleet_node_is_tarball_install /bin/sh`); err == nil {
		t.Error("fleet_node_is_tarball_install claimed /bin/sh — a distro binary is dnf's, not ours to replace")
	}

	// Idempotent: with the newest release already unpacked, a re-run re-links
	// without fetching the tarball (it is gone from the dist now).
	if err := os.Remove(filepath.Join(d.latest, file)); err != nil {
		t.Fatal(err)
	}
	if out, err := e.install(t, d); err != nil {
		t.Fatalf("re-run with the release already installed failed: %v\n%s", err, out)
	}

	// A newer patch release is picked up and the links move to it — the job
	// dnf does for an RPM install, which `doctor --node` relies on.
	file2, sum2 := fakeNodeRelease(t, d.latest, "26.2.0", arch)
	d.publish(t, g, sum2+"  "+file2)
	if out, err := e.install(t, d); err != nil {
		t.Fatalf("refresh to 26.2.0 failed: %v\n%s", err, out)
	}
	ver, _ = exec.Command(filepath.Join(e.bindir, "node-26"), "-v").Output()
	if strings.TrimSpace(string(ver)) != "v26.2.0" {
		t.Errorf("after the refresh node-26 -v = %q, want v26.2.0", ver)
	}
}

func TestNodeTarballInstallRefusesATamperedTarball(t *testing.T) {
	arch := nodeDistArch(t)
	g := newTestGPG(t, "node release (test) <release@example.invalid>")
	d := newNodeDist(t)
	file, _ := fakeNodeRelease(t, d.latest, "26.1.0", arch)
	// Signed, but over a different sha256 than the bytes served.
	d.publish(t, g, strings.Repeat("ab", 32)+"  "+file)
	e := newTarballEnv(t, g)

	out, err := e.install(t, d)
	if err == nil {
		t.Fatalf("installed a tarball whose sha256 does not match the signed line:\n%s", out)
	}
	if !strings.Contains(out, "does not match its signed sha256") {
		t.Errorf("refusal does not say why:\n%s", out)
	}
	e.assertNothingInstalled(t)
}

func TestNodeTarballInstallRefusesASignatureFromAnUntrustedKey(t *testing.T) {
	arch := nodeDistArch(t)
	trusted := newTestGPG(t, "node release (test) <release@example.invalid>")
	attacker := newTestGPG(t, "not a node releaser <mallory@example.invalid>")
	d := newNodeDist(t)
	file, sum := fakeNodeRelease(t, d.latest, "26.1.0", arch)
	d.publish(t, attacker, sum+"  "+file) // correct sums, wrong signer
	e := newTarballEnv(t, trusted)

	out, err := e.install(t, d)
	if err == nil {
		t.Fatalf("installed a release signed by a key outside the vendored keyring:\n%s", out)
	}
	if !strings.Contains(out, "NOT signed by a node release key") {
		t.Errorf("refusal does not say why:\n%s", out)
	}
	e.assertNothingInstalled(t)
}

// Text outside the clearsigned block is not covered by the signature, so the
// version and hash must come from gpgv's --output, never from the raw file. A
// line smuggled in front of the signed block names a different tarball whose
// sha256 matches its bytes; it must not be the one installed.
func TestNodeTarballInstallReadsOnlyTheSignedText(t *testing.T) {
	arch := nodeDistArch(t)
	g := newTestGPG(t, "node release (test) <release@example.invalid>")
	d := newNodeDist(t)
	file, sum := fakeNodeRelease(t, d.latest, "26.1.0", arch)
	evilFile, evilSum := fakeNodeRelease(t, d.latest, "26.9.9", arch)
	d.publish(t, g, sum+"  "+file)
	asc := filepath.Join(d.latest, "SHASUMS256.txt.asc")
	signed, err := os.ReadFile(asc)
	if err != nil {
		t.Fatal(err)
	}
	smuggled := evilSum + "  " + evilFile + "\n" + string(signed)
	if err := os.WriteFile(asc, []byte(smuggled), 0o644); err != nil {
		t.Fatal(err)
	}
	e := newTarballEnv(t, g)

	out, err := e.install(t, d)
	if err != nil {
		t.Fatalf("install failed: %v\n%s", err, out)
	}
	ver, _ := exec.Command(filepath.Join(e.bindir, "node-26"), "-v").Output()
	if got := strings.TrimSpace(string(ver)); got != "v26.1.0" {
		t.Fatalf("node-26 is %q — the unsigned line steered the install; want the signed v26.1.0", got)
	}
}

// An older release is still validly signed, so a replayed or stale dist must
// not walk an installed newer patch back: the links stay where they are.
func TestNodeTarballInstallRefusesADowngrade(t *testing.T) {
	arch := nodeDistArch(t)
	g := newTestGPG(t, "node release (test) <release@example.invalid>")
	d := newNodeDist(t)
	file, sum := fakeNodeRelease(t, d.latest, "26.2.0", arch)
	d.publish(t, g, sum+"  "+file)
	e := newTarballEnv(t, g)
	if out, err := e.install(t, d); err != nil {
		t.Fatalf("install of 26.2.0 failed: %v\n%s", err, out)
	}

	old, oldSum := fakeNodeRelease(t, d.latest, "26.1.0", arch)
	d.publish(t, g, oldSum+"  "+old)
	out, err := e.install(t, d)
	if err == nil {
		t.Fatalf("installed an older signed release over 26.2.0:\n%s", out)
	}
	if !strings.Contains(out, "refusing to downgrade") {
		t.Errorf("refusal does not say why:\n%s", out)
	}
	ver, _ := exec.Command(filepath.Join(e.bindir, "node-26"), "-v").Output()
	if got := strings.TrimSpace(string(ver)); got != "v26.2.0" {
		t.Errorf("node-26 is %q after the refused downgrade, want v26.2.0", got)
	}
}

// The vendored keyring is the installer's whole trust root, and the .list is
// what a reviewer and the weekly drift check read. gpgv trusts every PRIMARY
// key in the .asc, so the two must agree exactly: an extra primary would be a
// signer nobody reviewed. (Subkeys are part of their primary and allowed.)
func TestVendoredNodeReleaseKeysMatchTheirList(t *testing.T) {
	if _, err := exec.LookPath("gpg"); err != nil {
		t.Skip("gpg not available")
	}
	root := repoRootFromTest(t)
	home, err := os.MkdirTemp("", "g")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(home) })
	asc, err := os.Open(filepath.Join(root, "scripts", "lib", "node-release-keys.asc"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = asc.Close() }()
	cmd := exec.Command("gpg", "--batch", "--show-keys", "--with-colons")
	cmd.Stdin = asc
	cmd.Env = append(os.Environ(), "GNUPGHOME="+home)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("gpg --show-keys: %v", err)
	}
	have := map[string]bool{}
	afterPub := false
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Split(line, ":")
		switch {
		case f[0] == "pub":
			afterPub = true
		case f[0] == "fpr" && afterPub && len(f) > 9:
			have[f[9]] = true
			afterPub = false
		case f[0] == "sub":
			afterPub = false
		}
	}
	raw, err := os.ReadFile(filepath.Join(root, "scripts", "lib", "node-release-keys.list"))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{}
	for _, fp := range strings.Fields(string(raw)) {
		want[fp] = true
		if !have[fp] {
			t.Errorf("%s is in node-release-keys.list but not in node-release-keys.asc", fp)
		}
	}
	for fp := range have {
		if !want[fp] {
			t.Errorf("node-release-keys.asc carries primary key %s, which node-release-keys.list does not name", fp)
		}
	}
	if len(want) == 0 {
		t.Error("node-release-keys.list is empty")
	}
}
