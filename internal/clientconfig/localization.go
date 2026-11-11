package clientconfig

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// Localizations contains translations of bundle-owned display text. Catalogs
// live outside manifest.yaml so older Fleet releases can still load a bundle.
// They never change connector names, form keys, paths, or stored user content.
type Localizations map[string]map[string]string

var localizationSlot = regexp.MustCompile(`\{[A-Za-z_0-9]+\}`)

func localizationSlots(text string) []string {
	slots := localizationSlot.FindAllString(text, -1)
	slices.Sort(slots)
	return slots
}

func loadLocalizations(dir string) (Localizations, error) {
	out := Localizations{}
	for _, language := range []string{"es", "pt"} {
		path := filepath.Join(dir, "locales", language+".json")
		data, err := os.ReadFile(path) // #nosec G304 — fixed filename under the operator-selected bundle.
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read bundle locale %s: %w", language, err)
		}
		var messages map[string]string
		if err := json.Unmarshal(data, &messages); err != nil {
			return nil, fmt.Errorf("parse bundle locale %s: %w", language, err)
		}
		if messages == nil {
			return nil, fmt.Errorf("bundle locale %s must be a JSON object", language)
		}
		for source, translated := range messages {
			if strings.TrimSpace(source) == "" || strings.TrimSpace(translated) == "" {
				return nil, fmt.Errorf("bundle locale %s contains an empty message", language)
			}
			if !slices.Equal(localizationSlots(source), localizationSlots(translated)) {
				return nil, fmt.Errorf("bundle locale %s changes interpolation fields for %q", language, source)
			}
		}
		out[language] = messages
	}
	return out, nil
}

// PublicLocalizations returns ONLY the copy already exposed by /brand/meta.
// Prompt cards, connector metadata, and other workspace copy stay behind the
// member-gated /client-config route, including their source-text keys.
func (b *Bundle) PublicLocalizations() Localizations {
	if b == nil {
		return nil
	}
	out := Localizations{}
	for language, messages := range b.Localizations {
		public := map[string]string{}
		for _, source := range []string{b.Branding.LoginTitle, b.Branding.LoginTagline, b.Branding.ShareTitle, b.Branding.ShareDescription} {
			if translated, ok := messages[source]; ok && source != "" {
				public[source] = translated
			}
		}
		if len(public) > 0 {
			out[language] = public
		}
	}
	return out
}

func loadBundleLocalizations(b *Bundle) (*Bundle, error) {
	var err error
	b.Localizations, err = loadLocalizations(b.Dir)
	if err != nil {
		return nil, err
	}
	return b, nil
}
