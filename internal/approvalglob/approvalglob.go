// Package approvalglob holds the ONE matcher for the glob of a session
// approval pattern ("argName=glob"). The server's session rules (httpapi)
// and the terminal's rules for handler-only cards (chattui) both use it, so a
// pattern means the same on either side.
//
// A glob has path.Match's syntax, and path.Match stops '*' and '?' at '/',
// which suits a file name. An argument is a shell command, a URL or text as
// often as a path, so a deny written as "command=rm *" was not covering
// "rm -rf /tmp/x". A glob is therefore read two ways, and which way counts
// depends on the rule, as with every other choice a pattern leaves open
// (httpapi.policyMatches): a deny covers a value when either reading matches,
// an approve only when path.Match's does. A deny never covers less than
// path.Match's reading, and an approve never covers more.
package approvalglob

import (
	"path"
	"unicode/utf8"
)

// ErrBadPattern reports a malformed glob. It is path.ErrBadPattern.
var ErrBadPattern = path.ErrBadPattern

// Check returns ErrBadPattern when glob is malformed or not valid UTF-8, and
// nil otherwise. Like path.Match, it reads the whole glob; filepath.Match,
// which the terminal used to check a glob against "", reads one only as far
// as the value lets it, so it reports no error for "a*[" against "".
func Check(glob string) error {
	_, err := compile(glob)
	return err
}

// ApproveCovers reports whether an approve rule's glob covers value:
// path.Match's reading, where '*' and '?' stop at '/'. A malformed glob
// covers nothing.
func ApproveCovers(glob, value string) bool {
	if Check(glob) != nil {
		return false
	}
	ok, err := path.Match(glob, value)
	return err == nil && ok
}

// DenyCovers reports whether a deny rule's glob covers value: when
// path.Match's reading matches, or the one where '*' and '?' match '/' too
// (matchAcrossSlash), so "rm *" covers "rm -rf /tmp/x". path.Match is asked
// first, so a deny covers whatever it matches, even for a glob Check
// refuses only for not being valid UTF-8. A malformed glob covers nothing.
func DenyCovers(glob, value string) bool {
	if ok, err := path.Match(glob, value); err == nil && ok {
		return true
	}
	items, err := compile(glob)
	if err != nil {
		return false
	}
	return matchAcrossSlash(items, value)
}

// The glob syntax, as path.Match's:
//
//	'*'             any run of characters
//	'?'             any one character
//	'[' class ']'   one character in class; "[^" class "]" one not in it
//	'\' c           c
//	c               c
//
// A class is one or more of c or lo '-' hi, each c, lo and hi a character
// other than '-' and ']', or one escaped with '\'.

type kind uint8

const (
	literal kind = iota // one given character
	anyOne              // '?'
	star                // '*'
	class               // '[...]'
)

type item struct {
	kind    kind
	r       rune
	negated bool
	ranges  [][2]rune
}

func (it item) matches(r rune) bool {
	switch it.kind {
	case literal:
		return it.r == r
	case anyOne:
		return true
	case class:
		in := false
		for _, rg := range it.ranges {
			if rg[0] <= r && r <= rg[1] {
				in = true
				break
			}
		}
		return in != it.negated
	case star:
		// matchAcrossSlash handles a star itself; it never asks one to
		// match a character.
	}
	return false
}

func compile(glob string) ([]item, error) {
	if !utf8.ValidString(glob) {
		return nil, ErrBadPattern
	}
	var items []item
	for rest := glob; rest != ""; {
		switch rest[0] {
		case '*':
			// A run of stars is one star.
			if n := len(items); n == 0 || items[n-1].kind != star {
				items = append(items, item{kind: star})
			}
			rest = rest[1:]
		case '?':
			items = append(items, item{kind: anyOne})
			rest = rest[1:]
		case '[':
			it, after, err := compileClass(rest[1:])
			if err != nil {
				return nil, err
			}
			items = append(items, it)
			rest = after
		case '\\':
			if len(rest) == 1 {
				return nil, ErrBadPattern
			}
			r, size := utf8.DecodeRuneInString(rest[1:])
			items = append(items, item{kind: literal, r: r})
			rest = rest[1+size:]
		default:
			r, size := utf8.DecodeRuneInString(rest)
			items = append(items, item{kind: literal, r: r})
			rest = rest[size:]
		}
	}
	return items, nil
}

// compileClass reads a class after its '[' and returns what follows its ']'.
func compileClass(s string) (item, string, error) {
	it := item{kind: class}
	if s != "" && s[0] == '^' {
		it.negated = true
		s = s[1:]
	}
	for {
		if s != "" && s[0] == ']' && len(it.ranges) > 0 {
			return it, s[1:], nil
		}
		lo, after, err := classChar(s)
		if err != nil {
			return item{}, "", err
		}
		hi := lo
		if after[0] == '-' {
			if hi, after, err = classChar(after[1:]); err != nil {
				return item{}, "", err
			}
		}
		it.ranges = append(it.ranges, [2]rune{lo, hi})
		s = after
	}
}

// classChar reads one character of a class. Like path.Match, it refuses a
// bare '-' or ']' and a class with nothing after the character, so what it
// returns is never empty.
func classChar(s string) (rune, string, error) {
	if s == "" || s[0] == '-' || s[0] == ']' {
		return 0, "", ErrBadPattern
	}
	if s[0] == '\\' {
		s = s[1:]
		if s == "" {
			return 0, "", ErrBadPattern
		}
	}
	r, size := utf8.DecodeRuneInString(s)
	if s = s[size:]; s == "" {
		return 0, "", ErrBadPattern
	}
	return r, s, nil
}

// matchAcrossSlash reports whether value matches the whole glob with '*' and
// '?' matching '/' like any other character. It is the usual wildcard walk:
// every item but a star matches exactly one character, so on a mismatch only
// the most recent star needs to take one more character (an earlier star's
// choice can't matter once a later star has matched), and the walk takes
// time proportional to len(items)*len(value) at worst.
func matchAcrossSlash(items []item, value string) bool {
	i, v := 0, 0
	starItem, starValue := -1, 0
	for v < len(value) {
		r, size := utf8.DecodeRuneInString(value[v:])
		switch {
		case i < len(items) && items[i].kind == star:
			starItem, starValue = i, v
			i++
		case i < len(items) && items[i].matches(r):
			i++
			v += size
		case starItem >= 0:
			_, skip := utf8.DecodeRuneInString(value[starValue:])
			starValue += skip
			i, v = starItem+1, starValue
		default:
			return false
		}
	}
	for i < len(items) && items[i].kind == star {
		i++
	}
	return i == len(items)
}
