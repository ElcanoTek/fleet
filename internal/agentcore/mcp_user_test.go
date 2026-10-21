package agentcore

import (
	"reflect"
	"testing"
)

func TestNewMCPUserIdentity(t *testing.T) {
	tests := []struct {
		email, firstName string
		want             MCPUserIdentity
	}{
		// The saved first name is used as-is (trimmed).
		{"elyse@elcanotek.com", "Elyse", MCPUserIdentity{Email: "elyse@elcanotek.com", Name: "Elyse"}},
		{"  Elyse.Messer@Example.COM ", "  Elyse ", MCPUserIdentity{Email: "elyse.messer@example.com", Name: "Elyse"}},
		{"jsmith@example.com", "Jo-Anne", MCPUserIdentity{Email: "jsmith@example.com", Name: "Jo-Anne"}},
		{"ops@example.com", "Mary Ann", MCPUserIdentity{Email: "ops@example.com", Name: "Mary Ann"}},
		// No saved name: the email resolves, the name does NOT — it is never
		// guessed from the local part, however name-like that looks.
		{"elyse@elcanotek.com", "", MCPUserIdentity{Email: "elyse@elcanotek.com"}},
		{"elyse.messer@example.com", "", MCPUserIdentity{Email: "elyse.messer@example.com"}},
		{"jsmith@example.com", "   ", MCPUserIdentity{Email: "jsmith@example.com"}},
		// A saved value that is not a name (an email, a handle with digits, a
		// token) is treated as no name, not passed through.
		{"elyse@elcanotek.com", "elyse@elcanotek.com", MCPUserIdentity{Email: "elyse@elcanotek.com"}},
		{"elyse@elcanotek.com", "ops2", MCPUserIdentity{Email: "elyse@elcanotek.com"}},
		{"elyse@elcanotek.com", "${FLEET_USER_EMAIL}", MCPUserIdentity{Email: "elyse@elcanotek.com"}},
		// Not an address fleet will hand a connector: nothing resolves, not
		// even a valid saved name.
		{"", "Elyse", MCPUserIdentity{}},
		{"no-at-sign", "Elyse", MCPUserIdentity{}},
		{"a@b@example.com", "", MCPUserIdentity{}},
		{"user@localhost", "", MCPUserIdentity{}},
		{"user@example..com", "", MCPUserIdentity{}},
		{"${FLEET_WORKSPACE}@example.com", "", MCPUserIdentity{}},
		{"us{er}@example.com", "", MCPUserIdentity{}},
		{"user name@example.com", "", MCPUserIdentity{}},
		{"user@exa\nmple.com", "", MCPUserIdentity{}},
	}
	for _, tt := range tests {
		if got := NewMCPUserIdentity(tt.email, tt.firstName); got != tt.want {
			t.Errorf("NewMCPUserIdentity(%q, %q) = %+v, want %+v", tt.email, tt.firstName, got, tt.want)
		}
	}
}

func TestMCPUserIdentitySanitized(t *testing.T) {
	got := MCPUserIdentity{Email: " Trader@Example.com ", Name: "Anne-Marie O'Neil"}.Sanitized()
	if want := (MCPUserIdentity{Email: "trader@example.com", Name: "Anne-Marie O'Neil"}); got != want {
		t.Fatalf("Sanitized = %+v, want %+v", got, want)
	}
	// Each field is judged on its own: a bad name never takes the email with it.
	for _, bad := range []string{"", "${FLEET_USER_EMAIL}", "Robert'); DROP", "a\nb", "<b>x</b>", "Two  Spaces", "123", "elyse@example.com"} {
		got := MCPUserIdentity{Email: "trader@example.com", Name: bad}.Sanitized()
		if got.Name != "" || got.Email != "trader@example.com" {
			t.Errorf("Sanitized(name=%q) = %+v, want the name cleared and the email kept", bad, got)
		}
	}
	if got := (MCPUserIdentity{Email: "not an email", Name: "Trader"}).Sanitized(); got != (MCPUserIdentity{Name: "Trader"}) {
		t.Errorf("Sanitized(bad email) = %+v, want only the name", got)
	}
}

func TestExpandUserEnv(t *testing.T) {
	env := map[string]string{
		"TO":       "${FLEET_USER_EMAIL}",
		"GREETING": "Hi ${FLEET_USER_NAME},",
		"BOTH":     "${FLEET_USER_NAME} <${FLEET_USER_EMAIL}>",
		"API_KEY":  "secret-value",
	}
	t.Run("signed-in user substitutes every token", func(t *testing.T) {
		got := ExpandUserEnv(env, MCPUserIdentity{Email: "elyse@example.com", Name: "Elyse"})
		want := map[string]string{
			"TO":       "elyse@example.com",
			"GREETING": "Hi Elyse,",
			"BOTH":     "Elyse <elyse@example.com>",
			"API_KEY":  "secret-value",
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
	t.Run("a missing name drops only the keys that need it", func(t *testing.T) {
		got := ExpandUserEnv(env, MCPUserIdentity{Email: "j.smith@example.com"})
		want := map[string]string{"TO": "j.smith@example.com", "API_KEY": "secret-value"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("ExpandUserEnv(email only) = %v, want %v", got, want)
		}
	})
	t.Run("an unvalidated value is treated as absent", func(t *testing.T) {
		got := ExpandUserEnv(env, MCPUserIdentity{Email: "x${FLEET_WORKSPACE}@example.com", Name: "${FLEET_TASK_ID}"})
		if want := map[string]string{"API_KEY": "secret-value"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("ExpandUserEnv(hostile) = %v, want %v", got, want)
		}
	})
	// Spawn-time backstop for spellings the loader refuses: an escape left
	// verbatim (e.g. nested in a default body) or a padded/colon form must be
	// dropped — never substituted into "$elyse@…", never shipped literally.
	t.Run("escaped or non-canonical spellings are dropped", func(t *testing.T) {
		got := ExpandUserEnv(map[string]string{
			"ESCAPED": "$${FLEET_USER_EMAIL}",
			"PADDED":  "${ FLEET_USER_EMAIL }",
			"DEFAULT": "${FLEET_USER_NAME:-team}",
			"MIXED":   "${FLEET_USER_EMAIL} / $${FLEET_USER_NAME}",
			"OK":      "${FLEET_USER_EMAIL}",
		}, MCPUserIdentity{Email: "elyse@example.com", Name: "Elyse"})
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
