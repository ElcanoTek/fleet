// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package scripts

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
)

// buildSandboxNeeds — every repo path build-sandbox-image.sh sources or reads
// from its own checkout ($REPO_ROOT): each `. "$REPO_ROOT/…"` file (and what
// those source in turn), and — when it falls back to the generic bundle — the
// generic bundle's manifest plus every file of its sandbox build context.
func buildSandboxNeeds(t *testing.T, root string) []string {
	t.Helper()
	sourceRE := regexp.MustCompile(`(?m)^\s*(?:\.|source)\s+"\$REPO_ROOT/([^"]+)"`)
	libSourceRE := regexp.MustCompile(`(?m)^\s*(?:\.|source)\s+"\$[A-Za-z_]+/(lib/[^"]+)"`)
	script := readFile(t, root, "scripts/build-sandbox-image.sh")
	var needs []string
	queue := []string{}
	for _, m := range sourceRE.FindAllStringSubmatch(script, -1) {
		queue = append(queue, m[1])
	}
	if len(queue) == 0 {
		t.Fatal(`build-sandbox-image.sh sources nothing via . "$REPO_ROOT/…" — the test no longer recognizes how it loads its helpers`)
	}
	seen := map[string]bool{}
	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]
		if seen[p] {
			continue
		}
		seen[p] = true
		needs = append(needs, p)
		for _, m := range libSourceRE.FindAllStringSubmatch(readFile(t, root, p), -1) {
			queue = append(queue, "scripts/"+m[1])
		}
	}
	if strings.Contains(script, "$REPO_ROOT/config/default") {
		needs = append(needs, "config/default/manifest.yaml")
		cf := "sandbox/Containerfile"
		if m := regexp.MustCompile(`(?m)^\s+containerfile:\s*"?([^"#\s]+)`).FindStringSubmatch(readFile(t, root, "config/default/manifest.yaml")); m != nil {
			cf = m[1]
		}
		ctx := filepath.Join("config/default", filepath.Dir(cf))
		entries, err := os.ReadDir(filepath.Join(root, ctx))
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if e.Type().IsRegular() {
				needs = append(needs, filepath.ToSlash(filepath.Join(ctx, e.Name())))
			}
		}
	}
	return needs
}

// TestSandboxWorkflowSparseCheckoutsCoverTheScript — the bundle workflows
// sparse-check out fleet into .fleet-core to run build-sandbox-image.sh; a
// file the script sources or reads that the sparse list misses fails the
// bundle's build at runtime, in another repository.
func TestSandboxWorkflowSparseCheckoutsCoverTheScript(t *testing.T) {
	root := repoRoot(t)
	needs := buildSandboxNeeds(t, root)
	for _, wf := range []string{"build-sandbox-image.yml", "publish-sandbox-image.yml"} {
		var doc struct {
			Jobs map[string]struct {
				Steps []struct {
					With map[string]any `yaml:"with"`
				} `yaml:"steps"`
			} `yaml:"jobs"`
		}
		if err := yaml.Unmarshal([]byte(readFile(t, root, ".github/workflows/"+wf)), &doc); err != nil {
			t.Fatalf("%s: %v", wf, err)
		}
		found := false
		for _, j := range doc.Jobs {
			for _, s := range j.Steps {
				if s.With["path"] != ".fleet-core" {
					continue
				}
				list, _ := s.With["sparse-checkout"].(string)
				if list == "" {
					continue // a full checkout covers everything
				}
				found = true
				var patterns []string
				for _, l := range strings.Split(list, "\n") {
					if l = strings.TrimSpace(l); l != "" {
						patterns = append(patterns, l)
					}
				}
				for _, need := range needs {
					ok := false
					for _, p := range patterns {
						if p == need || (strings.HasSuffix(p, "/") && strings.HasPrefix(need, p)) {
							ok = true
						}
					}
					if !ok {
						t.Errorf("%s: the .fleet-core sparse checkout %v misses %s, which build-sandbox-image.sh needs", wf, patterns, need)
					}
				}
			}
		}
		if !found {
			t.Errorf("%s: no sparse .fleet-core checkout found — the test no longer recognizes the workflow", wf)
		}
	}
}
