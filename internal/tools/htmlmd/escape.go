// Ported from github.com/JohannesKaufmann/html-to-markdown v1.6.0,
// escape/escape.go (modified: the per-call regexp hoisted). Copyright (c) 2018
// Johannes Kaufmann; see LICENSE. SPDX-License-Identifier: MIT

package htmlmd

import (
	"regexp"
	"strings"
)

var (
	escBackslash         = regexp.MustCompile(`\\(\S)`)
	escHeading           = regexp.MustCompile(`(?m)^(#{1,6} )`)
	escOrderedList       = regexp.MustCompile(`(?m)^(\W* {0,3})(\d+)\. `)
	escUnorderedList     = regexp.MustCompile(`(?m)^([^\\\w]*)[*+-] `)
	escListMarker        = regexp.MustCompile(`([*+-])`)
	escHorizontalDivider = regexp.MustCompile(`(?m)^([-*_] *){3,}$`)
	escBlockquote        = regexp.MustCompile(`(?m)^(\W* {0,3})> `)
	escLink              = regexp.MustCompile(`([\[\]])`)

	escReplacer = strings.NewReplacer(
		`*`, `\*`,
		`_`, `\_`,
		"`", "\\`",
		`|`, `\|`,
	)
)

// escapeMarkdown escapes the characters that would turn page text into
// Markdown syntax, so `<p>**Not Bold**</p>` reads `\*\*Not Bold\*\*`.
func escapeMarkdown(text string) string {
	// Escape backslash escapes!
	text = escBackslash.ReplaceAllString(text, `\\$1`)

	// Headings.
	text = escHeading.ReplaceAllString(text, `\$1`)

	// Horizontal rules.
	text = escHorizontalDivider.ReplaceAllStringFunc(text, func(t string) string {
		if strings.Contains(t, "-") {
			return strings.Replace(t, "-", `\-`, 3)
		} else if strings.Contains(t, "_") {
			return strings.Replace(t, "_", `\_`, 3)
		}
		return strings.Replace(t, "*", `\*`, 3)
	})

	// Ordered and unordered list markers.
	text = escOrderedList.ReplaceAllString(text, `$1$2\. `)
	text = escUnorderedList.ReplaceAllStringFunc(text, func(t string) string {
		return escListMarker.ReplaceAllString(t, `\$1`)
	})

	// Blockquotes.
	text = escBlockquote.ReplaceAllString(text, `$1\> `)

	// Emphasis, strong and code delimiters, and table pipes.
	text = escReplacer.Replace(text)

	// Link and image brackets.
	return escLink.ReplaceAllString(text, `\$1`)
}
