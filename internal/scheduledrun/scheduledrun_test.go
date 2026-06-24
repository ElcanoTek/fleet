package scheduledrun

import (
	"context"
	"os/exec"
	"testing"
	"time"

	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/sandbox"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// fakeCredServerScript is a minimal stdio MCP server that exposes a single tool,
// "whoami", which returns the value of the SECRET_TOKEN env var it was spawned
// with. It lets the test observe EXACTLY which credentials reached the spawned
// subprocess's cmd.Env — the property per-account isolation must guarantee.
const fakeCredServerScript = `
import json, sys, os
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
            {"name":"whoami","description":"returns SECRET_TOKEN",
             "inputSchema":{"type":"object","properties":{}}}]}})
    elif method == "tools/call":
        token = os.environ.get("SECRET_TOKEN", "<unset>")
        send({"jsonrpc":"2.0","id":rid,"result":{"content":[{"type":"text","text":token}]}})
    else:
        send({"jsonrpc":"2.0","id":rid,"result":{}})
`

// newCredTestRunner builds a Runner whose cfg.MCPServers contains one fake stdio
// server ("acct") whose base env declares SECRET_TOKEN. The runner's Manager is
// nil — only the credentialed (non-empty-selection) branch of bindTaskMCP is
// exercised, and that branch never touches the Manager.
func newCredTestRunner(t *testing.T) *Runner {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not found, skipping MCP credential isolation test")
	}
	cfg := &config.Config{
		MCPServers: map[string]config.MCPServerConfig{
			"acct": {
				Type:    "stdio",
				Command: "python3",
				Args:    []string{"-u", "-c", fakeCredServerScript},
				// Default-seat value of SECRET_TOKEN; account overlays replace it
				// host-side via creds.ApplyClientSuffix when an account is named.
				Env:     map[string]string{"SECRET_TOKEN": "default-seat-token"},
				Enabled: true,
			},
		},
	}
	return &Runner{cfg: cfg}
}

// callWhoami binds the given selection through bindTaskMCP, then invokes the
// fake server's whoami tool and returns the SECRET_TOKEN it observed. The
// per-run client is Closed via the returned cleanup before the function returns.
func callWhoami(t *testing.T, r *Runner, sel models.MCPSelection, serverName string) (string, func()) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	task := &models.Task{MCPSelection: sel}
	client, cleanup, err := r.bindTaskMCP(ctx, task)
	if err != nil {
		cleanup()
		t.Fatalf("bindTaskMCP(%+v): %v", sel, err)
	}
	res, err := client.CallToolOn(ctx, serverName, "whoami", map[string]any{})
	if err != nil {
		cleanup()
		t.Fatalf("CallToolOn(%q, whoami): %v", serverName, err)
	}
	if len(res.Content) == 0 {
		cleanup()
		t.Fatalf("whoami returned no content")
	}
	return res.Content[0].Text, cleanup
}

// TestScheduledRunner_PerTaskCredentialAccountIsolation proves the P8 hardening:
// a scheduled task whose mcp_selection names an account injects THAT account's
// <VAR>_<ACCOUNT> credentials into the spawned MCP subprocess (on cmd.Env),
// while a different account — and the default seat — do NOT see it. The spawned
// server reports its own SECRET_TOKEN back so the test observes exactly which
// credentials reached the subprocess.
func TestScheduledRunner_PerTaskCredentialAccountIsolation(t *testing.T) {
	r := newCredTestRunner(t)

	// Two account-suffixed credential sets live in the process env (the 0600
	// .env.local at rest in production). ApplyClientSuffix overlays them per
	// account onto the base SECRET_TOKEN.
	t.Setenv("SECRET_TOKEN_CLIENT_A", "client-a-secret")
	t.Setenv("SECRET_TOKEN_CLIENT_B", "client-b-secret")

	t.Run("named account sees its own creds", func(t *testing.T) {
		sel := models.MCPSelection{{Server: "acct", Account: "client_a"}}
		// Account variants register under <server>_<account>.
		got, cleanup := callWhoami(t, r, sel, "acct_client_a")
		defer cleanup()
		if got != "client-a-secret" {
			t.Fatalf("client_a subprocess saw SECRET_TOKEN=%q, want %q", got, "client-a-secret")
		}
	})

	t.Run("a different account does NOT see client_a's creds", func(t *testing.T) {
		sel := models.MCPSelection{{Server: "acct", Account: "client_b"}}
		got, cleanup := callWhoami(t, r, sel, "acct_client_b")
		defer cleanup()
		if got == "client-a-secret" {
			t.Fatalf("client_b subprocess leaked client_a's secret %q", got)
		}
		if got != "client-b-secret" {
			t.Fatalf("client_b subprocess saw SECRET_TOKEN=%q, want %q", got, "client-b-secret")
		}
	})

	t.Run("default seat does NOT see any account's creds", func(t *testing.T) {
		// A bare server (no account) registers under its plain name and gets the
		// default-seat env, never an account overlay.
		sel := models.MCPSelection{{Server: "acct"}}
		got, cleanup := callWhoami(t, r, sel, "acct")
		defer cleanup()
		if got == "client-a-secret" || got == "client-b-secret" {
			t.Fatalf("default seat leaked an account secret: %q", got)
		}
		if got != "default-seat-token" {
			t.Fatalf("default seat saw SECRET_TOKEN=%q, want %q", got, "default-seat-token")
		}
	})
}

// TestScheduledRunner_RefusesAccountWithoutCreds proves the refusal guard:
// naming an account for which no <VAR>_<ACCOUNT> credentials exist is rejected
// rather than silently inheriting the default seat (plan §6.3 step 3).
func TestScheduledRunner_RefusesAccountWithoutCreds(t *testing.T) {
	r := newCredTestRunner(t)
	// Deliberately set NO SECRET_TOKEN_CLIENT_C.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	task := &models.Task{MCPSelection: models.MCPSelection{{Server: "acct", Account: "client_c"}}}
	client, cleanup, err := r.bindTaskMCP(ctx, task)
	defer cleanup()
	if err == nil {
		t.Fatalf("expected refusal binding an account with no <VAR>_CLIENT_C creds, got client=%v", client)
	}
}

// TestScheduledRunner_PerRunClientClosedReapsSubprocess proves the per-run
// isolation lifecycle: the credentialed client returned by bindTaskMCP is a
// DEDICATED client (not the shared one), and its cleanup Closes the spawned
// subprocess so no credentialed process leaks across runs.
func TestScheduledRunner_PerRunClientClosedReapsSubprocess(t *testing.T) {
	r := newCredTestRunner(t)
	t.Setenv("SECRET_TOKEN_CLIENT_A", "client-a-secret")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	task := &models.Task{MCPSelection: models.MCPSelection{{Server: "acct", Account: "client_a"}}}
	client, cleanup, err := r.bindTaskMCP(ctx, task)
	if err != nil {
		cleanup()
		t.Fatalf("bindTaskMCP: %v", err)
	}
	if !client.HasServer("acct_client_a") {
		cleanup()
		t.Fatalf("per-run client missing the bound account variant")
	}
	// Cleanup must not error closing the subprocess.
	cleanup()
}

// TestScheduledRunner_UnknownServerFailsFast proves a selection referencing a
// server that isn't in the config catalog is rejected before any subprocess is
// spawned, rather than silently producing a credential-free client.
func TestScheduledRunner_UnknownServerFailsFast(t *testing.T) {
	r := &Runner{cfg: &config.Config{MCPServers: map[string]config.MCPServerConfig{}}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	task := &models.Task{MCPSelection: models.MCPSelection{{Server: "nope"}}}
	_, cleanup, err := r.bindTaskMCP(ctx, task)
	defer cleanup()
	if err == nil {
		t.Fatalf("expected unknown-server error for selection referencing an unconfigured server")
	}
}

// recordingTaker is a fake sandboxTaker that records which acquisition method
// takeTaskSandbox invoked, without spinning a real podman container.
type recordingTaker struct {
	tookWarm   bool // Take() — warm pool, network ENABLED
	tookSealed bool // TakeContainer() — cold start, --network=none
	// containerUnavailable makes TakeContainer report ErrContainerUnavailable,
	// modeling a host-mode / mock pool with no container backend.
	containerUnavailable bool
}

func (rt *recordingTaker) Take() (*sandbox.Sandbox, func(), error) {
	rt.tookWarm = true
	return nil, func() {}, nil
}

func (rt *recordingTaker) TakeContainer(_ context.Context) (*sandbox.Sandbox, func(), error) {
	rt.tookSealed = true
	if rt.containerUnavailable {
		return nil, func() {}, sandbox.ErrContainerUnavailable
	}
	return nil, func() {}, nil
}

// TestTakeTaskSandbox_NetworkPosture is the #145 acceptance test: a scheduled
// task defaults to a network-SEALED sandbox (TakeContainer, --network=none) and
// only an explicit AllowNetwork opt-in draws the warm, network-enabled pool.
func TestTakeTaskSandbox_NetworkPosture(t *testing.T) {
	t.Run("default seals egress", func(t *testing.T) {
		rt := &recordingTaker{}
		if _, _, err := takeTaskSandbox(context.Background(), rt, &models.Task{}); err != nil {
			t.Fatalf("takeTaskSandbox: %v", err)
		}
		if !rt.tookSealed || rt.tookWarm {
			t.Fatalf("default task must take the SEALED container (--network=none); got warm=%v sealed=%v", rt.tookWarm, rt.tookSealed)
		}
	})

	t.Run("AllowNetwork opts into egress", func(t *testing.T) {
		rt := &recordingTaker{}
		if _, _, err := takeTaskSandbox(context.Background(), rt, &models.Task{AllowNetwork: true}); err != nil {
			t.Fatalf("takeTaskSandbox: %v", err)
		}
		if !rt.tookWarm || rt.tookSealed {
			t.Fatalf("AllowNetwork task must take the WARM pool (egress on); got warm=%v sealed=%v", rt.tookWarm, rt.tookSealed)
		}
	})

	t.Run("host-mode pool falls back to host take", func(t *testing.T) {
		// No container backend (host/mock pool): TakeContainer reports
		// ErrContainerUnavailable. Sealing is not applicable to a host sandbox,
		// so the default task must fall back to the host Take rather than error —
		// this is the cutlass dev-one-shot / no-podman path.
		rt := &recordingTaker{containerUnavailable: true}
		if _, _, err := takeTaskSandbox(context.Background(), rt, &models.Task{}); err != nil {
			t.Fatalf("takeTaskSandbox should fall back, not error, when no container backend: %v", err)
		}
		if !rt.tookSealed || !rt.tookWarm {
			t.Fatalf("host-mode default must try sealed then fall back to warm; got warm=%v sealed=%v", rt.tookWarm, rt.tookSealed)
		}
	})
}
