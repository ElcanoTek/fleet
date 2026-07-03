package scheduledrun

import (
	"strings"
	"testing"
)

// The inline skills section carries full bodies, keeps a deterministic order,
// and drops LOUDLY past the budget (a silent drop would read as "no skill").
func TestRenderUserSkillsSection(t *testing.T) {
	docs := []UserSkillDoc{
		{Name: "deal-check", Description: "verify a deal sheet", Body: "1. Check totals.\n"},
		{Name: "pacing", Description: "weekly pacing report", Body: strings.Repeat("x", userSkillsInlineBudget)},
		{Name: "small", Description: "fits after the big one is dropped", Body: "tiny\n"},
	}
	out := renderUserSkillsSection(docs)
	if !strings.Contains(out, "## Your user's skills") ||
		!strings.Contains(out, "### deal-check") ||
		!strings.Contains(out, "1. Check totals.") {
		t.Fatalf("section malformed:\n%s", out)
	}
	// The oversized body blows the remaining budget and is dropped — loudly.
	if strings.Contains(out, strings.Repeat("x", 1024)) {
		t.Error("oversized skill body should have been dropped")
	}
	if !strings.Contains(out, "### small") {
		t.Error("later small skill should still fit")
	}
	if !strings.Contains(out, "omitted for space") {
		t.Error("dropped skills must be announced, not silent")
	}
}
