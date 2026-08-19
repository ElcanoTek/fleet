package redact

import (
	"fmt"
	"strings"
	"sync"
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
		// Keyword as an interior token of a longer key name (#569): the marker
		// rule must match aws_secret_access_key-style names, not just names that
		// END at the keyword.
		{"aws secret ini form", "aws_secret_access_key = wJalrXUtnFEMI/K7MDENGbPxRfiCY", "wJalrXUtnFEMI/K7MDENGbPxRfiCY", "aws_secret_access_key"},
		{"aws secret json form", `{"aws_secret_access_key":"wJalrXUtnFEMIsupersecret"}`, "wJalrXUtnFEMIsupersecret", "aws_secret_access_key"},
		{"secret access key colon", "secret_access_key: AKIAIOSFODNN7supersecret", "AKIAIOSFODNN7supersecret", "secret_access_key"},
		{"refresh token", "gcp_refresh_token=1//0eXaMpLeReFrEsH", "1//0eXaMpLeReFrEsH", "gcp_refresh_token"},
		// HTTP Basic auth (#569): the base64 credential decodes to plaintext
		// user:password, so it must be scrubbed like a Bearer token.
		{"basic auth", "Authorization: Basic dXNlcjpzdXBlcnNlY3JldA==", "dXNlcjpzdXBlcnNlY3JldA==", "Authorization"},
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
	cases := []string{
		"The quick brown fox jumped over 12 lazy dogs at 9am. some_value: short.",
		// Guards against the interior-keyword extension (#569) over-matching:
		// prose words that merely EMBED a keyword (secretary, tokenizer) have no
		// _/- boundary after it, so they must not trigger the marker rule.
		"the secretary scheduled tokenizer = byte-pair-encoding for review",
		"passwords must be rotated quarterly per the security policy",
	}
	for _, in := range cases {
		if got := r.Redact(in); got != in {
			t.Errorf("normal prose was altered:\n in:  %q\n got: %q", in, got)
		}
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

// TestRedactor_AddLiteralDedupes pins that re-registering the same value —
// which the runtime token-acquisition hook does on every turn (#1124) — does
// not grow the literal scan list.
func TestRedactor_AddLiteralDedupes(t *testing.T) {
	r := NewRedactor(nil)
	for i := 0; i < 100; i++ {
		r.AddLiteral("repeat-token-abcdef12")
	}
	if n := len(r.literals); n != 1 {
		t.Errorf("literals grew to %d entries for one distinct value, want 1", n)
	}
	if got := r.Redact("saw repeat-token-abcdef12 here"); strings.Contains(got, "repeat-token") {
		t.Errorf("deduped literal not redacted: %q", got)
	}
}

// TestRedactor_ConcurrentAddLiteralRedact exercises the #1124 concurrency
// contract under -race: AddLiteral (a token acquired mid-serve) racing Redact
// must be safe, and a literal added before a Redact call must be scrubbed by
// that call.
func TestRedactor_ConcurrentAddLiteralRedact(t *testing.T) {
	r := NewRedactor(nil)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for g := 0; g < 4; g++ {
		wg.Add(2)
		go func(g int) {
			defer wg.Done()
			<-start
			for i := 0; i < 200; i++ {
				r.AddLiteral(fmt.Sprintf("runtime-token-%02d-%04d", g, i))
			}
		}(g)
		go func(g int) {
			defer wg.Done()
			<-start
			for i := 0; i < 200; i++ {
				_ = r.Redact(fmt.Sprintf("error for runtime-token-%02d-%04d and friends", g, i))
			}
		}(g)
	}
	close(start)
	wg.Wait()
	// Registration is immediately visible to later Redact calls.
	if got := r.Redact("late check runtime-token-00-0000"); strings.Contains(got, "runtime-token-00-0000") {
		t.Errorf("literal registered concurrently was not redacted afterwards: %q", got)
	}
}
