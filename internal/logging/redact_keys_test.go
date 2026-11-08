package logging

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"
)

// A secret can be a map KEY as easily as a value (a token used as a lookup
// key, a header line used as a field name). Every map the redactor rebuilds
// scrubs its keys with the same routine as its values — in a slog.Any map,
// in a child record's array of objects — and two keys that redact to the
// same text are both kept, suffixed deterministically (#2074 review).
func TestMapKeysAreRedacted(t *testing.T) {
	const opaque = "keyOpaqueRegistered-5Tq8Wz"
	const other = "keyOpaqueRegistered-9Lm2Xc"
	RegisterSecretLiterals("", false, opaque, other)

	t.Run("slog.Any map", func(t *testing.T) {
		var buf bytes.Buffer
		slog.New(NewHandler("json", &buf, slog.LevelDebug)).Info("x", slog.Any("bySeat", map[string]any{
			opaque: "first",
			other:  "second",
			"ok":   map[string]string{opaque: "nested"},
		}))
		out := buf.String()
		if strings.Contains(out, opaque) || strings.Contains(out, other) {
			t.Fatalf("a secret map key leaked: %s", out)
		}
		var m map[string]any
		if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		got, _ := m["bySeat"].(map[string]any)
		if got[redactedValue] != "first" || got[redactedValue+"#2"] != "second" {
			t.Fatalf("colliding redacted keys not kept deterministically: %v", got)
		}
	})

	t.Run("child record array of objects", func(t *testing.T) {
		out := captureOutput(t)
		w := ChildStderr("mcp-broker")
		_, _ = w.Write([]byte(`{"level":"INFO","msg":"seats","items":[{"` + opaque + `":1},{"name":{"` + other + `":"x"}}],"` + opaque + `":"top"}` + "\n"))
		got := out.String()
		if strings.Contains(got, opaque) || strings.Contains(got, other) {
			t.Fatalf("a secret key in a child record leaked: %s", got)
		}
		if recs := out.records(t); len(recs) != 1 || recs[0]["msg"] != "seats" || recs[0][redactedValue] != "top" {
			t.Fatalf("records = %v", recs)
		}
	})
}
