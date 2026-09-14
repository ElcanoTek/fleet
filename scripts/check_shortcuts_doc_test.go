// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package scripts

import (
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// A shortcut table is prose that claims to describe wiring, and prose does not
// compile. docs/keyboard-shortcuts.md advertised Mod+F, Mod+N and Mod+J for
// months after the app stopped binding them (they are browser-reserved — Chrome
// will not even let a page intercept Mod+N), and in 2026-09 that stale table
// was copied in good faith into the user guide, where it would have told real
// users to press keys that open a download panel.
//
// So: every modifier chord any of these documents names must be one the shell
// actually binds. The authority is the help-overlay catalog in
// chat-experience.tsx — the same list the in-app "?" overlay renders, which
// KeyboardShortcutsOverlay.tsx already documents as the single source of truth.
//
// Scope is deliberately one-directional and limited to chords carrying a
// modifier: a doc may omit a binding (not every page lists every key), but it
// may never invent one. Bare letters are left out — they are unambiguous in
// context and have never been the failure mode.
func TestShortcutDocsOnlyNameRealChords(t *testing.T) {
	root := repoRoot(t)

	wired := wiredChords(t, root)
	if len(wired) < 4 {
		t.Fatalf("parsed only %d modifier chords from the help-overlay catalog (%v) — "+
			"the catalog's shape probably changed; fix this parser rather than deleting the test",
			len(wired), sortedKeys(wired))
	}

	for _, doc := range []string{
		filepath.Join("docs", "keyboard-shortcuts.md"),
		filepath.Join("internal", "clientconfig", "builtin_skills", "fleet-guide", "chat.md"),
		filepath.Join("web", "src", "app", "help", "guides", "chat.md"),
	} {
		for _, chord := range documentedChords(t, root, doc) {
			if !wired[chord] {
				t.Errorf("%s names the shortcut %q, which the app does not bind.\n"+
					"Wired modifier chords are: %v.\n"+
					"Fix the document (or wire the chord) — a shortcut table that lies "+
					"sends a user's keystroke to their browser instead.",
					doc, chord, sortedKeys(wired))
			}
		}
	}
}

// chipsBlock matches one `chips: [...]` entry of the help-overlay catalog.
var chipsBlock = regexp.MustCompile(`chips:\s*\[([^\]]*)\]`)

// chipLabel matches a literal chip, e.g. `{ label: "K" }`.
var chipLabel = regexp.MustCompile(`label:\s*"([^"]+)"`)

// wiredChords reads the help-overlay catalog and returns the normalized
// modifier chords it lists.
func wiredChords(t *testing.T, root string) map[string]bool {
	t.Helper()
	src := readFile(t, root, filepath.Join("web", "src", "app", "chat", "ui", "chat-experience.tsx"))
	out := map[string]bool{}
	for _, m := range chipsBlock.FindAllStringSubmatch(src, -1) {
		body := m[1]
		var parts []string
		if strings.Contains(body, "mod: true") {
			parts = append(parts, "MOD")
		}
		if strings.Contains(body, "shift: true") {
			parts = append(parts, "SHIFT")
		}
		if len(parts) == 0 {
			continue // bare key — out of scope, see the doc comment.
		}
		for _, label := range chipLabel.FindAllStringSubmatch(body, -1) {
			parts = append(parts, strings.ToUpper(label[1]))
		}
		out[strings.Join(parts, "+")] = true
	}
	return out
}

// docChord matches a backticked key chip in a Markdown table cell, in either
// spelling the docs use: `⌘ ⇧ O` / `⇧ Esc` (the user guide) or
// `Mod` + `Shift` + `O` (docs/keyboard-shortcuts.md).
var docChord = regexp.MustCompile("`([^`]+)`(?:\\s*\\+\\s*`([^`]+)`)?(?:\\s*\\+\\s*`([^`]+)`)?")

// documentedChords returns the normalized modifier chords a Markdown document's
// tables name. Only the first cell of a table row is read: later cells are
// prose, where a chord may legitimately be discussed (including the three this
// repo deliberately leaves to the browser).
func documentedChords(t *testing.T, root, rel string) []string {
	t.Helper()
	var chords []string
	for _, line := range strings.Split(readFile(t, root, rel), "\n") {
		if !strings.HasPrefix(strings.TrimSpace(line), "|") {
			continue
		}
		cells := strings.Split(strings.Trim(strings.TrimSpace(line), "|"), "|")
		if len(cells) < 2 {
			continue
		}
		for _, m := range docChord.FindAllStringSubmatch(cells[0], -1) {
			var parts []string
			for _, raw := range m[1:] {
				for _, token := range strings.Fields(raw) {
					switch strings.ToUpper(token) {
					case "":
					case "⌘", "MOD", "CTRL":
						parts = append(parts, "MOD")
					case "⇧", "SHIFT":
						parts = append(parts, "SHIFT")
					default:
						parts = append(parts, strings.ToUpper(token))
					}
				}
			}
			chord := strings.Join(parts, "+")
			if strings.Contains(chord, "MOD") || strings.Contains(chord, "SHIFT") {
				chords = append(chords, chord)
			}
		}
	}
	return chords
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
