package config

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

// PinnedModel is one operator-pinned picker row (FLEET_PINNED_MODELS): an
// OpenRouter (or any routable) model slug the chat and task pickers list
// directly under the recommended tiers, optionally shown under an
// operator-chosen display label, a short tag pill ("alpha", "beta", …), and
// a displayed price: "free", or per-million input / output USD overrides.
//
// This is a PRESENTATION alias only. The slug is still what the browser sends
// and what the run loop resolves, prices and audits — the label renames the
// row, it does not hide the slug from anyone who reads a request, an export,
// or the admin surfaces. Likewise the price fields (and free) change only the cost band
// the pickers show: metering, cost ceilings and recorded spend keep using the
// provider's real price. See docs/PINNED-MODELS.md.
type PinnedModel struct {
	Slug  string `json:"slug"`
	Label string `json:"label,omitempty"`
	Tag   string `json:"tag,omitempty"`
	// Free shows the row as "Free" instead of a cost band. Exclusive with the
	// two price fields — one way to say it, so the payload cannot disagree.
	Free bool `json:"free,omitempty"`
	// Pointers so "not overridden" (nil) differs from "shown as free" (0).
	InputPricePerMtok  *float64 `json:"input_price_per_mtok,omitempty"`
	OutputPricePerMtok *float64 `json:"output_price_per_mtok,omitempty"`
}

// Bounds keep a pin a picker row rather than a payload: a label is a model
// name, a tag is a one-word pill.
const (
	maxPinnedModels     = 20
	maxPinnedLabelRunes = 80
	maxPinnedTagRunes   = 16
	// A displayed price above this is a typo (per-token instead of
	// per-million, or an extra zero), not a real model's list price.
	maxPinnedPricePerMtok = 10_000
)

// parsePinnedModels decodes FLEET_PINNED_MODELS: a JSON array of
// {"slug", "label", "tag", "free", "input_price_per_mtok",
// "output_price_per_mtok"} objects. Unset/blank is nil. A malformed value is a
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
		for _, f := range []struct {
			name string
			v    *float64
		}{{"input_price_per_mtok", p.InputPricePerMtok}, {"output_price_per_mtok", p.OutputPricePerMtok}} {
			if f.v != nil && (math.IsNaN(*f.v) || *f.v < 0 || *f.v > maxPinnedPricePerMtok) {
				return nil, fmt.Errorf("entry %d: %s must be between 0 and %d USD per million tokens", i, f.name, maxPinnedPricePerMtok)
			}
		}
		if p.Free && (p.InputPricePerMtok != nil || p.OutputPricePerMtok != nil) {
			return nil, fmt.Errorf("entry %d: free and the price fields are mutually exclusive", i)
		}
		seen[p.Slug] = true
		out = append(out, p)
	}
	return out, nil
}
