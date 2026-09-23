// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package scripts

import (
	"regexp"
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/agentcore"
)

// The compiled-in model tiers live in Go (agentcore.DefaultCoreModel /
// DefaultMaxModel) and are repeated in exactly one web file, because the web
// bundle cannot import Go constants (and Turbopack will not build through a
// symlink out of the Next root). Everything else derives from those two
// places: config.DefaultTitleModel and the fake-LLM catalog use the Go
// constants, and web code and tests use DEFAULT_MODEL / ADVANCED_MODEL and
// their labels. This test is what keeps the one repeated copy honest, and it
// also catches the user guide still naming the previous models after a swap.
// The swap checklist is docs/MODEL-DEFAULTS.md.
func TestModelDefaultsAreInSync(t *testing.T) {
	root := repoRoot(t)
	web := readFile(t, root, "web/src/app/lib/modelAliases.ts")

	constant := func(name string) string {
		t.Helper()
		m := regexp.MustCompile(`export const ` + name + ` = "([^"]+)";`).FindStringSubmatch(web)
		if m == nil {
			t.Fatalf("modelAliases.ts: no `export const %s = \"...\";`", name)
		}
		return m[1]
	}

	for _, tc := range []struct{ web, goName, goValue string }{
		{"DEFAULT_MODEL", "agentcore.DefaultCoreModel", agentcore.DefaultCoreModel},
		{"ADVANCED_MODEL", "agentcore.DefaultMaxModel", agentcore.DefaultMaxModel},
	} {
		if got := constant(tc.web); got != tc.goValue {
			t.Errorf("web %s = %q but %s = %q: change both (docs/MODEL-DEFAULTS.md)", tc.web, got, tc.goName, tc.goValue)
		}
	}

	// The chat guide names both defaults in prose ("By default those two are
	// **GPT-6 Luna Pro** … and **Claude Opus 5.5** …"). The label's provider
	// prefix ("OpenAI: ") is dropped there.
	guide := readFile(t, root, "internal/clientconfig/builtin_skills/fleet-guide/chat.md")
	for _, name := range []string{"DEFAULT_MODEL_LABEL", "ADVANCED_MODEL_LABEL"} {
		label := constant(name)
		if i := strings.Index(label, ": "); i >= 0 {
			label = label[i+2:]
		}
		if !strings.Contains(guide, "**"+label+"**") {
			t.Errorf("fleet-guide/chat.md does not name the default %q (from %s); update the guide and run make sync-guides", label, name)
		}
	}
}
