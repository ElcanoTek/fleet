// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package scripts

import (
	"html"
	"path/filepath"
	"regexp"
	"slices"
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

// The switch's description is what a person reads when deciding whether to
// turn alerts on, and it fell behind the pushes fleet sends: it named finished
// and waiting tasks and approvals after the trigger matrix in
// docs/PUSH-NOTIFICATIONS.md had grown progress notes, budget alerts, an
// approved action's outcome and a chat that did not continue on its own. So:
// every push title in the matrix must be one pushDescribedAs knows, the words
// it maps to must be in the description, and every title it knows must still
// be in the matrix.
func TestBrowserNotificationsSwitchNamesEveryPush(t *testing.T) {
	root := repoRoot(t)
	titles := triggerMatrixTitles(t, readFile(t, root, filepath.Join("docs", "PUSH-NOTIFICATIONS.md")))
	desc := browserNotificationsDesc(t, readFile(t, root, filepath.Join("web", "src", "app", "settings", "BrowserNotificationsRow.tsx")))

	for _, title := range titles {
		phrases, ok := pushDescribedAs[title]
		if !ok {
			t.Errorf("the trigger matrix in docs/PUSH-NOTIFICATIONS.md sends a push titled %q that this check does not know: "+
				"say when it arrives in BrowserNotificationsRow's description, then add it to pushDescribedAs", title)
			continue
		}
		for _, phrase := range phrases {
			if !strings.Contains(desc, phrase) {
				t.Errorf("BrowserNotificationsRow's description does not say %q, the words that name the %q push:\n%s", phrase, title, desc)
			}
		}
	}
	for title := range pushDescribedAs {
		if !slices.Contains(titles, title) {
			t.Errorf("pushDescribedAs knows a push titled %q that the trigger matrix no longer lists: "+
				"if fleet stopped sending it, take it out of BrowserNotificationsRow's description and out of pushDescribedAs", title)
		}
	}
}

// pushDescribedAs maps each push title in the trigger matrix (the words
// before its colon) to the words the switch's description names it with.
var pushDescribedAs = map[string][]string{
	"✓ Task complete":           {"a task you created finishes"},
	"✗ Task failed":             {"a task you created finishes"},
	"⏸ Waiting for your answer": {"is waiting for your answer"}, // an ask pause
	"✎ Progress note":           {"sends a progress note"},      // the agent's mid-run notify tool
	"⚠ Budget alert":            {"past your budget's soft limit"},
	"⚠ Approval needed":         {"needs your approval"},
	"✓ Done":                    {"an approved one finishes"},
	"✗ Not applied":             {"an approved one finishes"},
	// The chat, after any settled card of an action that carries on by itself
	// (noteApprovalSettled runs on a decline and a time-out too), so the
	// description must not tie it to approvals alone.
	"⏸ Waiting for you": {"does not continue on its own", "approved, declined or timed out"},
}

// backtickedSpan is one `code span` in a Markdown table cell.
var backtickedSpan = regexp.MustCompile("`([^`]+)`")

// triggerMatrixTitles returns the title of every push the trigger matrix
// lists: each code span in its Notification column, up to the colon.
func triggerMatrixTitles(t *testing.T, doc string) []string {
	t.Helper()
	_, section, ok := strings.Cut(doc, "\n## Trigger matrix\n")
	if !ok {
		t.Fatal(`docs/PUSH-NOTIFICATIONS.md has no "## Trigger matrix" section — the doc's shape changed; fix this check rather than deleting it`)
	}
	var rows []string
	for _, line := range strings.Split(section, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "|") {
			rows = append(rows, line)
		} else if len(rows) > 0 {
			break
		}
	}
	if len(rows) < 3 {
		t.Fatal("the trigger matrix in docs/PUSH-NOTIFICATIONS.md is not a table with a header and rows — fix this check rather than deleting it")
	}
	col := slices.Index(tableCells(rows[0]), "Notification")
	if col < 0 {
		t.Fatal(`the trigger matrix in docs/PUSH-NOTIFICATIONS.md has no "Notification" column — fix this check rather than deleting it`)
	}
	var titles []string
	for _, row := range rows[2:] {
		cells := tableCells(row)
		if col >= len(cells) {
			t.Fatalf("trigger matrix row %q has no Notification cell", row)
		}
		spans := backtickedSpan.FindAllStringSubmatch(cells[col], -1)
		if len(spans) == 0 {
			t.Errorf("trigger matrix row %q names no notification in backticks", row)
		}
		for _, m := range spans {
			title, _, _ := strings.Cut(m[1], ":")
			titles = append(titles, strings.TrimSpace(title))
		}
	}
	return titles
}

// tableCells splits one Markdown table row into its trimmed cells.
func tableCells(row string) []string {
	cells := strings.Split(strings.Trim(row, "|"), "|")
	for i := range cells {
		cells[i] = strings.TrimSpace(cells[i])
	}
	return cells
}

// browserNotificationsDesc returns the switch's description as a reader sees
// it: the text that opens the row's desc fragment, up to the first JSX
// expression (the note under it), with entities decoded and wrapping folded.
func browserNotificationsDesc(t *testing.T, row string) string {
	t.Helper()
	_, rest, ok := strings.Cut(row, "desc={")
	if ok {
		_, rest, ok = strings.Cut(rest, "<>")
	}
	text, _, found := strings.Cut(rest, "{")
	if !ok || !found || strings.TrimSpace(text) == "" {
		t.Fatal("BrowserNotificationsRow's desc no longer opens with plain text in a fragment (desc={<> text {note …}</>}) — the row's shape changed; fix this check rather than deleting it")
	}
	return strings.Join(strings.Fields(html.UnescapeString(text)), " ")
}
