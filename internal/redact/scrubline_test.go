package redact

import (
	"encoding/json"
	"strings"
	"testing"
)

// ScrubLine is the log backstop: registered literals in raw and encoded
// forms and vendor credential shapes are replaced inside an encoded line,
// every other byte is kept, the line stays valid JSON, and the generic
// marker=value rule (which decides by key name) is not applied.
func TestScrubLine(t *testing.T) {
	r := NewRedactor(nil)
	const plain = "scrubLineLiteral-5Kq"
	const quoted = `has"quote\and-slash9Z`
	r.AddLiteral(plain)
	r.AddLiteral(quoted)
	shaped := "sk-ant-" + strings.Repeat("R", 32)
	enc, _ := json.Marshal(quoted)
	line := `{"time":"t","level":"INFO","msg":"saw ` + plain + `","q":` + string(enc) + `,"k":"` + shaped + `","token_ttl":30000000000,"h":"Authorization: Bearer abcdefgh12345678"}`
	got := r.ScrubLine(line)
	for _, leak := range []string{plain, string(enc[1 : len(enc)-1]), shaped, "abcdefgh12345678"} {
		if strings.Contains(got, leak) {
			t.Fatalf("leaked %q: %s", leak, got)
		}
	}
	if !json.Valid([]byte(got)) {
		t.Fatalf("not valid JSON: %s", got)
	}
	if !strings.Contains(got, `"token_ttl":30000000000`) || !strings.HasPrefix(got, `{"time":"t","level":"INFO","msg":"saw `) {
		t.Fatalf("bytes outside a match changed (or the marker rule ran): %s", got)
	}
	if clean := `{"msg":"nothing to see","n":1}`; r.ScrubLine(clean) != clean {
		t.Fatalf("a clean line was altered")
	}
}
