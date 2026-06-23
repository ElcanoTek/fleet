package acpruntime

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	acp "github.com/coder/acp-go-sdk"

	"github.com/ElcanoTek/fleet/internal/agentcore"
)

// External ACP flavor (Plan v4, P-ACP-2): fleet's ClientRuntime drives an
// ARBITRARY external ACP agent (Claude Code via the claude-agent-acp bridge,
// Goose's native ACP, or — in CI — the coder SDK example/agent). Unlike the
// native flavor, an external agent SELF-EXECUTES inside its own sandbox: it
// does NOT delegate fs/terminal/extension calls to us. This is a fundamentally
// weaker trust posture than native, so it gets its OWN tier:
//
//	┌── governance tier ──────────────────────────────────────────────────────┐
//	│ native-inprocess / native-acp → FULLY GOVERNED                           │
//	│   per-tool-call policy + audit + notes + per-task MCP creds; tool         │
//	│   execution happens on the HOST under the real agentcore Executor.        │
//	│                                                                           │
//	│ external acp → CONTAINMENT (delegatedPolicy)                              │
//	│   the agent self-executes in a locked sandbox; fleet does NOT enforce     │
//	│   per-tool policy. Instead we CONTAIN the blast radius:                   │
//	│     - governance: delegated stamped into the session log;                 │
//	│     - audit is the agent's SELF-REPORT (its session/update stream),       │
//	│       captured as observed/audited, NOT enforced;                         │
//	│     - egress restricted to the model endpoint (network: model_only);      │
//	│     - env scrubbed: NO fleet secrets / MCP creds — only the provider's    │
//	│       own model key, declared in the manifest;                            │
//	│     - scratch-only workspace; coordinated teardown (process group +       │
//	│       container; we never trust --rm alone);                              │
//	│     - session/request_permission is routed to a HUMAN (default-deny on    │
//	│       timeout, no approve-all).                                           │
//	└───────────────────────────────────────────────────────────────────────┘
//
// Honesty caveat, surfaced in the docs and stamped in the log: an external
// agent that egresses to its own model endpoint can send the workspace contents
// to that endpoint. Containment bounds what it can DO on the host; it cannot
// stop a self-executing agent from transmitting what it READS to its provider.

// GovernanceTier is stamped into the session log so the audit trail records
// which trust posture a turn ran under. The string values are stable wire/log
// identifiers — do not rename without a migration of consumers.
type GovernanceTier string

const (
	// GovernanceGoverned is the native tier: fleet enforces per-tool-call policy
	// + audit + notes + creds (native-inprocess / native-acp).
	GovernanceGoverned GovernanceTier = "governed"
	// GovernanceDelegated is the external/containment tier: the agent
	// self-executes in a locked sandbox; fleet contains the blast radius and
	// observes the self-report, but does not enforce per-tool policy.
	GovernanceDelegated GovernanceTier = "delegated"
)

// EventGovernance is the event type the runtime emits at the start of an
// external turn so the session log + SSE record the governance tier. Carried on
// the Observer as ("governance", {...}); persisted alongside the turn events.
const EventGovernance = "governance"

// PermissionRequest is the neutral shape the runtime surfaces to the human when
// an external agent calls session/request_permission. It is provider-agnostic:
// the title/kind/locations/rawInput come from the agent's ToolCallUpdate, and
// Options are the agent's offered choices (each with an opaque OptionId the
// runtime must echo back). The host UI renders this and the human picks one
// option — or the request times out and is DENIED.
type PermissionRequest struct {
	// RequestID is a runtime-assigned id correlating the SSE prompt with the
	// human's decision (the decision endpoint passes it back).
	RequestID string `json:"requestId"`
	// SessionID scopes the request to the ACP session.
	SessionID string `json:"sessionId"`
	// ToolCallID / Title / Kind describe what the agent wants to do.
	ToolCallID string `json:"toolCallId,omitempty"`
	Title      string `json:"title,omitempty"`
	Kind       string `json:"kind,omitempty"`
	// Locations are the file paths the action touches (if the agent declared
	// any), for the human to review.
	Locations []string `json:"locations,omitempty"`
	// RawInput is the agent's tool input, surfaced verbatim for review.
	RawInput json.RawMessage `json:"rawInput,omitempty"`
	// Options are the choices the human may pick from. The runtime maps the
	// human's pick back to an ACP option by OptionId.
	Options []PermissionOption `json:"options"`
}

// PermissionOption is one offered choice. Kind is the ACP option kind
// ("allow_once" / "reject_once" / …); the UI renders allow-shaped vs
// reject-shaped buttons from it. We deliberately do NOT surface allow_always /
// reject_always as a one-click "approve all" — every request is decided on its
// own merits (see the design note: no approve-all).
type PermissionOption struct {
	OptionID string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
}

// PermissionDecision is the human's answer. Allowed picks an OptionID to echo
// back to the agent; a non-Allowed decision (deny, or a timeout) selects a
// reject-shaped option if the agent offered one, else cancels the request.
type PermissionDecision struct {
	// Allowed is true when the human approved (and OptionID names the picked
	// allow-shaped option). False denies.
	Allowed bool
	// OptionID is the agent option the human chose (allow path). Empty on deny.
	OptionID string
}

// PermissionBroker routes an external agent's permission request to a human and
// blocks until a decision arrives or ctx is cancelled. The host (httpapi) wires
// an implementation that emits the SSE prompt and waits on the decision
// endpoint; tests inject a deterministic one. The CONTRACT is default-deny:
// when ctx hits the timeout or is cancelled, the implementation MUST return a
// deny (Allowed=false), never an allow.
type PermissionBroker interface {
	// RequestDecision surfaces req to the human and blocks for the decision.
	// On ctx cancellation/timeout it returns a deny decision (the caller then
	// maps that onto a reject/cancel ACP outcome). It returns an error only for
	// an internal failure (the caller still treats an error as a deny).
	RequestDecision(ctx context.Context, req PermissionRequest) (PermissionDecision, error)
}

// ExternalDeps are the host-side dependencies for an EXTERNAL acp turn. Unlike
// the native Deps, there is NO Executor — the external agent self-executes and
// never delegates a tool call to us. The Observer captures the agent's
// self-reported session/update stream (observed/audited, not enforced), and the
// PermissionBroker routes session/request_permission to the human.
type ExternalDeps struct {
	// Observer receives the agent's streamed text/thought/tool events as fleet
	// run events (the containment tier's audit = this self-report). REQUIRED.
	Observer agentcore.Observer
	// PermissionBroker routes session/request_permission to the human. When nil,
	// every permission request is DENIED (fail-closed) — an external flavor
	// without a wired broker must never silently auto-allow.
	PermissionBroker PermissionBroker
	// PermissionTimeout caps how long a single permission request waits for the
	// human before it defaults to DENY. Zero uses DefaultPermissionTimeout.
	PermissionTimeout time.Duration
}

// DefaultPermissionTimeout is the default-deny window for an external agent's
// permission request when ExternalDeps.PermissionTimeout is unset.
const DefaultPermissionTimeout = 5 * time.Minute

// ExternalConfig configures one external-agent spawn. It differs from the
// native ClientConfig in its DEFAULTS and intent: model_only egress, a scrubbed
// env carrying ONLY the provider's own model key, and the delegated-policy
// containment tier.
type ExternalConfig struct {
	// Image is the provider's agent container image (digest-pinned in prod).
	Image string
	// PodmanBinary overrides the executable (default "podman").
	PodmanBinary string
	// Args are extra arguments passed to the agent ENTRYPOINT inside the
	// container (after the image), e.g. a bridge's mode flag. Provider-specific.
	Args []string
	// ProviderEnv is the SCRUBBED env handed to the agent container: ONLY the
	// provider's own model-endpoint credentials (e.g. ANTHROPIC_API_KEY for
	// Claude Code). fleet secrets and MCP credentials are NEVER included — the
	// external agent is not trusted with them. Declared per provider in the
	// manifest (the model-cred env var names) and populated host-side.
	ProviderEnv map[string]string
	// StartTimeout caps how long Initialize may take after spawn.
	StartTimeout time.Duration
	// NoNetwork seals the agent container's network namespace (`--network=none`)
	// — the enforced egress posture for a flavor whose manifest declares
	// `network: none`. (`model_only` stays declaration-only: fleet scrubs the env
	// but the packet-level restriction is the host firewall's job.) The caller
	// sets this from flavor.Network.
	NoNetwork bool
	// Workspace, when set, is a HOST directory bind-mounted READ-ONLY at
	// /workspace so the external agent can READ the conversation/task workspace
	// (matching the data-residency caveat in docs/USING-AGENTS.md). It is
	// deliberately read-only: a self-executing third-party agent must not write
	// to the host — its ephemeral writes go to the writable /tmp scratch tmpfs,
	// discarded on teardown. Empty ("") keeps the legacy scratch-only behavior
	// (a writable /workspace tmpfs), which the credential-free example agent +
	// the deterministic tests use.
	Workspace string
}

// ExternalRuntime is fleet's ClientRuntime specialized for an EXTERNAL ACP
// agent. It spawns the provider's agent in a locked, model-only-egress sandbox,
// drives Initialize → NewSession → Prompt, captures the agent's self-reported
// session/update stream onto the Observer (containment-tier audit), and routes
// session/request_permission to the human. Coordinated teardown reaps the whole
// process group + container.
type ExternalRuntime struct {
	cfg ExternalConfig
}

// NewExternalRuntime builds an ExternalRuntime over the given config.
func NewExternalRuntime(cfg ExternalConfig) *ExternalRuntime {
	if cfg.PodmanBinary == "" {
		cfg.PodmanBinary = "podman"
	}
	if cfg.StartTimeout <= 0 {
		cfg.StartTimeout = 60 * time.Second
	}
	return &ExternalRuntime{cfg: cfg}
}

// Run spawns the external agent, drives one turn, and tears the whole group
// down. The turn is governed at the CONTAINMENT tier: the runtime stamps
// governance: delegated, observes (does not enforce) the self-report, and
// routes permission requests to deps.PermissionBroker (default-deny). promptText
// is the user's turn; the external agent receives it as an ACP prompt content
// block (no fleet RunSpec/PromptMeta — the external agent is not our native one
// and does not understand those).
func (r *ExternalRuntime) Run(ctx context.Context, promptText string, deps ExternalDeps) (Result, error) {
	if deps.Observer == nil {
		return Result{}, fmt.Errorf("acpruntime: ExternalDeps.Observer is required (containment-tier audit)")
	}
	if r.cfg.Image == "" {
		return Result{}, fmt.Errorf("acpruntime: ExternalConfig.Image is required")
	}

	// Stamp the containment tier into the session log BEFORE anything runs, so
	// the audit trail records the trust posture even if the turn errors early.
	deps.Observer.Observe(EventGovernance, map[string]any{
		"tier":  string(GovernanceDelegated),
		"image": r.cfg.Image,
		// The honesty caveat, recorded in the log alongside the turn.
		"note": "external agent self-executes in a locked sandbox; fleet observes the self-report and contains blast radius but does not enforce per-tool policy; the agent may transmit workspace contents to its own model endpoint.",
	})

	proc, err := r.spawn()
	if err != nil {
		return Result{}, fmt.Errorf("spawn external agent: %w", err)
	}
	defer proc.teardown()

	timeout := deps.PermissionTimeout
	if timeout <= 0 {
		timeout = DefaultPermissionTimeout
	}
	cl := &externalClient{
		obs:         deps.Observer,
		broker:      deps.PermissionBroker,
		permTimeout: timeout,
	}
	conn := acp.NewClientSideConnection(cl, proc.stdin, bufio.NewReader(proc.stdout))

	initCtx, cancelInit := context.WithTimeout(ctx, r.cfg.StartTimeout)
	defer cancelInit()
	// We still advertise fs + terminal so a generic external agent that DOES try
	// to delegate gets a host-governed surface; a self-executing agent simply
	// won't call them. The trust posture (containment) is independent of the
	// advertised capabilities.
	initResp, err := conn.Initialize(initCtx, acp.InitializeRequest{
		ProtocolVersion: acp.ProtocolVersionNumber,
		ClientCapabilities: acp.ClientCapabilities{
			Fs:       acp.FileSystemCapabilities{ReadTextFile: true, WriteTextFile: true},
			Terminal: true,
		},
	})
	if err != nil {
		return Result{}, fmt.Errorf("initialize external agent: %w", err)
	}
	if err := checkInitializeResponse(initResp, "external agent"); err != nil {
		return Result{}, err
	}

	sess, err := conn.NewSession(ctx, acp.NewSessionRequest{
		Cwd:        "/workspace",
		McpServers: []acp.McpServer{},
	})
	if err != nil {
		return Result{}, fmt.Errorf("new session (external): %w", err)
	}

	promptResp, err := conn.Prompt(ctx, acp.PromptRequest{
		SessionId: sess.SessionId,
		Prompt:    []acp.ContentBlock{acp.TextBlock(promptText)},
	})
	if err != nil {
		// Set Usage on EVERY exit path (mirroring ClientRuntime.Run) so the driver
		// records whatever the agent self-reported before the error/cancel — never a
		// misleading $0/zero-token run at the very tier that drives its own endpoint.
		if ctx.Err() != nil {
			return Result{FinalText: cl.finalText(), Cancelled: true, Usage: cl.usageSnapshot()}, nil
		}
		return Result{FinalText: cl.finalText(), Usage: cl.usageSnapshot()}, fmt.Errorf("prompt (external): %w", err)
	}

	// Token totals come from the (UNSTABLE) PromptResponse.Usage; cost was folded
	// in from SessionUsageUpdate notifications during the stream. nil-safe.
	cl.capturePromptUsage(promptResp.Usage)

	return Result{
		FinalText:  cl.finalText(),
		StopReason: string(promptResp.StopReason),
		Cancelled:  promptResp.StopReason == acp.StopReasonCancelled,
		Usage:      cl.usageSnapshot(),
	}, nil
}

// spawn launches the external agent via `podman run -i` in the containment
// sandbox: hardened (--read-only, --cap-drop=ALL, no-new-privileges, scratch
// tmpfs workspace), with ONLY the provider's scrubbed model-endpoint env, and
// the network posture the manifest declared. We rely on the host firewall /
// egress policy to enforce model_only at the network layer; the runtime stamps
// the intent and keeps the env scrubbed so even an unrestricted container holds
// no fleet secrets. Own process group so teardown reaps the whole tree.
func (r *ExternalRuntime) spawn() (*agentProc, error) {
	return spawnAgentProc(r.cfg.PodmanBinary, r.runArgs())
}

// runArgs builds the `podman run` argv for the containment sandbox. Extracted
// from spawn so the workspace + hardening posture is unit-testable without
// launching a container (see external_test.go).
func (r *ExternalRuntime) runArgs() []string {
	base := []string{
		"run", "-i", "--rm",
		"--read-only",
		"--cap-drop=ALL",
		"--security-opt=no-new-privileges",
		// A writable scratch tmpfs the agent self-executes against, discarded on
		// teardown. With a read-only /workspace bind (below), this is where the
		// agent's ephemeral writes land — nothing persists to the host workspace.
		"--tmpfs=/tmp:rw,size=64m",
	}
	if r.cfg.NoNetwork {
		// Seal the network namespace for a `network: none` flavor. (model_only
		// is intentionally NOT enforced here — it is a declaration the host
		// firewall enforces; the env is scrubbed regardless.)
		base = append(base, "--network=none")
	}
	if r.cfg.Workspace != "" {
		// READ-ONLY bind of the conversation/task workspace: the external agent
		// READS the real files (the documented data-residency posture) but cannot
		// write to the host. `,z` relabels for SELinux shared access (same option
		// the native sandbox uses for its bind mounts).
		base = append(base, "--volume="+r.cfg.Workspace+":/workspace:ro,z")
	} else {
		// Scratch-only workspace: a writable tmpfs, nothing persists to the host.
		// Used by the credential-free example agent + the deterministic tests.
		base = append(base, "--tmpfs=/workspace:rw,size=256m")
	}
	envKeys := sortedKeys(r.cfg.ProviderEnv)
	args := make([]string, 0, len(base)+2*len(envKeys)+1+len(r.cfg.Args))
	args = append(args, base...)
	// Pass ONLY the provider's own model key(s), sorted for a stable
	// invocation. fleet secrets / MCP creds are never present in this map.
	for _, k := range envKeys {
		args = append(args, "--env", k+"="+r.cfg.ProviderEnv[k])
	}
	args = append(args, r.cfg.Image)
	args = append(args, r.cfg.Args...)
	return args
}

// externalClient implements acp.Client for the CONTAINMENT tier. Unlike the
// native hostClient, it FORWARDS the agent's session/update stream onto the
// Observer (the external agent's self-report is the only audit source — there
// is no _fleet/event), and routes session/request_permission to a human via the
// broker (default-deny). It exposes NO _fleet/* extension execution and NO host
// Executor — a self-executing external agent must not be able to run tools on
// the host.
type externalClient struct {
	obs         agentcore.Observer
	broker      PermissionBroker
	permTimeout time.Duration

	mu    sync.Mutex
	final strings.Builder
	reqN  int
	// usage is the external agent's SELF-REPORTED token + cost accounting,
	// assembled from two UNSTABLE SDK surfaces: token totals from
	// PromptResponse.Usage (captured in Run) and cumulative cost from
	// SessionUsageUpdate notifications (captured in SessionUpdate). The agent
	// drives its OWN model endpoint, so this is its self-report, not something
	// fleet meters — an unreported field stays zero, which the driver documents
	// as "unmetered", never a true $0 (see issue #31).
	usage agentcore.RunUsage
}

var _ acp.Client = (*externalClient)(nil)

func (c *externalClient) finalText() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.final.String()
}

// usageSnapshot returns the self-reported usage accumulated so far. Mirrors the
// native hostClient.usageSnapshot so ExternalRuntime.Run sets Result.Usage on
// every exit path exactly as ClientRuntime.Run does.
func (c *externalClient) usageSnapshot() agentcore.RunUsage {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.usage
}

// capturePromptUsage records the token totals from the agent's PromptResponse.
// The SDK Usage is cumulative across the session; the cost split is reported
// separately over SessionUsageUpdate, so this only touches the token fields and
// leaves any captured CostUSD intact. nil (the agent reported no usage) is a
// no-op — the tokens stay zero, honestly reflecting "the agent did not report".
func (c *externalClient) capturePromptUsage(u *acp.Usage) {
	if u == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.usage.PromptTokens = u.InputTokens
	c.usage.CompletionTokens = u.OutputTokens
	if u.CachedReadTokens != nil {
		c.usage.CachedTokens = *u.CachedReadTokens
	}
	if u.CachedWriteTokens != nil {
		c.usage.CacheCreationTokens = *u.CachedWriteTokens
	}
}

// SessionUpdate captures the external agent's self-reported stream. Text chunks
// accumulate into the final reply AND stream to the Observer (→ SSE) so the
// user sees the turn; thoughts and tool-call notices are forwarded as observed/
// audited events (NOT enforced — the agent already ran them in its sandbox).
func (c *externalClient) SessionUpdate(_ context.Context, p acp.SessionNotification) error {
	u := p.Update
	switch {
	case u.AgentMessageChunk != nil && u.AgentMessageChunk.Content.Text != nil:
		text := u.AgentMessageChunk.Content.Text.Text
		c.mu.Lock()
		c.final.WriteString(text)
		c.mu.Unlock()
		c.obs.Observe("text.delta", map[string]any{"text": text})
	case u.AgentThoughtChunk != nil && u.AgentThoughtChunk.Content.Text != nil:
		c.obs.Observe("reasoning.delta", map[string]any{"text": u.AgentThoughtChunk.Content.Text.Text})
	case u.ToolCall != nil:
		// Observed (not enforced): the external agent self-executed this; we
		// record it for the audit/self-report trail.
		c.obs.Observe("tool.call", map[string]any{
			"id":    string(u.ToolCall.ToolCallId),
			"title": u.ToolCall.Title,
			"kind":  string(u.ToolCall.Kind),
		})
	case u.ToolCallUpdate != nil:
		c.obs.Observe("tool.result", map[string]any{
			"id": string(u.ToolCallUpdate.ToolCallId),
		})
	case u.UsageUpdate != nil:
		// UNSTABLE SDK surface: the external agent self-reports cumulative session
		// cost here (token totals arrive on PromptResponse.Usage). We record the
		// cost ONLY when it is USD, because RunUsage.CostUSD is dollars — stamping a
		// EUR amount as USD would be a worse lie than the honest unmetered zero.
		c.recordReportedCost(u.UsageUpdate.Cost)
	}
	return nil
}

// recordReportedCost captures the agent's self-reported cumulative cost when it
// is denominated in USD (or carries no currency). A non-USD cost is observed for
// the audit trail but NOT folded into CostUSD — see SessionUpdate. nil is a
// no-op, leaving CostUSD at its honest unmetered zero.
func (c *externalClient) recordReportedCost(cost *acp.Cost) {
	if cost == nil {
		return
	}
	if cost.Currency != "" && !strings.EqualFold(cost.Currency, "USD") {
		c.obs.Observe("usage", map[string]any{
			"cost_unmetered_currency": cost.Currency,
			"cost_unmetered_amount":   cost.Amount,
		})
		return
	}
	c.mu.Lock()
	c.usage.CostUSD = cost.Amount
	c.mu.Unlock()
	c.obs.Observe("usage", map[string]any{"cost_usd": cost.Amount})
}

// RequestPermission routes the external agent's request to the human (broker),
// blocking up to permTimeout, then DEFAULT-DENIES. There is NO approve-all: each
// request is decided on its own. A nil broker fail-closes (deny). The result is
// mapped onto the agent's offered options: allow → the picked allow option;
// deny → a reject-shaped option if offered, else a Cancelled outcome.
func (c *externalClient) RequestPermission(ctx context.Context, p acp.RequestPermissionRequest) (acp.RequestPermissionResponse, error) {
	c.mu.Lock()
	c.reqN++
	reqID := fmt.Sprintf("perm-%d", c.reqN)
	c.mu.Unlock()

	rejectOption := firstRejectOption(p.Options)

	// No broker wired → fail-closed deny. An external flavor must never
	// silently auto-allow.
	if c.broker == nil {
		c.obs.Observe("permission.resolved", map[string]any{
			"request_id": reqID, "allowed": false, "reason": "no permission broker wired (fail-closed deny)",
		})
		return denyOutcome(rejectOption), nil
	}

	req := PermissionRequest{
		RequestID:  reqID,
		SessionID:  string(p.SessionId),
		ToolCallID: string(p.ToolCall.ToolCallId),
		Options:    toPermissionOptions(p.Options),
	}
	if p.ToolCall.Title != nil {
		req.Title = *p.ToolCall.Title
	}
	if p.ToolCall.Kind != nil {
		req.Kind = string(*p.ToolCall.Kind)
	}
	for _, loc := range p.ToolCall.Locations {
		req.Locations = append(req.Locations, loc.Path)
	}
	if p.ToolCall.RawInput != nil {
		if raw, err := json.Marshal(p.ToolCall.RawInput); err == nil {
			req.RawInput = raw
		}
	}

	decCtx, cancel := context.WithTimeout(ctx, c.permTimeout)
	defer cancel()
	dec, err := c.broker.RequestDecision(decCtx, req)
	if err != nil || !dec.Allowed {
		reason := "denied by user"
		if err != nil {
			reason = "default-deny: " + err.Error()
		} else if decCtx.Err() != nil {
			reason = "default-deny on timeout"
		}
		c.obs.Observe("permission.resolved", map[string]any{
			"request_id": reqID, "allowed": false, "reason": reason,
		})
		return denyOutcome(rejectOption), nil
	}

	// Allowed. Echo the human's chosen option; default to the first allow-shaped
	// option if the broker did not specify one.
	chosen := dec.OptionID
	if chosen == "" {
		chosen = firstAllowOption(p.Options)
	}
	c.obs.Observe("permission.resolved", map[string]any{
		"request_id": reqID, "allowed": true, "option_id": chosen,
	})
	return acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{
		Selected: &acp.RequestPermissionOutcomeSelected{OptionId: acp.PermissionOptionId(chosen)},
	}}, nil
}

// denyOutcome maps a deny onto the agent's options: pick a reject-shaped option
// if the agent offered one (so the agent gets a clean "rejected" signal), else
// cancel the request (the agent stops the action).
func denyOutcome(rejectOptionID string) acp.RequestPermissionResponse {
	if rejectOptionID != "" {
		return acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{
			Selected: &acp.RequestPermissionOutcomeSelected{OptionId: acp.PermissionOptionId(rejectOptionID)},
		}}
	}
	return acp.RequestPermissionResponse{Outcome: acp.RequestPermissionOutcome{
		Cancelled: &acp.RequestPermissionOutcomeCancelled{},
	}}
}

func toPermissionOptions(opts []acp.PermissionOption) []PermissionOption {
	out := make([]PermissionOption, 0, len(opts))
	for _, o := range opts {
		out = append(out, PermissionOption{
			OptionID: string(o.OptionId),
			Name:     o.Name,
			Kind:     string(o.Kind),
		})
	}
	return out
}

func firstAllowOption(opts []acp.PermissionOption) string {
	for _, o := range opts {
		if o.Kind == acp.PermissionOptionKindAllowOnce || o.Kind == acp.PermissionOptionKindAllowAlways {
			return string(o.OptionId)
		}
	}
	if len(opts) > 0 {
		return string(opts[0].OptionId)
	}
	return ""
}

func firstRejectOption(opts []acp.PermissionOption) string {
	for _, o := range opts {
		if o.Kind == acp.PermissionOptionKindRejectOnce || o.Kind == acp.PermissionOptionKindRejectAlways {
			return string(o.OptionId)
		}
	}
	return ""
}

// --- fs / terminal: backed minimally; a self-executing external agent does not
// use them, but the capability is advertised so a delegating one is host-bound.

func (c *externalClient) ReadTextFile(_ context.Context, _ acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	return acp.ReadTextFileResponse{}, acp.NewMethodNotFound(acp.ClientMethodFsReadTextFile)
}

func (c *externalClient) WriteTextFile(_ context.Context, _ acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	return acp.WriteTextFileResponse{}, acp.NewMethodNotFound(acp.ClientMethodFsWriteTextFile)
}

func (c *externalClient) CreateTerminal(_ context.Context, _ acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	return acp.CreateTerminalResponse{}, acp.NewMethodNotFound(acp.ClientMethodTerminalCreate)
}

func (c *externalClient) TerminalOutput(_ context.Context, _ acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	return acp.TerminalOutputResponse{}, acp.NewMethodNotFound(acp.ClientMethodTerminalOutput)
}

func (c *externalClient) ReleaseTerminal(_ context.Context, _ acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	return acp.ReleaseTerminalResponse{}, acp.NewMethodNotFound(acp.ClientMethodTerminalRelease)
}

func (c *externalClient) WaitForTerminalExit(_ context.Context, _ acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
	return acp.WaitForTerminalExitResponse{}, acp.NewMethodNotFound(acp.ClientMethodTerminalWaitForExit)
}

func (c *externalClient) KillTerminal(_ context.Context, _ acp.KillTerminalRequest) (acp.KillTerminalResponse, error) {
	return acp.KillTerminalResponse{}, acp.NewMethodNotFound(acp.ClientMethodTerminalKill)
}

var _ = sort.Strings
