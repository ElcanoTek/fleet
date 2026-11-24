package pgtext

import (
	"strings"
	"testing"
	"unicode"
)

func TestContent(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ in, want string }{
		{"ab", "ab"},
		{"a\x00b", "ab"},
		{"\x00\x00a\x00", "a"},
		{"\x00", ""},
		{`a\u0000b`, `a\u0000b`}, // the six characters are text here
	} {
		if got := Content(c.in); got != c.want {
			t.Errorf("Content(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestJSON(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ in, want string }{
		{`{"k":"ab"}`, `{"k":"ab"}`},
		{`{"k":"a\u0000b"}`, `{"k":"ab"}`},
		{`["\u0000","\u0000\u0000"]`, `["",""]`},
		{`"\u00000"`, `"0"`},
		{`"\\u0000"`, `"\\u0000"`},           // a doubled backslash, then text
		{`"\\\u0000"`, `"\\"`},               // a doubled backslash, then the sequence
		{`"\u0001\u000a"`, `"\u0001\u000a"`}, // other control sequences stay
		{"\"a\x00b\"", `"ab"`},
		{"\"\\\x00u0000\"", `""`}, // a 0x00 byte inside the sequence does not hide it
		{`"\`, `"\`},              // a trailing backslash is copied (Postgres reports the bad JSON)
		{`\u0000`, ``},
		{``, ``},
	} {
		if got := JSON(c.in); got != c.want {
			t.Errorf("JSON(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestValidIdentity(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		in   string
		want bool
	}{
		{"alice@example.com", true},
		{"ünïcode@example.com", true},
		{"", true}, // emptiness is the caller's own check
		{"alice@example.com\x00", false},
		{"alice\x01@example.com", false},
		{"alice\x1f@example.com", false},
		{"alice\x7f@example.com", false},
		{"alice\u0085@example.com", false}, // C1 control
		{"alice\n@example.com", false},
		{"alice�@example.com", false},
		{"alice\xff@example.com", false},   // not UTF-8
		{"alice\u200b@example.com", false}, // format characters (Cf)
		{"alice\u202e@example.com", false},
		{"alice\u200c@example.com", false}, // ZWNJ and ZWJ are format characters too
		{"alice\u200d@example.com", false},
		{"\ufeffalice@example.com", false},
		{"\u212aate@example.com", false}, // KELVIN SIGN lowercases to k
		{"\u017fam@example.com", false},  // LONG S folds to s
		{"\u0130van@example.com", false}, // lowercases to i
		{"ıvan@example.com", true},       // dotless i: neither lowercases nor folds to ASCII
		{"ÉMILE@example.com", true},
		{"José@example.com", true},
	} {
		if got := ValidIdentity(c.in); got != c.want {
			t.Errorf("ValidIdentity(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestCheckedEmail(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		in, want string
		ok       bool
	}{
		{" Alice@Example.com\t", "alice@example.com", true},
		{"", "", true},
		{"\u212aate@example.com", "", false}, // refused as sent, not after lowercasing to kate
		{"\u0130van@example.com", "", false},
		{"A\\b@example.com", "a\\b@example.com", true}, // a backslash is a list rule, not a sign-in rule
		{"a\u200c@example.com", "", false},             // ZWNJ
		{"a\u200d@example.com", "", false},             // ZWJ
		{"José@Example.com", "josé@example.com", true},
	} {
		got, ok := CheckedEmail(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("CheckedEmail(%q) = %q, %v; want %q, %v", c.in, got, ok, c.want, c.ok)
		}
	}
	if !ValidIdentity(`DOMAIN\user`) {
		t.Error("ValidIdentity refused a backslash in a subject")
	}
	// The list rule adds the backslash: a multiplayer entry with the
	// characters \u0000 would read like a damaged list's sequence.
	for _, c := range []struct {
		in   string
		want bool
	}{
		{"bob@example.com", true},
		{`a\u0000@example.com`, false},
		{`a\b@example.com`, false},
		{"a\x01@example.com", false},
		{"\u212aate@example.com", false},
	} {
		if got := ValidListEmail(c.in); got != c.want {
			t.Errorf("ValidListEmail(%q) = %v, want %v", c.in, got, c.want)
		}
	}
	for _, c := range []struct {
		in   string
		want bool
	}{
		{"\u0130van@example.com", true},
		{"\u212aate@example.com", true},
		{"\u017fam@example.com", false}, // already lowercase: a stored email may hold it
		{"josé@example.com", false},
	} {
		if got := LowercasesOntoASCII(c.in); got != c.want {
			t.Errorf("LowercasesOntoASCII(%q) = %v, want %v", c.in, got, c.want)
		}
	}
	for _, c := range []struct {
		in   string
		want bool
	}{
		{"kate@example.com", false},
		{"\u212aate@example.com", true},
		{"\u017fam@example.com", true},
		{"\u0130van@example.com", true},
		{"old\x01@example.com", false},
		{"josé@example.com", false},
	} {
		if got := HasASCIILookalike(c.in); got != c.want {
			t.Errorf("HasASCIILookalike(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}

func TestEmailKey(t *testing.T) {
	t.Parallel()
	for _, c := range []struct{ in, want string }{
		{" Alice@Example.com ", "alice@example.com"},
		{"old\x01@Example.com", "old\x01@example.com"},     // control characters survive, exactly
		{"\u212aate@Example.com", "\u212aate@Example.com"}, // a look-alike is only trimmed
		{"\u017fam@example.com", "\u017fam@example.com"},
		{"\u0130van@example.com", "\u0130van@example.com"},
	} {
		if got := EmailKey(c.in); got != c.want {
			t.Errorf("EmailKey(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestSameEmail(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		a, b string
		want bool
	}{
		// One account, written in other capitals or with spaces around it.
		{"alice@example.com", "alice@example.com", true},
		{"Alice@Example.COM", "alice@example.com", true},
		{" alice@example.com\t", "alice@example.com", true},
		{"ÉMILE@example.com", "émile@example.com", true},
		{"Σ@example.com", "σ@example.com", true}, // capital sigma lowercases to σ
		{"", "", true},
		// Two lowercase addresses Unicode case folding calls the same letter:
		// each can be an account of its own, so they never match.
		{"σ@example.com", "ς@example.com", false},
		{"Σ@example.com", "ς@example.com", false},
		{"µ@example.com", "μ@example.com", false}, // MICRO SIGN, GREEK SMALL LETTER MU
		{"ϐ@example.com", "β@example.com", false},
		{"ϑ@example.com", "θ@example.com", false},
		{"ϕ@example.com", "φ@example.com", false},
		{"ϖ@example.com", "π@example.com", false},
		{"ϰ@example.com", "κ@example.com", false},
		{"ϱ@example.com", "ρ@example.com", false},
		{"ϵ@example.com", "ε@example.com", false},
		{"ẛ@example.com", "ṡ@example.com", false},
		{"ᲀ@example.com", "в@example.com", false},
		{"ᲁ@example.com", "д@example.com", false},
		{"ᲂ@example.com", "о@example.com", false},
		{"ᲃ@example.com", "с@example.com", false},
		{"ᲄ@example.com", "т@example.com", false},
		{"ᲅ@example.com", "т@example.com", false},
		{"ᲆ@example.com", "ъ@example.com", false},
		{"ᲇ@example.com", "ѣ@example.com", false},
		{"ᲈ@example.com", "ꙋ@example.com", false},
		// Look-alikes of ASCII letters stay exact, so they match no ASCII
		// account (and an old stored ſ address matches only itself).
		{"Kate@example.com", "kate@example.com", false},
		{"Kate@example.com", "Kate@example.com", false},
		{"ſam@example.com", "sam@example.com", false},
		{"ſam@example.com", "ſam@example.com", true},
		{"İvan@example.com", "ivan@example.com", false},
		{"alice@example.com", "bob@example.com", false},
		{"alice@example.com", "", false},
	} {
		if got := SameEmail(c.a, c.b); got != c.want {
			t.Errorf("SameEmail(%q, %q) = %v, want %v", c.a, c.b, got, c.want)
		}
		if got := SameEmail(c.b, c.a); got != c.want {
			t.Errorf("SameEmail(%q, %q) = %v, want %v (not symmetric)", c.b, c.a, got, c.want)
		}
	}
}

// TestSameEmailAcrossEveryFoldPair walks every simple case-fold orbit in
// Unicode: two letters strings.EqualFold equates are the same account under
// SameEmail exactly when lowercasing maps them to one letter and neither is a
// look-alike of an ASCII letter. Each ASCII letter still matches its capital.
func TestSameEmailAcrossEveryFoldPair(t *testing.T) {
	t.Parallel()
	addr := func(r rune) string { return "a" + string(r) + "@example.com" }
	distinct := 0
	for r := rune(0); r <= unicode.MaxRune; r++ {
		if r >= 0xD800 && r <= 0xDFFF {
			continue
		}
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			if !strings.EqualFold(addr(r), addr(f)) {
				t.Fatalf("EqualFold(%U, %U) = false inside one fold orbit", r, f)
			}
			want := unicode.ToLower(r) == unicode.ToLower(f) && !HasASCIILookalike(string(r)) && !HasASCIILookalike(string(f))
			if got := SameEmail(addr(r), addr(f)); got != want {
				t.Errorf("SameEmail(%q, %q) = %v, want %v", addr(r), addr(f), got, want)
			}
			if !want {
				distinct++
			}
		}
	}
	if distinct == 0 {
		t.Fatal("no fold pair kept apart: the walk compared nothing")
	}
	for r := 'a'; r <= 'z'; r++ {
		if !SameEmail(addr(r), addr(unicode.ToUpper(r))) {
			t.Errorf("SameEmail(%q, %q) = false for one ASCII letter", addr(r), addr(unicode.ToUpper(r)))
		}
	}
}
