package apikeys

import (
	"net/http"
	"path/filepath"
	"testing"
)

// A user key (ADR-0108) names the account it acts as on the chat API, and
// carries no Operations Center scope at all.
func TestUserKey(t *testing.T) {
	dir := t.TempDir()
	m, err := NewManager(filepath.Join(dir, "keys.json"), filepath.Join(dir, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := m.CreateUserKey("cli", "  ", 0, nil, ""); err == nil {
		t.Fatal("a user key without an account was minted")
	}
	if _, _, err := m.CreateTypedKey("cli", KeyTypeUser, nil, 0, nil, ""); err == nil {
		t.Fatal("CreateTypedKey minted a user key with no account")
	}
	key, raw, err := m.CreateUserKey("cli", " Me@Example.com ", 0, nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if key.Type != KeyTypeUser || key.UserEmail != "me@example.com" || len(key.Permissions) != 0 {
		t.Fatalf("key = %+v", key)
	}
	if kt, _, err := ParseAPIKey(raw); err != nil || kt != KeyTypeUser {
		t.Fatalf("ParseAPIKey = %q, %v", kt, err)
	}
	ok, got, _ := m.ValidateKey(raw, nil, nil, nil)
	if !ok || got.UserEmail != "me@example.com" {
		t.Fatalf("ValidateKey = %v %+v", ok, got)
	}
	for _, method := range []string{http.MethodGet, http.MethodPost} {
		if TypeAllowsMethod(KeyTypeUser, method) {
			t.Errorf("a user key may %s a scoped-key route", method)
		}
	}
	if resp := key.ToResponse(); resp.UserEmail != "me@example.com" || resp.Type != "user" {
		t.Errorf("response = %+v", resp)
	}
	// Rotation keeps the account.
	rotated, _, err := m.RotateKey(key.KeyID, 1)
	if err != nil || rotated.UserEmail != "me@example.com" || rotated.Type != KeyTypeUser {
		t.Fatalf("rotate = %+v, %v", rotated, err)
	}
}
