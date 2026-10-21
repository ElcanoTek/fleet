package main

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/mcpbroker"
)

// brokerUserServerScript echoes two env vars — one a bundle maps to
// ${FLEET_USER_EMAIL}, one that (wrongly) names the refused FLEET_USER_NAME —
// "<unset>" when the variable is absent: the distinction the drop contract is
// about (unset, never a literal or a blank).
const brokerUserServerScript = `
import json, os, sys
def send(obj):
    sys.stdout.write(json.dumps(obj) + "\n"); sys.stdout.flush()
for line in sys.stdin:
    if not line.strip():
        continue
    req = json.loads(line)
    rid, method = req.get("id"), req.get("method")
    if method == "initialize":
        send({"jsonrpc":"2.0","id":rid,"result":{"capabilities":{}}})
    elif method == "tools/list":
        send({"jsonrpc":"2.0","id":rid,"result":{"tools":[
            {"name":"whoami","description":"echo the user env",
             "inputSchema":{"type":"object","properties":{}}}]}})
    elif method == "tools/call":
        text = "|".join([
            os.environ.get("DEAL_SHEET_DEFAULT_TO", "<unset>"),
            os.environ.get("DEAL_SHEET_GREETING", "<unset>"),
        ])
        send({"jsonrpc":"2.0","id":rid,"result":{"content":[{"type":"text","text":text}]}})
    elif rid is not None:
        send({"jsonrpc":"2.0","id":rid,"result":{}})
`

func newBrokerBackendForUserTest(t *testing.T) *brokerBackend {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not found")
	}
	client := mcp.NewClient()
	b := &brokerBackend{
		MCPBroker: agentcore.NewLocalMCPBroker(client, agentcore.DefaultRemediationHints),
		client:    client,
		bases: map[string]agentcore.MCPServerBase{
			"deals": {
				Command: "python3",
				Args:    []string{"-u", "-c", brokerUserServerScript},
				BaseEnv: map[string]string{
					"DEAL_SHEET_DEFAULT_TO": "${FLEET_USER_EMAIL}",
					"DEAL_SHEET_GREETING":   "Hi ${FLEET_USER_NAME},",
					"DEALS_API_KEY":         "default-seat-key",
				},
				IdentityEnv: []string{"DEALS_API_KEY"},
			},
		},
		enabled: map[string]bool{"deals": true},
		scopes:  make(map[string]*brokerScope),
	}
	t.Cleanup(func() { _ = b.Close() })
	return b
}

func callWhoami(t *testing.T, b *brokerBackend, spec mcpbroker.ScopeSpec) string {
	t.Helper()
	return callWhoamiOn(t, b, spec, "deals")
}

func callWhoamiOn(t *testing.T, b *brokerBackend, spec mcpbroker.ScopeSpec, server string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	id, _, _, err := b.OpenScope(ctx, spec)
	if err != nil {
		t.Fatalf("OpenScope: %v", err)
	}
	defer func() { _ = b.CloseScope(ctx, id) }()
	text, isErr, err := b.CallMCPInScope(ctx, id, server, "whoami", nil)
	if err != nil || isErr {
		t.Fatalf("CallMCPInScope = (%q, %v, %v)", text, isErr, err)
	}
	return strings.TrimSpace(text)
}

// TestBrokerBackend_ScopeSubstitutesSignedInUser pins the credential-owner half
// of ADR-0087: an interactive scope's user is substituted into the bundle
// server's env, and a scope without one leaves those variables UNSET.
func TestBrokerBackend_ScopeSubstitutesSignedInUser(t *testing.T) {
	b := newBrokerBackendForUserTest(t)
	selection := []mcpbroker.ScopeChoice{{Server: "deals"}}

	// The email is substituted; the FLEET_USER_NAME key is dropped even for
	// a signed-in user — there is no name token, so nothing is derived from
	// the address and the connector falls back to "Hi team,".
	got := callWhoami(t, b, mcpbroker.ScopeSpec{
		Selection: selection,
		User:      &mcpbroker.ScopeUser{Email: "elyse@example.com"},
	})
	if got != "elyse@example.com|<unset>" {
		t.Fatalf("interactive scope env = %q, want the email substituted and the name key dropped", got)
	}

	if got := callWhoami(t, b, mcpbroker.ScopeSpec{Selection: selection}); got != "<unset>|<unset>" {
		t.Fatalf("user-less scope env = %q, want both keys dropped", got)
	}

	// The child re-validates: a value the parent should never have sent is
	// treated as absent, not passed through.
	got = callWhoami(t, b, mcpbroker.ScopeSpec{
		Selection: selection,
		User:      &mcpbroker.ScopeUser{Email: "x${FLEET_TASK_ID}@example.com"},
	})
	if got != "<unset>|<unset>" {
		t.Fatalf("scope env with an invalid email = %q, want both keys dropped", got)
	}
}

// TestBrokerBackend_NamedSeatScopeSubstitutesSignedInUser: on a named account
// the user is written into the env at spawn, AFTER the account overlay and its
// inherited-identity guard (which still runs on DEALS_API_KEY), so a named seat
// gets the user exactly like the default seat.
func TestBrokerBackend_NamedSeatScopeSubstitutesSignedInUser(t *testing.T) {
	b := newBrokerBackendForUserTest(t)
	t.Setenv("DEALS_API_KEY_CLIENT_A", "client-a-key")
	got := callWhoamiOn(t, b, mcpbroker.ScopeSpec{
		Selection: []mcpbroker.ScopeChoice{{Server: "deals", Account: "client_a"}},
		User:      &mcpbroker.ScopeUser{Email: "elyse@example.com"},
	}, "deals_client_a")
	if got != "elyse@example.com|<unset>" {
		t.Fatalf("named-seat scope env = %q, want the signed-in user substituted", got)
	}
}

// TestBrokerBackend_TaskScopeNeverSeesAUser: the scheduled path takes no user
// at all, so its token-bearing keys are dropped even on the dedicated scope.
func TestBrokerBackend_TaskScopeNeverSeesAUser(t *testing.T) {
	b := newBrokerBackendForUserTest(t)
	got := callWhoami(t, b, mcpbroker.ScopeSpec{
		Selection: []mcpbroker.ScopeChoice{{Server: "deals"}},
		TaskID:    "task-9",
	})
	if got != "<unset>|<unset>" {
		t.Fatalf("task scope env = %q, want both keys dropped", got)
	}
}

// TestBrokerScopeUser: the parent never puts a user on a task scope's wire
// form, and sends nothing when it has nothing valid to send.
func TestBrokerScopeUser(t *testing.T) {
	user := agentcore.NewMCPUserIdentity("elyse@example.com")
	if got := brokerScopeUser("task-1", user); got != nil {
		t.Fatalf("task scope user = %+v, want nil", got)
	}
	if got := brokerScopeUser("", agentcore.MCPUserIdentity{}); got != nil {
		t.Fatalf("zero user = %+v, want nil", got)
	}
	if got := brokerScopeUser("", agentcore.MCPUserIdentity{Email: "bad email"}); got != nil {
		t.Fatalf("invalid user = %+v, want nil", got)
	}
	got := brokerScopeUser("", user)
	if got == nil || got.Email != "elyse@example.com" {
		t.Fatalf("interactive scope user = %+v, want the validated identity", got)
	}
}
