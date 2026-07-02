// Package truncate provides the shared rune-safe string clamp used where fleet
// bounds free text before persisting or prompting it (#595). Slicing a string
// on a raw byte boundary can split a multi-byte UTF-8 rune and emit invalid
// UTF-8 — which Postgres rejects outright for a TEXT parameter (leaving, e.g.,
// a dataset row stuck 'running' when its outcome write fails) and which
// corrupts prompt/log material. Clamp keeps the byte budget of the historical
// byte-slice call sites but always cuts on a rune boundary.
package truncate

import "unicode/utf8"

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
