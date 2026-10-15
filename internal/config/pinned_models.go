package config

import (
	"encoding/json"
	"fmt"
	"strings"
)

// PinnedModel is one operator-pinned picker row (FLEET_PINNED_MODELS): an
// OpenRouter (or any routable) model slug the chat and task pickers list
// directly under the recommended tiers, optionally shown under an
// operator-chosen display label and a short tag pill ("alpha", "beta", …).
//
// This is a PRESENTATION alias only. The slug is still what the browser sends
// and what the run loop resolves, prices and audits — the label renames the
// row, it does not hide the slug from anyone who reads a request, an export,
// or the admin surfaces. See docs/PINNED-MODELS.md.
type PinnedModel struct {
	Slug  string `json:"slug"`
	Label string `json:"label,omitempty"`
	Tag   string `json:"tag,omitempty"`
}

// Bounds keep a pin a picker row rather than a payload: a label is a model
// name, a tag is a one-word pill.
const (
	maxPinnedModels     = 20
	maxPinnedLabelRunes = 80
	maxPinnedTagRunes   = 16
)

// parsePinnedModels decodes FLEET_PINNED_MODELS: a JSON array of
// {"slug", "label", "tag"} objects. Unset/blank is nil. A malformed value is a
// boot error — the same posture as the IP lists — so a typo surfaces at start
// instead of silently dropping the pins.
func parsePinnedModels(raw string) ([]PinnedModel, error) {
	raw = stripQuotes(strings.TrimSpace(raw))
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	var in []PinnedModel
	if err := dec.Decode(&in); err != nil {
		return nil, fmt.Errorf(`want a JSON array like [{"slug":"vendor/model","label":"Name","tag":"alpha"}]: %w`, err)
	}
	if len(in) > maxPinnedModels {
		return nil, fmt.Errorf("at most %d pinned models, got %d", maxPinnedModels, len(in))
	}
	out := make([]PinnedModel, 0, len(in))
	seen := make(map[string]bool, len(in))
	for i, p := range in {
		p.Slug = strings.TrimSpace(p.Slug)
		p.Label = strings.TrimSpace(p.Label)
		p.Tag = strings.TrimSpace(p.Tag)
		if p.Slug == "" {
			return nil, fmt.Errorf("entry %d: slug is required", i)
		}
		if strings.ContainsAny(p.Slug, " \t\r\n") {
			return nil, fmt.Errorf("entry %d: slug %q must not contain whitespace", i, p.Slug)
		}
		if seen[p.Slug] {
			return nil, fmt.Errorf("entry %d: slug %q is pinned twice", i, p.Slug)
		}
		if n := len([]rune(p.Label)); n > maxPinnedLabelRunes {
			return nil, fmt.Errorf("entry %d: label is %d characters, max %d", i, n, maxPinnedLabelRunes)
		}
		if n := len([]rune(p.Tag)); n > maxPinnedTagRunes {
			return nil, fmt.Errorf("entry %d: tag is %d characters, max %d", i, n, maxPinnedTagRunes)
		}
		seen[p.Slug] = true
		out = append(out, p)
	}
	return out, nil
}
