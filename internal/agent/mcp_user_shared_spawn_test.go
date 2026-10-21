package agent

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// sharedSpawnWhoamiScript is a minimal stdio MCP server whose one tool echoes
// the two env vars a bundle would fill from ${FLEET_USER_EMAIL} /
// ${FLEET_USER_NAME}, or "<unset>" when the variable is absent.
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
            os.environ.get("DEAL_SHEET_GREETING", "<unset>"),
        ])
        send({"jsonrpc":"2.0","id":rid,"result":{"content":[{"type":"text","text":text}]}})
    elif rid is not None:
        send({"jsonrpc":"2.0","id":rid,"result":{}})
`

// TestBuildMCPClient_SharedSpawnDropsUserKeys: the process-lifetime client
// (boot catalog, local-client mode, and the shared client every scheduled run
// without an mcp_selection uses) serves every user, so its ${FLEET_USER_*}
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
				"DEAL_SHEET_GREETING":   "Hi ${FLEET_USER_NAME},",
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
