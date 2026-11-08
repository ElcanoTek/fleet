package redact

import "testing"

// A literal equal to a reserved key is left alone only in key position —
// JSON `"key":` after `{` or `,`, logfmt `key=` at a field's start — and
// replaced everywhere else (#2074 review).
func TestScrubLineKeepsReservedKeysOnlyInKeyPosition(t *testing.T) {
	r := NewRedactor(nil)
	r.SetReservedKeys("request_id")
	r.AddLiteral("request_id")
	for _, c := range []struct {
		json       bool
		line, want string
	}{
		{true, `{"request_id":"r1","g":{"request_id":"r2"}}`, `{"request_id":"r1","g":{"request_id":"r2"}}`},
		{true, `{"msg":"request_id","x":"a request_id b"}`, `{"msg":"[REDACTED]","x":"a [REDACTED] b"}`},
		{true, `{"xrequest_id":"v"}`, `{"x[REDACTED]":"v"}`},
		{false, `time=t request_id=r1 msg="saw request_id" v=request_id` + "\n", `time=t request_id=r1 msg="saw [REDACTED]" v=[REDACTED]` + "\n"},
		{false, `request_id=r1 g.request_id=r2` + "\n", `request_id=r1 g.[REDACTED]=r2` + "\n"},
	} {
		if got := r.ScrubLine(c.line, c.json); got != c.want {
			t.Errorf("ScrubLine(%q) = %q, want %q", c.line, got, c.want)
		}
	}
}
