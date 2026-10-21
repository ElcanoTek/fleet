package agentcore

import (
	"reflect"
	"strings"
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

// TestResolveMCPVariant_RefusesUserTokenFromOverlay: a <VAR>_<ACCOUNT> overlay
// comes from the process env, not the bundle, so one that names the user
// token must never be filled with the signed-in user at spawn — in an
// identity_env key that would route a seat by whoever is at the keyboard. The
// seat is refused instead (ADR-0087).
func TestResolveMCPVariant_RefusesUserTokenFromOverlay(t *testing.T) {
	base := MCPServerBase{
		BaseEnv:     map[string]string{"DEALS_API_KEY": "default-key", "OWNER_ID": "default-owner"},
		IdentityEnv: []string{"OWNER_ID"},
		Command:     "python",
	}
	t.Setenv("DEALS_API_KEY_CLIENT_A", "client-a-key")
	t.Setenv("OWNER_ID_CLIENT_A", "${FLEET_USER_EMAIL}")
	if name, env, err := resolveMCPVariant("deals", base, "client_a"); err == nil || !strings.Contains(err.Error(), "OWNER_ID_CLIENT_A names the reserved signed-in-user token") {
		t.Fatalf("resolveMCPVariant = (%q, %v, %v), want the seat refused", name, env, err)
	}
	// An ordinary overlay still binds.
	t.Setenv("OWNER_ID_CLIENT_A", "client-a-owner")
	if _, env, err := resolveMCPVariant("deals", base, "client_a"); err != nil || env["OWNER_ID"] != "client-a-owner" {
		t.Fatalf("resolveMCPVariant(ordinary overlay) = (%v, %v), want the client_a owner", env, err)
	}
}

// TestSpawnEnv_UserTokenExpandsFirst: the user token is expanded on the
// bundle-written values BEFORE the workspace and task substitutions, so a
// workspace path that happens to contain the token text can never turn a key
// the bundle did not write it into (an identity_env key included) into a
// user-dependent one: that text stays literal, and nothing is dropped on a
// user-less path either (ADR-0087).
func TestSpawnEnv_UserTokenExpandsFirst(t *testing.T) {
	t.Setenv("FLEET_WORKSPACE_ROOT", "/srv/${FLEET_USER_EMAIL}")
	env := map[string]string{
		"OWNER_ID": "${FLEET_WORKSPACE_ROOT}/owner",
		"WORKDIR":  "${FLEET_WORKSPACE}",
		"TO":       "${FLEET_USER_EMAIL}",
	}
	user := MCPUserIdentity{Email: "elyse@example.com"}
	got := SpawnEnv(env, "/work/${FLEET_USER_EMAIL}", "", user)
	want := map[string]string{
		"OWNER_ID": "/srv/${FLEET_USER_EMAIL}/owner",
		"WORKDIR":  "/work/${FLEET_USER_EMAIL}",
		"TO":       "elyse@example.com",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SpawnEnv = %v, want %v", got, want)
	}
	got = SpawnEnv(env, "/work/${FLEET_USER_EMAIL}", "", MCPUserIdentity{})
	delete(want, "TO")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("SpawnEnv(no user) = %v, want %v (only the bundle-written key dropped)", got, want)
	}
	// The task id is applied by the same helper (stamped on the base, never
	// pre-substituted), after the user token: text in it stays literal too.
	got = SpawnEnv(map[string]string{"RUN": "${FLEET_TASK_ID}", "TO": "${FLEET_USER_EMAIL}"}, "", "task-${FLEET_USER_EMAIL}", MCPUserIdentity{})
	if want := map[string]string{"RUN": "task-${FLEET_USER_EMAIL}"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("SpawnEnv(task) = %v, want %v", got, want)
	}
	if got := SpawnEnv(map[string]string{"RUN": "${FLEET_TASK_ID}"}, "", "", MCPUserIdentity{}); len(got) != 0 {
		t.Fatalf("SpawnEnv(no task) = %v, want the task key dropped", got)
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
