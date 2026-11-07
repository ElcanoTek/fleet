// Package truncate provides the shared rune-safe string clamp used where fleet
// bounds free text before persisting or prompting it (#595). Slicing a string
// on a raw byte boundary can split a multi-byte UTF-8 rune and emit invalid
// UTF-8 — which Postgres rejects outright for a TEXT parameter (leaving, e.g.,
// a dataset row stuck 'running' when its outcome write fails) and which
// corrupts prompt/log material. Clamp keeps the byte budget of the historical
// byte-slice call sites but always cuts on a rune boundary.
package truncate

import (
	"strings"
	"unicode/utf8"
)

// Clamp returns s unchanged when it fits in maxBytes bytes; otherwise it cuts
// s at the largest rune boundary <= maxBytes and appends marker. The budget
// bounds only s (the marker rides on top), matching the byte-slice call sites
// this replaces. Input that is already invalid UTF-8 is cut at maxBytes after
// at most utf8.UTFMax-1 backoff steps — Clamp never makes a string less valid
// than it was.
func Clamp(s string, maxBytes int, marker string) string {
	if maxBytes < 0 {
		maxBytes = 0
	}
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for steps := 0; steps < utf8.UTFMax-1 && cut > 0 && !utf8.RuneStart(s[cut]); steps++ {
		cut--
	}
	return s[:cut] + marker
}

// Runes returns s unchanged when it has at most n runes; otherwise it keeps the
// first n runes and appends marker. Use it where the budget is a count of
// characters a person reads (a card line, a progress preview) rather than
// bytes.
func Runes(s string, n int, marker string) string {
	if n < 0 {
		n = 0
	}
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	i := 0
	for count := 0; count < n; count++ {
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
	}
	return s[:i] + marker
}

// Tail returns the last at most maxBytes bytes of s, starting on a rune
// boundary — the tail half of a head-and-tail clamp (pair it with Clamp for
// the head). Like Clamp it never makes a string less valid than it was.
func Tail(s string, maxBytes int) string {
	if maxBytes < 0 {
		maxBytes = 0
	}
	if len(s) <= maxBytes {
		return s
	}
	cut := len(s) - maxBytes
	for steps := 0; steps < utf8.UTFMax-1 && cut < len(s) && !utf8.RuneStart(s[cut]); steps++ {
		cut++
	}
	return s[cut:]
}

// TailRunes returns s unchanged when it has at most n runes; otherwise it
// keeps the last n runes. Use it where the end of the text is what matters (a
// live preview, the close of a transcript) and the budget is characters.
func TailRunes(s string, n int) string {
	if n < 0 {
		n = 0
	}
	count := utf8.RuneCountInString(s)
	if count <= n {
		return s
	}
	i := 0
	for skip := count - n; skip > 0; skip-- {
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
	}
	return s[i:]
}

// Ends returns s unchanged when it has at most head+tail runes; otherwise it
// keeps the first head runes and the last tail runes with marker between
// them, dropping the middle. head 0 gives a marker-led tail ("…" + the last
// tail runes).
func Ends(s string, head, tail int, marker string) string {
	if head < 0 {
		head = 0
	}
	if tail < 0 {
		tail = 0
	}
	if utf8.RuneCountInString(s) <= head+tail {
		return s
	}
	return Runes(s, head, "") + marker + TailRunes(s, tail)
}

// OneLine folds every run of whitespace (newlines included) into a single
// space and trims both ends, so multi-line text renders on one line.
func OneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
