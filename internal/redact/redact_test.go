package redact

import (
	"strings"
	"testing"
)

func TestRedactor_CanonicalPatterns(t *testing.T) {
	r := NewRedactor(nil)
	cases := []struct {
		name   string
		in     string
		secret string // substring that must be gone
		keep   string // optional substring that must remain
	}{
		{"anthropic", "key sk-ant-api03AAAAAAAAAAAAAAAAAAAAAAAAAAAA end", "sk-ant-api03AAAAAAAAAAAAAAAAAAAAAAAAAAAA", "key"},
		{"openrouter", "x sk-or-v1-0123456789abcdef0123456789abcdef y", "sk-or-v1-0123456789abcdef0123456789abcdef", "x"},
		{"openai", "OPENAI=sk-ABCDEFGHIJKLMNOPQRSTUVWXYZ012345", "sk-ABCDEFGHIJKLMNOPQRSTUVWXYZ012345", ""},
		{"github", "tok ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789 ok", "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789", "ok"},
		{"gitlab", "glpat-ABCDEFGHIJKLMNOPQRST here", "glpat-ABCDEFGHIJKLMNOPQRST", "here"},
		{"aws", "AKIAIOSFODNN7EXAMPLE rest", "AKIAIOSFODNN7EXAMPLE", "rest"},
		{"bearer", "Authorization: Bearer abc.def.ghijklmnop123", "abc.def.ghijklmnop123", "Authorization"},
		{"marker eq", "api_key=supersecretvalue123", "supersecretvalue123", "api_key"},
		{"marker json", `{"api_key":"supersecretvalue123"}`, "supersecretvalue123", "api_key"},
		{"secret colon", "secret: hunter2hunter2", "hunter2hunter2", ""},
		{"password", `password="p@ssw0rd-longvalue"`, "p@ssw0rd-longvalue", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := r.Redact(c.in)
			if strings.Contains(got, c.secret) {
				t.Errorf("secret survived: %q -> %q", c.in, got)
			}
			if !strings.Contains(got, placeholder) {
				t.Errorf("no redaction placeholder in %q", got)
			}
			if c.keep != "" && !strings.Contains(got, c.keep) {
				t.Errorf("redaction ate context %q: %q", c.keep, got)
			}
		})
	}
}

func TestRedactor_PEMBlock(t *testing.T) {
	r := NewRedactor(nil)
	in := "before\n-----BEGIN RSA PRIVATE KEY-----\nMIIabc\nDEFghi\n-----END RSA PRIVATE KEY-----\nafter"
	got := r.Redact(in)
	if strings.Contains(got, "MIIabc") || strings.Contains(got, "BEGIN RSA PRIVATE KEY") {
		t.Errorf("PEM block survived: %q", got)
	}
	if !strings.Contains(got, "before") || !strings.Contains(got, "after") {
		t.Errorf("redaction ate surrounding text: %q", got)
	}
}

func TestRedactor_Literal(t *testing.T) {
	r := NewRedactor(nil)
	r.AddLiteral("novel-key-format-9f8e7d6c") // a shape no pattern recognizes
	got := r.Redact("token is novel-key-format-9f8e7d6c here")
	if strings.Contains(got, "novel-key-format-9f8e7d6c") {
		t.Errorf("literal not redacted: %q", got)
	}
	// Too-short literals are ignored (avoid scrubbing common short strings).
	r.AddLiteral("yes")
	if got := r.Redact("the answer is yes"); !strings.Contains(got, "yes") {
		t.Errorf("short literal was redacted: %q", got)
	}
}

func TestRedactor_RegisterEnvLiterals(t *testing.T) {
	r := NewRedactor(nil)
	r.RegisterEnvLiterals([]string{
		"OPENROUTER_API_KEY=or-novel-abc12345",
		"PATH=/usr/bin:/bin", // not a secret name → must NOT be registered
		"HOME=/root",
	})
	if got := r.Redact("using or-novel-abc12345 now"); strings.Contains(got, "or-novel-abc12345") {
		t.Errorf("env secret not redacted: %q", got)
	}
	if got := r.Redact("path is /usr/bin:/bin"); !strings.Contains(got, "/usr/bin:/bin") {
		t.Errorf("PATH was wrongly redacted as a literal: %q", got)
	}
}

func TestRedactor_LeavesProseAlone(t *testing.T) {
	r := NewRedactor(nil)
	in := "The quick brown fox jumped over 12 lazy dogs at 9am. some_value: short."
	if got := r.Redact(in); got != in {
		t.Errorf("normal prose was altered:\n in:  %q\n got: %q", in, got)
	}
}

func TestRedactor_NilAndEmpty(t *testing.T) {
	var r *Redactor
	if got := r.Redact("anything"); got != "anything" {
		t.Errorf("nil redactor changed input: %q", got)
	}
	if got := NewRedactor(nil).Redact(""); got != "" {
		t.Errorf("empty input changed: %q", got)
	}
}
