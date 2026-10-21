package agentcore

import (
	"reflect"
	"testing"
)

func TestNewMCPUserIdentity(t *testing.T) {
	tests := []struct {
		email string
		want  MCPUserIdentity
	}{
		{"elyse@elcanotek.com", MCPUserIdentity{Email: "elyse@elcanotek.com"}},
		{"  Elyse.Messer@Example.COM ", MCPUserIdentity{Email: "elyse.messer@example.com"}},
		{"o'neil+deals@example.co.uk", MCPUserIdentity{Email: "o'neil+deals@example.co.uk"}},
		// Not an address fleet will hand a connector: nothing resolves.
		{"", MCPUserIdentity{}},
		{"no-at-sign", MCPUserIdentity{}},
		{"a@b@example.com", MCPUserIdentity{}},
		{"user@localhost", MCPUserIdentity{}},
		{"user@example..com", MCPUserIdentity{}},
		{"${FLEET_WORKSPACE}@example.com", MCPUserIdentity{}},
		{"us{er}@example.com", MCPUserIdentity{}},
		{"user name@example.com", MCPUserIdentity{}},
		{"user@exa\nmple.com", MCPUserIdentity{}},
	}
	for _, tt := range tests {
		if got := NewMCPUserIdentity(tt.email); got != tt.want {
			t.Errorf("NewMCPUserIdentity(%q) = %+v, want %+v", tt.email, got, tt.want)
		}
	}
}

func TestMCPUserIdentitySanitized(t *testing.T) {
	got := MCPUserIdentity{Email: " Trader@Example.com "}.Sanitized()
	if want := (MCPUserIdentity{Email: "trader@example.com"}); got != want {
		t.Fatalf("Sanitized = %+v, want %+v", got, want)
	}
	if got := (MCPUserIdentity{Email: "not an email"}).Sanitized(); !got.IsZero() {
		t.Errorf("Sanitized(bad email) = %+v, want the zero identity", got)
	}
}

func TestExpandUserEnv(t *testing.T) {
	env := map[string]string{
		"TO":      "${FLEET_USER_EMAIL}",
		"REPLY":   "Reply-To: <${FLEET_USER_EMAIL}>",
		"API_KEY": "secret-value",
	}
	t.Run("signed-in user substitutes the token", func(t *testing.T) {
		got := ExpandUserEnv(env, MCPUserIdentity{Email: "elyse@example.com"})
		want := map[string]string{
			"TO":      "elyse@example.com",
			"REPLY":   "Reply-To: <elyse@example.com>",
			"API_KEY": "secret-value",
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("ExpandUserEnv = %v, want %v", got, want)
		}
		if env["TO"] != "${FLEET_USER_EMAIL}" {
			t.Fatal("ExpandUserEnv mutated its input map")
		}
	})
	t.Run("no user drops every token-bearing key", func(t *testing.T) {
		got := ExpandUserEnv(env, MCPUserIdentity{})
		if want := map[string]string{"API_KEY": "secret-value"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("ExpandUserEnv(zero) = %v, want %v", got, want)
		}
	})
	t.Run("an unvalidated value is treated as absent", func(t *testing.T) {
		got := ExpandUserEnv(env, MCPUserIdentity{Email: "x${FLEET_WORKSPACE}@example.com"})
		if want := map[string]string{"API_KEY": "secret-value"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("ExpandUserEnv(hostile) = %v, want %v", got, want)
		}
	})
	// FLEET_USER_NAME is reserved with no token behind it: a key that still
	// carries it is dropped, never handed a name, a literal, or a blank.
	t.Run("the refused name variable drops its key", func(t *testing.T) {
		got := ExpandUserEnv(map[string]string{
			"GREETING": "Hi ${FLEET_USER_NAME},",
			"BOTH":     "${FLEET_USER_NAME} <${FLEET_USER_EMAIL}>",
			"OK":       "${FLEET_USER_EMAIL}",
		}, MCPUserIdentity{Email: "elyse@example.com"})
		if want := map[string]string{"OK": "elyse@example.com"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("ExpandUserEnv = %v, want %v", got, want)
		}
	})
	// Spawn-time backstop for spellings the loader refuses: an escape left
	// verbatim (e.g. nested in a default body) or a padded/colon form must be
	// dropped, never substituted into "$elyse@...", never shipped literally.
	t.Run("escaped or non-canonical spellings are dropped", func(t *testing.T) {
		got := ExpandUserEnv(map[string]string{
			"ESCAPED": "$${FLEET_USER_EMAIL}",
			"PADDED":  "${ FLEET_USER_EMAIL }",
			"DEFAULT": "${FLEET_USER_EMAIL:-team@example.com}",
			"MIXED":   "${FLEET_USER_EMAIL} / $${FLEET_USER_EMAIL}",
			// Unicode padding: RE2's \s is ASCII-only, so these used to be
			// passed through as literal placeholders.
			"NBSP":       "${\u00a0FLEET_USER_EMAIL\u00a0}",
			"EM_SPACE":   "${\u2003FLEET_USER_EMAIL}",
			"ZERO_WIDTH": "${\u200bFLEET_USER_EMAIL}",
			"ESC_NBSP":   "$${\u00a0FLEET_USER_EMAIL}",
			"OK":         "${FLEET_USER_EMAIL}",
		}, MCPUserIdentity{Email: "elyse@example.com"})
		if want := map[string]string{"OK": "elyse@example.com"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("ExpandUserEnv = %v, want %v", got, want)
		}
	})
	t.Run("a token-free map is returned as-is", func(t *testing.T) {
		plain := map[string]string{"A": "1"}
		got := ExpandUserEnv(plain, MCPUserIdentity{Email: "elyse@example.com"})
		got["B"] = "2"
		if plain["B"] != "2" {
			t.Fatal("token-free map was copied; the no-token path should cost nothing")
		}
	})
}
