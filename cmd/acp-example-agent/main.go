// Command acp-example-agent is a deterministic, CREDENTIAL-FREE external ACP
// agent used as the TEST PROVIDER for fleet's external (type: acp) flavor. It is
// a faithful, trimmed copy of the coder/acp-go-sdk example/agent: it self-reports
// a streamed turn (text + a tool-call notice) and issues exactly one
// session/request_permission, reflecting the human's allow/deny outcome.
//
// It has NO AI model and NO network — it proves fleet's GENERIC external path
// end-to-end (real ACP over podman-stdio: a turn streams, a permission request is
// handled) without any provider credentials. Claude Code (via the
// claude-agent-acp bridge) and Goose (native `goose acp`) are the real-world
// providers wired the SAME way — see docs/USING-AGENTS.md.
//
// The live E2E test (internal/acpruntime/external_podman_e2e_test.go, gated on
// FLEET_ACP_EXTERNAL_E2E_IMAGE) builds this into an image and has fleet's
// ExternalRuntime drive it.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"time"

	acp "github.com/coder/acp-go-sdk"
)

type exampleAgent struct {
	conn     *acp.AgentSideConnection
	mu       sync.Mutex
	sessions map[string]struct{}
}

var _ acp.Agent = (*exampleAgent)(nil)

func newExampleAgent() *exampleAgent {
	return &exampleAgent{sessions: map[string]struct{}{}}
}

// SetAgentConnection implements acp.AgentConnAware so the SDK hands us the
// connection after construction.
func (a *exampleAgent) SetAgentConnection(conn *acp.AgentSideConnection) { a.conn = conn }

func (a *exampleAgent) Initialize(context.Context, acp.InitializeRequest) (acp.InitializeResponse, error) {
	return acp.InitializeResponse{
		ProtocolVersion:   acp.ProtocolVersionNumber,
		AgentCapabilities: acp.AgentCapabilities{},
		AgentInfo:         &acp.Implementation{Name: "acp-example-agent", Version: "fleet-test"},
	}, nil
}

func (a *exampleAgent) NewSession(context.Context, acp.NewSessionRequest) (acp.NewSessionResponse, error) {
	sid := randomID()
	a.mu.Lock()
	a.sessions[sid] = struct{}{}
	a.mu.Unlock()
	return acp.NewSessionResponse{SessionId: acp.SessionId(sid)}, nil
}

func (a *exampleAgent) Authenticate(context.Context, acp.AuthenticateRequest) (acp.AuthenticateResponse, error) {
	return acp.AuthenticateResponse{}, nil
}

func (a *exampleAgent) Prompt(ctx context.Context, p acp.PromptRequest) (acp.PromptResponse, error) {
	sid := p.SessionId
	if err := a.conn.SessionUpdate(ctx, acp.SessionNotification{
		SessionId: sid,
		Update:    acp.UpdateAgentMessageText("ACP example agent — demo only (no AI model). "),
	}); err != nil {
		return acp.PromptResponse{}, err
	}
	if err := a.conn.SessionUpdate(ctx, acp.SessionNotification{
		SessionId: sid,
		Update: acp.StartToolCall(
			acp.ToolCallId("call_read"),
			"Reading project files",
			acp.WithStartKind(acp.ToolKindRead),
			acp.WithStartStatus(acp.ToolCallStatusCompleted),
		),
	}); err != nil {
		return acp.PromptResponse{}, err
	}

	resp, err := a.conn.RequestPermission(ctx, acp.RequestPermissionRequest{
		SessionId: sid,
		ToolCall: acp.ToolCallUpdate{
			ToolCallId: acp.ToolCallId("call_edit"),
			Title:      acp.Ptr("Modifying critical configuration file"),
			Kind:       acp.Ptr(acp.ToolKindEdit),
			Status:     acp.Ptr(acp.ToolCallStatusPending),
			Locations:  []acp.ToolCallLocation{{Path: "/workspace/config.json"}},
			RawInput:   map[string]any{"path": "/workspace/config.json", "content": "{\"host\":\"new\"}"},
		},
		Options: []acp.PermissionOption{
			{Kind: acp.PermissionOptionKindAllowOnce, Name: "Allow this change", OptionId: acp.PermissionOptionId("allow")},
			{Kind: acp.PermissionOptionKindRejectOnce, Name: "Skip this change", OptionId: acp.PermissionOptionId("reject")},
		},
	})
	if err != nil {
		return acp.PromptResponse{}, err
	}

	final := " I'll skip the configuration update."
	if resp.Outcome.Selected != nil && string(resp.Outcome.Selected.OptionId) == "allow" {
		final = " The configuration changes have been applied."
	}
	if err := a.conn.SessionUpdate(ctx, acp.SessionNotification{
		SessionId: sid,
		Update:    acp.UpdateAgentMessageText(final),
	}); err != nil {
		return acp.PromptResponse{}, err
	}
	return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
}

func (a *exampleAgent) Cancel(context.Context, acp.CancelNotification) error { return nil }
func (a *exampleAgent) Logout(context.Context, acp.LogoutRequest) (acp.LogoutResponse, error) {
	return acp.LogoutResponse{}, acp.NewMethodNotFound(acp.AgentMethodLogout)
}
func (a *exampleAgent) CloseSession(context.Context, acp.CloseSessionRequest) (acp.CloseSessionResponse, error) {
	return acp.CloseSessionResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionClose)
}
func (a *exampleAgent) ListSessions(context.Context, acp.ListSessionsRequest) (acp.ListSessionsResponse, error) {
	return acp.ListSessionsResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionList)
}
func (a *exampleAgent) ResumeSession(context.Context, acp.ResumeSessionRequest) (acp.ResumeSessionResponse, error) {
	return acp.ResumeSessionResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionResume)
}
func (a *exampleAgent) SetSessionConfigOption(context.Context, acp.SetSessionConfigOptionRequest) (acp.SetSessionConfigOptionResponse, error) {
	return acp.SetSessionConfigOptionResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionSetConfigOption)
}
func (a *exampleAgent) SetSessionMode(context.Context, acp.SetSessionModeRequest) (acp.SetSessionModeResponse, error) {
	return acp.SetSessionModeResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionSetMode)
}

func randomID() string {
	var b [12]byte
	if _, err := io.ReadFull(rand.Reader, b[:]); err != nil {
		return fmt.Sprintf("sess_%d", time.Now().UnixNano())
	}
	return "sess_" + hex.EncodeToString(b[:])
}

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, os.Kill)
	defer cancel()

	ag := newExampleAgent()
	// ACP over stdio: stdout is the protocol channel, stderr is diagnostics.
	asc := acp.NewAgentSideConnection(ag, os.Stdout, os.Stdin)
	asc.SetLogger(slog.Default())
	ag.SetAgentConnection(asc)

	select {
	case <-asc.Done():
	case <-ctx.Done():
	}
}
