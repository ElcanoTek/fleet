package logging

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
)

// A registered secret equal to one of the package's own field names
// (request_id) stays registered: the request_id KEY survives on every line,
// in both formats, while the same text as a value — or inside one — is
// redacted (#2074 review: refusing to register it left it unscrubbed).
func TestReservedFieldNameSecretIsScrubbedOutsideKeyPosition(t *testing.T) {
	RegisterSecretLiterals("", false, KeyRequestID)
	const dynamic = "dynamicSecretKeyQ8w3Zr"
	RegisterSecretLiterals("", false, dynamic)
	for _, format := range []string{"json", "text"} {
		var buf bytes.Buffer
		lg := slog.New(NewHandler(format, &buf, slog.LevelDebug))
		prev := slog.Default()
		slog.SetDefault(lg)
		ctx := WithRequestID(context.Background(), "req-1")
		From(ctx).Info("first", "v", KeyRequestID)
		From(ctx).Info("second", "note", "saw request_id here", dynamic, "x")
		lg.InfoContext(ctx, "third", KeyRequestID, "req-2")
		slog.SetDefault(prev)
		lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
		if len(lines) != 3 {
			t.Fatalf("%s: lines = %q", format, lines)
		}
		keyed := `"request_id":"req-1"`
		if format == "text" {
			keyed = "request_id=req-1"
		}
		for i, l := range lines[:2] {
			if !strings.Contains(l, keyed) {
				t.Fatalf("%s line %d: the request_id key or its value was rewritten: %s", format, i, l)
			}
		}
		third := `"request_id":"req-2"`
		if format == "text" {
			third = "request_id=req-2"
		}
		if !strings.Contains(lines[2], third) {
			t.Fatalf("%s: a call site's request_id key was rewritten: %s", format, lines[2])
		}
		// As a value, and inside one, it is redacted.
		if strings.Count(lines[0], "request_id") != 1 || !strings.Contains(lines[0], "[REDACTED]") {
			t.Fatalf("%s: the value equal to the secret survived: %s", format, lines[0])
		}
		if strings.Contains(lines[1], "saw request_id") || strings.Contains(lines[1], dynamic) {
			t.Fatalf("%s: the secret inside a value, or used as a key, survived: %s", format, lines[1])
		}
	}
}
