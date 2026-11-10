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

// TestRedactJSONKeepsVaultReferences: a "$secret:<field_id>" reference in a
// *_ref argument names a vaulted credential and is kept, so a saved tool call
// still says which credential it used. Anything else shaped like one is not:
// a literal under a *_ref key, a reference under another key, and a secret
// wrapped in the reference syntax (a vendor key shape, a registered literal,
// a credential seen elsewhere in the same payload).
func TestRedactJSONKeepsVaultReferences(t *testing.T) {
	r := NewRedactor(nil)
	r.RegisterSecrets("test", false, "Hunter2Registered9")
	in := `{"password_ref":"$secret:password","secret_ref":"$secret:api_token","username_ref":"$secret:creds[0].user-name",` +
		`"api_key":"literal-key-value-1928","token":"$secret:password","note":"$secret:not-a-ref-key",` +
		`"api_key_ref":"$secret:sk-ant-api03-abcdefghijklmnopqrstuvwxyz0123456789",` +
		`"login":{"password_ref":"$secret:Hunter2Registered9"},` +
		`"header":"Authorization: Bearer abcdefgh12345678","Secret_Ref_3":"$secret:abcdefgh12345678",` + // gitleaks:allow (made-up test values)
		`"basic":"Authorization: Basic dXNlcjpwYXNzd29yZA==","token_ref":"$secret:dXNlcjpwYXNzd29yZA",` + // gitleaks:allow (made-up test values)
		`"long_ref":"$secret:` + strings.Repeat("a1", 33) + `"}` // gitleaks:allow (made-up test values)
	got := r.Redact(in)
	for _, keep := range []string{`"password_ref":"$secret:password"`, `"secret_ref":"$secret:api_token"`, `"username_ref":"$secret:creds[0].user-name"`} {
		if !strings.Contains(got, keep) {
			t.Errorf("Redact = %s, want it to keep %s", got, keep)
		}
	}
	for _, gone := range []string{"literal-key-value-1928", `"token":"$secret:password"`, "sk-ant-api03", "Hunter2Registered9", "$secret:abcdefgh12345678", "$secret:dXNlcjpwYXNzd29yZA", strings.Repeat("a1", 33)} { // gitleaks:allow (made-up test values)
		if strings.Contains(got, gone) {
			t.Errorf("Redact = %s, want %q redacted", got, gone)
		}
	}
}
