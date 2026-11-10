package redact

import (
	"encoding/json"
	"strings"
	"testing"
)

// An SSP deal token is the deal ID a trader sends to buyers. On 2026-11-10 a
// Raptive trader created two Magnite deals from chat and could not see their
// deal IDs: every "deal_token" came back "[REDACTED]" because the key ends in
// "token". These keys keep their values; real credential keys still do not.
func TestPublicIdentifierKeysKeepTheirValues(t *testing.T) {
	r := NewRedactor(nil)
	doc, _ := json.Marshal(map[string]any{
		"deal": map[string]any{
			"id": 3888139,
			"dsps": []any{
				map[string]any{"id": 2307, "name": "TheTradeDesk OpenRTB", "deal_token": "TRD-9262-016c4"},
			},
			"dealToken":              "INV-9262-014fe",
			"marketplace_deal_token": "MGNI-MD-131-37466",
			"auction_package_token":  "MGNI-AP-12-998877",
			"deal_tokens":            []any{"TRD-9262-016c5", "TRD-9262-016c6"},
		},
		"access_token":  "fixture-access-0123456789abcdef",
		"refresh_token": "fixture-refresh-0123456789abcdef",
		"api_token":     "fixture-api-0123456789abcdef",
		"deal_secret":   "fixture-dealsecret-0123456789",
	})
	out := r.Redact(string(doc))
	if !json.Valid([]byte(out)) {
		t.Fatalf("redaction broke JSON: %s", out)
	}
	for _, id := range []string{"TRD-9262-016c4", "INV-9262-014fe", "MGNI-MD-131-37466", "MGNI-AP-12-998877", "TRD-9262-016c5", "TRD-9262-016c6"} {
		if !strings.Contains(out, id) {
			t.Errorf("deal ID %s was redacted: %s", id, out)
		}
	}
	for _, secret := range []string{"fixture-access", "fixture-refresh", "fixture-api", "fixture-dealsecret"} {
		if strings.Contains(out, secret) {
			t.Errorf("credential %s survived: %s", secret, out)
		}
	}
}

// The same rule in plain text (a log line, a Python repr, prose): the marker
// pattern keeps a deal token and still scrubs a token-named credential,
// including one whose key merely contains "deal".
func TestPublicIdentifierKeysInPlainText(t *testing.T) {
	r := NewRedactor(nil)
	// Credential fixtures are built, not written out, so the secret scanner
	// does not take them for real keys (the same idiom as json_test.go).
	fake := func(name string) string { return "fixture-" + name + "-" + strings.Repeat("x", 12) }
	pair := func(key, value string) string { return " " + key + "=" + value }
	camelDealID := "INV-9262-014fe"
	in := `created {'deal_token': 'TRD-9262-016c4', 'marketplace_deal_token': 'MGNI-MD-131-37466'}` +
		pair("dealToken", camelDealID) +
		pair("access"+"_token", fake("access")) +
		pair("deal_access"+"_token", fake("dealaccess")) +
		pair("secret_deal"+"_token", fake("secretdeal"))
	out := r.Redact(in)
	for _, id := range []string{"TRD-9262-016c4", "MGNI-MD-131-37466", "INV-9262-014fe"} {
		if !strings.Contains(out, id) {
			t.Errorf("deal ID %s was redacted: %s", id, out)
		}
	}
	for _, secret := range []string{"fixture-access", "fixture-dealaccess", "fixture-secretdeal"} {
		if strings.Contains(out, secret) {
			t.Errorf("credential %s survived: %s", secret, out)
		}
	}
}

// A value shaped like a credential is still caught by its shape under a
// public-identifier key: only the key-name rule is relaxed.
func TestPublicIdentifierKeyStillScrubsCredentialShapes(t *testing.T) {
	r := NewRedactor(nil)
	key := "sk-ant-" + strings.Repeat("a", 40)
	doc, _ := json.Marshal(map[string]any{"deal_token": key})
	if out := r.Redact(string(doc)); strings.Contains(out, key) {
		t.Fatalf("credential-shaped value survived under deal_token: %s", out)
	}
	if out := r.Redact("deal_token: " + key); strings.Contains(out, key) {
		t.Fatalf("credential-shaped value survived in text: %s", out)
	}
}

// A deal token's value runs to the next delimiter the marker pattern knows,
// and "&", "=" and ";" are not among them: a credential that follows in the
// same query string or cookie-style list must still be scrubbed (Codex,
// PR #2122).
func TestPublicIdentifierValueIsRescanned(t *testing.T) {
	r := NewRedactor(nil)
	secret := "fixture-access-" + strings.Repeat("y", 16)
	for _, sep := range []string{"&", ";", "&amp;"} {
		in := "GET /x?deal" + "_token=TRD-9262-016c4" + sep + "access" + "_token=" + secret
		out := r.Redact(in)
		if strings.Contains(out, secret) {
			t.Errorf("sep %q: credential after a deal token survived: %s", sep, out)
		}
		if !strings.Contains(out, "TRD-9262-016c4") {
			t.Errorf("sep %q: deal ID was redacted: %s", sep, out)
		}
	}
	// Two deal tokens in a row both survive.
	two := "deal" + "_token=TRD-9262-016c4&deal" + "_token=TRD-9262-016c5"
	if out := r.Redact(two); !strings.Contains(out, "TRD-9262-016c4") || !strings.Contains(out, "TRD-9262-016c5") {
		t.Errorf("deal IDs were redacted: %s", out)
	}
}

// A key with a non-ASCII prefix is not the public key it ends with (Codex,
// PR #2122): "認証deal_token" keeps its value scrubbed.
func TestPublicIdentifierNeedsTheWholeKey(t *testing.T) {
	r := NewRedactor(nil)
	secret := "fixture-access-" + strings.Repeat("z", 16)
	for _, prefix := range []string{"認証", "é", "Ünd"} {
		in := "x " + prefix + "deal" + "_token=" + secret
		if out := r.Redact(in); strings.Contains(out, secret) {
			t.Errorf("prefix %q: value kept: %s", prefix, out)
		}
	}
}

// Many public tokens on one line are handled in one pass: every ID survives,
// a trailing credential is still scrubbed, and the line does not take
// quadratic time (Codex, PR #2122).
func TestManyPublicIdentifiersOnOneLine(t *testing.T) {
	r := NewRedactor(nil)
	secret := "fixture-access-" + strings.Repeat("w", 16)
	var b strings.Builder
	const n = 20000
	for i := 0; i < n; i++ {
		b.WriteString("deal" + "_token=TRD-12345678&")
	}
	b.WriteString("access" + "_token=" + secret)
	out := r.Redact(b.String())
	if strings.Contains(out, secret) {
		t.Fatal("trailing credential survived")
	}
	if got := strings.Count(out, "TRD-12345678"); got != n {
		t.Fatalf("kept %d of %d deal IDs", got, n)
	}
}

// With no public-identifier key in the input, the one-pass marker replace
// gives exactly what the plain regex replace gives: the exemption is the only
// behavior change.
func TestMarkerReplaceMatchesRegexWithoutPublicKeys(t *testing.T) {
	var marker pattern
	for _, p := range canonicalPatterns() {
		if p.keyMarker {
			marker = p
		}
	}
	if !marker.keyMarker {
		t.Fatal("no key-marker pattern")
	}
	parts := []string{
		"tok" + "en", "api_" + "key", "Secret", "passwd", "authorization", "_x", "-y", "_id",
		"=", ": ", ":", "'", "\"", " ", "&", ";", "{", "}", ",",
		"bearer ", "Basic ", "abcdefgh", "12345678", "xy", "ünï", "認証", "\n",
	}
	seed := uint64(1)
	next := func(n int) int {
		seed = seed*6364136223846793005 + 1442695040888963407
		return int((seed >> 33) % uint64(n))
	}
	for i := 0; i < 20000; i++ {
		var b strings.Builder
		for j := next(14); j >= 0; j-- {
			b.WriteString(parts[next(len(parts))])
		}
		in := b.String()
		if want, got := marker.re.ReplaceAllString(in, marker.repl), marker.replace(in); got != want {
			t.Fatalf("input %q:\n regex: %q\n  pass: %q", in, want, got)
		}
	}
}

// A deal-token key counts only where a field starts; a qualified name
// ("secret.deal_token", "a/deal_token", "x:deal_token") is a different key and
// keeps its value scrubbed (Codex, PR #2122). Ordinary field starts still
// keep the deal ID.
func TestPublicIdentifierNeedsAFieldBoundary(t *testing.T) {
	r := NewRedactor(nil)
	secret := "fixture-access-" + strings.Repeat("v", 16)
	for _, prefix := range []string{"secret.", "a/", "x:", "cfg|", "k@", "n#"} {
		in := "y " + prefix + "deal" + "_token=" + secret
		if out := r.Redact(in); strings.Contains(out, secret) {
			t.Errorf("prefix %q: value kept: %s", prefix, out)
		}
	}
	id := "TRD-9262-016c4"
	for _, lead := range []string{"", " ", "\t", "?", "&", ";", "{", ",", "(", "[", "'", "\""} {
		in := lead + "deal" + "_token=" + id
		if out := r.Redact(in); !strings.Contains(out, id) {
			t.Errorf("lead %q: deal ID was redacted: %s", lead, out)
		}
	}
}
