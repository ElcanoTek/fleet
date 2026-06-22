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
	Executor agentcore.Executor
	// Observer receives the agent's streamed text/tool/progress + `_fleet/event`
	// notifications, re-emitted as fleet's real run events. REQUIRED.
	Observer agentcore.Observer
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
		// A ctx-cancellation is a clean stop: return the partial transcript so
		// the driver persists what streamed before the cancel.
		if ctx.Err() != nil {
			return Result{FinalText: cl.finalText(), Cancelled: true}, nil
		}
		return Result{FinalText: cl.finalText()}, fmt.Errorf("prompt: %w", err)
	}

	return Result{
		FinalText:  cl.finalText(),
		StopReason: string(promptResp.StopReason),
		Cancelled:  promptResp.StopReason == acp.StopReasonCancelled,
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

	// Not bound to a per-call timeout ctx: the agent process lives for the whole
	// run and is torn down explicitly by teardown(). We DO honor ctx via the
	// connection-level request contexts and the teardown on the deferred path.
	cmd := exec.Command(r.cfg.PodmanBinary, args...) //nolint:gosec,noctx // podman binary + args are operator-configured (image is a manifest ref), not user input; lifetime is managed by teardown(), not a request ctx.
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
	default:
		return nil, acp.NewMethodNotFound(method)
	}
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
func (c *hostClient) handleEvent(params json.RawMessage) (any, error) {
	var ev EventNotification
	if err := json.Unmarshal(params, &ev); err != nil {
		return nil, acp.NewInvalidParams(map[string]any{"error": err.Error()})
	}
	c.deps.Observer.Observe(ev.EventType, ev.Payload)
	return map[string]any{}, nil
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
