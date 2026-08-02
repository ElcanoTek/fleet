package scheduledrun

import (
	"context"
	"errors"
	"os/exec"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/sandbox"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

type scheduledRecordingBroker struct{}

func (*scheduledRecordingBroker) CallMCP(context.Context, string, string, map[string]any) (string, bool, error) {
	return "", false, nil
}

func TestBuildTaskRemoteOverlayUsesInjectedOpenerWithoutResolver(t *testing.T) {
	ownerID := uuid.New()
	taskID := uuid.New()
	var gotEmail string
	r := &Runner{
		ownerEmail: func(_ context.Context, got uuid.UUID) (string, error) {
			if got != ownerID {
				t.Fatalf("owner ID = %s, want %s", got, ownerID)
			}
			return "owner@example.com", nil
		},
		openRemoteMCPOverlay: func(_ context.Context, email string, shadowed, enabled map[string]bool) (*agent.RemoteMCPOverlay, error) {
			gotEmail = email
			if !shadowed["bundle"] || len(shadowed) != 1 {
				t.Fatalf("shadowed = %v, want bundle only", shadowed)
			}
			if enabled != nil {
				t.Fatalf("scheduled enabled filter = %v, want nil", enabled)
			}
			return &agent.RemoteMCPOverlay{
				Broker:     &scheduledRecordingBroker{},
				Servers:    map[string]bool{"remote": true},
				CloseScope: func(context.Context) error { return nil },
			}, nil
		},
	}
	task := &models.Task{ID: taskID, CreatedBy: &ownerID}

	overlay := r.buildTaskRemoteOverlay(context.Background(), task, []mcp.ServerTool{{ServerName: "bundle"}})
	if !overlay.Active() {
		t.Fatal("injected broker overlay is not active")
	}
	if gotEmail != "owner@example.com" {
		t.Fatalf("email = %q", gotEmail)
	}
}

func TestBindTaskMCPRuntime_UsesBrokerScope(t *testing.T) {
	t.Setenv("FLEET_WORKSPACE_ROOT", t.TempDir())
	taskID := uuid.New()
	broker := &scheduledRecordingBroker{}
	var (
		gotSelection agentcore.MCPSelection
		gotTaskID    string
		gotWorkspace string
		closeCtxErr  = errors.New("scope not closed")
	)
	r := &Runner{
		cfg: &config.Config{},
		mcpServerInventory: func() map[string]TaskMCPServerInfo {
			return map[string]TaskMCPServerInfo{
				"alpha": {UsesWorkspace: true},
				"beta":  {},
			}
		},
		openTaskMCPScope: func(_ context.Context, selection agentcore.MCPSelection, taskID, workspace string) (*agent.MCPScope, error) {
			gotSelection = append(agentcore.MCPSelection(nil), selection...)
			gotTaskID = taskID
			gotWorkspace = workspace
			return &agent.MCPScope{
				Broker:  broker,
				Catalog: []mcp.ServerTool{{ServerName: "alpha", Tool: mcp.Tool{Name: "lookup"}}},
				Close: func(ctx context.Context) error {
					closeCtxErr = ctx.Err()
					return nil
				},
			}, nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	binding, err := r.bindTaskMCPRuntime(ctx, &models.Task{ID: taskID})
	if err != nil {
		t.Fatalf("bindTaskMCPRuntime: %v", err)
	}
	want := agentcore.MCPSelection{{Server: "alpha"}, {Server: "beta"}}
	if !reflect.DeepEqual(gotSelection, want) {
		t.Fatalf("scope selection = %#v, want all enabled servers %#v", gotSelection, want)
	}
	if gotTaskID != taskID.String() {
		t.Errorf("scope task ID = %q, want %q", gotTaskID, taskID)
	}
	if gotWorkspace == "" || binding.workdir != gotWorkspace {
		t.Errorf("scope workspace = %q, binding workdir = %q; want one per-run workspace", gotWorkspace, binding.workdir)
	}
	if binding.client != nil || binding.broker != broker || len(binding.catalog) != 1 {
		t.Fatalf("broker binding = %+v", binding)
	}
	cancel()
	binding.cleanup()
	if closeCtxErr != nil {
		t.Fatalf("scope close inherited cancelled run context: %v", closeCtxErr)
	}
}

func TestTaskMCPSelection_UsesLivePublicInventory(t *testing.T) {
	inventory := map[string]TaskMCPServerInfo{"alpha": {}}
	r := &Runner{
		cfg: &config.Config{},
		mcpServerInventory: func() map[string]TaskMCPServerInfo {
			return inventory
		},
	}
	task := &models.Task{}
	if got := r.taskMCPSelection(task, true); !reflect.DeepEqual(got, agentcore.MCPSelection{{Server: "alpha"}}) {
		t.Fatalf("initial selection = %#v", got)
	}
	inventory = map[string]TaskMCPServerInfo{"beta": {}, "gamma": {UsesWorkspace: true}}
	want := agentcore.MCPSelection{{Server: "beta"}, {Server: "gamma"}}
	if got := r.taskMCPSelection(task, true); !reflect.DeepEqual(got, want) {
		t.Fatalf("reloaded selection = %#v, want %#v", got, want)
	}
}

func TestBindTaskMCPRuntime_PreservesExplicitSelection(t *testing.T) {
	var got agentcore.MCPSelection
	r := &Runner{
		cfg: &config.Config{MCPServers: map[string]config.MCPServerConfig{
			"alpha": {Enabled: true},
			"beta":  {Enabled: true},
		}},
		openTaskMCPScope: func(_ context.Context, selection agentcore.MCPSelection, _, _ string) (*agent.MCPScope, error) {
			got = append(agentcore.MCPSelection(nil), selection...)
			return &agent.MCPScope{Broker: &scheduledRecordingBroker{}, Catalog: []mcp.ServerTool{}, Close: func(context.Context) error { return nil }}, nil
		},
	}
	binding, err := r.bindTaskMCPRuntime(context.Background(), &models.Task{
		ID:           uuid.New(),
		MCPSelection: models.MCPSelection{{Server: "beta", Account: "backup"}},
	})
	if err != nil {
		t.Fatalf("bindTaskMCPRuntime: %v", err)
	}
	defer binding.cleanup()
	want := agentcore.MCPSelection{{Server: "beta", Account: "backup"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("scope selection = %#v, want exact task selection %#v", got, want)
	}
	if binding.catalog == nil {
		t.Fatal("explicit empty scope catalog became nil")
	}
}

func TestBindTaskMCPRuntime_ScopeOpenFailureFailsClosed(t *testing.T) {
	wantErr := errors.New("broker unavailable")
	r := &Runner{
		cfg: &config.Config{},
		openTaskMCPScope: func(context.Context, agentcore.MCPSelection, string, string) (*agent.MCPScope, error) {
			return nil, wantErr
		},
	}
	_, err := r.bindTaskMCPRuntime(context.Background(), &models.Task{ID: uuid.New()})
	if !errors.Is(err, wantErr) {
		t.Fatalf("bindTaskMCPRuntime error = %v, want scope-open failure", err)
	}
}

func TestBindTaskMCPRuntime_IncompleteScopeIsClosed(t *testing.T) {
	closed := false
	r := &Runner{
		cfg: &config.Config{},
		openTaskMCPScope: func(context.Context, agentcore.MCPSelection, string, string) (*agent.MCPScope, error) {
			return &agent.MCPScope{Close: func(context.Context) error { closed = true; return nil }}, nil
		},
	}
	_, err := r.bindTaskMCPRuntime(context.Background(), &models.Task{ID: uuid.New()})
	if err == nil {
		t.Fatal("incomplete scope did not fail closed")
	}
	if !closed {
		t.Fatal("incomplete opened scope was not released")
	}
}

func TestTaskMCPBinding_LocalDiscoveryRemainsLive(t *testing.T) {
	client := mcp.NewClient()
	binding := taskMCPBinding{client: client}
	client.AddHTTPTools([]mcp.HTTPToolSpec{{Name: "first", URL: "https://example.invalid"}})
	if got := len(binding.discoveryCatalog()); got != 1 {
		t.Fatalf("initial local discovery count = %d, want 1", got)
	}
	client.AddHTTPTools([]mcp.HTTPToolSpec{{Name: "second", URL: "https://example.invalid"}})
	if got := len(binding.discoveryCatalog()); got != 2 {
		t.Fatalf("local discovery froze after client mutation: count = %d, want 2", got)
	}
	if binding.catalog != nil {
		t.Fatal("local binding populated a static catalog and would break MCP-dirty rebuilds")
	}
}

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
	client, cleanup, _, err := r.bindTaskMCP(ctx, task)
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
	client, cleanup, _, err := r.bindTaskMCP(ctx, task)
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
	client, cleanup, _, err := r.bindTaskMCP(ctx, task)
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
	_, cleanup, _, err := r.bindTaskMCP(ctx, task)
	defer cleanup()
	if err == nil {
		t.Fatalf("expected unknown-server error for selection referencing an unconfigured server")
	}
}

func TestScheduledRunner_DisabledServerFailsFast(t *testing.T) {
	r := &Runner{cfg: &config.Config{MCPServers: map[string]config.MCPServerConfig{
		"disabled": {Enabled: false, Command: "should-not-run"},
	}}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	task := &models.Task{MCPSelection: models.MCPSelection{{Server: "disabled"}}}
	_, cleanup, _, err := r.bindTaskMCP(ctx, task)
	defer cleanup()
	if err == nil {
		t.Fatal("disabled server remained selectable through the per-run binder")
	}
}

// recordingTaker is a fake sandboxTaker that records which acquisition method
// takeTaskSandbox invoked, without spinning a real podman container.
type recordingTaker struct {
	tookWarm   bool // Take() — warm pool, network ENABLED
	tookSealed bool // TakeContainer() — cold start, --network=none
	// tookOverrides records a TakeContainerWithOverrides call (#205) and what it
	// received, so a test can assert the per-task limits + network posture applied.
	tookOverrides bool
	gotOverride   sandbox.ResourceOverride
	gotNoNetwork  bool
	// containerUnavailable makes the container takes report ErrContainerUnavailable,
	// modeling a host-mode / mock pool with no container backend.
	containerUnavailable bool
	// tookEgress records a TakeContainerWithEgress call (#211) + its allowlist.
	// egressMode/egressAllowlist drive EgressDefault — empty mode = open/current.
	tookEgress      bool
	gotEgressList   []string
	egressMode      string
	egressAllowlist []string
}

func (rt *recordingTaker) TakeContainerWithEgress(_ context.Context, ov sandbox.ResourceOverride, allowlist []string) (*sandbox.Sandbox, func(), error) {
	rt.tookEgress = true
	rt.gotOverride = ov
	rt.gotEgressList = allowlist
	if rt.containerUnavailable {
		return nil, func() {}, sandbox.ErrContainerUnavailable
	}
	return nil, func() {}, nil
}

func (rt *recordingTaker) EgressDefault() (string, []string) {
	return rt.egressMode, rt.egressAllowlist
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

func (rt *recordingTaker) TakeContainerWithOverrides(_ context.Context, ov sandbox.ResourceOverride, noNetwork bool) (*sandbox.Sandbox, func(), error) {
	rt.tookOverrides = true
	rt.gotOverride = ov
	rt.gotNoNetwork = noNetwork
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

// TestTakeTaskSandbox_EgressMode is the #211 acceptance test for the fleet-wide
// network mode: allowlisted routes a networked task through the egress proxy with
// the configured allowlist; lockdown seals even an AllowNetwork task; an empty
// mode is byte-identical to the pre-#211 behavior.
func TestTakeTaskSandbox_EgressMode(t *testing.T) {
	limits := &models.TaskSandboxLimits{MemoryMB: 2048, CPUs: 2.0, Pids: 512}

	t.Run("allowlisted routes a networked task through egress", func(t *testing.T) {
		rt := &recordingTaker{egressMode: sandbox.NetworkModeAllowlisted, egressAllowlist: []string{"pypi.org", "*.github.com"}}
		if _, _, err := takeTaskSandbox(context.Background(), rt, &models.Task{AllowNetwork: true}); err != nil {
			t.Fatalf("takeTaskSandbox: %v", err)
		}
		if !rt.tookEgress || rt.tookWarm || rt.tookSealed {
			t.Fatalf("allowlisted+AllowNetwork must take the egress path; got egress=%v warm=%v sealed=%v", rt.tookEgress, rt.tookWarm, rt.tookSealed)
		}
		if len(rt.gotEgressList) != 2 || rt.gotEgressList[0] != "pypi.org" {
			t.Errorf("egress allowlist = %v, want the configured list", rt.gotEgressList)
		}
	})

	t.Run("allowlisted still seals a non-network task", func(t *testing.T) {
		rt := &recordingTaker{egressMode: sandbox.NetworkModeAllowlisted, egressAllowlist: []string{"pypi.org"}}
		if _, _, err := takeTaskSandbox(context.Background(), rt, &models.Task{}); err != nil {
			t.Fatalf("takeTaskSandbox: %v", err)
		}
		if !rt.tookSealed || rt.tookEgress {
			t.Fatalf("a non-AllowNetwork task must stay sealed even in allowlisted mode; got sealed=%v egress=%v", rt.tookSealed, rt.tookEgress)
		}
	})

	t.Run("lockdown seals even an AllowNetwork task", func(t *testing.T) {
		rt := &recordingTaker{egressMode: sandbox.NetworkModeLockdown}
		if _, _, err := takeTaskSandbox(context.Background(), rt, &models.Task{AllowNetwork: true}); err != nil {
			t.Fatalf("takeTaskSandbox: %v", err)
		}
		if !rt.tookSealed || rt.tookWarm || rt.tookEgress {
			t.Fatalf("lockdown kill-switch must seal an AllowNetwork task; got sealed=%v warm=%v egress=%v", rt.tookSealed, rt.tookWarm, rt.tookEgress)
		}
	})

	t.Run("allowlisted + limits routes through egress with overrides", func(t *testing.T) {
		rt := &recordingTaker{egressMode: sandbox.NetworkModeAllowlisted, egressAllowlist: []string{"pypi.org"}}
		if _, _, err := takeTaskSandbox(context.Background(), rt, &models.Task{AllowNetwork: true, SandboxLimits: limits}); err != nil {
			t.Fatalf("takeTaskSandbox: %v", err)
		}
		if !rt.tookEgress || rt.tookOverrides {
			t.Fatalf("allowlisted+limits must take the egress path (with the override applied there); got egress=%v overrides=%v", rt.tookEgress, rt.tookOverrides)
		}
		want := sandbox.ResourceOverride{MemoryLimit: "2048m", CPULimit: "2.00", PidsLimit: 512}
		if rt.gotOverride != want {
			t.Errorf("egress override = %+v, want %+v", rt.gotOverride, want)
		}
	})

	t.Run("lockdown + limits seals via the override path", func(t *testing.T) {
		rt := &recordingTaker{egressMode: sandbox.NetworkModeLockdown}
		if _, _, err := takeTaskSandbox(context.Background(), rt, &models.Task{AllowNetwork: true, SandboxLimits: limits}); err != nil {
			t.Fatalf("takeTaskSandbox: %v", err)
		}
		if !rt.tookOverrides || !rt.gotNoNetwork || rt.tookEgress {
			t.Fatalf("lockdown+limits must seal via overrides (noNetwork=true), not egress; got overrides=%v noNetwork=%v egress=%v", rt.tookOverrides, rt.gotNoNetwork, rt.tookEgress)
		}
	})

	t.Run("empty mode preserves pre-211 behavior", func(t *testing.T) {
		rt := &recordingTaker{} // egressMode == ""
		if _, _, err := takeTaskSandbox(context.Background(), rt, &models.Task{AllowNetwork: true}); err != nil {
			t.Fatalf("takeTaskSandbox: %v", err)
		}
		if !rt.tookWarm || rt.tookEgress {
			t.Fatalf("empty mode + AllowNetwork must take the warm pool (open), not egress; got warm=%v egress=%v", rt.tookWarm, rt.tookEgress)
		}
	})
}

// TestTakeTaskSandbox_PerTaskLimits is the #205 acceptance test: a task carrying
// SandboxLimits cold-starts through the override path with the converted
// podman-ready values, keeps its network posture (sealed unless AllowNetwork),
// and falls back to the host take on a container-less pool.
func TestTakeTaskSandbox_PerTaskLimits(t *testing.T) {
	limits := &models.TaskSandboxLimits{MemoryMB: 2048, CPUs: 2.0, Pids: 512}

	t.Run("sealed task applies overrides", func(t *testing.T) {
		rt := &recordingTaker{}
		if _, _, err := takeTaskSandbox(context.Background(), rt, &models.Task{SandboxLimits: limits}); err != nil {
			t.Fatalf("takeTaskSandbox: %v", err)
		}
		if !rt.tookOverrides || rt.tookWarm || rt.tookSealed {
			t.Fatalf("limited task must take the override path; got overrides=%v warm=%v sealed=%v", rt.tookOverrides, rt.tookWarm, rt.tookSealed)
		}
		if !rt.gotNoNetwork {
			t.Error("a non-AllowNetwork limited task must stay sealed (noNetwork=true)")
		}
		want := sandbox.ResourceOverride{MemoryLimit: "2048m", CPULimit: "2.00", PidsLimit: 512}
		if rt.gotOverride != want {
			t.Errorf("override = %+v, want %+v", rt.gotOverride, want)
		}
	})

	t.Run("AllowNetwork limited task keeps egress", func(t *testing.T) {
		rt := &recordingTaker{}
		if _, _, err := takeTaskSandbox(context.Background(), rt, &models.Task{AllowNetwork: true, SandboxLimits: limits}); err != nil {
			t.Fatalf("takeTaskSandbox: %v", err)
		}
		if !rt.tookOverrides || rt.gotNoNetwork {
			t.Fatalf("AllowNetwork limited task must use the override path with egress on; got overrides=%v noNetwork=%v", rt.tookOverrides, rt.gotNoNetwork)
		}
	})

	t.Run("partial limits convert only set fields", func(t *testing.T) {
		rt := &recordingTaker{}
		if _, _, err := takeTaskSandbox(context.Background(), rt, &models.Task{SandboxLimits: &models.TaskSandboxLimits{CPUs: 4}}); err != nil {
			t.Fatalf("takeTaskSandbox: %v", err)
		}
		want := sandbox.ResourceOverride{CPULimit: "4.00"} // memory/pids left to the pool default
		if rt.gotOverride != want {
			t.Errorf("partial override = %+v, want %+v", rt.gotOverride, want)
		}
	})

	t.Run("container-less pool falls back to host take", func(t *testing.T) {
		rt := &recordingTaker{containerUnavailable: true}
		if _, _, err := takeTaskSandbox(context.Background(), rt, &models.Task{SandboxLimits: limits}); err != nil {
			t.Fatalf("takeTaskSandbox should fall back, not error: %v", err)
		}
		if !rt.tookOverrides || !rt.tookWarm {
			t.Fatalf("must try overrides then fall back to warm; got overrides=%v warm=%v", rt.tookOverrides, rt.tookWarm)
		}
	})

	t.Run("all-zero limits use the normal path", func(t *testing.T) {
		rt := &recordingTaker{}
		if _, _, err := takeTaskSandbox(context.Background(), rt, &models.Task{SandboxLimits: &models.TaskSandboxLimits{}}); err != nil {
			t.Fatalf("takeTaskSandbox: %v", err)
		}
		if rt.tookOverrides || !rt.tookSealed {
			t.Fatalf("all-zero limits must NOT take the override path; got overrides=%v sealed=%v", rt.tookOverrides, rt.tookSealed)
		}
	})
}
