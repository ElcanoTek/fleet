package clientconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goccy/go-yaml"
)

// The core directory and default bundle ship with Fleet, so additions must
// have translations in the same PR. Customer bundles own their own catalogs.
func TestCoreMetadataLocalizations(t *testing.T) {
	t.Parallel()
	messages := map[string]bool{}
	add := func(values ...string) {
		for _, value := range values {
			if strings.TrimSpace(value) != "" {
				messages[value] = true
			}
		}
	}
	entries, err := loadBuiltinRemoteCatalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		add(entry.DisplayName, entry.Description, entry.SetupHint)
		for _, variant := range entry.URLVariants {
			add(variant.Label)
		}
	}
	root := filepath.Join("..", "..")
	for _, dir := range []string{"builtin_skills", filepath.Join(root, "config/default/skills")} {
		skills, problems := ReadSkills(dir)
		if len(problems) != 0 {
			t.Fatal(problems)
		}
		for _, skill := range skills {
			add(skill.Description)
		}
	}
	readDefaultLocalizationSources(t, root, add)
	for _, language := range []string{"es", "pt"} {
		data, err := os.ReadFile(filepath.Join(root, "web/src/app/lib/i18n", language+".json"))
		if err != nil {
			t.Fatal(err)
		}
		var catalog map[string]string
		if err := json.Unmarshal(data, &catalog); err != nil {
			t.Fatal(err)
		}
		for message := range messages {
			if strings.TrimSpace(catalog[message]) == "" {
				t.Errorf("%s missing core metadata translation: %q", language, message)
			}
		}
	}
}

func readDefaultLocalizationSources(t *testing.T, root string, add func(...string)) {
	t.Helper()
	dir := filepath.Join(root, "config/default")
	data, err := os.ReadFile(filepath.Join(dir, "manifest.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]any
	if err := yaml.Unmarshal(data, &manifest); err != nil {
		t.Fatal(err)
	}
	for _, section := range []string{"branding", "empty_state", "task_templates"} {
		collectDefaultDisplayText(manifest[section], add)
	}
	var bundle Bundle
	var branding struct {
		Branding Branding `yaml:"branding"`
	}
	if err := yaml.Unmarshal(data, &branding); err != nil {
		t.Fatal(err)
	}
	bundle.Branding = branding.Branding
	for _, category := range bundle.ConnectorCategories() {
		add(category.Name)
	}
	prompts, problems := ReadPrompts(filepath.Join(dir, "prompts"))
	if len(problems) != 0 {
		t.Fatal(problems)
	}
	for _, prompt := range prompts {
		add(prompt.Name, prompt.Description, prompt.PromptTemplate)
		for _, field := range prompt.Fields {
			add(field.Label, field.Placeholder, field.Hint)
			add(field.Options...)
		}
	}
}

func collectDefaultDisplayText(value any, add func(...string)) {
	switch typed := value.(type) {
	case []any:
		for _, child := range typed {
			collectDefaultDisplayText(child, add)
		}
	case map[string]any:
		for key, child := range typed {
			switch key {
			case "login_title", "login_tagline", "share_title", "share_description",
				"title", "section", "desc", "cta", "promptTemplate", "starterPrompt",
				"label", "placeholder", "hint", "name", "description", "prompt":
				if text, ok := child.(string); ok {
					add(text)
				}
			default:
				collectDefaultDisplayText(child, add)
			}
		}
	}
}
