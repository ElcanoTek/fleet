package clientconfig

import (
	"slices"
	"strings"
	"testing"
)

// TestReservedUserTokensSurviveLoad pins the load half of ADR-0087: the
// ${FLEET_USER_EMAIL} / ${FLEET_USER_NAME} tokens pass through both
// interpolation passes verbatim, for the credential owner to substitute when it
// opens an interactive scope. An operator-exported variable of the same name is
// the realistic hijack — resolved here it would give every user's turn one
// fixed person's identity — so it is set and must not appear.
func TestReservedUserTokensSurviveLoad(t *testing.T) {
	t.Setenv("FLEET_USER_EMAIL", "operator@example.com")
	t.Setenv("FLEET_USER_NAME", "Operator")
	b, err := Load(writeManifest(t, `
mcp_servers:
  - name: deals
    type: stdio
    command: python3
    args: ["deals.py"]
    always: true
    env:
      DEAL_SHEET_DEFAULT_TO: "${FLEET_USER_EMAIL}"
      DEAL_SHEET_GREETING: "Hi ${FLEET_USER_NAME},"
    optional_env: ["DEAL_SHEET_DEFAULT_TO"]
`))
	if err != nil {
		t.Fatalf("user tokens in an env map are the substitution contract; load failed: %v", err)
	}
	env := b.MCPServerConfigs()["deals"].Env
	if got := env["DEAL_SHEET_DEFAULT_TO"]; got != "${FLEET_USER_EMAIL}" {
		t.Errorf("DEAL_SHEET_DEFAULT_TO = %q, want the reserved token preserved verbatim", got)
	}
	if got := env["DEAL_SHEET_GREETING"]; got != "Hi ${FLEET_USER_NAME}," {
		t.Errorf("DEAL_SHEET_GREETING = %q, want token + text preserved", got)
	}
	names := b.EnvVarNames()
	if slices.Contains(names, "FLEET_USER_EMAIL") || slices.Contains(names, "FLEET_USER_NAME") {
		t.Errorf("EnvVarNames must exclude the reserved user tokens, got %v", names)
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
`, "RESERVED runtime token"},
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
`, "without spaces inside the braces"},
		{"padded workspace spelling", `
mcp_servers:
  - name: deals
    command: python3
    args: ["deals.py"]
    always: true
    env:
      RUN_WORKDIR: "${FLEET_WORKSPACE }"
`, "without spaces inside the braces"},
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
		{"escaped name in a url", `
mcp_servers:
  - name: remote
    type: http
    url: "https://example.test/$${FLEET_USER_NAME}"
    always: true
`, "an escaped FLEET_USER_NAME cannot be written"},
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
`, "reserved runtime token"},
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
