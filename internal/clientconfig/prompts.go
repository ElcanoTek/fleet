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
	maxBundlePrompts    = 256
	maxBundlePromptSize = 256 << 10
)

// Prompt is one read-only, bundle-owned prompt-library entry. Content is the
// exact file body: structured YAML prompts stay structured when inserted into
// chat or a scheduled task, rather than being flattened and losing intent.
type Prompt struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Content     string `json:"content"`
	Source      string `json:"source"`
	Visibility  string `json:"visibility"`
	ReadOnly    bool   `json:"read_only"`
	Path        string `json:"path,omitempty"`
}

// ReadPrompts loads the optional prompts/ content directory. Only regular
// .yaml/.yml/.md/.txt files are accepted; symlinks and README files are skipped.
// A bad individual file is reported and omitted without hiding the rest of the
// library, matching the bundle skills loader's degrade-loud behavior.
func ReadPrompts(dir string) (prompts []Prompt, problems []string) {
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, []string{fmt.Sprintf("read prompts directory: %v", err)}
	}

	for _, entry := range entries {
		if len(prompts) >= maxBundlePrompts {
			problems = append(problems, fmt.Sprintf("prompt library exceeds %d files; remaining entries skipped", maxBundlePrompts))
			break
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		filename := entry.Name()
		ext := strings.ToLower(filepath.Ext(filename))
		if ext != ".yaml" && ext != ".yml" && ext != ".md" && ext != ".txt" {
			continue
		}
		if strings.EqualFold(strings.TrimSuffix(filename, ext), "readme") {
			continue
		}
		info, statErr := entry.Info()
		if statErr != nil {
			problems = append(problems, fmt.Sprintf("prompt %s: %v", filename, statErr))
			continue
		}
		if !info.Mode().IsRegular() {
			continue
		}
		if info.Size() > maxBundlePromptSize {
			problems = append(problems, fmt.Sprintf("prompt %s exceeds %d bytes", filename, maxBundlePromptSize))
			continue
		}
		path := filepath.Join(dir, filename)
		raw, readErr := os.ReadFile(path) // #nosec G304 -- direct child of the operator-owned bundle directory.
		if readErr != nil {
			problems = append(problems, fmt.Sprintf("prompt %s: %v", filename, readErr))
			continue
		}
		if len(raw) == 0 || !utf8.Valid(raw) {
			problems = append(problems, fmt.Sprintf("prompt %s is empty or not UTF-8", filename))
			continue
		}
		displayName, description := promptMetadata(filename, ext, raw)
		prompts = append(prompts, Prompt{
			ID: "git:" + filename, Name: displayName, Description: description,
			Content: string(raw), Source: "git", Visibility: "workspace",
			ReadOnly: true, Path: "prompts/" + filename,
		})
	}

	sort.Slice(prompts, func(i, j int) bool {
		return strings.ToLower(prompts[i].Name) < strings.ToLower(prompts[j].Name)
	})
	return prompts, problems
}

func promptMetadata(filename, ext string, raw []byte) (string, string) {
	name := strings.TrimSuffix(filename, ext)
	name = strings.TrimSpace(strings.NewReplacer("_", " ", "-", " ").Replace(name))
	var description string
	if ext == ".yaml" || ext == ".yml" {
		var header struct {
			Name        string `yaml:"name"`
			Description string `yaml:"description"`
			Goal        string `yaml:"goal"`
		}
		if yaml.Unmarshal(raw, &header) == nil {
			if strings.TrimSpace(header.Name) != "" {
				name = strings.TrimSpace(header.Name)
			}
			description = strings.TrimSpace(header.Description)
			if description == "" {
				description = strings.TrimSpace(header.Goal)
			}
		}
	} else {
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "# ") {
				name = strings.TrimSpace(strings.TrimPrefix(line, "# "))
				continue
			}
			if line != "" && !strings.HasPrefix(line, "#") {
				description = line
				break
			}
		}
	}
	description = strings.Join(strings.Fields(description), " ")
	if runes := []rune(description); len(runes) > 240 {
		description = string(runes[:239]) + "…"
	}
	return name, description
}

// Prompts returns the current Git-backed prompt library. It intentionally reads
// on each call so updating the external config checkout does not require a fleet
// restart.
func (b *Bundle) Prompts() ([]Prompt, []string) {
	if b == nil || b.PromptsDir == "" {
		return nil, nil
	}
	return ReadPrompts(b.PromptsDir)
}
