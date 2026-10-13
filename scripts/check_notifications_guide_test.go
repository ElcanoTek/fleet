// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package scripts

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The browser-notification switch moved from Settings → Connections to
// Settings → General when the settings pages were redesigned, and the user
// guide and the push design note went on sending people to Connections for it,
// a page that no longer has it. So: every written path to the switch must name
// the section the settings page actually renders it in.
func TestDocsNameWhereTheBrowserNotificationsSwitchIs(t *testing.T) {
	root := repoRoot(t)

	// The authority: the General settings page renders the row, and the row's
	// label is the name the documents use.
	page := readFile(t, root, filepath.Join("web", "src", "app", "settings", "page.tsx"))
	start := strings.Index(page, `title="General"`)
	if start < 0 {
		t.Fatal(`settings/page.tsx has no title="General" section — the page's shape changed; fix this check rather than deleting it`)
	}
	general := page[start:]
	end := strings.Index(general, "</SetSection>")
	if end < 0 {
		t.Fatal(`settings/page.tsx's General section has no closing </SetSection> — the page's shape changed; fix this check rather than deleting it`)
	}
	if !strings.Contains(general[:end], "<BrowserNotificationsRow") {
		t.Fatal("the General settings section no longer renders <BrowserNotificationsRow />: update this check and every document below to the switch's new home")
	}
	row := readFile(t, root, filepath.Join("web", "src", "app", "settings", "BrowserNotificationsRow.tsx"))
	if !strings.Contains(row, `label="Browser notifications"`) {
		t.Fatal(`BrowserNotificationsRow no longer carries label="Browser notifications": the documents name it by that label`)
	}

	skillGuide := filepath.Join("internal", "clientconfig", "builtin_skills", "fleet-guide", "chat.md")
	for _, doc := range []string{
		filepath.Join("docs", "PUSH-NOTIFICATIONS.md"),
		skillGuide,
		filepath.Join("internal", "clientconfig", "builtin_skills", "fleet-guide", "operations-center.md"),
		filepath.Join("web", "src", "app", "help", "guides", "chat.md"),
		filepath.Join("web", "src", "app", "help", "guides", "operations-center.md"),
	} {
		sections := switchPathSections(readFile(t, root, doc))
		if doc == skillGuide && len(sections) == 0 {
			t.Errorf("%s no longer tells people where the Browser notifications switch is; the user guide should", doc)
		}
		for _, section := range sections {
			if !strings.EqualFold(section, "General") {
				t.Errorf("%s sends people to Settings → %s → Browser notifications; the switch is under Settings → General.\n"+
					"For a guide, edit internal/clientconfig/builtin_skills/fleet-guide/ and run `make sync-guides`.", doc, section)
			}
		}
	}
}

// switchPathSegment matches "Settings <sep> <section> <sep> Browser
// notifications" once emphasis and line wrapping are folded away, with any of
// the separators the docs use (→, ->, >) and a section of any length.
var switchPathSegment = regexp.MustCompile(`(?i)settings\s*(?:→|->|>)\s*([^→>]+?)\s*(?:→|->|>)\s*browser notifications`)

// switchPathSections returns the section each written path to the switch
// names. Markdown emphasis is stripped (a path may be bolded as a whole or
// part by part) and whitespace runs are folded (a path may wrap across lines).
func switchPathSections(markdown string) []string {
	text := strings.NewReplacer("*", "", "_", "").Replace(markdown)
	text = strings.Join(strings.Fields(text), " ")
	matches := switchPathSegment.FindAllStringSubmatch(text, -1)
	sections := make([]string, 0, len(matches))
	for _, m := range matches {
		sections = append(sections, strings.TrimSpace(m[1]))
	}
	return sections
}

// The matcher must see every way a document writes the path, or a wrong
// section slips past the check above.
func TestSwitchPathSections(t *testing.T) {
	for _, tc := range []struct {
		doc  string
		want []string
	}{
		{"**Settings → General → Browser notifications**", []string{"General"}},
		{"(**Settings → Connections → Browser\n   notifications**), you also", []string{"Connections"}},
		{"**Settings** → **Shared files** → **Browser notifications**", []string{"Shared files"}},
		{"settings -> general -> browser notifications", []string{"general"}},
		{"Settings > General > Browser notifications", []string{"General"}},
		{"Settings → Admin → Users, and Browser notifications elsewhere", nil},
	} {
		got := switchPathSections(tc.doc)
		if strings.Join(got, "|") != strings.Join(tc.want, "|") {
			t.Errorf("switchPathSections(%q) = %q, want %q", tc.doc, got, tc.want)
		}
	}
}
