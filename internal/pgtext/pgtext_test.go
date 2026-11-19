package pgtext

import "testing"

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
