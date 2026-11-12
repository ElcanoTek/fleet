package approvalglob

import (
	"errors"
	"path"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// A deny covers a value when either reading of its glob matches; an approve
// only on path.Match's reading, where '*' and '?' stop at '/'.
func TestDenyAndApproveCovers(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		glob, value   string
		deny, approve bool
	}{
		{"rm *", "rm -rf /tmp/x", true, false},
		{"rm *", "rm build", true, true},
		{"rm *", "ls /tmp", false, false},
		{"ls *", "ls /tmp", true, false},
		{"*", "*/15 * * * *", true, false},
		{"*", "0 9 * * *", true, true},
		{"https://docs.example.com/*", "https://docs.example.com/guide", true, true},
		{"https://docs.example.com/*", "https://docs.example.com/guide/install", true, false},
		{"https://*.example.com/*", "https://other.net/x.example.com/y", true, false},
		{"https://*.example.com/*", "https://docs.example.com/y", true, true},
		{"/workspace/*", "/workspace/a/b", true, false},
		{"/workspace?a?notes.md", "/workspace/a/notes.md", true, false},
		{"[/]x", "/x", true, true},
		// Not valid UTF-8: path.Match's reading still counts for a deny.
		{"\xb0", "\xb0", true, false},
		// path.Match steps a star by bytes, so it finds a "[^é]" inside "é"
		// (its second byte). A deny never covers less than that reading.
		{"*[^é]", "é", true, true},
		// The whole value, not a prefix or a part of it.
		{"rm", "rm -rf build", false, false},
		{"build", "rm -rf build", false, false},
		// A malformed glob covers nothing, whatever the value.
		{"a*[", "abc", false, false},
		{"a*[", "b", false, false},
		{"rm [", "rm [", false, false},
	} {
		if got := DenyCovers(tc.glob, tc.value); got != tc.deny {
			t.Errorf("DenyCovers(%q, %q) = %v, want %v", tc.glob, tc.value, got, tc.deny)
		}
		if got := ApproveCovers(tc.glob, tc.value); got != tc.approve {
			t.Errorf("ApproveCovers(%q, %q) = %v, want %v", tc.glob, tc.value, got, tc.approve)
		}
	}
}

func TestMatchAcrossSlash(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		glob, value string
		want        bool
	}{
		{"rm *", "rm -rf /tmp/x", true},
		{"*", "", true},
		{"*", "a/b/c", true},
		{"a?c", "a/c", true},
		{"a?c", "abbc", false},
		{"*/*", "a/b/c", true},
		{"*/*", "abc", false},
		{"/tmp/*/x", "/tmp/a/b/x", true},
		{"*.log", "/var/log/app.log", true},
		{"*.log", "/var/log/app.log.1", false},
		// A run of stars is one star; a star matches nothing too.
		{"a**b", "ab", true},
		{"a*b*c", "a/b/c", true},
		{"a*b*c", "a/c/b", false},
		{"*a*a*b", "aaab", true},
		{"*a*a*b", "aab/", false},
		// Classes, negation, ranges and escapes are path.Match's.
		{"[^/]", "/", false},
		{"[a-c]*", "b/x", true},
		{"[a-c]*", "d/x", false},
		{"[\\]]", "]", true},
		{"[\\-]", "-", true},
		{"[z-a]", "b", false},
		{"\\*", "*", true},
		{"\\*", "a", false},
		{"\\a", "a", true},
		{"a\\*b", "a/b", false},
		{"[*]", "*", true},
		{"[*]", "a", false},
		// Characters, not bytes.
		{"?", "é", true},
		{"??", "é", false},
		{"[à-ü]", "é", true},
		{"*[^é]", "é", false},
		{"日*語", "日本/語", true},
		{"", "", true},
		{"", "a", false},
	} {
		items, err := compile(tc.glob)
		if err != nil {
			t.Errorf("compile(%q): %v", tc.glob, err)
			continue
		}
		if got := matchAcrossSlash(items, tc.value); got != tc.want {
			t.Errorf("matchAcrossSlash(%q, %q) = %v, want %v", tc.glob, tc.value, got, tc.want)
		}
	}
}

func TestCheckRefusesAMalformedGlob(t *testing.T) {
	t.Parallel()

	for _, glob := range []string{
		"[", "a[", "[a", "[]", "[]a]", "[^]", "[a-", "[a-]", "[-a]", "[a--b]",
		"\\", "a\\", "[\\", "[a\\]", "*[", "a*[", "[a]*[", "0 9 * * [",
		"\xff", "a\xffb",
	} {
		if err := Check(glob); !errors.Is(err, ErrBadPattern) {
			t.Errorf("Check(%q) = %v, want ErrBadPattern", glob, err)
		}
	}
	for _, glob := range []string{"", "*", "rm *", "[a-c]", "[^/]", "\\[", "]", "a]b", "-", "^", "0 9 * * *"} {
		if err := Check(glob); err != nil {
			t.Errorf("Check(%q) = %v, want nil", glob, err)
		}
	}
}

// For an ASCII value without '/', both readings are path.Match's, and Check
// refuses exactly the globs path.Match refuses.
func TestMatchAcrossSlashAgreesWithPathMatchWithoutASlash(t *testing.T) {
	t.Parallel()

	globs := []string{
		"", "*", "**", "?", "a", "a*", "*a", "a*b", "*a*", "a?b", "?*?", "*?*",
		"[abc]", "[^abc]", "[a-c]", "[^a-c]x", "[a-cx-z]*", "[\\]]", "[\\-a]", "[\\^]", "[^^]",
		"\\*", "\\?", "\\[", "\\]", "]", "a]b", "-", "^",
		"*a*a*b", "a*b*c*d", "*[ab]*", "[*]", "[?]", "é*", "[à-ü]?",
		"[", "[]", "[^]", "[a-", "\\", "a*[", "*[", "[a]\\", "b[", "[b]*[",
	}
	values := []string{
		"", "a", "b", "ab", "abc", "aab", "acb", "abcd", "axbxcxd", "x", "]", "-", "^", "*", "?", "[",
		"a]b", "ba", "zzz", "aaab", "xab",
	}
	for _, glob := range globs {
		malformed := false
		for _, value := range values {
			if _, err := path.Match(glob, value); err != nil {
				malformed = true
			}
		}
		if (Check(glob) != nil) != malformed {
			t.Errorf("Check(%q) = %v; path.Match refuses the glob: %v", glob, Check(glob), malformed)
			continue
		}
		if malformed {
			continue
		}
		items, _ := compile(glob)
		for _, value := range values {
			want, _ := path.Match(glob, value)
			if got := matchAcrossSlash(items, value); got != want {
				t.Errorf("matchAcrossSlash(%q, %q) = %v; path.Match says %v", glob, value, got, want)
			}
		}
	}
}

func FuzzReadings(f *testing.F) {
	for _, seed := range [][2]string{
		{"rm *", "rm -rf /tmp/x"}, {"a*b?c", "axxbyc"}, {"[^a-c]*", "dz"}, {"\\[*", "[x"}, {"*[", "a"},
		{"[]a]", "a"}, {"a*[", "b"}, {"*[^é]", "é"}, {"/workspace/*", "/workspace/a/b"},
	} {
		f.Add(seed[0], seed[1])
	}
	f.Fuzz(func(t *testing.T, glob, value string) {
		want, wantErr := path.Match(glob, value)
		if wantErr != nil && Check(glob) == nil {
			t.Fatalf("Check(%q) = nil; path.Match refuses it: %v", glob, wantErr)
		}
		if utf8.ValidString(glob) && wantErr == nil && Check(glob) != nil {
			t.Fatalf("Check(%q) = %v; path.Match accepts it", glob, Check(glob))
		}
		deny, approve := DenyCovers(glob, value), ApproveCovers(glob, value)
		if wantErr == nil && want && !deny {
			t.Fatalf("DenyCovers(%q, %q) = false; path.Match matches", glob, value)
		}
		if approve && !deny {
			t.Fatalf("%q against %q: the approve covers it and the deny doesn't", glob, value)
		}
		if Check(glob) == nil && approve != (wantErr == nil && want) {
			t.Fatalf("ApproveCovers(%q, %q) = %v; path.Match says %v, %v", glob, value, approve, want, wantErr)
		}
		if Check(glob) != nil || !isASCII(value) {
			return
		}
		items, _ := compile(glob)
		// On an ASCII value without '/', the two readings agree.
		if !strings.ContainsRune(value, '/') {
			if got := matchAcrossSlash(items, value); got != (wantErr == nil && want) {
				t.Fatalf("matchAcrossSlash(%q, %q) = %v; path.Match says %v, %v", glob, value, got, want, wantErr)
			}
		}
		// With each '/' in the value made a character the glob can't name
		// ('/' and '\x01' absent, no class), path.Match reads '*' and '?' as
		// the slash-crossing reading does.
		if !strings.ContainsAny(glob, "/[\x01") && !strings.ContainsRune(value, '\x01') {
			mapped, _ := path.Match(glob, strings.ReplaceAll(value, "/", "\x01"))
			if got := matchAcrossSlash(items, value); got != mapped {
				t.Fatalf("matchAcrossSlash(%q, %q) = %v; path.Match with '/' renamed says %v", glob, value, got, mapped)
			}
		}
	})
}

// A glob with many stars against a long value that almost matches takes time
// proportional to their lengths, not exponential in the stars.
func TestMatchStaysQuickOnManyStars(t *testing.T) {
	t.Parallel()

	glob := strings.Repeat("*a", 64) + "*b"
	items, err := compile(glob)
	if err != nil {
		t.Fatal(err)
	}
	value := strings.Repeat("a/", 8192)
	start := time.Now()
	if matchAcrossSlash(items, value) || DenyCovers(glob, value) {
		t.Fatal("matched a value with no b")
	}
	if !matchAcrossSlash(items, value+"b") || !DenyCovers(glob, value+"b") {
		t.Fatal("did not match the value ending in b")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("four matches took %v", d)
	}
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}
