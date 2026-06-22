package acpruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"
	acp "github.com/coder/acp-go-sdk"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/sandbox"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// AgentRunner is the agent-side of the native ACP flavor (baked into the
// native sandbox image as cmd/fleet-native-agent). It implements acp.Agent and,
// on each Prompt, reconstructs and runs the SAME agentcore.Run loop the
// in-process path runs — but with DELEGATING deps:
//
//   - Executor: a sandbox.NewDelegating backed by an ACP `_fleet/tool` forwarder,
//     so bash/run_python execute in the CLIENT's host sandbox. The agent holds
//     no local executor → it cannot self-execute.
//   - Observer: a delegating observer that emits text/tool/progress as ACP
//     session/update and structured events as `_fleet/event`.
//   - Policy: the SAME InteractivePolicy / ScheduledPolicy the in-process path
//     uses (running here, inside the loop) → identical governance.
//
// Reusing agentcore.Run + tools.NewTurnTools(sb) + the real Policy is what makes
// native-acp govern AND behave identically to native-inprocess.
type AgentRunner struct {
	// modelFor resolves a slug to a fantasy.LanguageModel. Production wires the
	// OpenRouter ModelResolver; tests inject a fake (the in-process delegation
	// test wires a mock model so it needs no network).
	modelFor func(ctx context.Context, slug string) (fantasy.LanguageModel, error)

	conn *acp.AgentSideConnection

	mu       sync.Mutex
	sessions map[string]*agentSession
}

type agentSession struct {
	spec RunSpec
}

var (
	_ acp.Agent                  = (*AgentRunner)(nil)
	_ acp.ExtensionMethodHandler = (*AgentRunner)(nil)
)

// NewAgentRunner builds the runner over a model resolver (built from
// OPENROUTER_API_KEY inside the agent container — the model endpoint is allowed
// egress; MCP credentials are NOT shipped into the container).
func NewAgentRunner(resolver *agentcore.ModelResolver) *AgentRunner {
	return newAgentRunner(resolver.Resolve)
}

// newAgentRunner is the internal constructor over a model-resolution function,
// so tests can inject a fake model without an OpenRouter provider.
func newAgentRunner(modelFor func(context.Context, string) (fantasy.LanguageModel, error)) *AgentRunner {
	return &AgentRunner{modelFor: modelFor, sessions: map[string]*agentSession{}}
}

// SetConn wires the agent-side connection after construction (mirrors the SDK
// example's SetAgentConnection pattern). The forwarders need it to call back to
// the client.
func (a *AgentRunner) SetConn(conn *acp.AgentSideConnection) { a.conn = conn }

func (a *AgentRunner) Initialize(_ context.Context, _ acp.InitializeRequest) (acp.InitializeResponse, error) {
	return acp.InitializeResponse{
		ProtocolVersion:   acp.ProtocolVersionNumber,
		AgentCapabilities: acp.AgentCapabilities{},
		AgentInfo:         &acp.Implementation{Name: "fleet-native-agent", Version: "p-acp-1"},
	}, nil
}

// NewSession decodes the RunSpec from the request _meta and registers the
// session.
func (a *AgentRunner) NewSession(_ context.Context, p acp.NewSessionRequest) (acp.NewSessionResponse, error) {
	spec, err := decodeRunSpec(p.Meta)
	if err != nil {
		return acp.NewSessionResponse{}, fmt.Errorf("decode run spec: %w", err)
	}
	sid := newSessionID()
	a.mu.Lock()
	a.sessions[sid] = &agentSession{spec: spec}
	a.mu.Unlock()
	return acp.NewSessionResponse{SessionId: acp.SessionId(sid)}, nil
}

// Prompt reconstructs the run and drives agentcore.Run with delegating deps.
func (a *AgentRunner) Prompt(ctx context.Context, p acp.PromptRequest) (acp.PromptResponse, error) {
	sid := string(p.SessionId)
	a.mu.Lock()
	sess, ok := a.sessions[sid]
	a.mu.Unlock()
	if !ok {
		return acp.PromptResponse{}, fmt.Errorf("session %s not found", sid)
	}
	spec := sess.spec

	meta, err := decodePromptMeta(p.Meta)
	if err != nil {
		return acp.PromptResponse{}, fmt.Errorf("decode prompt meta: %w", err)
	}
	messages, err := decodeMessages(meta.MessagesJSON, p.Prompt)
	if err != nil {
		return acp.PromptResponse{}, fmt.Errorf("decode messages: %w", err)
	}

	model, err := a.modelFor(ctx, spec.ModelSlug)
	if err != nil {
		return acp.PromptResponse{}, fmt.Errorf("resolve model %q: %w", spec.ModelSlug, err)
	}
	var fallback fantasy.LanguageModel
	if spec.FallbackSlug != "" {
		if fb, ferr := a.modelFor(ctx, spec.FallbackSlug); ferr == nil {
			fallback = fb
		} else {
			// Non-fatal: the run proceeds without a fallback, but log it so a
			// misconfigured/unavailable fallback slug isn't a silent no-fallback.
			log.Printf("fleet-native-agent: fallback model %q unavailable; running without fallback: %v", spec.FallbackSlug, ferr)
		}
	}

	// Delegating sandbox: bash/python ride _fleet/tool back to the client's host
	// sandbox. The agent has NO local executor.
	sb := sandbox.NewDelegating(&toolForwarder{conn: a.conn, sessionID: sid})
	turn := tools.NewTurnTools(sb)

	obs := &delegatingObserver{conn: a.conn, sessionID: acp.SessionId(sid)}

	mode := agentcore.ModeInteractive
	if spec.Mode == agentcore.ModeScheduled.String() {
		mode = agentcore.ModeScheduled
	}

	headers := agentcore.DefaultProviderHeaders
	if spec.ProviderXTitle != "" {
		headers.XTitle = spec.ProviderXTitle
	}
	if spec.ProviderHTTPReferer != "" {
		headers.HTTPReferer = spec.ProviderHTTPReferer
	}

	var policy agentcore.Policy
	includeConfirmAudit := false
	if mode == agentcore.ModeScheduled {
		sp := agentcore.NewScheduledPolicy(nil, 0)
		policy = sp
		includeConfirmAudit = true
	} else {
		policy = agentcore.NewInteractivePolicy(spec.MaxCostUSD, spec.MaxTotalTokens, nil, nil)
	}

	deps := agentcore.Deps{
		Input:    promptInput{system: spec.SystemPrompt, messages: messages, label: spec.Label},
		Observer: obs,
		Policy:   policy,
		Executor: agentExecutor{sb: sb},
		Model:    model, FallbackModel: fallback,
	}
	cfg := agentcore.RunConfig{
		EnvPrefix:           agentcore.CanonicalEnvPrefix,
		Temperature:         spec.Temperature,
		MaxCompletionTokens: spec.MaxTokens,
		NativeTools:         turn.Tools,
		IncludeConfirmAudit: includeConfirmAudit,
		ProviderHeaders:     headers,
	}

	res, runErr := agentcore.Run(ctx, mode, cfg, deps)
	if runErr != nil {
		if ctx.Err() != nil {
			//nolint:nilerr // intentional: a cancelled ctx is a clean stop reported via StopReasonCancelled, not an error — mirrors agentcore.Run's cancellation contract.
			return acp.PromptResponse{StopReason: acp.StopReasonCancelled}, nil
		}
		return acp.PromptResponse{}, runErr
	}
	if res.Cancelled {
		return acp.PromptResponse{StopReason: acp.StopReasonCancelled}, nil
	}
	return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
}

func (a *AgentRunner) Cancel(_ context.Context, _ acp.CancelNotification) error { return nil }

// HandleExtensionMethod: the agent does not accept inbound extension calls.
func (a *AgentRunner) HandleExtensionMethod(_ context.Context, method string, _ json.RawMessage) (any, error) {
	return nil, acp.NewMethodNotFound(method)
}

// promptInput adapts the reconstructed prompt to agentcore.InputSource.
type promptInput struct {
	system   string
	messages []fantasy.Message
	label    string
}

func (p promptInput) Prompt(_ context.Context) (string, []fantasy.Message, string, error) {
	return p.system, p.messages, p.label, nil
}

// agentExecutor adapts the delegating sandbox to agentcore.Executor (held on
// Deps so the loop/finalize can surface execution). It runs through the SAME
// delegating sandbox the native tools use.
type agentExecutor struct{ sb *sandbox.Sandbox }

func (e agentExecutor) RunBash(ctx context.Context, command string) (string, error) {
	res, err := e.sb.RunBash(ctx, sandbox.BashRequest{Command: command, Timeout: 5 * time.Minute})
	if err != nil {
		return "", err
	}
	return combine(res.Stdout, res.Stderr), nil
}

func (e agentExecutor) RunPython(ctx context.Context, code string) (string, error) {
	res, err := e.sb.RunPython(ctx, sandbox.PythonRequest{Code: code, Timeout: 5 * time.Minute})
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	sb.WriteString(res.Stdout)
	if res.Stderr != "" {
		sb.WriteString("\n")
		sb.WriteString(res.Stderr)
	}
	return sb.String(), nil
}

func combine(stdout, stderr []byte) string {
	var sb strings.Builder
	sb.Write(stdout)
	if len(stderr) > 0 {
		if sb.Len() > 0 {
			sb.WriteString("\n")
		}
		sb.Write(stderr)
	}
	return sb.String()
}

// --- unsupported acp.Agent methods (the native flavor is a single-turn loop) ---

func (a *AgentRunner) Authenticate(_ context.Context, _ acp.AuthenticateRequest) (acp.AuthenticateResponse, error) {
	return acp.AuthenticateResponse{}, nil
}
func (a *AgentRunner) Logout(_ context.Context, _ acp.LogoutRequest) (acp.LogoutResponse, error) {
	return acp.LogoutResponse{}, acp.NewMethodNotFound(acp.AgentMethodLogout)
}
func (a *AgentRunner) CloseSession(_ context.Context, _ acp.CloseSessionRequest) (acp.CloseSessionResponse, error) {
	return acp.CloseSessionResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionClose)
}
func (a *AgentRunner) ListSessions(_ context.Context, _ acp.ListSessionsRequest) (acp.ListSessionsResponse, error) {
	return acp.ListSessionsResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionList)
}
func (a *AgentRunner) ResumeSession(_ context.Context, _ acp.ResumeSessionRequest) (acp.ResumeSessionResponse, error) {
	return acp.ResumeSessionResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionResume)
}
func (a *AgentRunner) SetSessionConfigOption(_ context.Context, _ acp.SetSessionConfigOptionRequest) (acp.SetSessionConfigOptionResponse, error) {
	return acp.SetSessionConfigOptionResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionSetConfigOption)
}
func (a *AgentRunner) SetSessionMode(_ context.Context, _ acp.SetSessionModeRequest) (acp.SetSessionModeResponse, error) {
	return acp.SetSessionModeResponse{}, acp.NewMethodNotFound(acp.AgentMethodSessionSetMode)
}
