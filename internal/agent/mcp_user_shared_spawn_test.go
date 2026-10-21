package agent

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// sharedSpawnWhoamiScript is a minimal stdio MCP server whose one tool echoes
// two env vars a bundle would fill from ${FLEET_USER_EMAIL}, or "<unset>"
// when the variable is absent.
const sharedSpawnWhoamiScript = `
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
            os.environ.get("DEAL_SHEET_REPLY_TO", "<unset>"),
        ])
        send({"jsonrpc":"2.0","id":rid,"result":{"content":[{"type":"text","text":text}]}})
    elif rid is not None:
        send({"jsonrpc":"2.0","id":rid,"result":{}})
`

// TestReloadMatchesFirstSpawnWhenWorkspacePathHasTokenText: hot reload's diff
// (MCPServerDefs) and the first shared spawn (BuildMCPClient) build the env
// with the same helper in the same order (agentcore.SpawnEnv: user token
// first). A workspace root whose path literally contains "${FLEET_USER_EMAIL}"
// must therefore come out identical on both — the text stays literal in the
// workspace keys, only the bundle-written user key is dropped — and a reload
// of an unchanged catalog restarts nothing.
func TestReloadMatchesFirstSpawnWhenWorkspacePathHasTokenText(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not found")
	}
	root := filepath.Join(t.TempDir(), "${FLEET_USER_EMAIL}")
	if err := os.MkdirAll(root, 0o750); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FLEET_WORKSPACE_ROOT", root)
	specs := map[string]MCPServerSpec{
		"deals": {
			Enabled: true,
			Command: "python3",
			Args:    []string{"-u", "-c", sharedSpawnWhoamiScript},
			Env: map[string]string{
				"DEAL_SHEET_DEFAULT_TO": "${FLEET_WORKSPACE}/out",
				"DEAL_SHEET_REPLY_TO":   "${FLEET_WORKSPACE_ROOT}",
				"SIGNED_IN_USER_EMAIL":  "${FLEET_USER_EMAIL}",
			},
		},
	}
	client := BuildMCPClient(specs, nil, nil)
	t.Cleanup(func() { _ = client.Close() })

	defs := MCPServerDefs(specs)
	if len(defs) != 1 {
		t.Fatalf("defs = %+v, want one", defs)
	}
	env := defs[0].Env
	if _, ok := env["SIGNED_IN_USER_EMAIL"]; ok {
		t.Errorf("SIGNED_IN_USER_EMAIL = %q, want dropped (no signed-in user)", env["SIGNED_IN_USER_EMAIL"])
	}
	for _, k := range []string{"DEAL_SHEET_DEFAULT_TO", "DEAL_SHEET_REPLY_TO"} {
		if !strings.Contains(env[k], root) {
			t.Errorf("%s = %q, want the literal workspace path kept (under %q)", k, env[k], root)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := client.CallToolOn(ctx, "deals", "whoami", nil)
	if err != nil || len(res.Content) != 1 {
		t.Fatalf("CallToolOn = (%+v, %v)", res, err)
	}
	if got, want := strings.TrimSpace(res.Content[0].Text), env["DEAL_SHEET_DEFAULT_TO"]+"|"+env["DEAL_SHEET_REPLY_TO"]; got != want {
		t.Fatalf("first spawn env = %q, reload diff env = %q: they must match", got, want)
	}
	summary, err := client.Reload(ctx, defs)
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if len(summary.Restarted) != 0 || len(summary.Added) != 0 || len(summary.Removed) != 0 {
		t.Fatalf("reload of an unchanged catalog = %+v, want nothing restarted", summary)
	}
}

// TestBuildMCPClient_SharedSpawnDropsUserKeys: the process-lifetime client
// (boot catalog, local-client mode, and the shared client every scheduled run
// without an mcp_selection uses) serves every user, so its ${FLEET_USER_EMAIL}
// keys are dropped — the server sees them unset, never a literal token, never
// an empty string, never anyone's identity (ADR-0087).
func TestBuildMCPClient_SharedSpawnDropsUserKeys(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not found")
	}
	client := BuildMCPClient(map[string]MCPServerSpec{
		"deals": {
			Enabled: true,
			Command: "python3",
			Args:    []string{"-u", "-c", sharedSpawnWhoamiScript},
			Env: map[string]string{
				"DEAL_SHEET_DEFAULT_TO": "${FLEET_USER_EMAIL}",
				"DEAL_SHEET_REPLY_TO":   "Reply-To: <${FLEET_USER_EMAIL}>",
			},
		},
	}, nil, nil)
	t.Cleanup(func() { _ = client.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	res, err := client.CallToolOn(ctx, "deals", "whoami", nil)
	if err != nil {
		t.Fatalf("CallToolOn: %v", err)
	}
	if len(res.Content) != 1 {
		t.Fatalf("content = %+v", res.Content)
	}
	if got := strings.TrimSpace(res.Content[0].Text); got != "<unset>|<unset>" {
		t.Fatalf("shared spawn env = %q, want both user keys dropped", got)
	}
}
