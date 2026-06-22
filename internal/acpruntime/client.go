package acpruntime

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"github.com/ElcanoTek/fleet/internal/agentcore"
)

// ClientRuntime is fleet's ACP CLIENT. It spawns a sandboxed ACP agent over
// podman-stdio and drives one run (Initialize → NewSession → Prompt) while
// owning the REAL host-side governance: the delegated tool calls it receives
// over the `_fleet/tool` extension are executed against the host agentcore
// Executor (the hardened per-turn sandbox — NO Podman-in-Podman), and the
// agent's session/update + `_fleet/event` notifications are forwarded to fleet's
// real Observer (→ SSE / session log).
//
// This drives ANY ACP agent — the native flavor now (cmd/fleet-native-agent),
// external flavors (Claude Code / Goose) in P-ACP-2 — over the same seam.
type ClientRuntime struct {
	cfg ClientConfig
}

// ClientConfig configures one ClientRuntime spawn.
type ClientConfig struct {
	// Image is the agent's container image reference (digest-pinned in prod).
	Image string
	// PodmanBinary overrides the executable (default "podman").
	PodmanBinary string
	// NoNetwork seals the agent container's network namespace. The native
	// flavor needs model-endpoint egress (the loop runs in the agent), so the
	// runtime leaves this false by default; an external self-executing flavor
	// that should not reach the network sets it true. The agent NEVER gets
	// MCP credentials regardless — those are brokered host-side at delegation.
	NoNetwork bool
	// ModelEnv is the set of env vars passed into the agent container — the
	// MODEL-endpoint credentials only (OPENROUTER_API_KEY, and the
	// OPENROUTER_BASE_URL override for dev/E2E). The model endpoint is the
	// agent's one allowed egress; MCP credentials are NEVER included here —
	// they are brokered host-side at delegation, never handed into the
	// container.
	ModelEnv map[string]string
	// ExtraRunArgs are appended to the podman run invocation before the image.
	ExtraRunArgs []string
	// StartTimeout caps how long Initialize may take after spawn.
	StartTimeout time.Duration
}

// NewClientRuntime builds a ClientRuntime over the given config.
func NewClientRuntime(cfg ClientConfig) *ClientRuntime {
	if cfg.PodmanBinary == "" {
		cfg.PodmanBinary = "podman"
	}
	if cfg.StartTimeout <= 0 {
		cfg.StartTimeout = 60 * time.Second
	}
	return &ClientRuntime{cfg: cfg}
}

// Deps are the host-side governance dependencies the client wires to the agent's
// delegated calls. They are the SAME agentcore seams the in-process path uses —
// the whole point is that native-acp governs identically.
type Deps struct {
	// Executor runs delegated bash/python in the host-managed sandbox. REQUIRED.
	// For a lockdown turn the host hands a no-network sandbox here, so lockdown's
	// tool-isolation holds for native-acp exactly as in-process.
	Executor agentcore.Executor
	// Observer receives the agent's streamed text/tool/progress + `_fleet/event`
	// notifications, re-emitted as fleet's real run events. REQUIRED.
	Observer agentcore.Observer
	// MCPBroker runs a delegated MCP tool call HOST-SIDE against the per-task
	// credentialed mcp.Client (P-ACP-2b). The agent advertises the tool surface
	// but never holds credentials; this is where the call physically happens, with
	// creds bound host-side. Nil when no MCP servers are selected (the agent then
	// advertises no MCP tools, so it is never invoked).
	MCPBroker MCPBroker
	// StageBroker stages an approval / memory / note proposal HOST-SIDE (the DB
	// write + SSE card). The agent's policy runs in-loop (identical governance)
	// but routes its staging EFFECT here. Nil leaves the staging gates inert
	// (matching an in-process turn with no stagers wired).
	StageBroker StageBroker
	// Verifier runs the host-side end-of-run verifier for a SCHEDULED run when the
	// agent's policy clears and reaches `_fleet/verify`. The agent ships the
	// tool-exec summary; the host runs the SAME verifier on its own fallback model
	// (host-side creds) and returns the missing required actions. Nil leaves the
	// verifier seam inert — the agent (which only calls it when RunSpec.VerifierWired
	// is set, i.e. the host wired one) treats a nil-broker reply as fail-open.
	Verifier VerifyBroker
}

// MCPBroker runs a delegated MCP tool call host-side against the per-task
// credentialed client. The host (agent driver) wires an implementation backed by
// the bound mcp.Client; the cred-isolation invariant lives here — credentials are
// applied host-side at this call, never shipped into the agent container.
type MCPBroker interface {
	// CallMCP runs server.tool with args and returns the flattened text, the
	// isError bit, and a transport error (distinct from a tool-level isError).
	CallMCP(ctx context.Context, server, tool string, args map[string]any) (text string, isError bool, err error)
}

// StageBroker stages a host-side approval / memory / note proposal. The host
// wires an implementation backed by the real ApprovalStager / MemoryProposer /
// NoteProposer. Each method mirrors the in-process staging contract.
type StageBroker interface {
	// StageApproval stages a critical tool call; returns the approval id.
	StageApproval(toolName, toolCallID, rawInput string) (approvalID string, err error)
	// StageSuggestion stages a suggest_advanced_model card; returns the approval
	// id (empty when suppressed) and the agent-facing message (always populated).
	StageSuggestion(reason string) (approvalID, msg string, err error)
	// StageMemory stages a propose_memory proposal; returns the proposal id.
	StageMemory(content string) (proposalID string, err error)
	// StageNote stages a propose_note proposal; returns the proposal id.
	StageNote(slug, title, body, reason string) (proposalID string, err error)
}

// VerifyBroker runs the host-side end-of-run verifier on the agent-supplied
// tool-exec summary and returns the required actions the task demanded that were
// never successfully attempted (empty = verified clean). The host wires an
// implementation backed by the SAME runEndOfRunVerifier the in-process scheduled
// path uses, so the verifier model call + its credentials stay host-side. An
// error means the host could not verify; the agent treats that as fail-open
// (allow finish), matching the in-process path.
type VerifyBroker interface {
	Verify(ctx context.Context, round int, records []ToolExecRecord) (missing []string, err error)
}

// Result is the run outcome the client surfaces to the driver.
type Result struct {
	// FinalText is the agent's final user-visible reply (accumulated from the
	// streamed agent_message chunks).
	FinalText string
	// StopReason is the ACP stop reason the agent returned.
	StopReason string
	// Cancelled reports the run ended because the caller's ctx was cancelled.
	Cancelled bool
	// Usage is the accumulated token + cost accounting the agent reported over
	// `_fleet/event` ("usage" events). The agent makes the LLM calls, so usage
	// accrues in its container; it reports each step's usage to the host, which
	// accumulates it here for the same accounting the in-process path produces.
	Usage agentcore.RunUsage
}

// Run spawns the agent, drives one run to completion, and tears the whole group
// down. spec describes the run; promptText/promptMeta carry the turn; deps wire
// host governance.
func (r *ClientRuntime) Run(ctx context.Context, spec RunSpec, promptText string, promptMeta PromptMeta, deps Deps) (Result, error) {
	if deps.Executor == nil {
		return Result{}, fmt.Errorf("acpruntime: Deps.Executor is required (host governance)")
	}
	if deps.Observer == nil {
		return Result{}, fmt.Errorf("acpruntime: Deps.Observer is required (host governance)")
	}
	if r.cfg.Image == "" {
		return Result{}, fmt.Errorf("acpruntime: ClientConfig.Image is required")
	}

	proc, err := r.spawn()
	if err != nil {
		return Result{}, fmt.Errorf("spawn agent: %w", err)
	}
	// Teardown the WHOLE group (agent process + its container) on every exit
	// path — ctx → SIGTERM → SIGKILL → reap. We never trust --rm alone.
	defer proc.teardown()

	cl := &hostClient{deps: deps}
	conn := acp.NewClientSideConnection(cl, proc.stdin, bufio.NewReader(proc.stdout))

	initCtx, cancelInit := context.WithTimeout(ctx, r.cfg.StartTimeout)
	defer cancelInit()
	if _, err := conn.Initialize(initCtx, acp.InitializeRequest{
		ProtocolVersion: acp.ProtocolVersionNumber,
		ClientCapabilities: acp.ClientCapabilities{
			Fs:       acp.FileSystemCapabilities{ReadTextFile: true, WriteTextFile: true},
			Terminal: true,
		},
	}); err != nil {
		return Result{}, fmt.Errorf("initialize: %w", err)
	}

	specJSON, err := json.Marshal(spec)
	if err != nil {
		return Result{}, fmt.Errorf("marshal run spec: %w", err)
	}
	sess, err := conn.NewSession(ctx, acp.NewSessionRequest{
		Cwd:        "/workspace",
		McpServers: []acp.McpServer{},
		Meta:       map[string]any{MetaKeyRunSpec: json.RawMessage(specJSON)},
	})
	if err != nil {
		return Result{}, fmt.Errorf("new session: %w", err)
	}

	metaJSON, err := json.Marshal(promptMeta)
	if err != nil {
		return Result{}, fmt.Errorf("marshal prompt meta: %w", err)
	}
	promptResp, err := conn.Prompt(ctx, acp.PromptRequest{
		SessionId: sess.SessionId,
		Prompt:    []acp.ContentBlock{acp.TextBlock(promptText)},
		Meta:      map[string]any{MetaKeyPromptMeta: json.RawMessage(metaJSON)},
	})
	if err != nil {
		// A ctx-cancellation is a clean stop: return the partial transcript +
		// whatever usage accrued so the driver persists what streamed before cancel.
		if ctx.Err() != nil {
			return Result{FinalText: cl.finalText(), Cancelled: true, Usage: cl.usageSnapshot()}, nil
		}
		return Result{FinalText: cl.finalText(), Usage: cl.usageSnapshot()}, fmt.Errorf("prompt: %w", err)
	}

	return Result{
		FinalText:  cl.finalText(),
		StopReason: string(promptResp.StopReason),
		Cancelled:  promptResp.StopReason == acp.StopReasonCancelled,
		Usage:      cl.usageSnapshot(),
	}, nil
}

// agentProc holds a spawned agent's process + pipes and the teardown machinery.
type agentProc struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	once   sync.Once
}

// spawn launches the agent via `podman run -i <image>` — NO -t (a PTY corrupts
// JSON-RPC framing); stderr is split from the protocol stdout (→ the journal).
// The container runs hardened: --read-only, --cap-drop=ALL, no-new-privileges.
// We put the podman process in its own process group so teardown can signal the
// whole tree. The agent process lives for the whole run and is torn down by
// teardown(), so it is intentionally not bound to a per-call context.
func (r *ClientRuntime) spawn() (*agentProc, error) {
	args := []string{
		"run", "-i", "--rm",
		"--read-only",
		"--cap-drop=ALL",
		"--security-opt=no-new-privileges",
		"--tmpfs=/tmp:rw,size=64m",
	}
	if r.cfg.NoNetwork {
		args = append(args, "--network=none")
	}
	// Pass ONLY the model-endpoint env into the container (sorted for a stable
	// invocation). MCP credentials are never included — they ride the host-side
	// delegation seam, never the agent container.
	for _, k := range sortedKeys(r.cfg.ModelEnv) {
		args = append(args, "--env", k+"="+r.cfg.ModelEnv[k])
	}
	args = append(args, r.cfg.ExtraRunArgs...)
	args = append(args, r.cfg.Image)

	return spawnAgentProc(r.cfg.PodmanBinary, args)
}

// spawnAgentProc launches `<podmanBinary> <args...>` as the agent subprocess
// with its own process group (so teardown reaps the whole tree), stderr split
// to the journal, and the protocol stdio piped. Shared by the native
// ClientRuntime and the ExternalRuntime so both spawn + tear down identically.
//
// Not bound to a per-call timeout ctx: the agent process lives for the whole run
// and is torn down explicitly by teardown(). ctx is honored via the
// connection-level request contexts and the deferred teardown.
func spawnAgentProc(podmanBinary string, args []string) (*agentProc, error) {
	cmd := exec.Command(podmanBinary, args...) //nolint:gosec,noctx // podman binary + args are operator-configured (image is a manifest ref), not user input; lifetime is managed by teardown(), not a request ctx.
	cmd.Stderr = os.Stderr
	// Own process group so SIGTERM/SIGKILL reaches the podman supervisor (and,
	// through it, the conmon/container) as a group — don't trust --rm alone.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("start podman: %w", err)
	}
	return &agentProc{cmd: cmd, stdin: stdin, stdout: stdout}, nil
}

// teardown closes the agent's stdin (signalling EOF so the loop exits cleanly),
// then escalates ctx → SIGTERM → SIGKILL across the whole process group and
// reaps it. Idempotent.
func (p *agentProc) teardown() {
	p.once.Do(func() {
		_ = p.stdin.Close()
		if p.cmd.Process == nil {
			return
		}
		pgid := -p.cmd.Process.Pid // negative pid → whole process group
		// Graceful first: SIGTERM the group.
		_ = syscall.Kill(pgid, syscall.SIGTERM)
		done := make(chan struct{})
		go func() {
			_, _ = p.cmd.Process.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			// Force: SIGKILL the group, then reap.
			_ = syscall.Kill(pgid, syscall.SIGKILL)
			<-done
		}
	})
}

// hostClient implements acp.Client + the `_fleet/*` extension handler. It is the
// host-side governance surface: delegated tool execution runs against
// deps.Executor (the real host sandbox), and the agent's session/update +
// `_fleet/event` events flow to deps.Observer.
type hostClient struct {
	deps Deps

	mu    sync.Mutex
	final strings.Builder
	usage agentcore.RunUsage
}

// usageSnapshot returns the accumulated usage the agent reported over
// `_fleet/event`. Safe to call after the run completes.
func (c *hostClient) usageSnapshot() agentcore.RunUsage {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.usage
}

var (
	_ acp.Client                 = (*hostClient)(nil)
	_ acp.ExtensionMethodHandler = (*hostClient)(nil)
)

func (c *hostClient) finalText() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.final.String()
}

// SessionUpdate is the spec-compliant streaming mirror. It accumulates the
// agent's text chunks into the final reply, but it does NOT forward events to
// the Observer — the agent ALSO sends the full neutral (eventType, payload)
// stream over `_fleet/event`, and that is the single authoritative source for
// fleet's real Observer (→ SSE / session log). Driving the Observer from both
// would double-emit. Mapping it here would also under-report (session/update
// can't carry the full event vocabulary the in-process path emits).
func (c *hostClient) SessionUpdate(_ context.Context, p acp.SessionNotification) error {
	if u := p.Update; u.AgentMessageChunk != nil && u.AgentMessageChunk.Content.Text != nil {
		c.mu.Lock()
		c.final.WriteString(u.AgentMessageChunk.Content.Text.Text)
		c.mu.Unlock()
	}
	return nil
}

// HandleExtensionMethod is the governance seam: _fleet/tool executes a delegated
// native tool against the HOST sandbox; _fleet/event forwards a structured run
// event to the Observer.
func (c *hostClient) HandleExtensionMethod(ctx context.Context, method string, params json.RawMessage) (any, error) {
	switch method {
	case ExtMethodTool:
		return c.handleTool(ctx, params)
	case ExtMethodEvent:
		return c.handleEvent(params)
	case ExtMethodMCP:
		return c.handleMCP(ctx, params)
	case ExtMethodStage:
		return c.handleStage(params)
	case ExtMethodVerify:
		return c.handleVerify(ctx, params)
	default:
		return nil, acp.NewMethodNotFound(method)
	}
}

// handleVerify runs the host-side end-of-run verifier on the agent-shipped
// tool-exec summary. The DECISION to verify was made by the agent's scheduled
// policy (identical to in-process); the verifier EFFECT — an LLM re-check on the
// host fallback model with host-side creds — runs here. A host-side failure rides
// VerifyResponse.Error (NOT an RPC error) so the agent has exactly one fail-open
// branch, mirroring the in-process verifier's "log and allow finish" on error.
func (c *hostClient) handleVerify(ctx context.Context, params json.RawMessage) (any, error) {
	var req VerifyRequest
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, acp.NewInvalidParams(map[string]any{"error": err.Error()})
	}
	if c.deps.Verifier == nil {
		// Defensive: the agent only calls verify when the host set
		// RunSpec.VerifierWired, which it does only when a broker is wired. A call
		// with no broker is a wiring bug — surface it so the agent fails open rather
		// than silently treating the run as verified.
		return VerifyResponse{Error: "verifier not wired host-side"}, nil
	}
	missing, err := c.deps.Verifier.Verify(ctx, req.Round, req.Records)
	if err != nil {
		//nolint:nilerr // a verifier failure is reported via VerifyResponse.Error (fail-open on the agent), not the RPC error — mirrors the in-process "log and allow finish" contract.
		return VerifyResponse{Error: err.Error()}, nil
	}
	return VerifyResponse{Missing: missing}, nil
}

// handleMCP runs a delegated MCP tool call HOST-SIDE against the per-task
// credentialed client (P-ACP-2b credential brokering). The agent advertises the
// tool surface but holds no credentials; the call physically happens here, with
// creds applied host-side (BindMCPSelection). MCP credentials NEVER enter the
// agent container.
func (c *hostClient) handleMCP(ctx context.Context, params json.RawMessage) (any, error) {
	var req MCPRequest
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, acp.NewInvalidParams(map[string]any{"error": err.Error()})
	}
	if c.deps.MCPBroker == nil {
		// Defensive: the agent only advertises MCP tools when the host shipped
		// descriptors, which it does only when a broker is wired. A call with no
		// broker is a wiring bug — surface it as a tool error, not a silent allow.
		return MCPResponse{Error: "MCP broker not wired host-side"}, nil
	}
	text, isErr, err := c.deps.MCPBroker.CallMCP(ctx, req.Server, req.Tool, req.Arguments)
	if err != nil {
		// The DELEGATION succeeded; the broker's transport failure rides the
		// response Error field (the agent surfaces it as a tool error, exactly as
		// the in-process mcpTool does). Returning a nil error here is intentional.
		//nolint:nilerr // a tool/transport failure is reported via MCPResponse.Error, not the RPC error — mirrors the in-process tool-error contract.
		return MCPResponse{Error: err.Error()}, nil
	}
	return MCPResponse{Text: text, IsError: isErr}, nil
}

// handleStage stages a host-side approval / memory / note proposal. The agent's
// in-loop policy decided to stage; the EFFECT (DB write + SSE card) belongs to
// the host and runs here through the real stagers.
func (c *hostClient) handleStage(params json.RawMessage) (any, error) {
	var req StageRequest
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, acp.NewInvalidParams(map[string]any{"error": err.Error()})
	}
	if c.deps.StageBroker == nil {
		return StageResponse{Error: "staging broker not wired host-side"}, nil
	}
	switch req.Kind {
	case StageApproval:
		id, err := c.deps.StageBroker.StageApproval(req.ToolName, req.ToolCallID, req.RawInput)
		return stageResp(id, "", err), nil
	case StageSuggestion:
		id, msg, err := c.deps.StageBroker.StageSuggestion(req.Reason)
		return stageResp(id, msg, err), nil
	case StageMemory:
		id, err := c.deps.StageBroker.StageMemory(req.Content)
		return stageResp(id, "", err), nil
	case StageNote:
		id, err := c.deps.StageBroker.StageNote(req.Slug, req.Title, req.Body, req.Reason)
		return stageResp(id, "", err), nil
	default:
		return StageResponse{Error: "unsupported stage kind: " + string(req.Kind)}, nil
	}
}

func stageResp(id, msg string, err error) StageResponse {
	resp := StageResponse{ProposalID: id, Message: msg}
	if err != nil {
		resp.Error = err.Error()
	}
	return resp
}

// handleTool runs the delegated bash/python in the host-managed sandbox via the
// real agentcore.Executor. The agent has no executor of its own; this is where
// execution physically happens — on the host, never nested in the agent
// container (no Podman-in-Podman).
func (c *hostClient) handleTool(ctx context.Context, params json.RawMessage) (any, error) {
	var req ToolRequest
	if err := json.Unmarshal(params, &req); err != nil {
		return nil, acp.NewInvalidParams(map[string]any{"error": err.Error()})
	}
	// Honor the per-call timeout the native tool requested (the model can set
	// bash/python timeout_seconds), so a delegated call enforces the SAME bound
	// the in-process path applies via sandbox.BashRequest.Timeout. The host
	// Executor adds its own default when ctx carries no deadline.
	if req.TimeoutSeconds > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Duration(req.TimeoutSeconds)*time.Second)
		defer cancel()
	}
	switch req.Tool {
	case ToolBash:
		out, err := c.deps.Executor.RunBash(ctx, req.Command)
		return toToolResponse(out, err), nil
	case ToolPython:
		out, err := c.deps.Executor.RunPython(ctx, req.Code)
		return toToolResponse(out, err), nil
	default:
		return ToolResponse{Error: "unsupported tool: " + string(req.Tool)}, nil
	}
}

// toToolResponse maps an Executor result onto the wire response. A tool's own
// failure rides Error (surfaced to the model exactly as the in-process path
// would); the delegation itself succeeds.
func toToolResponse(out string, err error) ToolResponse {
	resp := ToolResponse{Output: out}
	if err != nil {
		resp.Error = err.Error()
	}
	return resp
}

// handleEvent re-emits a structured agent run event onto fleet's real Observer.
// The "usage" event additionally accumulates token/cost accounting host-side:
// the agent makes the LLM calls (in its container), so usage accrues there and is
// reported here per step. The host accumulates it so the driver surfaces the same
// usage/cost the in-process path produces — and the cost ceiling is enforced
// in-loop by the agent's policy (same ceilings, shipped in the RunSpec).
func (c *hostClient) handleEvent(params json.RawMessage) (any, error) {
	var ev EventNotification
	if err := json.Unmarshal(params, &ev); err != nil {
		return nil, acp.NewInvalidParams(map[string]any{"error": err.Error()})
	}
	if ev.EventType == EventUsage {
		c.accumulateUsage(ev.Payload)
	}
	c.deps.Observer.Observe(ev.EventType, ev.Payload)
	return map[string]any{}, nil
}

// accumulateUsage records the latest CUMULATIVE usage snapshot the agent
// reported. The agent ships its running totals (usageSnapshot — already summed
// across the run's steps) on every step, so the host takes the latest report
// rather than re-summing. The final value therefore equals the agent's
// end-of-run usage, which is exactly what the in-process path returns from
// usageSnapshot(orch). LastStepInputTokens is the latest step's input, carried
// verbatim. Reports are monotonic, so even an out-of-order delivery converges to
// the max on each field — but ACP notifications preserve order, so the last wins.
func (c *hostClient) accumulateUsage(payload map[string]any) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.usage = agentcore.RunUsage{
		PromptTokens:        intFromPayload(payload, usageKeyPromptTokens),
		LastStepInputTokens: intFromPayload(payload, usageKeyLastStepInputTokens),
		CompletionTokens:    intFromPayload(payload, usageKeyCompletionTokens),
		CachedTokens:        intFromPayload(payload, usageKeyCachedTokens),
		CacheCreationTokens: intFromPayload(payload, usageKeyCacheCreationTokens),
		CostUSD:             floatFromPayload(payload, usageKeyCostUSD),
	}
}

// RequestPermission: P-ACP-1 native flavor is fully governed by the in-loop
// Policy (which runs inside the agent — the same code as in-process), so a
// delegated permission request is auto-allowed here. P-ACP-2 wires the real
// human/policy permission UI for self-executing external flavors (default-deny
// on timeout, no approve-all).
func (c *hostClient) RequestPermission(_ context.Context, p acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	if len(p.Options) == 0 {
		return acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{
			Cancelled: &acp.RequestPermissionOutcomeCancelled{},
		}}, nil
	}
	// Prefer an allow-once option; else the first option.
	chosen := p.Options[0].OptionId
	for _, opt := range p.Options {
		if opt.Kind == acp.PermissionOptionKindAllowOnce || opt.Kind == acp.PermissionOptionKindAllowAlways {
			chosen = opt.OptionId
			break
		}
	}
	return acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{
		Selected: &acp.RequestPermissionOutcomeSelected{OptionId: chosen},
	}}, nil
}

// fs capabilities — the agent reads/writes the workspace through the client so
// file operations are host-governed (the workspace lives host-side). P-ACP-1
// native tools route file ops through the in-loop host path; these handlers back
// the advertised capability for any agent that uses fs/* directly.
func (c *hostClient) ReadTextFile(_ context.Context, p acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	b, err := os.ReadFile(p.Path)
	if err != nil {
		return acp.ReadTextFileResponse{}, err
	}
	return acp.ReadTextFileResponse{Content: string(b)}, nil
}

func (c *hostClient) WriteTextFile(_ context.Context, p acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	if err := os.WriteFile(p.Path, []byte(p.Content), 0o644); err != nil { //nolint:gosec // host-governed workspace path written under the loop's trust boundary.
		return acp.WriteTextFileResponse{}, err
	}
	return acp.WriteTextFileResponse{}, nil
}

// terminal capabilities — advertised so external flavors can use the terminal
// surface; the native flavor delegates bash/python via _fleet/tool instead.
// P-ACP-1 backs these minimally (the native flavor does not call them).
func (c *hostClient) CreateTerminal(_ context.Context, _ acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	return acp.CreateTerminalResponse{}, acp.NewMethodNotFound(acp.ClientMethodTerminalCreate)
}

func (c *hostClient) TerminalOutput(_ context.Context, _ acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	return acp.TerminalOutputResponse{}, acp.NewMethodNotFound(acp.ClientMethodTerminalOutput)
}

func (c *hostClient) ReleaseTerminal(_ context.Context, _ acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	return acp.ReleaseTerminalResponse{}, acp.NewMethodNotFound(acp.ClientMethodTerminalRelease)
}

func (c *hostClient) WaitForTerminalExit(_ context.Context, _ acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
	return acp.WaitForTerminalExitResponse{}, acp.NewMethodNotFound(acp.ClientMethodTerminalWaitForExit)
}

func (c *hostClient) KillTerminal(_ context.Context, _ acp.KillTerminalRequest) (acp.KillTerminalResponse, error) {
	return acp.KillTerminalResponse{}, acp.NewMethodNotFound(acp.ClientMethodTerminalKill)
}

// sortedKeys returns the map's keys in sorted order, for a deterministic podman
// invocation (stable across runs / easy to assert in tests).
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// logf is a thin logging helper kept for diagnostics on the host side.
func logf(format string, args ...any) { log.Printf("acpruntime: "+format, args...) }

var _ = logf
