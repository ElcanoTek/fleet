package clientconfig

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const emailTemplateYAML = `---
name: "Weekly update"
description: >
  The weekly
  client note.
audience: client
use_when: "Every week."
max_words: 260
subject: {instruction: "Campaign and the one result.", max_chars: 70}
rules: ["Short sentences.", "  "]
never_say: ["no optimizations", "re:\\bELC\\d{3,}\\b"]
sections:
  - id: summary
    title: Summary
    heading: Summary
    block: text
    instruction: "Greeting, then up to three bullets."
    limits: {max_paragraphs: 2, max_bullets: 3, max_words: 100}
  - id: trend
    title: Trend chart
    block: chart
    required: false
    instruction: "One chart."
  - id: untitled
    block: closing
sample: {subject: "Sample", blocks: []}
fields:
  - {key: campaign, label: Campaign, type: text, required: true}
promptTemplate: "Write the update for {campaign}."
`

func writeEmailTemplateFile(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestReadEmailTemplates(t *testing.T) {
	dir := t.TempDir()
	writeEmailTemplateFile(t, dir, "weekly-update.yaml", emailTemplateYAML)
	writeEmailTemplateFile(t, dir, "previews/weekly-update.html", "<!doctype html><p>Hi Team,</p>")
	writeEmailTemplateFile(t, dir, "brief.yml", "name: A brief\nsections:\n  - {id: flags, block: text}\n")
	writeEmailTemplateFile(t, dir, "README.md", "# docs")
	writeEmailTemplateFile(t, dir, "readme.yaml", "name: not a template\n")
	writeEmailTemplateFile(t, dir, "notes.txt", "not a template")

	got, problems := ReadEmailTemplates(dir)
	if len(problems) != 0 {
		t.Fatalf("problems = %v", problems)
	}
	if len(got) != 2 || got[0].ID != "brief" || got[1].ID != "weekly-update" {
		t.Fatalf("templates = %+v, want brief then weekly-update (by name)", got)
	}

	w := got[1]
	if w.Name != "Weekly update" || w.Description != "The weekly client note." || w.Audience != "client" ||
		w.Path != "email_templates/weekly-update.yaml" || w.MaxWords != 260 {
		t.Errorf("metadata = %+v", w)
	}
	if w.Subject.MaxChars != 70 || w.Subject.Instruction == "" {
		t.Errorf("subject = %+v", w.Subject)
	}
	if len(w.Rules) != 1 || len(w.NeverSay) != 2 {
		t.Errorf("rules = %q, never_say = %q; blank entries are dropped", w.Rules, w.NeverSay)
	}
	if len(w.Sections) != 3 {
		t.Fatalf("sections = %+v", w.Sections)
	}
	summary, trend, untitled := w.Sections[0], w.Sections[1], w.Sections[2]
	if !summary.Required || summary.Heading != "Summary" || summary.Limits["max_bullets"] != 3 {
		t.Errorf("summary = %+v", summary)
	}
	if trend.Required {
		t.Error("required: false must be kept; only an absent key defaults to required")
	}
	if untitled.Title != "untitled" || !untitled.Required {
		t.Errorf("a section with no title is shown by its id: %+v", untitled)
	}
	if w.PreviewHTML != "<!doctype html><p>Hi Team,</p>" {
		t.Errorf("preview = %q", w.PreviewHTML)
	}
	if len(w.Fields) != 1 || w.PromptTemplate != "Write the update for {campaign}." {
		t.Errorf("form = %+v / %q", w.Fields, w.PromptTemplate)
	}
	if got[0].PreviewHTML != "" || got[0].Fields != nil {
		t.Errorf("a template without a preview or form carries neither: %+v", got[0])
	}
}

func TestReadEmailTemplatesReportsAndKeepsGoing(t *testing.T) {
	dir := t.TempDir()
	writeEmailTemplateFile(t, dir, "good.yaml", "name: Good\nsections:\n  - {id: a, block: text}\n")
	writeEmailTemplateFile(t, dir, "unparsable.yaml", "name: [unclosed\n")
	writeEmailTemplateFile(t, dir, "nameless.yaml", "sections:\n  - {id: a, block: text}\n")
	writeEmailTemplateFile(t, dir, "sectionless.yaml", "name: No sections\nsections:\n  - {title: no id or block}\n")
	writeEmailTemplateFile(t, dir, "empty.yaml", "")
	writeEmailTemplateFile(t, dir, "badform.yaml",
		"name: Bad form\nsections:\n  - {id: a, block: text}\nfields: [{key: a, label: A, type: radio}]\npromptTemplate: \"{a}\"\n")
	if err := os.Symlink(filepath.Join(dir, "good.yaml"), filepath.Join(dir, "link.yaml")); err != nil {
		t.Fatal(err)
	}

	got, problems := ReadEmailTemplates(dir)
	ids := make([]string, 0, len(got))
	for _, tpl := range got {
		ids = append(ids, tpl.ID)
	}
	if strings.Join(ids, ",") != "badform,good" {
		t.Fatalf("templates = %v, want badform (served without its form) and good", ids)
	}
	if got[0].Fields != nil || got[0].PromptTemplate != "" {
		t.Errorf("an invalid form must not be served: %+v", got[0])
	}
	joined := strings.Join(problems, "\n")
	for _, want := range []string{
		"unparsable.yaml: the YAML does not parse",
		"nameless.yaml: needs a name",
		"sectionless.yaml: needs a name",
		"empty.yaml: is empty or not UTF-8",
		"badform.yaml: invalid form, served without one",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("problems lack %q:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "link.yaml") {
		t.Errorf("a symlink is skipped silently:\n%s", joined)
	}
}

func TestReadEmailTemplatesPreviewBounds(t *testing.T) {
	dir := t.TempDir()
	body := "name: T\nsections:\n  - {id: a, block: text}\n"
	writeEmailTemplateFile(t, dir, "big.yaml", body)
	writeEmailTemplateFile(t, dir, "previews/big.html", strings.Repeat("x", maxBundleEmailTemplatePreview+1))
	writeEmailTemplateFile(t, dir, "linked.yaml", body)
	writeEmailTemplateFile(t, dir, "secret.html", "<p>elsewhere</p>")
	if err := os.Symlink(filepath.Join(dir, "secret.html"), filepath.Join(dir, "previews", "linked.html")); err != nil {
		t.Fatal(err)
	}

	got, problems := ReadEmailTemplates(dir)
	if len(got) != 2 {
		t.Fatalf("both templates are still served: %+v", got)
	}
	for _, tpl := range got {
		if tpl.PreviewHTML != "" {
			t.Errorf("%s: an oversized or symlinked preview must not be served", tpl.ID)
		}
	}
	if len(problems) != 1 || !strings.Contains(problems[0], "big.yaml: preview exceeds") {
		t.Errorf("problems = %v, want only the oversized preview", problems)
	}
}

func TestReadEmailTemplatesMissingDirAndNilBundle(t *testing.T) {
	if got, problems := ReadEmailTemplates(filepath.Join(t.TempDir(), "absent")); got != nil || problems != nil {
		t.Errorf("a bundle without the directory has an empty library: %v %v", got, problems)
	}
	var b *Bundle
	if got, problems := b.EmailTemplates(); got != nil || problems != nil {
		t.Errorf("nil bundle: %v %v", got, problems)
	}
}
