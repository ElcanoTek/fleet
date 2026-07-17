package tools

import (
	"slices"
	"testing"
)

// The interactive staging-card tools must never reach a headless scheduled
// run: their raw Runs are mis-wiring tripwires whose non-nil Go error is
// FATAL to the agent loop (a scheduled model calling preview_email to present
// its finished report dead-lettered the whole task), and nothing headless
// intercepts them.
func TestExcludeInteractiveOnly(t *testing.T) {
	all := DefaultTools()
	filtered := ExcludeInteractiveOnly(all)

	names := make([]string, 0, len(filtered))
	for _, tool := range filtered {
		names = append(names, tool.Info().Name)
	}
	for name := range interactiveOnlyToolNames {
		if slices.Contains(names, name) {
			t.Errorf("interactive-only tool %q survived the scheduled-roster filter", name)
		}
	}
	if want := len(all) - len(interactiveOnlyToolNames); len(filtered) != want {
		t.Errorf("filtered %d tools, want %d (only the %d interactive-only entries removed)",
			len(filtered), want, len(interactiveOnlyToolNames))
	}
	// The workhorse tools must survive.
	for _, keep := range []string{"bash", "run_python", "view_file", "write_file"} {
		if !slices.Contains(names, keep) {
			t.Errorf("expected %q to survive the filter; got %v", keep, names)
		}
	}
	// Every name in the exclusion set must correspond to a REAL registered
	// tool — a renamed tool would otherwise silently escape the filter.
	allNames := make([]string, 0, len(all))
	for _, tool := range all {
		allNames = append(allNames, tool.Info().Name)
	}
	for name := range interactiveOnlyToolNames {
		if !slices.Contains(allNames, name) {
			t.Errorf("exclusion entry %q matches no registered default tool (renamed?)", name)
		}
	}
}
