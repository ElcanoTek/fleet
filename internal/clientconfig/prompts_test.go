package clientconfig

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadPromptsHybridFormats(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("daily.yaml", "name: Daily Health Scan\ngoal: >\n  Check the important systems and report exceptions.\nsteps:\n  - inspect\n")
	write("weekly.md", "# Weekly Brief\n\nSummarize the week for leadership.\n")
	write("README.md", "# documentation, not a prompt")
	write("ignored.json", `{}`)

	got, problems := ReadPrompts(dir)
	if len(problems) != 0 {
		t.Fatalf("problems = %v", problems)
	}
	if len(got) != 2 {
		t.Fatalf("prompts = %d, want 2: %+v", len(got), got)
	}
	if got[0].Name != "Daily Health Scan" || got[0].Description != "Check the important systems and report exceptions." {
		t.Errorf("yaml metadata = %+v", got[0])
	}
	if got[0].Content != "name: Daily Health Scan\ngoal: >\n  Check the important systems and report exceptions.\nsteps:\n  - inspect\n" {
		t.Error("YAML content was not preserved verbatim")
	}
	if got[1].Name != "Weekly Brief" || got[1].Source != "git" || !got[1].ReadOnly {
		t.Errorf("markdown prompt = %+v", got[1])
	}
}

func TestReadPromptsMissingDirectoryIsEmpty(t *testing.T) {
	got, problems := ReadPrompts(filepath.Join(t.TempDir(), "missing"))
	if len(got) != 0 || len(problems) != 0 {
		t.Fatalf("got prompts=%v problems=%v", got, problems)
	}
}
