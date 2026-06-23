package acpruntime

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"github.com/ElcanoTek/fleet/internal/agentcore"
)

// fakeExternalAgent is a deterministic, CREDENTIAL-FREE external ACP agent for
// CI: it streams a text chunk, then issues a session/request_permission with
// allow/reject options, then streams a final chunk reflecting the outcome. It
// models the shape of a real self-executing external agent (Claude Code / Goose)
// — and mirrors the coder SDK example/agent used by the live test — without any
// model, network, or fs delegation. It implements only the acp.Agent surface
// the ExternalRuntime drives.
type fakeExternalAgent struct {
	conn *acp.AgentSideConnection
	// permGranted records the outcome the client returned for the permission
	// request, so the test can assert default-deny vs allow round-tripped.
	mu          sync.Mutex
	permGranted bool
	permSeen    bool
}

var _ acp.Agent = (*fakeExternalAgent)(nil)

func (a *fakeExternalAgent) SetConn(c *acp.AgentSideConnection) { a.conn = c }

func (a *fakeExternalAgent) Initialize(context.Context, acp.InitializeRequest) (acp.InitializeResponse, error) {
	return acp.InitializeResponse{
		ProtocolVersion: acp.ProtocolVersionNumber,
		AgentInfo:       &acp.Implementation{Name: "fake-external-agent", Version: "test"},
	}, nil
}

func (a *fakeExternalAgent) NewSession(context.Context, acp.NewSessionRequest) (acp.NewSessionResponse, error) {
	return acp.NewSessionResponse{SessionId: acp.SessionId("ext-sess-1")}, nil
}

func (a *fakeExternalAgent) Prompt(ctx context.Context, p acp.PromptRequest) (acp.PromptResponse, error) {
	sid := p.SessionId
	// Self-reported stream: an opening text chunk.
	if err := a.conn.SessionUpdate(ctx, acp.SessionNotification{
		SessionId: sid,
		Update:    acp.UpdateAgentMessageText("working on it. "),
	}); err != nil {
		return acp.PromptResponse{}, err
	}

	// A sensitive action: request permission from the human (via the client).
	resp, err := a.conn.RequestPermission(ctx, acp.RequestPermissionRequest{
		SessionId: sid,
		ToolCall: acp.ToolCallUpdate{
			ToolCallId: acp.ToolCallId("call_edit"),
			Title:      acp.Ptr("Modify config.json"),
			Kind:       acp.Ptr(acp.ToolKindEdit),
			Locations:  []acp.ToolCallLocation{{Path: "/workspace/config.json"}},
			RawInput:   map[string]any{"path": "/workspace/config.json"},
		},
		Options: []acp.PermissionOption{
			{Kind: acp.PermissionOptionKindAllowOnce, Name: "Allow", OptionId: acp.PermissionOptionId("allow")},
			{Kind: acp.PermissionOptionKindRejectOnce, Name: "Skip", OptionId: acp.PermissionOptionId("reject")},
		},
	})
	if err != nil {
		return acp.PromptResponse{}, err
	}

	a.mu.Lock()
	a.permSeen = true
	a.permGranted = resp.Outcome.Selected != nil &&
		string(resp.Outcome.Selected.OptionId) == "allow"
	granted := a.permGranted
	a.mu.Unlock()

	final := "skipped the change."
	if granted {
		final = "applied the change."
	}
	if err := a.conn.SessionUpdate(ctx, acp.SessionNotification{
		SessionId: sid,
		Update:    acp.UpdateAgentMessageText(final),
	}); err != nil {
		return acp.PromptResponse{}, err
	}
	return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
}

func (a *fakeExternalAgent) Cancel(context.Context, acp.CancelNotification) error { return nil }
func (a *fakeExternalAgent) Authenticate(context.Context, acp.AuthenticateRequest) (acp.AuthenticateResponse, error) {
	return acp.AuthenticateResponse{}, nil
}
func (a *fakeExternalAgent) Logout(context.Context, acp.LogoutRequest) (acp.LogoutResponse, error) {
	return acp.LogoutResponse{}, acp.NewMethodNotFound(acp.AgentMethodLogout)
}
func (a *fakeExternalAgent) CloseSession(context.Context, acp.CloseSessionRequest) (acp.CloseSessionResponse, error) {
	return acp.CloseSessionResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionClose)
}
func (a *fakeExternalAgent) ListSessions(context.Context, acp.ListSessionsRequest) (acp.ListSessionsResponse, error) {
	return acp.ListSessionsResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionList)
}
func (a *fakeExternalAgent) ResumeSession(context.Context, acp.ResumeSessionRequest) (acp.ResumeSessionResponse, error) {
	return acp.ResumeSessionResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionResume)
}
func (a *fakeExternalAgent) SetSessionConfigOption(context.Context, acp.SetSessionConfigOptionRequest) (acp.SetSessionConfigOptionResponse, error) {
	return acp.SetSessionConfigOptionResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionSetConfigOption)
}
func (a *fakeExternalAgent) SetSessionMode(context.Context, acp.SetSessionModeRequest) (acp.SetSessionModeResponse, error) {
	return acp.SetSessionModeResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionSetMode)
}

// brokerFunc adapts a func to a PermissionBroker for tests.
type brokerFunc func(ctx context.Context, req PermissionRequest) (PermissionDecision, error)

func (f brokerFunc) RequestDecision(ctx context.Context, req PermissionRequest) (PermissionDecision, error) {
	return f(ctx, req)
}

// wireExternal connects an externalClient to a fakeExternalAgent in-process over
// io.Pipe and drives Initialize → NewSession → Prompt, returning the externalClient
// (for finalText) and the agent (for permGranted). Mirrors the native round-trip
// harness but exercises the EXTERNAL/containment client.
func wireExternal(t *testing.T, obs *recordingObserver, broker PermissionBroker, permTimeout time.Duration) (*externalClient, *fakeExternalAgent) {
	t.Helper()
	clientToAgentR, clientToAgentW := io.Pipe()
	agentToClientR, agentToClientW := io.Pipe()

	ag := &fakeExternalAgent{}
	agentConn := acp.NewAgentSideConnection(ag, agentToClientW, clientToAgentR)
	ag.SetConn(agentConn)

	if permTimeout <= 0 {
		permTimeout = 5 * time.Second
	}
	cl := &externalClient{obs: obs, broker: broker, permTimeout: permTimeout}
	clientConn := acp.NewClientSideConnection(cl, clientToAgentW, agentToClientR)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)

	if _, err := clientConn.Initialize(ctx, acp.InitializeRequest{
		ProtocolVersion:    acp.ProtocolVersionNumber,
		ClientCapabilities: acp.ClientCapabilities{},
	}); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	sess, err := clientConn.NewSession(ctx, acp.NewSessionRequest{Cwd: "/workspace", McpServers: []acp.McpServer{}})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	if _, err := clientConn.Prompt(ctx, acp.PromptRequest{
		SessionId: sess.SessionId,
		Prompt:    []acp.ContentBlock{acp.TextBlock("do the thing")},
	}); err != nil {
		t.Fatalf("prompt: %v", err)
	}
	_ = clientToAgentW.Close()
	_ = agentToClientW.Close()
	return cl, ag
}

// TestExternalAllowRoundTrip: the human ALLOWS via the broker → the agent gets
// the allow option echoed back, the self-reported stream reaches the Observer,
// and the permission.resolved audit event records allowed=true.
func TestExternalAllowRoundTrip(t *testing.T) {
	obs := &recordingObserver{}
	var gotReq PermissionRequest
	broker := brokerFunc(func(_ context.Context, req PermissionRequest) (PermissionDecision, error) {
		gotReq = req
		return PermissionDecision{Allowed: true, OptionID: "allow"}, nil
	})

	cl, ag := wireExternal(t, obs, broker, 0)

	ag.mu.Lock()
	seen, granted := ag.permSeen, ag.permGranted
	ag.mu.Unlock()
	if !seen {
		t.Fatal("agent never issued a permission request")
	}
	if !granted {
		t.Fatal("agent did not receive the allow outcome")
	}
	if gotReq.Title != "Modify config.json" {
		t.Errorf("broker saw title %q, want 'Modify config.json'", gotReq.Title)
	}
	if len(gotReq.Locations) != 1 || gotReq.Locations[0] != "/workspace/config.json" {
		t.Errorf("broker saw locations %v", gotReq.Locations)
	}
	if !strings.Contains(cl.finalText(), "applied the change") {
		t.Errorf("final text = %q, want it to reflect the allow", cl.finalText())
	}
	// The self-report streamed to the Observer (containment-tier audit).
	if !strings.Contains(obs.text.String(), "working on it") {
		t.Errorf("observer text = %q, want the self-reported stream", obs.text.String())
	}
	assertPermissionResolved(t, obs, true)
}

// TestExternalDenyRoundTrip: the human DENIES → the agent gets the reject option
// echoed back and reflects the skip; the audit records allowed=false.
func TestExternalDenyRoundTrip(t *testing.T) {
	obs := &recordingObserver{}
	broker := brokerFunc(func(_ context.Context, _ PermissionRequest) (PermissionDecision, error) {
		return PermissionDecision{Allowed: false}, nil
	})

	cl, ag := wireExternal(t, obs, broker, 0)

	ag.mu.Lock()
	granted := ag.permGranted
	ag.mu.Unlock()
	if granted {
		t.Fatal("agent should NOT have been granted permission")
	}
	if !strings.Contains(cl.finalText(), "skipped the change") {
		t.Errorf("final text = %q, want it to reflect the deny", cl.finalText())
	}
	assertPermissionResolved(t, obs, false)
}

// TestExternalDefaultDenyOnTimeout: a broker that never answers within the
// per-request timeout must DEFAULT-DENY — the agent gets the reject outcome, the
// turn still completes, and the audit records the timeout deny. This is the core
// safety property: no human, no allow.
func TestExternalDefaultDenyOnTimeout(t *testing.T) {
	obs := &recordingObserver{}
	broker := brokerFunc(func(ctx context.Context, _ PermissionRequest) (PermissionDecision, error) {
		<-ctx.Done() // never decides; wait out the timeout
		return PermissionDecision{Allowed: false}, ctx.Err()
	})

	_, ag := wireExternal(t, obs, broker, 150*time.Millisecond)

	ag.mu.Lock()
	granted := ag.permGranted
	ag.mu.Unlock()
	if granted {
		t.Fatal("timeout must DEFAULT-DENY, never allow")
	}
	assertPermissionResolved(t, obs, false)
}

// TestExternalNoBrokerFailsClosed: a nil broker (misconfigured external flavor)
// must DENY every permission request, never silently auto-allow.
func TestExternalNoBrokerFailsClosed(t *testing.T) {
	obs := &recordingObserver{}
	_, ag := wireExternal(t, obs, nil, 0)
	ag.mu.Lock()
	granted := ag.permGranted
	ag.mu.Unlock()
	if granted {
		t.Fatal("a nil broker must fail closed (deny), not auto-allow")
	}
	assertPermissionResolved(t, obs, false)
}

// TestExternalUsageMapping pins issue #31's core: the external (containment-tier)
// client captures the agent's SELF-REPORTED usage from both UNSTABLE SDK
// surfaces — cumulative cost via a SessionUsageUpdate notification (routed through
// SessionUpdate), token totals via PromptResponse.Usage (capturePromptUsage, the
// exact call ExternalRuntime.Run makes) — and maps them onto agentcore.RunUsage so
// Result.Usage is non-zero instead of a misleading $0/zero-token run.
func TestExternalUsageMapping(t *testing.T) {
	cl := &externalClient{obs: &recordingObserver{}}

	// Cost arrives over the stream as a SessionUsageUpdate; SessionUpdate must route
	// it to the cost field (USD).
	if err := cl.SessionUpdate(context.Background(), acp.SessionNotification{
		SessionId: acp.SessionId("s"),
		Update:    acp.SessionUpdate{UsageUpdate: &acp.SessionUsageUpdate{Cost: &acp.Cost{Amount: 0.42, Currency: "USD"}}},
	}); err != nil {
		t.Fatalf("SessionUpdate(usage): %v", err)
	}
	// Token totals arrive on PromptResponse.Usage at end-of-turn.
	cl.capturePromptUsage(&acp.Usage{
		InputTokens:       100,
		OutputTokens:      50,
		CachedReadTokens:  acp.Ptr(20),
		CachedWriteTokens: acp.Ptr(5),
	})

	got := cl.usageSnapshot()
	if got.PromptTokens != 100 || got.CompletionTokens != 50 {
		t.Errorf("tokens = (prompt %d, completion %d), want (100, 50)", got.PromptTokens, got.CompletionTokens)
	}
	if got.CachedTokens != 20 || got.CacheCreationTokens != 5 {
		t.Errorf("cache tokens = (read %d, write %d), want (20, 5)", got.CachedTokens, got.CacheCreationTokens)
	}
	if got.CostUSD != 0.42 {
		t.Errorf("CostUSD = %v, want 0.42", got.CostUSD)
	}
}

// TestExternalUsageNonUSDNotFabricated: a non-USD self-reported cost is NOT
// stamped onto CostUSD — a false dollar figure is worse than the honest unmetered
// zero (issue #31's Note). nil usage is a no-op (stays zero), never a panic.
func TestExternalUsageNonUSDNotFabricated(t *testing.T) {
	cl := &externalClient{obs: &recordingObserver{}}
	cl.recordReportedCost(&acp.Cost{Amount: 9.99, Currency: "EUR"})
	if got := cl.usageSnapshot(); got.CostUSD != 0 {
		t.Errorf("non-USD cost must not be recorded as USD; CostUSD = %v, want 0", got.CostUSD)
	}
	// nil-safety: neither surface panics or mutates on a no-report run.
	cl.recordReportedCost(nil)
	cl.capturePromptUsage(nil)
	if got := cl.usageSnapshot(); got != (agentcore.RunUsage{}) {
		t.Errorf("nil reports must leave usage zero; got %+v", got)
	}
}

func assertPermissionResolved(t *testing.T, obs *recordingObserver, wantAllowed bool) {
	t.Helper()
	obs.mu.Lock()
	defer obs.mu.Unlock()
	for _, e := range obs.raw {
		if e.eventType != "permission.resolved" {
			continue
		}
		allowed, _ := e.payload["allowed"].(bool)
		if allowed != wantAllowed {
			t.Fatalf("permission.resolved allowed=%v, want %v (payload=%v)", allowed, wantAllowed, e.payload)
		}
		return
	}
	t.Fatalf("no permission.resolved event observed; events=%v", obs.events)
}

var _ = json.Marshal

// TestExternalRunArgs_WorkspacePosture is the regression guard for #81: the
// containment sandbox exposes the conversation workspace READ-ONLY when a
// Workspace is configured (so a real Claude Code/Goose can read the user's
// files) and falls back to a scratch-only tmpfs when it is not. It also pins the
// non-negotiable hardening flags so they can't silently regress.
func TestExternalRunArgs_WorkspacePosture(t *testing.T) {
	const hostWS = "/var/lib/fleet/workspace/conv-abc"

	withWS := NewExternalRuntime(ExternalConfig{Image: "img", Workspace: hostWS}).runArgs()
	joined := strings.Join(withWS, " ")
	if !strings.Contains(joined, "--volume="+hostWS+":/workspace:ro,z") {
		t.Fatalf("workspace set: expected a READ-ONLY bind of %s, got args: %v", hostWS, withWS)
	}
	if strings.Contains(joined, "--tmpfs=/workspace") {
		t.Fatalf("workspace set: must NOT also mount a /workspace tmpfs, got: %v", withWS)
	}
	// Hardening invariants must hold regardless of workspace.
	for _, must := range []string{"--read-only", "--cap-drop=ALL", "--security-opt=no-new-privileges", "--tmpfs=/tmp:rw,size=64m"} {
		if !strings.Contains(joined, must) {
			t.Errorf("missing hardening flag %q in %v", must, withWS)
		}
	}

	scratch := NewExternalRuntime(ExternalConfig{Image: "img"}).runArgs()
	sj := strings.Join(scratch, " ")
	if !strings.Contains(sj, "--tmpfs=/workspace:rw,size=256m") {
		t.Fatalf("no workspace: expected a scratch /workspace tmpfs, got: %v", scratch)
	}
	if strings.Contains(sj, "--volume=") {
		t.Fatalf("no workspace: must NOT bind any host volume, got: %v", scratch)
	}
}

// TestExternalRunArgs_NetworkPosture is the regression guard for #85: a
// `network: none` flavor (NoNetwork=true) seals the container's network
// namespace; other postures leave default egress (no --network flag — model_only
// is firewall-enforced, not sealed here).
func TestExternalRunArgs_NetworkPosture(t *testing.T) {
	sealed := strings.Join(NewExternalRuntime(ExternalConfig{Image: "img", NoNetwork: true}).runArgs(), " ")
	if !strings.Contains(sealed, "--network=none") {
		t.Fatalf("NoNetwork=true must seal the namespace with --network=none, got: %s", sealed)
	}
	open := strings.Join(NewExternalRuntime(ExternalConfig{Image: "img"}).runArgs(), " ")
	if strings.Contains(open, "--network") {
		t.Fatalf("default posture must NOT pass a --network flag, got: %s", open)
	}
}
