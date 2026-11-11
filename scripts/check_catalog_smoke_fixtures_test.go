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

// TestCatalogSmokeFixturesMatchWorkflow — an api_key fixture in the weekly
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
	// A row is {Entry: "name", ...} with an optional Variant: "id" anywhere
	// in it: the variant names a regional endpoint and its own secret,
	// FLEET_CATALOG_KEY_<ENTRY>_<ID>. The whole row is captured so the field
	// order does not matter.
	variantField := regexp.MustCompile(`Variant:\s*"([a-z0-9-]+)"`)
	for _, m := range regexp.MustCompile(`\{Entry:\s*"([a-z0-9-]+)"([^}]*)\}`).FindAllStringSubmatch(goSrc, -1) {
		name := "FLEET_CATALOG_KEY_" + strings.ToUpper(strings.ReplaceAll(m[1], "-", "_"))
		if v := variantField.FindStringSubmatch(m[2]); v != nil {
			name += "_" + strings.ToUpper(strings.ReplaceAll(v[1], "-", "_"))
		}
		if fixtures[name] {
			// "foo-eu" and "foo" + variant "eu" would both read FLEET_CATALOG_KEY_FOO_EU.
			t.Errorf("two catalogKeyFixtures rows read the same secret %s", name)
		}
		fixtures[name] = true
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

// TestScanCronAlarmWatchesScheduledWorkflows pins scan-cron-alarm.yml's
// workflow_run.workflows display-name list against .github/workflows/*.yml:
// GitHub matches workflow_run triggers by display `name:`, so renaming a
// watched workflow without updating scan-cron-alarm.yml silently disarms its
// alarm with no error from GitHub or actionlint.
func TestScanCronAlarmWatchesScheduledWorkflows(t *testing.T) {
	root := repoRoot(t)
	alarmRaw, err := os.ReadFile(filepath.Join(root, ".github", "workflows", "scan-cron-alarm.yml"))
	if err != nil {
		t.Fatalf("read scan-cron-alarm.yml: %v", err)
	}
	var alarm struct {
		On struct {
			WorkflowRun struct {
				Workflows []string `yaml:"workflows"`
			} `yaml:"workflow_run"`
		} `yaml:"on"`
	}
	if err := yaml.Unmarshal(alarmRaw, &alarm); err != nil {
		t.Fatalf("parse scan-cron-alarm.yml: %v", err)
	}
	if len(alarm.On.WorkflowRun.Workflows) == 0 {
		t.Fatal("scan-cron-alarm.yml watches no workflows")
	}

	dir := filepath.Join(root, ".github", "workflows")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read workflows dir: %v", err)
	}
	scheduledByName := map[string]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yml") {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		var wf struct {
			Name string         `yaml:"name"`
			On   map[string]any `yaml:"on"`
		}
		if err := yaml.Unmarshal(raw, &wf); err != nil {
			t.Fatalf("parse %s: %v", e.Name(), err)
		}
		if _, hasSchedule := wf.On["schedule"]; hasSchedule && wf.Name != "" {
			scheduledByName[wf.Name] = e.Name()
		}
	}

	for _, watched := range alarm.On.WorkflowRun.Workflows {
		if _, ok := scheduledByName[watched]; !ok {
			t.Errorf("scan-cron-alarm.yml watches %q, which matches no scheduled workflow's `name:` in .github/workflows/", watched)
		}
	}
}
