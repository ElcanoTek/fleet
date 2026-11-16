package clientconfig

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/goccy/go-yaml"
)

const (
	maxBundleEmailTemplates       = 64
	maxBundleEmailTemplateSize    = 256 << 10
	maxBundleEmailTemplatePreview = 1 << 20
	maxBundleEmailPreviewStyles   = 64 << 10
	maxEmailPreviewStyleEntries   = 64
)

// emailPreviewColor is the only thing a preview recolor table may hold: the
// browser swaps one such color for another inside the preview's markup.
var emailPreviewColor = regexp.MustCompile(`^#[0-9A-Fa-f]{6}$`)

// EmailTemplate is one read-only, bundle-owned email template: the fixed
// shape of an email a team sends repeatedly. The bundle's mail connector
// enforces the same file when it renders an email, so what the library shows
// here is what the renderer checks (docs/EMAIL-TEMPLATES.md).
//
// Fleet only presents a template. It never renders or checks an email itself:
// the preview is the bundle's own pre-rendered sample, shown in a sandboxed
// frame, and PromptTemplate is text for the composer like any library prompt.
type EmailTemplate struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	// Audience is "client" or "internal"; the library badges it.
	Audience string `json:"audience,omitempty"`
	UseWhen  string `json:"use_when,omitempty"`
	BasedOn  string `json:"based_on,omitempty"`
	Path     string `json:"path"`
	// MaxWords caps the prose of the whole email; 0 means no cap.
	MaxWords int                  `json:"max_words,omitempty"`
	Subject  EmailTemplateSubject `json:"subject"`
	// Rules are the writing rules; NeverSay the phrases the renderer refuses
	// (a "re:" prefix marks a regular expression).
	Rules    []string               `json:"rules,omitempty"`
	NeverSay []string               `json:"never_say,omitempty"`
	Sections []EmailTemplateSection `json:"sections"`
	// HasPreview reports that the bundle ships a rendered sample
	// (email_templates/previews/<id>.html). PreviewHTML is that file; only
	// ReadEmailTemplate fills it, so listing the library never reads or sends
	// every preview.
	HasPreview  bool   `json:"has_preview"`
	PreviewHTML string `json:"preview_html,omitempty"`
	// PreviewStyles lets the library show a chosen look on the preview; it
	// comes with PreviewHTML when the bundle ships previews/styles.json.
	PreviewStyles *EmailPreviewStyles `json:"preview_styles,omitempty"`
	// Fields and PromptTemplate are the optional form, in the form-prompt
	// schema (docs/PROMPT-LIBRARY.md, "Form prompts").
	Fields         []PromptField `json:"fields,omitempty"`
	PromptTemplate string        `json:"prompt_template,omitempty"`
}

// EmailPreviewStyles is the bundle's recolor table for its previews
// (email_templates/previews/styles.json). Fleet does not know what a theme is:
// the table says which colors of the preview become which for each value a
// form's palette field can take, and the library applies it in the browser as
// the person picks. Every color is a #RRGGBB value.
type EmailPreviewStyles struct {
	// Options maps a palette option to {preview color: its color}.
	Options map[string]map[string]string `json:"options,omitempty"`
	// Custom maps a palette's custom color name to the preview colors the
	// picked color replaces.
	Custom map[string][]string `json:"custom,omitempty"`
}

// EmailTemplateSubject is the subject-line rule.
type EmailTemplateSubject struct {
	Instruction string `json:"instruction,omitempty"`
	MaxChars    int    `json:"max_chars,omitempty"`
}

// EmailTemplateSection is one section of the email, in order.
type EmailTemplateSection struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// Heading is the fixed heading the renderer sets, when the template has one.
	Heading string `json:"heading,omitempty"`
	// Block is the mail connector's block type (text, table, chart, ...).
	Block       string `json:"block"`
	Required    bool   `json:"required"`
	Instruction string `json:"instruction,omitempty"`
	// Limits are the enforced bounds, by name (max_words, max_bullets, ...).
	Limits map[string]int `json:"limits,omitempty"`
}

type emailTemplateFile struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Audience    string `yaml:"audience"`
	UseWhen     string `yaml:"use_when"`
	BasedOn     string `yaml:"based_on"`
	MaxWords    int    `yaml:"max_words"`
	Subject     struct {
		Instruction string `yaml:"instruction"`
		MaxChars    int    `yaml:"max_chars"`
	} `yaml:"subject"`
	Rules    []string `yaml:"rules"`
	NeverSay []string `yaml:"never_say"`
	Sections []struct {
		ID          string         `yaml:"id"`
		Title       string         `yaml:"title"`
		Heading     string         `yaml:"heading"`
		Block       string         `yaml:"block"`
		Required    *bool          `yaml:"required"`
		Instruction string         `yaml:"instruction"`
		Limits      map[string]int `yaml:"limits"`
	} `yaml:"sections"`
}

// ReadEmailTemplates loads the optional email_templates/ content directory:
// one regular .yaml/.yml file per template. Symlinks and README files are
// skipped. A bad file is reported and omitted without hiding the rest, as
// ReadPrompts does. Previews are only noted (HasPreview), not read: see
// ReadEmailTemplate.
//
// The bundle's mail connector owns the template schema and validates it
// strictly; this reader takes only what the library displays, so a key it does
// not know is not an error here.
func ReadEmailTemplates(dir string) (templates []EmailTemplate, problems []string) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, []string{fmt.Sprintf("read email templates directory: %v", err)}
	}

	for _, entry := range entries {
		if len(templates) >= maxBundleEmailTemplates {
			problems = append(problems, fmt.Sprintf("email template library exceeds %d files; remaining entries skipped", maxBundleEmailTemplates))
			break
		}
		filename := entry.Name()
		ext := strings.ToLower(filepath.Ext(filename))
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || (ext != ".yaml" && ext != ".yml") {
			continue
		}
		id := strings.TrimSuffix(filename, filepath.Ext(filename))
		if strings.EqualFold(id, "readme") {
			continue
		}
		raw, problem := readBundleFile(filepath.Join(dir, filename), entry, maxBundleEmailTemplateSize)
		if problem != "" {
			problems = append(problems, fmt.Sprintf("email template %s: %s", filename, problem))
			continue
		}
		if raw == nil {
			continue
		}
		var file emailTemplateFile
		if err := yaml.Unmarshal(raw, &file); err != nil {
			msg, _, _ := strings.Cut(strings.TrimSpace(err.Error()), "\n")
			problems = append(problems, fmt.Sprintf("email template %s: the YAML does not parse: %s", filename, msg))
			continue
		}
		template := EmailTemplate{
			ID: id, Name: strings.TrimSpace(file.Name), Description: oneLine(file.Description),
			Audience: strings.TrimSpace(file.Audience), UseWhen: oneLine(file.UseWhen), BasedOn: oneLine(file.BasedOn),
			Path: "email_templates/" + filename, MaxWords: max(file.MaxWords, 0),
			Subject:  EmailTemplateSubject{Instruction: oneLine(file.Subject.Instruction), MaxChars: max(file.Subject.MaxChars, 0)},
			Rules:    trimmedNonEmpty(file.Rules),
			NeverSay: trimmedNonEmpty(file.NeverSay),
		}
		for _, s := range file.Sections {
			section := EmailTemplateSection{
				ID: strings.TrimSpace(s.ID), Title: strings.TrimSpace(s.Title), Heading: strings.TrimSpace(s.Heading),
				Block: strings.TrimSpace(s.Block), Required: s.Required == nil || *s.Required,
				Instruction: oneLine(s.Instruction), Limits: s.Limits,
			}
			if section.Title == "" {
				section.Title = section.ID
			}
			if section.ID != "" && section.Block != "" {
				template.Sections = append(template.Sections, section)
			}
		}
		if template.Name == "" || len(template.Sections) == 0 {
			problems = append(problems, fmt.Sprintf("email template %s: needs a name and at least one section with an id and a block", filename))
			continue
		}

		fields, promptTemplate, issues := parsePromptForm(raw)
		if len(issues) > 0 {
			problems = append(problems, fmt.Sprintf("email template %s: invalid form, served without one: %s", filename, strings.Join(issues, "; ")))
		} else {
			template.Fields, template.PromptTemplate = fields, promptTemplate
		}

		// Lstat, not Stat: a symlinked preview could point outside the bundle.
		if info, statErr := os.Lstat(emailTemplatePreviewPath(dir, id)); statErr == nil && info.Mode().IsRegular() {
			template.HasPreview = info.Size() <= maxBundleEmailTemplatePreview
			if !template.HasPreview {
				problems = append(problems, fmt.Sprintf("email template %s: preview exceeds %d bytes; served without one", filename, maxBundleEmailTemplatePreview))
			}
		}
		templates = append(templates, template)
	}

	sort.Slice(templates, func(i, j int) bool {
		return strings.ToLower(templates[i].Name) < strings.ToLower(templates[j].Name)
	})
	return templates, problems
}

func emailTemplatePreviewPath(dir, id string) string {
	return filepath.Join(dir, "previews", id+".html")
}

// ReadEmailTemplate returns one template with its preview. The id is matched
// against the templates the directory actually holds, never joined into a
// path as given, so it cannot name a file outside the library.
func ReadEmailTemplate(dir, id string) (EmailTemplate, bool, []string) {
	templates, problems := ReadEmailTemplates(dir)
	for _, template := range templates {
		if template.ID != id {
			continue
		}
		if template.HasPreview {
			preview, err := os.ReadFile(emailTemplatePreviewPath(dir, template.ID)) // #nosec G304 -- the id is one ReadEmailTemplates found in the operator-owned bundle directory.
			switch {
			case err != nil:
				problems = append(problems, fmt.Sprintf("email template %s: preview: %v", template.ID, err))
				template.HasPreview = false
			case len(preview) > maxBundleEmailTemplatePreview || !utf8.Valid(preview):
				problems = append(problems, fmt.Sprintf("email template %s: preview is too large or not UTF-8; served without one", template.ID))
				template.HasPreview = false
			default:
				template.PreviewHTML = string(preview)
				styles, problem := readEmailPreviewStyles(dir)
				if problem != "" {
					problems = append(problems, problem)
				}
				template.PreviewStyles = styles
			}
		}
		return template, true, problems
	}
	return EmailTemplate{}, false, problems
}

// readEmailPreviewStyles reads previews/styles.json. A missing file is not a
// problem (the preview just keeps its own colors); a file that is not exactly
// a table of #RRGGBB colors is reported and not served.
func readEmailPreviewStyles(dir string) (*EmailPreviewStyles, string) {
	path := filepath.Join(dir, "previews", "styles.json")
	// Lstat, not Stat: a symlink could point outside the bundle.
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, ""
	}
	bad := func(what string) (*EmailPreviewStyles, string) {
		return nil, "email template previews/styles.json: " + what + "; previews keep their own colors"
	}
	if info.Size() > maxBundleEmailPreviewStyles {
		return bad(fmt.Sprintf("exceeds %d bytes", maxBundleEmailPreviewStyles))
	}
	raw, err := os.ReadFile(path) // #nosec G304 -- a fixed name under the operator-owned bundle directory.
	if err != nil {
		return bad(err.Error())
	}
	var styles EmailPreviewStyles
	if err := json.Unmarshal(raw, &styles); err != nil {
		return bad("not the expected JSON")
	}
	if what := emailPreviewStylesProblem(styles); what != "" {
		return bad(what)
	}
	if len(styles.Options) == 0 && len(styles.Custom) == 0 {
		return nil, ""
	}
	return &styles, ""
}

// emailPreviewStylesProblem says what is wrong with a recolor table, or "".
func emailPreviewStylesProblem(styles EmailPreviewStyles) string {
	if len(styles.Options) > maxEmailPreviewStyleEntries || len(styles.Custom) > maxEmailPreviewStyleEntries {
		return "too many entries"
	}
	for option, table := range styles.Options {
		if len(table) > maxEmailPreviewStyleEntries {
			return fmt.Sprintf("option %q changes too many colors", option)
		}
		for from, to := range table {
			if !emailPreviewColor.MatchString(from) || !emailPreviewColor.MatchString(to) {
				return fmt.Sprintf("option %q has a value that is not a #RRGGBB color", option)
			}
		}
	}
	for name, replaced := range styles.Custom {
		if len(replaced) > maxEmailPreviewStyleEntries {
			return fmt.Sprintf("custom color %q replaces too many colors", name)
		}
		for _, color := range replaced {
			if !emailPreviewColor.MatchString(color) {
				return fmt.Sprintf("custom color %q has a value that is not a #RRGGBB color", name)
			}
		}
	}
	return ""
}

// readBundleFile reads one regular, non-empty, UTF-8 file of at most limit
// bytes. It returns (nil, "") for an entry that is not a regular file, which
// the caller skips silently, and a problem for anything it should report.
func readBundleFile(path string, entry os.DirEntry, limit int64) ([]byte, string) {
	info, err := entry.Info()
	if err != nil {
		return nil, err.Error()
	}
	if !info.Mode().IsRegular() {
		return nil, ""
	}
	if info.Size() > limit {
		return nil, fmt.Sprintf("exceeds %d bytes", limit)
	}
	raw, err := os.ReadFile(path) // #nosec G304 -- direct child of the operator-owned bundle directory.
	if err != nil {
		return nil, err.Error()
	}
	if len(raw) == 0 || !utf8.Valid(raw) {
		return nil, "is empty or not UTF-8"
	}
	return raw, ""
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func trimmedNonEmpty(in []string) []string {
	var out []string
	for _, s := range in {
		if s = oneLine(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

// EmailTemplates returns the bundle's current email templates. Like Prompts,
// it reads on each call so a config-repo pull needs no fleet restart.
func (b *Bundle) EmailTemplates() ([]EmailTemplate, []string) {
	if b == nil || b.EmailTemplatesDir == "" {
		return nil, nil
	}
	return ReadEmailTemplates(b.EmailTemplatesDir)
}

// EmailTemplate returns one template with its preview; false when the bundle
// has no template of that id.
func (b *Bundle) EmailTemplate(id string) (EmailTemplate, bool, []string) {
	if b == nil || b.EmailTemplatesDir == "" {
		return EmailTemplate{}, false, nil
	}
	return ReadEmailTemplate(b.EmailTemplatesDir, id)
}
