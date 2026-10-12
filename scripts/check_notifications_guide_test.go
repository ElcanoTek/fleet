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
	if end := strings.Index(general, "</SetSection>"); end >= 0 {
		general = general[:end]
	}
	if !strings.Contains(general, "<BrowserNotificationsRow") {
		t.Fatal("the General settings section no longer renders <BrowserNotificationsRow />: update this check and every document below to the switch's new home")
	}
	row := readFile(t, root, filepath.Join("web", "src", "app", "settings", "BrowserNotificationsRow.tsx"))
	if !strings.Contains(row, `label="Browser notifications"`) {
		t.Fatal(`BrowserNotificationsRow no longer carries label="Browser notifications": the documents name it by that label`)
	}

	// A path can wrap across Markdown lines, so whitespace runs are folded
	// before matching.
	path := regexp.MustCompile(`Settings → (\S+) → Browser notifications`)
	found := 0
	for _, doc := range []string{
		filepath.Join("docs", "PUSH-NOTIFICATIONS.md"),
		filepath.Join("internal", "clientconfig", "builtin_skills", "fleet-guide", "chat.md"),
		filepath.Join("internal", "clientconfig", "builtin_skills", "fleet-guide", "operations-center.md"),
		filepath.Join("web", "src", "app", "help", "guides", "chat.md"),
		filepath.Join("web", "src", "app", "help", "guides", "operations-center.md"),
	} {
		text := strings.Join(strings.Fields(readFile(t, root, doc)), " ")
		for _, m := range path.FindAllStringSubmatch(text, -1) {
			found++
			if m[1] != "General" {
				t.Errorf("%s sends people to Settings → %s → Browser notifications; the switch is under Settings → General", doc, m[1])
			}
		}
	}
	if found == 0 {
		t.Fatal("no document names the Browser notifications switch's path any more — the user guide should")
	}
}
