package redact

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestJSONRedactionPreservesEscapesAndScrubsBearerAliases(t *testing.T) {
	credential := "fixture-" + strings.Repeat("x", 32)
	original, _ := json.Marshal(map[string]any{
		"next_step":     `curl -H "Authorization: Bearer ` + credential + `" https://example.invalid/upload`,
		"ticket":        credential,
		"nested":        []any{map[string]any{"password": "odd\\value\"with-quotes"}},
		"large_integer": json.Number("9007199254740993"),
	})
	r := NewRedactor(nil)
	output := r.Redact(string(original))
	if !json.Valid([]byte(output)) {
		t.Fatalf("redaction broke JSON: %s", output)
	}
	if strings.Contains(output, credential) || strings.Contains(output, "with-quotes") {
		t.Fatal("credential survived")
	}
	var result map[string]any
	decoder := json.NewDecoder(strings.NewReader(output))
	decoder.UseNumber()
	if err := decoder.Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result["ticket"] != placeholder {
		t.Fatal("bearer alias survived")
	}
	if result["next_step"] != `curl -H "Authorization: Bearer [REDACTED]" https://example.invalid/upload` {
		t.Fatalf("command framing changed: %v", result["next_step"])
	}
	if result["large_integer"] != json.Number("9007199254740993") {
		t.Fatal("number precision changed")
	}
	if again := r.Redact(output); again != output {
		t.Fatalf("not idempotent: %s", again)
	}
	if r.LiteralCount() != 0 {
		t.Fatal("ephemeral bearer retained permanently")
	}
}

func TestJSONRedactionLeavesUntouchedDocumentExact(t *testing.T) {
	original := "{ \"rows\": [1.000, 9007199254740993], \"label\": \"Northwind\" }"
	if got := NewRedactor(nil).Redact(original); got != original {
		t.Fatalf("changed harmless JSON: %s", got)
	}
}

func TestJSONRedactionCustomPatternsAndNumericSecrets(t *testing.T) {
	r := NewRedactor([]string{`"custom"\s*:\s*"[a-z]+"`})
	if got := r.Redact(`{"custom" : "sensitive", "n":1}`); !json.Valid([]byte(got)) || strings.Contains(got, "sensitive") {
		t.Fatalf("unsafe custom redaction: %s", got)
	}
	if got := r.Redact(`{"password":123456789}`); strings.Contains(got, "123456789") || !json.Valid([]byte(got)) {
		t.Fatalf("numeric secret survived: %s", got)
	}
}
