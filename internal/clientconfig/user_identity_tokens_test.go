package clientconfig

import (
	"slices"
	"strings"
	"testing"
)

// TestReservedUserTokensSurviveLoad pins the load half of ADR-0087: the
// ${FLEET_USER_EMAIL} token passes through both interpolation passes verbatim,
// for the credential owner to substitute when it
// opens an interactive scope. An operator-exported variable of the same name is
// the realistic hijack — resolved here it would give every user's turn one
// fixed person's identity — so it is set and must not appear.
func TestReservedUserTokensSurviveLoad(t *testing.T) {
	t.Setenv("FLEET_USER_EMAIL", "operator@example.com")
	b, err := Load(writeManifest(t, `
mcp_servers:
  - name: deals
    type: stdio
    command: python3
    args: ["deals.py"]
    always: true
    env:
      DEAL_SHEET_DEFAULT_TO: "${FLEET_USER_EMAIL}"
      DEAL_SHEET_REPLY_TO: "Reply-To: <${FLEET_USER_EMAIL}>"
    optional_env: ["DEAL_SHEET_DEFAULT_TO"]
`))
	if err != nil {
		t.Fatalf("user tokens in an env map are the substitution contract; load failed: %v", err)
	}
	env := b.MCPServerConfigs()["deals"].Env
	if got := env["DEAL_SHEET_DEFAULT_TO"]; got != "${FLEET_USER_EMAIL}" {
		t.Errorf("DEAL_SHEET_DEFAULT_TO = %q, want the reserved token preserved verbatim", got)
	}
	if got := env["DEAL_SHEET_REPLY_TO"]; got != "Reply-To: <${FLEET_USER_EMAIL}>" {
		t.Errorf("DEAL_SHEET_REPLY_TO = %q, want token + text preserved", got)
	}
	names := b.EnvVarNames()
	if slices.Contains(names, "FLEET_USER_EMAIL") {
		t.Errorf("EnvVarNames must exclude the reserved user token, got %v", names)
	}
}

// TestReservedUserTokenMisuseFailsLoad: like the workspace tokens, the user
// tokens take only the bare spelling and only in an mcp_servers env value. A
// default form would resolve from the process env (or bake a fixed person in);
// a header or url would ship the literal token on the wire.
func TestReservedUserTokenMisuseFailsLoad(t *testing.T) {
	cases := []struct{ name, manifest, want string }{
		{"email default form", `
mcp_servers:
  - name: deals
    command: python3
    args: ["deals.py"]
    always: true
    env:
      TO: "${FLEET_USER_EMAIL:-team@example.com}"
`, "RESERVED runtime token"},
		{"name required form", `
mcp_servers:
  - name: deals
    command: python3
    args: ["deals.py"]
    always: true
    env:
      NAME: "${FLEET_USER_NAME:?who}"
`, "fleet has no ${FLEET_USER_NAME} token"},
		// The spawn paths match only the exact "${NAME}" spelling, so a padded
		// one would ship as a literal placeholder to every connector — even a
		// scheduled run's, which must never see anything for these keys.
		{"padded email spelling", `
mcp_servers:
  - name: deals
    command: python3
    args: ["deals.py"]
    always: true
    env:
      TO: "${ FLEET_USER_EMAIL }"
`, "spaces inside the braces"},
		{"padded workspace spelling", `
mcp_servers:
  - name: deals
    command: python3
    args: ["deals.py"]
    always: true
    env:
      RUN_WORKDIR: "${FLEET_WORKSPACE }"
`, "spaces inside the braces"},
		// An escape collapses to the exact token one pass before spawn, which
		// would hand the connector the user's email instead of the literal
		// text the bundle asked for — so it is refused.
		{"escaped email", `
mcp_servers:
  - name: deals
    command: python3
    args: ["deals.py"]
    always: true
    env:
      TEMPLATE: "Reply to $${FLEET_USER_EMAIL}"
`, "an escaped FLEET_USER_EMAIL cannot be written"},
		// The interpolator never expands a default body, so an escape nested
		// in one would survive verbatim to spawn — caught on the raw text.
		{"escaped email nested in a default", `
mcp_servers:
  - name: deals
    command: python3
    args: ["deals.py"]
    always: true
    env:
      TEMPLATE: "${DEALS_TEMPLATE:-$${FLEET_USER_EMAIL}}"
`, "an escaped FLEET_USER_EMAIL cannot be written"},
		// Fleet never spawns an http server, so its env could never carry
		// the user; accepting it would claim a contract that does not exist.
		{"user token in an http server env", `
mcp_servers:
  - name: remote
    type: http
    url: "https://example.test/mcp"
    always: true
    env:
      TO: "${FLEET_USER_EMAIL}"
`, "only for stdio servers"},
		{"escaped name in a url", `
mcp_servers:
  - name: remote
    type: http
    url: "https://example.test/$${FLEET_USER_NAME}"
    always: true
`, "fleet has no ${FLEET_USER_NAME} token"},
		// The signed-in user is data, never routing identity: a key that
		// routes a seat or money must not be filled from the keyboard.
		{"user token in identity_env", `
mcp_servers:
  - name: deals
    command: python3
    args: ["deals.py"]
    always: true
    env:
      OWNER_ID: "${FLEET_USER_EMAIL}"
    identity_env: ["OWNER_ID"]
`, "must never route a seat or money"},
		{"email in a header", `
mcp_servers:
  - name: remote
    type: http
    url: "https://example.test/mcp"
    always: true
    headers:
      X-User: "${FLEET_USER_EMAIL}"
`, "reserved runtime token"},
		{"name in a url", `
mcp_servers:
  - name: remote
    type: http
    url: "https://example.test/${FLEET_USER_NAME}"
    always: true
`, "fleet has no ${FLEET_USER_NAME} token"},
		// The spawn paths substitute env VALUES only: a token in an env key
		// would name a literal "${FLEET_USER_EMAIL}" variable.
		{"user token as an env key", `
mcp_servers:
  - name: deals
    command: python3
    args: ["deals.py"]
    always: true
    env:
      "${FLEET_USER_EMAIL}": "x"
`, "env.${FLEET_USER_EMAIL} (map key): ${FLEET_USER_EMAIL} is a reserved runtime token"},
		{"workspace token as an env key", `
mcp_servers:
  - name: deals
    command: python3
    args: ["deals.py"]
    always: true
    env:
      "${FLEET_WORKSPACE}": "x"
`, "(map key): ${FLEET_WORKSPACE} is a reserved runtime token"},
		// Go's \s is ASCII-only: Unicode padding must be refused too, both
		// written literally (caught on the raw text) and as a YAML escape
		// that only becomes padding once parsed (caught per server).
		{"NBSP-padded email", "\nmcp_servers:\n  - name: deals\n    command: python3\n    args: [\"deals.py\"]\n    always: true\n    env:\n      TO: \"${\u00a0FLEET_USER_EMAIL\u00a0}\"\n",
			"RESERVED runtime token"},
		{"zero-width-padded email", "\nmcp_servers:\n  - name: deals\n    command: python3\n    args: [\"deals.py\"]\n    always: true\n    env:\n      TO: \"${\u200bFLEET_USER_EMAIL}\"\n",
			"RESERVED runtime token"},
		{"escaped ideographic-space-padded email", "\nmcp_servers:\n  - name: deals\n    command: python3\n    args: [\"deals.py\"]\n    always: true\n    env:\n      TO: \"$${\u3000FLEET_USER_EMAIL}\"\n",
			"an escaped FLEET_USER_EMAIL cannot be written"},
		{"YAML-escaped NBSP padding", `
mcp_servers:
  - name: deals
    command: python3
    args: ["deals.py"]
    always: true
    env:
      TO: "${\u00a0FLEET_USER_EMAIL}"
`, "RESERVED runtime token"},
		{"YAML-escaped token in an arg", `
mcp_servers:
  - name: deals
    command: python3
    args: ["deals.py", "--to=${\u0020FLEET_USER_EMAIL}"]
    always: true
`, "mcp_servers[0].args[1]: ${ FLEET_USER_EMAIL} is a reserved runtime token"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeManifest(t, tc.manifest))
			if err == nil {
				t.Fatal("expected the load to fail")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %v, want it to contain %q", err, tc.want)
			}
		})
	}
}

// TestEscapedOrdinaryVarStillLoads: the escaped-user-token refusal is scoped
// to the two user tokens; any other $${...} keeps its literal meaning.
func TestEscapedOrdinaryVarStillLoads(t *testing.T) {
	b, err := Load(writeManifest(t, `
mcp_servers:
  - name: deals
    command: python3
    args: ["deals.py"]
    always: true
    env:
      TEMPLATE: "$${NOT_A_FLEET_TOKEN}"
`))
	if err != nil {
		t.Fatalf("an ordinary escape must still load: %v", err)
	}
	if got := b.MCPServerConfigs()["deals"].Env["TEMPLATE"]; got != "${NOT_A_FLEET_TOKEN}" {
		t.Errorf("TEMPLATE = %q, want the literal text", got)
	}
}

// TestUserTokenFromProcessEnvCountsAsUnset: the user token is honoured only
// where the bundle writes it. A process value that names it — reached through
// an ordinary ${VAR}, at load or at catalog-build time — counts as unset, so
// `OWNER_ID: "${OWNER_ID}"` with OWNER_ID=${FLEET_USER_EMAIL} exported can
// never become the signed-in user's email at spawn (in an identity_env key a
// seat would follow the user). ADR-0087.
func TestUserTokenFromProcessEnvCountsAsUnset(t *testing.T) {
	manifest := `
mcp_servers:
  - name: deals
    command: python3
    args: ["deals.py"]
    always: true
    env:
      OWNER_ID: "${DEALS_OWNER_ID}"
      REPLY_TO: "${DEALS_REPLY_TO:-ops@example.com}"
      GREETING: "${DEALS_GREETING}"
      TO: "${FLEET_USER_EMAIL}"
    identity_env: ["OWNER_ID"]
    optional_env: ["GREETING"]
`
	check := func(t *testing.T, b *Bundle) {
		t.Helper()
		env := b.MCPServerConfigs()["deals"].Env
		for k, v := range env {
			if k != "TO" && strings.Contains(v, "FLEET_USER_") {
				t.Errorf("env %s = %q: a process value carried the user token into the spawn env", k, v)
			}
		}
		if env["OWNER_ID"] != "" {
			t.Errorf("OWNER_ID = %q, want unset (empty)", env["OWNER_ID"])
		}
		if env["REPLY_TO"] != "ops@example.com" {
			t.Errorf("REPLY_TO = %q, want the manifest default", env["REPLY_TO"])
		}
		if _, ok := env["GREETING"]; ok {
			t.Errorf("GREETING present (%q), want the optional key dropped", env["GREETING"])
		}
		if env["TO"] != "${FLEET_USER_EMAIL}" {
			t.Errorf("TO = %q, want the bundle-written token kept", env["TO"])
		}
	}
	t.Run("exported before load", func(t *testing.T) {
		t.Setenv("DEALS_OWNER_ID", "${FLEET_USER_EMAIL}")
		t.Setenv("DEALS_REPLY_TO", "<${FLEET_USER_EMAIL}>")
		t.Setenv("DEALS_GREETING", "Hi ${FLEET_USER_NAME},")
		b, err := Load(writeManifest(t, manifest))
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		check(t, b)
	})
	t.Run("exported after load", func(t *testing.T) {
		b, err := Load(writeManifest(t, manifest))
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		t.Setenv("DEALS_OWNER_ID", "$${FLEET_USER_EMAIL}")
		t.Setenv("DEALS_REPLY_TO", "${ FLEET_USER_EMAIL }")
		t.Setenv("DEALS_GREETING", "${FLEET_USER_EMAIL}")
		check(t, b)
	})
}

// TestUserNameTokenIsRefused: fleet has no name for the signed-in user (sign-in
// carries an email and no name claim, and a name is never guessed from the
// email), so FLEET_USER_NAME is reserved with no token behind it. Every
// spelling fails the load with an error that says so — the exact one too, and
// with an operator-exported FLEET_USER_NAME that must never be read.
func TestUserNameTokenIsRefused(t *testing.T) {
	t.Setenv("FLEET_USER_NAME", "Operator")
	for name, value := range map[string]string{
		"exact":            "${FLEET_USER_NAME}",
		"in a greeting":    "Hi ${FLEET_USER_NAME},",
		"beside the email": "${FLEET_USER_NAME} <${FLEET_USER_EMAIL}>",
		"escaped":          "$${FLEET_USER_NAME}",
		"padded":           "${ FLEET_USER_NAME }",
		"default form":     "${FLEET_USER_NAME:-team}",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Load(writeManifest(t, `
mcp_servers:
  - name: deals
    command: python3
    args: ["deals.py"]
    always: true
    env:
      GREETING: "`+value+`"
`))
			if err == nil || !strings.Contains(err.Error(), "fleet has no ${FLEET_USER_NAME} token") {
				t.Fatalf("error = %v, want the no-name-token refusal", err)
			}
		})
	}
}
