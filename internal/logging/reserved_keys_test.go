package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

// A registered secret equal to one of the package's own field names
// (request_id, stdlib_log) is refused, so the line backstop never rewrites
// that key in every record; a secret used as a call site's key is still
// redacted (#2074 review).
func TestReservedFieldNamesAreNeverScrubbedAsLiterals(t *testing.T) {
	RegisterSecretLiterals("", false, KeyRequestID, StdlibLogKey)
	const dynamic = "dynamicSecretKeyQ8w3Zr"
	RegisterSecretLiterals("", false, dynamic)
	var buf bytes.Buffer
	lg := slog.New(NewHandler("json", &buf, slog.LevelDebug))
	lg.InfoContext(context.Background(), "x", KeyRequestID, "req-1", dynamic, "v")
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("%s: %v", buf.String(), err)
	}
	if m[KeyRequestID] != "req-1" {
		t.Fatalf("the reserved key was rewritten: %s", buf.String())
	}
	if strings.Contains(buf.String(), dynamic) {
		t.Fatalf("a secret used as a key leaked: %s", buf.String())
	}
}
