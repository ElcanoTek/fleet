// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package scripts

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestCatalogSmokeFixturesMatchWorkflow — an api_key fixture in the nightly
// catalog smoke has two halves: a row in catalogKeyFixtures
// (internal/remotemcp/catalog_live_test.go), which reads
// FLEET_CATALOG_KEY_<ENTRY> from the environment, and a line in the `env:`
// block of .github/workflows/mcp-catalog-smoke.yml that forwards the
// repository secret of the same name. A row without its env line skips every
// night with the same "not set" message as an unarmed fixture, so nobody
// notices; an env line without its row forwards a secret nothing reads. Pin
// the two together the way check_action_pins_test.go pins other workflow
// facts: the invariant is a test, not a review habit.
func TestCatalogSmokeFixturesMatchWorkflow(t *testing.T) {
	root := repoRoot(t)
	goSrc := readFile(t, root, filepath.Join("internal", "remotemcp", "catalog_live_test.go"))
	workflow := readFile(t, root, filepath.Join(".github", "workflows", "mcp-catalog-smoke.yml"))

	fixtures := map[string]bool{}
	for _, m := range regexp.MustCompile(`\{Entry:\s*"([a-z0-9-]+)"`).FindAllStringSubmatch(goSrc, -1) {
		fixtures["FLEET_CATALOG_KEY_"+strings.ToUpper(strings.ReplaceAll(m[1], "-", "_"))] = true
	}
	if len(fixtures) == 0 {
		t.Fatal("no catalogKeyFixtures rows found; the regexp or the table moved")
	}

	forwarded := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^\s*(FLEET_CATALOG_KEY_[A-Z0-9_]+):\s*\$\{\{\s*secrets\.(FLEET_CATALOG_KEY_[A-Z0-9_]+)\s*\}\}`).FindAllStringSubmatch(workflow, -1) {
		if m[1] != m[2] {
			t.Errorf("workflow forwards secret %s under the name %s; the two must match", m[2], m[1])
		}
		forwarded[m[1]] = true
	}

	for name := range fixtures {
		if !forwarded[name] {
			t.Errorf("fixture %s has no `%s: ${{ secrets.%s }}` line in mcp-catalog-smoke.yml; it would skip forever", name, name, name)
		}
	}
	for name := range forwarded {
		if !fixtures[name] {
			t.Errorf("mcp-catalog-smoke.yml forwards %s but no catalogKeyFixtures row reads it", name)
		}
	}
}
