// Package pgtext handles text Postgres cannot store as given.
//
// Postgres refuses U+0000 in a text value (SQLSTATE 22021), and jsonb (not
// json) also refuses the JSON sequence \u0000 (22P05). fleet stores free-form
// text it does not control — what a user types, what a model writes, a webhook
// payload — so the store layers pass the content fields they write through
// Content (text) or JSON (JSON text bound for jsonb), which remove the
// character. The character carries no meaning in text.
//
// Keys are never changed: ids, emails, issuers, subjects, slugs, tokens and
// the names a row is looked up by go to Postgres exactly as given, so a key
// holding U+0000 fails its query and can never match a row stored under a
// different key. Identity values are checked where they enter fleet instead
// (ValidIdentity), so they get a clear refusal rather than a failed query.
package pgtext

import (
	"bytes"
	"strings"
	"unicode"
	"unicode/utf8"
)

// jsonNULSequence is how JSON writes U+0000.
const jsonNULSequence = `\u0000`

var jsonNULSequenceBytes = []byte(jsonNULSequence)

// Content returns s without U+0000, for a free-form text field about to be
// stored. It returns s itself when there is nothing to remove.
func Content(s string) string {
	if strings.IndexByte(s, 0) < 0 {
		return s
	}
	return strings.ReplaceAll(s, "\x00", "")
}

// JSON returns the JSON text s without U+0000, for a jsonb content column: the
// 0x00 bytes go first, then the \u0000 sequences, so 0x00 bytes inside a
// sequence cannot hide it. A backslash always starts a two-character sequence,
// so a doubled backslash followed by the characters u0000 is text and is kept.
// It returns s itself when there is nothing to remove.
func JSON(s string) string {
	if strings.IndexByte(s, 0) < 0 && !strings.Contains(s, jsonNULSequence) {
		return s
	}
	b := []byte(Content(s))
	if !bytes.Contains(b, jsonNULSequenceBytes) {
		return string(b)
	}
	out := b[:0] // in place: the write index never passes the read index
	for i := 0; i < len(b); i++ {
		c := b[i]
		if c == '\\' && i+1 < len(b) {
			if bytes.HasPrefix(b[i:], jsonNULSequenceBytes) {
				i += len(jsonNULSequence) - 1
				continue
			}
			out = append(out, c, b[i+1])
			i++
			continue
		}
		out = append(out, c)
	}
	return string(out)
}

// ValidIdentity reports whether s may be used as an identity value — an
// email (through CheckedEmail), an identity provider's subject or issuer, a
// tenant. It refuses:
//
//   - a control character (Unicode Cc, U+0000 among them) or a format
//     character (Cf: U+200B, U+202E, U+FEFF, and U+200C/U+200D, which IDNA2008
//     allows in some scripts' domain names), which no address needs and which
//     make two different values look alike;
//   - U+FFFD and text that is not UTF-8;
//   - a non-ASCII character whose lowercase form or case-fold orbit holds an
//     ASCII letter (U+212A KELVIN SIGN folds to k, U+017F LONG S to s, U+0130
//     lowercases to i), which strings.ToLower and strings.EqualFold would
//     otherwise map onto another account's ASCII address.
//
// Such a value names no account; callers refuse it where it enters.
func ValidIdentity(s string) bool {
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == utf8.RuneError {
			return false
		}
		if r >= utf8.RuneSelf && foldsToASCII(r) {
			return false
		}
	}
	return true
}

// ValidListEmail is the rule for an entry written into a multiplayer
// participant or excluded list: ValidIdentity, and no backslash. The lists are
// stored as JSON text, and an entry with the characters \u0000 in it would
// read there as the sequence a damaged list carries (which the list readers
// treat as granting nothing). It is a list rule only: signing in and
// resolving a principal do not refuse a backslash.
func ValidListEmail(s string) bool {
	return ValidIdentity(s) && !strings.ContainsRune(s, '\\')
}

// CheckedEmail is the one way an email from outside becomes a key: it trims
// raw, refuses it unless ValidIdentity holds for it AS SENT, and only then
// lowercases it. Checking after lowercasing would be too late, since
// lowercasing maps U+212A or U+0130 onto an ASCII letter. ok is false for a
// refused value; an empty value is returned as "" with ok true, for the
// caller's own "required" answer.
func CheckedEmail(raw string) (email string, ok bool) {
	t := strings.TrimSpace(raw)
	if !ValidIdentity(t) {
		return "", false
	}
	return strings.ToLower(t), true
}

// EmailKey is the key an email is stored and looked up by: trimmed and
// lowercased — unless it holds a character HasASCIILookalike finds, which
// lowercasing would turn into another, ASCII address. Such a value is only
// trimmed, so a lookup with it matches nothing but an entry stored with
// exactly that text. Entry points refuse such values outright
// (CheckedEmail); this is what every key derived from an email goes through.
func EmailKey(e string) string {
	t := strings.TrimSpace(e)
	if HasASCIILookalike(t) {
		return t
	}
	return strings.ToLower(t)
}

// SameEmail reports whether a and b name the same account: whether their
// EmailKeys are equal. It is the one comparison for identity emails (an
// owner, a sender, a viewer, a participant, an OAuth flow's user), so a check
// accepts exactly the addresses the store keys to the same account. An
// address written with other capitals (Alice@x.com and alice@x.com, ÉMILE@x.com
// and émile@x.com) or with surrounding spaces still matches. Two different
// lowercase addresses never match, even where Unicode case folding calls
// their letters the same (strings.EqualFold equates ς and σ, µ and μ, ϑ and
// θ, ſ and s): each of them can be an account of its own.
func SameEmail(a, b string) bool {
	return EmailKey(a) == EmailKey(b)
}

// HasASCIILookalike reports whether s holds a non-ASCII character that
// lowercases or case-folds onto an ASCII letter (U+212A, U+017F, U+0130).
// U+017F is already lowercase, so an email stored before these checks may
// hold it; EmailKey keeps such a value exact, so it still matches that entry
// and nothing else.
func HasASCIILookalike(s string) bool {
	for _, r := range s {
		if r >= utf8.RuneSelf && foldsToASCII(r) {
			return true
		}
	}
	return false
}

// LowercasesOntoASCII reports whether s holds a non-ASCII character that
// lowercasing turns into an ASCII letter: U+0130 (to i) and U+212A (to k). No
// stored email can hold one, since every stored email was lowercased first.
func LowercasesOntoASCII(s string) bool {
	for _, r := range s {
		if r >= utf8.RuneSelf && unicode.ToLower(r) < utf8.RuneSelf {
			return true
		}
	}
	return false
}

// foldsToASCII reports whether the non-ASCII rune r lowercases to, or shares a
// simple case-fold orbit with, an ASCII rune.
func foldsToASCII(r rune) bool {
	if unicode.ToLower(r) < utf8.RuneSelf {
		return true
	}
	for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
		if f < utf8.RuneSelf {
			return true
		}
	}
	return false
}
