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
	if !w.HasPreview || w.PreviewHTML != "" {
		t.Errorf("the list notes a preview without reading it: has=%v html=%q", w.HasPreview, w.PreviewHTML)
	}
	one, ok, problems := ReadEmailTemplate(dir, "weekly-update")
	if !ok || len(problems) != 0 || one.PreviewHTML != "<!doctype html><p>Hi Team,</p>" || one.Name != w.Name {
		t.Errorf("ReadEmailTemplate = %+v ok=%v problems=%v", one, ok, problems)
	}
	for _, id := range []string{"missing", "../weekly-update", "previews/weekly-update", "README", ""} {
		if _, ok, _ := ReadEmailTemplate(dir, id); ok {
			t.Errorf("ReadEmailTemplate(%q) found a template; an id is matched, never used as a path", id)
		}
	}
	if len(w.Fields) != 1 || w.PromptTemplate != "Write the update for {campaign}." {
		t.Errorf("form = %+v / %q", w.Fields, w.PromptTemplate)
	}
	if got[0].HasPreview || got[0].Fields != nil {
		t.Errorf("a template without a preview or form carries neither: %+v", got[0])
	}
}

// The bundle's recolor table rides with the preview, never with the list, and
// only when it holds nothing but colors.
func TestReadEmailTemplatePreviewStyles(t *testing.T) {
	dir := t.TempDir()
	writeEmailTemplateFile(t, dir, "weekly-update.yaml", emailTemplateYAML)
	writeEmailTemplateFile(t, dir, "previews/weekly-update.html", "<!doctype html><p>Hi Team,</p>")
	one, ok, problems := ReadEmailTemplate(dir, "weekly-update")
	if !ok || len(problems) != 0 || one.PreviewStyles != nil {
		t.Errorf("no styles.json, so no recolor table: %+v", one.PreviewStyles)
	}

	writeEmailTemplateFile(t, dir, "previews/styles.json",
		`{"options": {"amazon": {"#1A0B1E": "#333E48"}, "elcano": {}}, "custom": {"Header": ["#1A0B1E"]}}`)
	one, _, problems = ReadEmailTemplate(dir, "weekly-update")
	if len(problems) != 0 || one.PreviewStyles == nil ||
		one.PreviewStyles.Options["amazon"]["#1A0B1E"] != "#333E48" || one.PreviewStyles.Custom["Header"][0] != "#1A0B1E" {
		t.Errorf("preview styles = %+v problems=%v", one.PreviewStyles, problems)
	}
	if listed, _ := ReadEmailTemplates(dir); len(listed) != 1 || listed[0].PreviewStyles != nil {
		t.Error("the list must not carry the recolor table")
	}
	// Anything but #RRGGBB colors could be markup once swapped into the preview.
	for _, bad := range []string{
		`{"options": {"x": {"#1A0B1E": "red;background:url(//evil)"}}}`,
		`{"options": {"x": {"</style>": "#333E48"}}}`,
		`{"custom": {"Header": ["#12345"]}}`,
		`not json`,
	} {
		writeEmailTemplateFile(t, dir, "previews/styles.json", bad)
		one, ok, problems = ReadEmailTemplate(dir, "weekly-update")
		if !ok || one.PreviewStyles != nil || one.PreviewHTML == "" || len(problems) != 1 || !strings.Contains(problems[0], "previews/styles.json") {
			t.Errorf("styles %q: styles=%+v problems=%v", bad, one.PreviewStyles, problems)
		}
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
		one, ok, _ := ReadEmailTemplate(dir, tpl.ID)
		if tpl.HasPreview || !ok || one.HasPreview || one.PreviewHTML != "" {
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
	if _, ok, _ := b.EmailTemplate("x"); ok {
		t.Error("nil bundle has no template")
	}
}

// The library serves the bundle's templates and the team's from one list. The
// team directory is written by the bundle's connector, so fleet reads it with
// the same distrust as any bundle file and never lets it replace a bundle
// template.
func TestEmailTemplateLibraryMergesTeamTemplates(t *testing.T) {
	bundleDir, teamDir := t.TempDir(), t.TempDir()
	writeEmailTemplateFile(t, bundleDir, "weekly-update.yaml", emailTemplateYAML)
	writeEmailTemplateFile(t, bundleDir, "previews/weekly-update.html", "<!doctype html><p>bundle</p>")
	team := "name: Monthly QBR recap\ncreated_by: jeanne@example.com\ncreated_at: \"2026-11-16T15:00:00Z\"\nsections:\n  - {id: flags, block: text}\n"
	writeEmailTemplateFile(t, teamDir, "monthly-qbr-recap.yaml", team)
	writeEmailTemplateFile(t, teamDir, "previews/monthly-qbr-recap.html", "<!doctype html><p>team</p>")
	// A team file under a bundle id must not shadow the bundle's template.
	writeEmailTemplateFile(t, teamDir, "weekly-update.yaml", "name: Hijack\nsections:\n  - {id: flags, block: text}\n")
	writeEmailTemplateFile(t, teamDir, "previews/weekly-update.html", "<!doctype html><p>hijack</p>")

	lib := EmailTemplateLibrary{Bundle: &Bundle{EmailTemplatesDir: bundleDir}, TeamDir: teamDir}
	got, problems := lib.Templates()
	if len(got) != 2 || got[0].ID != "monthly-qbr-recap" || got[1].ID != "weekly-update" {
		t.Fatalf("templates = %+v", got)
	}
	qbr, weekly := got[0], got[1]
	if qbr.Source != EmailTemplateSourceTeam || qbr.CreatedBy != "jeanne@example.com" || qbr.CreatedAt != "2026-11-16T15:00:00Z" || qbr.Path != "team/monthly-qbr-recap.yaml" || !qbr.HasPreview {
		t.Errorf("team template = %+v", qbr)
	}
	if weekly.Source != EmailTemplateSourceBundle || weekly.Name == "Hijack" || weekly.CreatedBy != "" {
		t.Errorf("bundle template = %+v", weekly)
	}
	if len(problems) != 1 || !strings.Contains(problems[0], "a bundle template has this id") {
		t.Errorf("problems = %v, want the shadowed team file reported", problems)
	}

	one, ok, _ := lib.Template("monthly-qbr-recap")
	if !ok || one.PreviewHTML != "<!doctype html><p>team</p>" || one.Source != EmailTemplateSourceTeam {
		t.Errorf("team template read = %+v ok=%v", one, ok)
	}
	one, ok, _ = lib.Template("weekly-update")
	if !ok || one.PreviewHTML != "<!doctype html><p>bundle</p>" || one.Source != EmailTemplateSourceBundle {
		t.Errorf("a shared id must read the bundle's template: %+v ok=%v", one, ok)
	}
	if _, ok, _ := lib.Template("../weekly-update"); ok {
		t.Error("an id is matched against the listed templates, never used as a path")
	}

	// A bundle template never claims an author, whatever its file says.
	writeEmailTemplateFile(t, bundleDir, "weekly-update.yaml", emailTemplateYAML+"created_by: someone@example.com\n")
	if got, _ := lib.Templates(); got[1].CreatedBy != "" {
		t.Errorf("created_by is a team field: %+v", got[1])
	}
}

func TestEmailTemplateLibraryWithoutATeamDirectory(t *testing.T) {
	bundleDir := t.TempDir()
	writeEmailTemplateFile(t, bundleDir, "weekly-update.yaml", emailTemplateYAML)
	for _, teamDir := range []string{"", filepath.Join(t.TempDir(), "absent")} {
		lib := EmailTemplateLibrary{Bundle: &Bundle{EmailTemplatesDir: bundleDir}, TeamDir: teamDir}
		got, problems := lib.Templates()
		if len(got) != 1 || len(problems) != 0 || got[0].Source != EmailTemplateSourceBundle {
			t.Errorf("team dir %q: templates=%+v problems=%v", teamDir, got, problems)
		}
		if _, ok, _ := lib.Template("monthly-qbr-recap"); ok {
			t.Errorf("team dir %q: no team template exists", teamDir)
		}
	}
	// A team directory swapped for a link is not followed.
	target, link := t.TempDir(), filepath.Join(t.TempDir(), "team")
	writeEmailTemplateFile(t, target, "planted.yaml", "name: Planted\nsections:\n  - {id: flags, block: text}\n")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	lib := EmailTemplateLibrary{Bundle: &Bundle{EmailTemplatesDir: bundleDir}, TeamDir: link}
	got, problems := lib.Templates()
	if len(got) != 1 || len(problems) != 1 || !strings.Contains(problems[0], "symlink") {
		t.Errorf("linked team dir: templates=%+v problems=%v", got, problems)
	}
}
