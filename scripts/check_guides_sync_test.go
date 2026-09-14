// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package scripts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The user guides (docs/USER-GUIDES.md) are ONE document with two readers:
//
//   - internal/clientconfig/builtin_skills/fleet-guide/*.md — what the built-in
//     `fleet-guide` skill hands the agent when a user asks how fleet works.
//   - web/src/app/help/guides/*.md — what /help renders in the app.
//
// Neither direction of linking is available: go:embed cannot reach outside its
// own package, and Turbopack refuses to build through a symlink that leaves the
// Next project root. So the web tree keeps a verbatim copy, and this test is
// what keeps a copy from quietly becoming a fork — the failure mode being an
// assistant that confidently cites a page the user is not reading.
//
// The skill's copy is the source of truth; `make sync-guides` refreshes the
// other one.
func TestUserGuidesAreInSync(t *testing.T) {
	root := repoRoot(t)
	const (
		skillDir = "internal/clientconfig/builtin_skills/fleet-guide"
		webDir   = "web/src/app/help/guides"
	)

	// Every .md in the skill folder EXCEPT SKILL.md (the agent's instructions
	// for using the guides — not a guide, and not something /help renders).
	entries, err := os.ReadDir(filepath.Join(root, skillDir))
	if err != nil {
		t.Fatalf("read %s: %v", skillDir, err)
	}
	var guides []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".md") || name == "SKILL.md" {
			continue
		}
		guides = append(guides, name)
	}
	if len(guides) == 0 {
		t.Fatalf("no guide Markdown found under %s — did the fleet-guide skill move?", skillDir)
	}

	for _, name := range guides {
		want := readFile(t, root, filepath.Join(skillDir, name))
		got, err := os.ReadFile(filepath.Join(root, webDir, name))
		if err != nil {
			t.Errorf("%s/%s is missing (the /help page reads it): run `make sync-guides`", webDir, name)
			continue
		}
		if string(got) != want {
			t.Errorf("%s/%s has drifted from %s/%s — edit the skill's copy, then run `make sync-guides`",
				webDir, name, skillDir, name)
		}
	}

	// The reverse direction: a guide added to the web tree alone would render
	// in the app and be invisible to the assistant.
	webEntries, err := os.ReadDir(filepath.Join(root, webDir))
	if err != nil {
		t.Fatalf("read %s: %v", webDir, err)
	}
	known := map[string]bool{}
	for _, name := range guides {
		known[name] = true
	}
	for _, e := range webEntries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		if !known[e.Name()] {
			t.Errorf("%s/%s has no counterpart in %s — a guide the app shows but the assistant cannot read; add it to the skill",
				webDir, e.Name(), skillDir)
		}
	}
}
