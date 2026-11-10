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
