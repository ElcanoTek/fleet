package handlers

import (
	"strings"
	"testing"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// TestValidateTaskCreate_Title pins the display label's contract: optional,
// trimmed, single-line, capped at maxTaskTitleChars RUNES (so multibyte titles
// are not penalized by bytes). The single-line rule matters because the title is
// rendered inline in a table cell and a calendar tile — a pasted multi-line
// prompt must be rejected rather than silently mangled.
func TestValidateTaskCreate_Title(t *testing.T) {
	h := newValidateTestHandlers()
	prompt := "do the thing for the team"

	t.Run("empty title accepted (untitled is the default)", func(t *testing.T) {
		if err := h.validateTaskCreate(&models.TaskCreate{Prompt: prompt}); err != nil {
			t.Fatalf("empty title should be accepted, got %v", err)
		}
	})

	t.Run("title is trimmed in place", func(t *testing.T) {
		tc := &models.TaskCreate{Prompt: prompt, Title: "  Daily pacing summary\t"}
		if err := h.validateTaskCreate(tc); err != nil {
			t.Fatalf("validate: %v", err)
		}
		if tc.Title != "Daily pacing summary" {
			t.Errorf("title = %q, want it trimmed to %q", tc.Title, "Daily pacing summary")
		}
	})

	t.Run("at the rune limit accepted", func(t *testing.T) {
		title := strings.Repeat("é", maxTaskTitleChars) // 2 bytes each, 1 rune each
		if err := h.validateTaskCreate(&models.TaskCreate{Prompt: prompt, Title: title}); err != nil {
			t.Fatalf("title at the rune limit should be accepted, got %v", err)
		}
	})

	t.Run("over the limit rejected", func(t *testing.T) {
		title := strings.Repeat("a", maxTaskTitleChars+1)
		err := h.validateTaskCreate(&models.TaskCreate{Prompt: prompt, Title: title})
		if err == nil || !strings.Contains(err.Error(), "title") {
			t.Fatalf("over-limit title should be rejected with a title error, got %v", err)
		}
	})

	t.Run("multi-line rejected", func(t *testing.T) {
		for _, title := range []string{"first\nsecond", "first\rsecond"} {
			err := h.validateTaskCreate(&models.TaskCreate{Prompt: prompt, Title: title})
			if err == nil || !strings.Contains(err.Error(), "single line") {
				t.Errorf("title %q should be rejected as multi-line, got %v", title, err)
			}
		}
	})
}

// TestTitleSurvivesCopiesUnlikeName is the whole reason title is not name: a
// copy MUST clear Name (it is the unique import/export identity key and would
// collide with the row it was copied from) but MUST carry Title, so every
// occurrence, re-run and clone of one job lists under the same label.
func TestTitleSurvivesCopiesUnlikeName(t *testing.T) {
	src := &models.Task{
		Name:       "reklaim-daily-health-scan",
		Title:      "Reklaim daily health scan",
		Prompt:     "do the work for the team",
		Recurrence: "0 9 * * *",
		Timezone:   "UTC",
	}

	for _, keepRecurrence := range []bool{false, true} {
		tc, err := buildRerunTaskCreate(src, keepRecurrence, taskRerunOverrides{}, nil)
		if err != nil {
			t.Fatalf("build(keepRecurrence=%v): %v", keepRecurrence, err)
		}
		if tc.Title != src.Title {
			t.Errorf("keepRecurrence=%v: copy title = %q, want it carried (%q)", keepRecurrence, tc.Title, src.Title)
		}
		if tc.Name != "" {
			t.Errorf("keepRecurrence=%v: copy name = %q, want it cleared", keepRecurrence, tc.Name)
		}
	}

	t.Run("an override can relabel the copy", func(t *testing.T) {
		relabelled := "Reklaim health scan (manual)"
		tc, err := buildRerunTaskCreate(src, false, taskRerunOverrides{Title: &relabelled}, nil)
		if err != nil {
			t.Fatalf("build: %v", err)
		}
		if tc.Title != relabelled {
			t.Errorf("title = %q, want the override %q", tc.Title, relabelled)
		}
	})
}
