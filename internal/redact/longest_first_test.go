package redact

import (
	"strings"
	"testing"
)

// A registered literal that contains another is replaced whole, whichever
// was registered first: the shorter one never cuts it, leaving the rest of
// the longer secret ("[REDACTED]-opaque-rest") in Redact or ScrubLine's
// output (#2074 review).
func TestContainedLiteralsReplaceLongestFirst(t *testing.T) {
	const short = "sharedPrefix9Kq"
	const long = short + "-opaque-rest"
	for name, order := range map[string][]string{
		"short first": {short, long},
		"long first":  {long, short},
	} {
		r := NewRedactor(nil)
		for _, v := range order {
			r.AddLiteral(v)
		}
		line := "saw " + long + " and " + short
		for what, got := range map[string]string{
			"Redact":         r.Redact(line),
			"ScrubLine json": r.ScrubLine(`{"msg":"`+line+`"}`, true),
			"ScrubLine text": r.ScrubLine(`msg="`+line+`"`, false),
		} {
			if strings.Contains(got, "opaque-rest") || strings.Contains(got, short) {
				t.Fatalf("%s, %s: %q", name, what, got)
			}
		}
	}
}
