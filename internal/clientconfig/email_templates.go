package clientconfig

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/goccy/go-yaml"
)

const (
	maxBundleEmailTemplates       = 64
	maxBundleEmailTemplateSize    = 256 << 10
	maxBundleEmailTemplatePreview = 1 << 20
)

// EmailTemplate is one read-only, bundle-owned email template: the fixed
// shape of an email a team sends repeatedly. The bundle's mail connector
// enforces the same file when it renders an email, so what the library shows
// here is what the renderer checks (docs/EMAIL-TEMPLATES.md).
//
// Fleet only presents a template. It never renders or checks an email itself:
// PreviewHTML is the bundle's own pre-rendered sample, shown in a sandboxed
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
	// PreviewHTML is the bundle's rendered sample
	// (email_templates/previews/<id>.html); empty when the bundle ships none.
	PreviewHTML string `json:"preview_html,omitempty"`
	// Fields and PromptTemplate are the optional form, in the form-prompt
	// schema (docs/PROMPT-LIBRARY.md, "Form prompts").
	Fields         []PromptField `json:"fields,omitempty"`
	PromptTemplate string        `json:"prompt_template,omitempty"`
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
// one regular .yaml/.yml file per template, plus previews/<id>.html beside
// them. Symlinks and README files are skipped. A bad file is reported and
// omitted without hiding the rest, as ReadPrompts does.
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

		previewPath := filepath.Join(dir, "previews", id+".html")
		// Lstat, not Stat: a symlinked preview could point outside the bundle.
		if info, statErr := os.Lstat(previewPath); statErr == nil && info.Mode().IsRegular() {
			if info.Size() > maxBundleEmailTemplatePreview {
				problems = append(problems, fmt.Sprintf("email template %s: preview exceeds %d bytes; served without one", filename, maxBundleEmailTemplatePreview))
			} else if preview, readErr := os.ReadFile(previewPath); readErr != nil { // #nosec G304 -- a fixed child of the operator-owned bundle directory.
				problems = append(problems, fmt.Sprintf("email template %s: preview: %v", filename, readErr))
			} else if !utf8.Valid(preview) {
				problems = append(problems, fmt.Sprintf("email template %s: preview is not UTF-8; served without one", filename))
			} else {
				template.PreviewHTML = string(preview)
			}
		}
		templates = append(templates, template)
	}

	sort.Slice(templates, func(i, j int) bool {
		return strings.ToLower(templates[i].Name) < strings.ToLower(templates[j].Name)
	})
	return templates, problems
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
