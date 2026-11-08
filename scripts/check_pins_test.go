// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package scripts

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
)

// Every pinned input of the release images and the workflows must have an
// automated update path (docs/RELEASE-ARTIFACTS.md, "How pins are kept
// current"): Dependabot for what it can read, scripts/update-pins.sh (run by
// pins-update.yml) for the rest. A pin with neither rots silently — that is
// what this test refuses.

// releaseImages are the Containerfiles release.yml builds and publishes.
var releaseImages = []string{"deploy/fleet.Containerfile", "config/default/sandbox/Containerfile"}

type pinRow struct{ name, kind string }

func loadPins(t *testing.T) map[string]pinRow {
	t.Helper()
	f, err := os.Open(filepath.Join(repoRoot(t), "scripts", "lib", "pins.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	rows := map[string]pinRow{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		cols := strings.Split(line, "\t")
		if len(cols) != 7 {
			t.Fatalf("pins.tsv: %q has %d columns, want 7", line, len(cols))
		}
		switch cols[1] {
		case "github", "pypi", "cosign", "codeql":
		default:
			t.Errorf("pins.tsv: %s has unknown kind %q", cols[0], cols[1])
		}
		rows[cols[0]] = pinRow{cols[0], cols[1]}
	}
	return rows
}

type dependabotCfg struct {
	Updates []struct {
		Ecosystem   string   `yaml:"package-ecosystem"`
		Directory   string   `yaml:"directory"`
		Directories []string `yaml:"directories"`
	} `yaml:"updates"`
}

func dependabotDirs(t *testing.T) map[string]map[string]bool {
	t.Helper()
	var cfg dependabotCfg
	if err := yaml.Unmarshal([]byte(readFile(t, repoRoot(t), ".github/dependabot.yml")), &cfg); err != nil {
		t.Fatal(err)
	}
	out := map[string]map[string]bool{}
	for _, u := range cfg.Updates {
		if out[u.Ecosystem] == nil {
			out[u.Ecosystem] = map[string]bool{}
		}
		if u.Directory != "" {
			out[u.Ecosystem][u.Directory] = true
		}
		for _, d := range u.Directories {
			out[u.Ecosystem][d] = true
		}
	}
	return out
}

func workflowFiles(t *testing.T) map[string]string {
	t.Helper()
	dir := filepath.Join(repoRoot(t), ".github", "workflows")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]string{}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".yml") {
			out[e.Name()] = readFile(t, filepath.Join(repoRoot(t), ".github", "workflows"), e.Name())
		}
	}
	return out
}

func TestEveryWorkflowPinHasAnUpdater(t *testing.T) {
	pins := loadPins(t)
	dep := dependabotDirs(t)
	if !dep["github-actions"]["/"] {
		t.Error(`dependabot.yml has no github-actions entry for "/" — the SHA-pinned actions in every workflow would never move`)
	}
	versionRE := regexp.MustCompile(`(?m)^\s+([A-Z][A-Z0-9_]*)_VERSION:\s*'`)
	shaRE := regexp.MustCompile(`(?m)^\s+([A-Z][A-Z0-9_]*)_SHA256:\s*'`)
	downloadRE := regexp.MustCompile(`releases/download/\S*`)
	seen := map[string]bool{}
	for name, body := range workflowFiles(t) {
		for _, m := range versionRE.FindAllStringSubmatch(body, -1) {
			seen[m[1]] = true
			if _, ok := pins[m[1]]; !ok {
				t.Errorf("%s pins %s_VERSION but scripts/lib/pins.tsv has no %s row, so nothing updates it", name, m[1], m[1])
			}
		}
		for _, m := range shaRE.FindAllStringSubmatch(body, -1) {
			if p, ok := pins[m[1]]; !ok || p.kind != "github" {
				t.Errorf("%s pins %s_SHA256 without a github row in pins.tsv to move it with its version", name, m[1])
			}
		}
		for _, url := range downloadRE.FindAllString(body, -1) {
			ok := false
			for pin := range pins {
				if strings.Contains(url, "${"+pin+"_VERSION}") {
					ok = true
				}
			}
			// release.yml's cosign install reads internal/release/cosign.lock.
			if strings.HasPrefix(url, "releases/download/${ver}/cosign-linux-") && strings.Contains(body, "sigstore/cosign/releases/download/${ver}") && strings.Contains(body, "internal/release/cosign.lock") {
				ok = true
			}
			if !ok {
				t.Errorf("%s downloads %q, which no pins.tsv row versions — add one so it has an updater", name, url)
			}
		}
	}
	for name, p := range pins {
		if p.kind != "cosign" && p.kind != "codeql" && !seen[name] {
			t.Errorf("pins.tsv lists %s but no workflow pins %s_VERSION — drop the stale row", name, name)
		}
	}
	if _, err := os.Stat(filepath.Join(repoRoot(t), "internal", "release", "cosign.lock")); err == nil {
		if p, ok := pins["COSIGN"]; !ok || p.kind != "cosign" {
			t.Error("internal/release/cosign.lock has no cosign row in pins.tsv")
		}
	}
	if !strings.Contains(readFile(t, repoRoot(t), ".github/workflows/pins-update.yml"), "scripts/update-pins.sh") {
		t.Error("pins-update.yml must run scripts/update-pins.sh")
	}
}

func TestReleaseImagePinsHaveUpdaters(t *testing.T) {
	root := repoRoot(t)
	dep := dependabotDirs(t)
	fromRE := regexp.MustCompile(`(?m)^FROM\s+(\S+)`)
	digestRE := regexp.MustCompile(`^[^@\s]+:[^@\s:]+@sha256:[0-9a-f]{64}$`)
	pipRE := regexp.MustCompile(`pip3? install(?:[^\n]*\\\n)*[^\n]*`)
	commentRE := regexp.MustCompile(`(?m)^\s*#.*$`)
	for _, cf := range releaseImages {
		body := commentRE.ReplaceAllString(readFile(t, root, cf), "")
		dir := "/" + filepath.Dir(cf)
		froms := fromRE.FindAllStringSubmatch(body, -1)
		if len(froms) == 0 {
			t.Errorf("%s has no FROM", cf)
		}
		for _, m := range froms {
			if !digestRE.MatchString(m[1]) {
				t.Errorf("%s: FROM %s is not pinned as <image>:<tag>@sha256:<digest> (Dependabot bumps tag and digest only in that shape)", cf, m[1])
			}
		}
		if !dep["docker"][dir] {
			t.Errorf("%s: no Dependabot docker entry for %s, so its base digest never moves", cf, dir)
		}
		for _, install := range pipRE.FindAllString(body, -1) {
			if !strings.Contains(install, "--require-hashes") || !strings.Contains(install, "--no-deps") || !strings.Contains(install, "-r ") {
				t.Errorf("%s: a pip install that is not `--require-hashes --no-deps -r <lock>`: %q", cf, install)
			}
		}
		for _, bad := range []string{"curl ", "wget ", "npm install", "npx "} {
			if strings.Contains(body, bad) {
				t.Errorf("%s fetches with %q, which no updater tracks", cf, strings.TrimSpace(bad))
			}
		}
		if strings.Contains(body, "pip") && strings.Contains(body, "install") && pipRE.MatchString(body) {
			lockDir := filepath.Dir(cf)
			if strings.Contains(body, "deploy/python/requirements.txt") {
				lockDir = "deploy/python"
			}
			lock := readFile(t, root, filepath.Join(lockDir, "requirements.txt"))
			if _, err := os.Stat(filepath.Join(root, lockDir, "requirements.in")); err != nil {
				t.Errorf("%s: no requirements.in beside the lock — Dependabot re-compiles the lock from it", lockDir)
			}
			pinned := regexp.MustCompile(`(?m)^[A-Za-z0-9._-]+==\S+ \\$`).FindAllString(lock, -1)
			if len(pinned) == 0 || strings.Count(lock, "--hash=sha256:") < len(pinned) {
				t.Errorf("%s/requirements.txt: every pinned line needs --hash entries (pip-compile --generate-hashes)", lockDir)
			}
			if !dep["pip"]["/"+lockDir] {
				t.Errorf("no Dependabot pip entry for /%s, so its lock never moves", lockDir)
			}
		}
	}
	doc := readFile(t, root, "docs/RELEASE-ARTIFACTS.md")
	i := strings.Index(doc, "## How pins are kept current")
	if i < 0 {
		t.Fatal(`docs/RELEASE-ARTIFACTS.md has no "## How pins are kept current" section`)
	}
	section := doc[i:]
	if j := strings.Index(section[3:], "\n## "); j >= 0 {
		section = section[:j+3]
	}
	for name := range loadPins(t) {
		if !strings.Contains(section, "`"+name+"`") {
			t.Errorf(`"How pins are kept current" does not list %s`, name)
		}
	}
	for _, want := range append([]string{"deploy/python/requirements.txt", "config/default/sandbox/requirements.txt"}, releaseImages...) {
		if !strings.Contains(section, want) {
			t.Errorf(`"How pins are kept current" does not list %s`, want)
		}
	}
}
