package admincli

import (
	"flag"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

func TestOptionalFlag_OmitVsClear(t *testing.T) {
	parse := func(args ...string) *string {
		t.Helper()
		fs := flag.NewFlagSet("test", flag.ContinueOnError)
		fb := fs.String("fallback-model", "", "")
		if err := fs.Parse(args); err != nil {
			t.Fatalf("parse: %v", err)
		}
		return optionalFlag(fs, "fallback-model", fb)
	}

	if got := parse(); got != nil {
		t.Fatalf("omitted flag = %v, want nil (keep existing)", got)
	}
	got := parse("--fallback-model", "")
	if got == nil || *got != "" {
		t.Fatalf("explicit empty = %v, want pointer to \"\"", got)
	}
	got = parse("--fallback-model", "fb/new")
	if got == nil || *got != "fb/new" {
		t.Fatalf("explicit slug = %v, want fb/new", got)
	}
}

func TestFormatFallbackChange(t *testing.T) {
	sp := func(s string) *string { return &s }
	cases := []struct {
		name    string
		current *string
		next    *string
		want    string
	}{
		{name: "omit keeps existing", current: sp("fb/old"), next: nil, want: "fb/old (unchanged)"},
		{name: "omit with no current", current: nil, next: nil, want: "<none> (unchanged)"},
		{name: "explicit clear", current: sp("fb/old"), next: sp(""), want: "fb/old → <cleared>"},
		{name: "explicit set", current: sp("fb/old"), next: sp("fb/new"), want: "fb/old → fb/new"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatFallbackChange(tc.current, tc.next); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFormatSetModelDryRun_ShowsEveryField(t *testing.T) {
	id := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	model := "old/model"
	fb := "fb/old"
	task := &models.Task{ID: id, Model: &model, FallbackModel: &fb}
	got := formatSetModelDryRun(task, "new/model", nil)
	if !strings.Contains(got, "model old/model → new/model") {
		t.Errorf("missing model change: %q", got)
	}
	if !strings.Contains(got, "fallback fb/old (unchanged)") {
		t.Errorf("missing unchanged fallback: %q", got)
	}
	cleared := formatSetModelDryRun(task, "new/model", ptr(""))
	if !strings.Contains(cleared, "fallback fb/old → <cleared>") {
		t.Errorf("missing clear: %q", cleared)
	}
}

func TestSetModelConfirmPrompt(t *testing.T) {
	got := setModelConfirmPrompt("new/model", nil, "")
	if !strings.Contains(got, "existing fallback_model values will be kept") {
		t.Errorf("omit should mention keep: %q", got)
	}
	empty := ""
	got = setModelConfirmPrompt("new/model", &empty, "old/model")
	if !strings.Contains(got, "fallback_model will be cleared") {
		t.Errorf("explicit empty should mention clear: %q", got)
	}
	if !strings.Contains(got, "pinned to old/model") {
		t.Errorf("from-model should narrow scope: %q", got)
	}
}

func TestConfirmBulkMutation_RefusesNonTTY(t *testing.T) {
	if isTerminal(os.Stdin) {
		t.Skip("stdin is a TTY; cannot exercise the non-interactive refuse path")
	}
	if code := confirmBulkMutation(false, "test mutation"); code != 1 {
		t.Fatalf("non-TTY without --no-confirm: got %d, want 1", code)
	}
	if code := confirmBulkMutation(true, "test mutation"); code != 0 {
		t.Fatalf("--no-confirm should proceed: got %d, want 0", code)
	}
}
