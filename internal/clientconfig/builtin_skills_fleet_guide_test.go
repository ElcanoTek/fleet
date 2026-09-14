package clientconfig

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The fleet-guide pack (docs/USER-GUIDES.md) is prose that points at prose: the
// SKILL.md tells the agent which of the two guide files to read, by path. Two
// things can rot silently here, and both end the same way — the assistant
// telling a user how the product works from memory instead of from the guide:
//
//  1. A guide file is renamed or moved and SKILL.md keeps citing the old path.
//     The agent's view_file then fails mid-answer.
//  2. A guide loses the sections the skill promises it covers.
//
// So the referenced paths are resolved for real against the materialized pack,
// the way builtin_skills_bento_test.go pins the bento pack's references.
func TestBuiltinSkillFleetGuideReferencesResolve(t *testing.T) {
	merged, err := materializeMergedSkills(filepath.Join(t.TempDir(), "no-bundle-skills"), true, nil, nil)
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	packDir := filepath.Join(merged, "fleet-guide")
	raw, err := os.ReadFile(filepath.Join(packDir, "SKILL.md"))
	if err != nil {
		t.Fatalf("read SKILL.md: %v", err)
	}
	body := string(raw)

	// Every `skills/fleet-guide/<file>` the instructions cite must exist. The
	// path form matters as much as the file: skills/ is what the sandbox mounts
	// and what the agent's workspace-relative reads resolve against.
	refs := regexp.MustCompile(`skills/fleet-guide/([A-Za-z0-9._-]+)`).FindAllStringSubmatch(body, -1)
	if len(refs) < 2 {
		t.Fatalf("SKILL.md cites %d guide paths; it must send the agent to both guides by path", len(refs))
	}
	for _, ref := range refs {
		if _, err := os.Stat(filepath.Join(packDir, ref[1])); err != nil {
			t.Errorf("SKILL.md cites skills/fleet-guide/%s, which is not in the pack: %v", ref[1], err)
		}
	}

	// Honesty is the whole contract of a self-describing skill: a deployment can
	// hide features and change windows, so the agent must be told to refuse to
	// invent UI rather than fill the gap plausibly.
	for _, needle := range []string{"Never invent", "say so"} {
		if !strings.Contains(body, needle) {
			t.Errorf("SKILL.md no longer says %q — without it the agent answers product questions "+
				"from guesswork, and a plausible wrong button costs a user more than \"I don't know\"", needle)
		}
	}

	// The two guides must still be the documents the skill advertises. These are
	// the section titles users arrive with a question about.
	for file, needles := range map[string][]string{
		"chat.md": {
			"## 6. Cards that ask for a decision",
			"## 9. The prompt library",
			"## 13. Keyboard shortcuts",
		},
		"operations-center.md": {
			"## 4. Run states",
			"`DEAD_LETTERED`",
			"`PAUSED_AWAITING_INPUT`",
		},
	} {
		guide, err := os.ReadFile(filepath.Join(packDir, file))
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		for _, needle := range needles {
			if !strings.Contains(string(guide), needle) {
				t.Errorf("%s no longer contains %q — the skill's description promises it covers this", file, needle)
			}
		}
	}
}
