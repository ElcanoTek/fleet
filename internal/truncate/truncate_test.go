package truncate

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// TestClamp_UnderBudgetUnchanged: strings within the budget pass through
// verbatim, marker-free — including exactly-at-budget.
func TestClamp_UnderBudgetUnchanged(t *testing.T) {
	for _, s := range []string{"", "plain ascii", "héllo 世界 🚀"} {
		if got := Clamp(s, len(s), "…"); got != s {
			t.Errorf("Clamp(%q, %d) = %q, want unchanged", s, len(s), got)
		}
	}
}

// TestClamp_MidRuneBoundaryStaysValidUTF8 pins the #595 regression: when the
// byte budget falls mid-rune, the naive byte slice emits invalid UTF-8 (which
// Postgres rejects for TEXT params); Clamp must back off to the rune boundary.
// The limits exercised are the real call-site budgets: the datasets note clamp
// (8000), the runner's carry-context handoff (2000), and the datasets error
// clamp (500).
func TestClamp_MidRuneBoundaryStaysValidUTF8(t *testing.T) {
	const marker = "…[truncated]"
	cases := []struct {
		name string
		rune string // multibyte rune whose width does not divide the budget
		max  int
	}{
		{"datasets note 3-byte", "世", 8000}, // 8000 % 3 != 0 → mid-rune
		{"datasets note 4-byte", "🚀", 8001}, // 8001 % 4 != 0 → mid-rune
		{"carry context 3-byte", "宇", 2000}, // 2000 % 3 != 0 → mid-rune
		{"datasets error 3-byte", "界", 500}, // 500 % 3 != 0 → mid-rune
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			width := len(tc.rune)
			s := strings.Repeat(tc.rune, tc.max/width+2) // just over the budget
			if tc.max%width == 0 {
				t.Fatalf("test bug: budget %d is a multiple of rune width %d — boundary not mid-rune", tc.max, width)
			}
			// The old byte-slice behavior really is the bug being fixed.
			if utf8.ValidString(s[:tc.max]) {
				t.Fatalf("test bug: naive byte slice at %d is valid UTF-8 — not exercising the regression", tc.max)
			}
			got := Clamp(s, tc.max, marker)
			if !utf8.ValidString(got) {
				t.Fatalf("Clamp emitted invalid UTF-8 at budget %d", tc.max)
			}
			if !strings.HasSuffix(got, marker) {
				t.Errorf("truncated result must carry the marker, got suffix %q", got[len(got)-len(marker):])
			}
			body := strings.TrimSuffix(got, marker)
			if len(body) > tc.max || tc.max-len(body) >= width {
				t.Errorf("cut at %d bytes, want the largest rune boundary <= %d", len(body), tc.max)
			}
		})
	}
}

// TestClamp_InvalidInputStillCuts: already-invalid input (raw bytes in an error
// string) is cut near the budget after the bounded backoff — Clamp must not
// walk far past the budget or loop.
func TestClamp_InvalidInputStillCuts(t *testing.T) {
	s := "ok" + strings.Repeat("\x80", 64) // continuation bytes only: no rune start to find
	got := Clamp(s, 10, "…")
	if len(got) > 10+len("…") {
		t.Errorf("invalid input cut to %d bytes, want <= budget+marker", len(got)-len("…"))
	}
}

// TestClamp_NonPositiveBudget: a degenerate budget yields just the marker
// rather than panicking on a negative slice bound.
func TestClamp_NonPositiveBudget(t *testing.T) {
	if got := Clamp("abc", 0, "…"); got != "…" {
		t.Errorf("Clamp(0) = %q, want marker only", got)
	}
	if got := Clamp("abc", -1, "…"); got != "…" {
		t.Errorf("Clamp(-1) = %q, want marker only", got)
	}
}

func TestRunes(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		want string
	}{
		{"hello", 10, "hello"},
		{"hello", 5, "hello"},
		{"hello", 3, "hel…"},
		{"héllo wörld", 4, "héll…"},
		{"日本語のテキスト", 3, "日本語…"},
		{"🙂🙂🙂", 1, "🙂…"},
		{"abc", 0, "…"},
		{"", 0, ""},
	}
	for _, tc := range cases {
		if got := Runes(tc.in, tc.n, "…"); got != tc.want {
			t.Errorf("Runes(%q, %d) = %q, want %q", tc.in, tc.n, got, tc.want)
		}
		if !utf8.ValidString(Runes(tc.in, tc.n, "")) {
			t.Errorf("Runes(%q, %d) produced invalid UTF-8", tc.in, tc.n)
		}
	}
}

func TestTail(t *testing.T) {
	cases := []struct {
		in   string
		max  int
		want string
	}{
		{"hello", 10, "hello"},
		{"hello", 3, "llo"},
		{"日本語", 4, "語"}, // 9 bytes; the last 4 would start mid-rune
		{"ab🙂", 3, ""},  // the emoji is 4 bytes and does not fit
		{"ab🙂", 4, "🙂"},
		{"abc", 0, ""},
	}
	for _, tc := range cases {
		got := Tail(tc.in, tc.max)
		if got != tc.want {
			t.Errorf("Tail(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
		}
		if !utf8.ValidString(got) || len(got) > tc.max && len(tc.in) > tc.max {
			t.Errorf("Tail(%q, %d) = %q is invalid or over budget", tc.in, tc.max, got)
		}
	}
}
