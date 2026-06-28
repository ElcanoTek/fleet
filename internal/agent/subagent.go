package agent

import (
	"context"
	"fmt"
	"log"
	"strings"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/agentcore"
)

// Governed sub-agents (#175, part b).
//
// A scheduled run may delegate a scoped piece of work to a CHILD run via the
// spawn_subagent native tool. The hard rule (ADR-0001, "governance is one core",
// extended by ADR-0007): the child is NOT a new or weaker loop — it is another
// agentcore.Run, governed exactly like the parent. The tool body below only
// adapts I/O around a fresh agent.Agent.Execute (→ agentcore.Run). It creates no
// second control-flow trunk, no second policy path, and no privileged executor.
//
// The four non-negotiable properties, each with its enforcement point named:
//
//  1. GOVERNANCE IS ONE CORE — the child is built with agent.NewAgent and run
//     through (*Agent).Execute, the SAME governed entrypoint the conformance test
//     (internal/agentcore/entrypoint_conformance_test.go) pins. See spawn().
//
//  2. MONOTONIC PRIVILEGE — the child inherits the parent's sandbox (so it shares
//     the parent's network-seal posture exactly — it has no namespace of its own
//     to widen), the parent's MCP client, and the parent's MCP/credential
//     allowlists, and may only SUBTRACT. See narrowedCredentialAllowlist and
//     childSelection: an allow_servers request is INTERSECTED with the parent's
//     loaded set / allowlist, never unioned. A per-child model is resolved
//     host-side (like the phone-a-friend reviewer), so credentials stay host-side.
//
//  3. HARD BUDGET SPLIT — the child's cost/token ceiling is sliced from the
//     parent's REMAINING budget (parent ceiling − parent spend so far, which
//     already includes earlier siblings), and the child's actual spend is charged
//     BACK into the parent after it returns. See spawn(): the budget read
//     (runtimePolicy.Budget) and the charge-back (runtimePolicy.ChargeChildUsage)
//     are the two enforcement points that make the parent ceiling a hard wall the
//     collective spend of all descendants can never breach.
//
//  4. RECURSION / FAN-OUT CAPS — depth and per-parent fan-out are capped; a spawn
//     that would exceed either is REFUSED with a tool error (never a panic, never
//     a silent allow). See spawn(): the depth check and the reserveChildSlot call.
//
//  5. CREDS HOST-SIDE — the child shares the parent's brokered MCP client; MCP
//     credentials are injected host-side by the broker, never enter any sandbox or
//     model context, and the child's tool calls run sandboxed under host policy —
//     identical to the parent, because it IS the same machinery.
//
// OFF by default (subagentConfig.enabled == false) so config/default behaviour is
// unchanged: the tool is not even registered unless FLEET_SUBAGENTS_ENABLED is set.

// subagentConfig is the per-Agent sub-agent state: the feature gate, the caps,
// this run's depth-in-tree, the per-child model resolution, and the live fan-out
// counter. Built by newSubagentConfig from the driver-supplied SubagentOptions.
type subagentConfig struct {
	enabled     bool
	depth       int // this run's depth in the spawn tree; root = 0
	maxDepth    int
	maxChildren int
	modelSlug   string // default child model slug; "" = inherit parent model
	resolver    ModelResolver

	// childrenSpawned counts how many children THIS run has spawned (fan-out). It
	// is guarded by the parent Agent's mu (reserveChildSlot/releaseChildSlot),
	// because tool calls within a run can execute concurrently (fantasy may run
	// parallel tools) and the fan-out cap must be a true invariant, not a racy
	// read-modify-write.
	childrenSpawned int
}

// newSubagentConfig normalizes the driver options into the per-run config,
// clamping non-positive caps to the package defaults so a misconfiguration can
// never mean "unbounded". A fresh root run starts at depth 0.
func newSubagentConfig(opts SubagentOptions) subagentConfig {
	maxDepth := opts.MaxDepth
	if maxDepth <= 0 {
		maxDepth = defaultSubagentMaxDepth
	}
	maxChildren := opts.MaxChildren
	if maxChildren <= 0 {
		maxChildren = defaultSubagentMaxChildren
	}
	return subagentConfig{
		enabled:     opts.Enabled,
		depth:       0,
		maxDepth:    maxDepth,
		maxChildren: maxChildren,
		modelSlug:   strings.TrimSpace(opts.ModelSlug),
		resolver:    opts.Resolver,
	}
}

// Sub-agent caps mirrored from config so the package has a self-contained default
// even when an Agent is constructed without a full config (tests, embedders).
const (
	defaultSubagentMaxDepth    = 2
	defaultSubagentMaxChildren = 4
)

// subagentMinChildCostUSD / subagentMinChildTokens are the floors below which a
// spawn is refused outright: a child handed a near-zero budget cannot make even
// one useful model call, so slicing it would waste a fan-out slot and confuse the
// model. They are deliberately tiny — large enough for one cheap completion.
const (
	subagentMinChildCostUSD = 0.01
	subagentMinChildTokens  = 1000
)

// spawnSubagentInput is the typed tool input.
type spawnSubagentInput struct {
	Task string `json:"task" description:"The self-contained task for the child agent to complete. It runs in a fresh context with NO access to this conversation's history, so include everything it needs."`
	// MaxCostUSD / MaxTotalTokens REQUEST a budget slice; the actual ceiling is
	// min(request, parent-remaining) — a child can never be granted more than the
	// parent has left. Omit (0) to let the parent decide a default slice.
	MaxCostUSD     float64 `json:"max_cost_usd,omitempty" description:"Optional cost budget (USD) to grant the child. Capped at the parent's remaining budget; omit to use a default slice of what remains."`
	MaxTotalTokens int     `json:"max_total_tokens,omitempty" description:"Optional token budget to grant the child. Capped at the parent's remaining budget; omit to use a default slice."`
	// Model optionally overrides the child's model slug (resolved host-side). Omit
	// to inherit the parent's model (or the configured default child model).
	Model string `json:"model,omitempty" description:"Optional model slug for the child (e.g. a cheaper/faster model for a narrow subtask). Omit to inherit the parent's model."`
	// AllowServers optionally NARROWS the child's MCP servers to a subset of the
	// servers the parent has loaded. It can only subtract: any name not already
	// available to the parent is dropped. Omit to inherit the parent's full set.
	AllowServers []string `json:"allow_servers,omitempty" description:"Optional list of MCP server names to allow the child (a subset of the servers you have loaded). The child can NEVER use a server you cannot; omit to inherit your full set."`
}

// defaultChildBudgetFraction is the slice of the parent's REMAINING budget a
// child gets when it does not request an explicit amount. <1 so an unspecified
// spawn leaves headroom for the parent and later siblings rather than draining
// the whole remaining budget into one child.
const defaultChildBudgetFraction = 0.5

// newSpawnSubagentTool builds the spawn_subagent native tool bound to this
// (parent) Agent. Registered only when the feature is enabled (see Execute).
func (a *Agent) newSpawnSubagentTool() fantasy.AgentTool {
	description := `Delegate a scoped subtask to a CHILD agent that runs to completion in its own fresh context, then returns its final answer to you.

Use this to parallelize or isolate a self-contained piece of work (research a sub-question, draft one section, analyze one dataset) so it doesn't crowd your own context. The child is a FULL governed agent: it runs sandboxed under the same policy you do, with the same tools and credentials, and its spend counts against YOUR budget.

Hard limits (a spawn that violates one is refused, not silently downgraded):
- The child gets a SLICE of your REMAINING cost/token budget; it can never spend beyond what you have left, and its spend reduces yours.
- Recursion depth and the number of children you may spawn are capped.
- The child inherits your sandbox and MCP/credential permissions and may only have FEWER, never more (least privilege).

Give the child a complete, standalone task — it cannot see this conversation.`

	return fantasy.NewAgentTool("spawn_subagent", description,
		func(ctx context.Context, in spawnSubagentInput, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return a.spawn(ctx, in)
		})
}

// spawn is the spawn_subagent tool body. It performs ALL governance checks BEFORE
// building or running the child, then runs the child through the one governed core
// and charges its spend back to the parent. Every refusal path returns a
// tool-level error (NewTextErrorResponse) — surfaced to the model as a tool
// result — not a transport error or a panic.
func (a *Agent) spawn(ctx context.Context, in spawnSubagentInput) (fantasy.ToolResponse, error) {
	if !a.subagent.enabled {
		// Defensive: the tool is not registered when disabled, so this is
		// unreachable in normal flow — but never silently allow if it is reached.
		return fantasy.NewTextErrorResponse("spawn_subagent is disabled."), nil
	}
	task := strings.TrimSpace(in.Task)
	if task == "" {
		return fantasy.NewTextErrorResponse("spawn_subagent requires a non-empty task."), nil
	}

	// ── (4a) RECURSION CAP ──────────────────────────────────────────────────
	// Refuse the spawn once THIS run's own depth has reached maxDepth, BEFORE
	// reserving a fan-out slot or touching the budget. Root = depth 0. With the
	// default maxDepth=2: root (0) spawns a child (depth 1), the child (1) spawns a
	// grandchild (depth 2), and the grandchild (2 >= 2) is refused — so the spawn
	// tree is at most maxDepth levels deep below the root.
	if a.subagent.depth >= a.subagent.maxDepth {
		return fantasy.NewTextErrorResponse(fmt.Sprintf(
			"SUBAGENT_DEPTH_EXCEEDED: this run is already at the maximum sub-agent depth (%d). "+
				"Do the work yourself instead of delegating further.", a.subagent.maxDepth)), nil
	}

	// ── (4b) FAN-OUT CAP ────────────────────────────────────────────────────
	// Atomically reserve a child slot under the parent's lock so concurrent tool
	// calls within this run cannot collectively exceed maxChildren. The slot is
	// released only if we refuse the spawn for a LATER reason (budget too small) —
	// a child that actually runs keeps its slot consumed.
	if !a.reserveChildSlot() {
		return fantasy.NewTextErrorResponse(fmt.Sprintf(
			"SUBAGENT_FANOUT_EXCEEDED: this run has already spawned the maximum of %d sub-agents. "+
				"Consolidate the remaining work or do it yourself.", a.subagent.maxChildren)), nil
	}

	// ── (3) HARD BUDGET SPLIT ───────────────────────────────────────────────
	// Read the PARENT's live budget from the SAME policy agentcore is enforcing.
	// remaining = parent ceiling − parent spend so far (spend already includes
	// every earlier sibling charged back via ChargeChildUsage), so successive
	// spawns see a strictly shrinking slice. -1 means "unlimited" (no parent
	// ceiling configured); in that case the child simply inherits "unlimited" too,
	// and the depth/fan-out caps remain the bound.
	var budget agentcore.BudgetState
	if a.runtimePolicy != nil {
		budget = a.runtimePolicy.Budget()
	}
	childCost, refusal := sliceCostBudget(budget, in.MaxCostUSD)
	if refusal != "" {
		a.releaseChildSlot()
		return fantasy.NewTextErrorResponse(refusal), nil
	}
	childTokens, refusal := sliceTokenBudget(budget, in.MaxTotalTokens)
	if refusal != "" {
		a.releaseChildSlot()
		return fantasy.NewTextErrorResponse(refusal), nil
	}

	// ── (2) MONOTONIC PRIVILEGE ─────────────────────────────────────────────
	// Resolve the child model host-side (inherit the parent handle unless an
	// override slug resolves). The child's MCP selection + credential allowlist
	// are NARROWED from the parent's — never widened.
	childModel, modelDesc := a.resolveChildModel(ctx, in.Model)
	childAllowlist := a.narrowedCredentialAllowlist()
	childAgent := a.buildChild(childModel, childAllowlist, in.AllowServers, childCost, childTokens)

	log.Printf("spawn_subagent: depth %d→%d, model=%s, cost_ceiling=%s, token_ceiling=%s",
		a.subagent.depth, a.subagent.depth+1, modelDesc,
		fmtCostCeiling(childCost), fmtTokenCeiling(childTokens))

	// ── (1) ONE GOVERNED CORE ───────────────────────────────────────────────
	// The child runs through (*Agent).Execute → agentcore.Run. No second loop.
	runErr := childAgent.Execute(ctx, task)

	// Charge the child's ACTUAL spend back into the parent's budget UNCONDITIONALLY
	// (even on error / partial run): the child may have spent before failing, and
	// that spend must count against the parent ceiling. This is the second half of
	// the hard budget split — after this, the parent's own checkCeilings and every
	// later sibling's budget read account for what this child spent.
	childUsage := childAgent.usageForParent()
	if a.runtimePolicy != nil {
		a.runtimePolicy.ChargeChildUsage(childUsage)
	}

	answer := strings.TrimSpace(latestAssistantText(childAgent.LogSession()))
	return fantasy.NewTextResponse(formatChildResult(answer, childUsage, runErr)), nil
}

// reserveChildSlot atomically increments the fan-out counter iff it is below the
// cap, returning false (no increment) when the cap is already reached. Guarded by
// the parent Agent's mu so parallel tool calls cannot both pass the check.
func (a *Agent) reserveChildSlot() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.subagent.childrenSpawned >= a.subagent.maxChildren {
		return false
	}
	a.subagent.childrenSpawned++
	return true
}

// releaseChildSlot returns a reserved slot when a spawn is refused AFTER reserving
// (e.g. the sliced budget was too small to be useful) so the refusal doesn't
// permanently consume a fan-out slot.
func (a *Agent) releaseChildSlot() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.subagent.childrenSpawned > 0 {
		a.subagent.childrenSpawned--
	}
}

// sliceCostBudget computes the child's cost ceiling from the parent's remaining
// cost budget and the (optional) requested amount. It returns (ceiling, "") on
// success — ceiling 0 means "unlimited" and is returned ONLY when the parent
// itself is unlimited. When the parent has too little left to be useful it returns
// (0, refusalMsg): a non-empty refusal string the caller surfaces as a tool error.
// The cap is the hard wall: a request is never honored beyond what the parent has
// remaining.
func sliceCostBudget(b agentcore.BudgetState, requested float64) (ceiling float64, refusal string) {
	remaining := b.RemainingCostUSD()
	if remaining < 0 {
		// Parent has no cost ceiling → child inherits "unlimited" cost too. Depth +
		// fan-out caps still bound the tree.
		return 0, ""
	}
	if remaining < subagentMinChildCostUSD {
		return 0, fmt.Sprintf("SUBAGENT_BUDGET_EXHAUSTED: only $%.4f of the cost budget remains — "+
			"too little to delegate; finish the work yourself with what's left", remaining)
	}
	grant := requested
	if grant <= 0 {
		grant = remaining * defaultChildBudgetFraction
	}
	if grant > remaining {
		// HARD CAP: never grant more than the parent has left.
		grant = remaining
	}
	if grant < subagentMinChildCostUSD {
		grant = subagentMinChildCostUSD
	}
	return grant, ""
}

// sliceTokenBudget mirrors sliceCostBudget for the token ceiling.
func sliceTokenBudget(b agentcore.BudgetState, requested int) (ceiling int, refusal string) {
	remaining := b.RemainingTokens()
	if remaining < 0 {
		return 0, "" // parent unlimited → child inherits unlimited tokens
	}
	if remaining < subagentMinChildTokens {
		return 0, fmt.Sprintf("SUBAGENT_BUDGET_EXHAUSTED: only %d tokens of the budget remain — "+
			"too little to delegate; finish the work yourself with what's left", remaining)
	}
	grant := requested
	if grant <= 0 {
		grant = int(float64(remaining) * defaultChildBudgetFraction)
	}
	if grant > remaining {
		grant = remaining // HARD CAP
	}
	if grant < subagentMinChildTokens {
		grant = subagentMinChildTokens
	}
	return grant, ""
}

// resolveChildModel returns the child's model handle + a human description for
// logs. Resolution is HOST-SIDE through the driver-supplied resolver (the same
// cached resolver the parent's model came from), so a per-child model choice is
// brokered exactly like the phone-a-friend reviewer and credentials never enter
// the sandbox or model context. On any resolution failure it falls back to the
// parent's model handle so a bad slug degrades rather than failing the spawn.
func (a *Agent) resolveChildModel(ctx context.Context, override string) (fantasy.LanguageModel, string) {
	slug := strings.TrimSpace(override)
	if slug == "" {
		slug = a.subagent.modelSlug // configured default child model ("" = inherit)
	}
	if slug == "" || a.subagent.resolver == nil {
		return a.model, "inherited(parent)"
	}
	m, err := a.subagent.resolver.Resolve(ctx, slug)
	if err != nil {
		log.Printf("spawn_subagent: child model %q unresolved (%v); inheriting parent model", slug, err)
		return a.model, "inherited(parent, resolve-failed)"
	}
	return m, slug
}

// narrowedCredentialAllowlist returns the credential allowlist for a child:
// IDENTICAL to the parent's (Gate-3). A child can never be granted a pair the
// parent lacks — it inherits the exact same allowlist value, which the child's
// own agentcore.Run enforces at the broker seam. (A nil parent allowlist means
// "inherit global"; the child inherits that same posture, which is still bounded
// by the shared MCP client's loaded/credentialed servers.) The narrowing axis the
// model can drive is the SERVER set, handled in childSelection — both axes only
// ever subtract.
func (a *Agent) narrowedCredentialAllowlist() agentcore.CredentialAllowlist {
	if a.credentialAllowlist == nil {
		return nil
	}
	// Copy so the child cannot mutate the parent's slice backing array.
	out := make(agentcore.CredentialAllowlist, len(a.credentialAllowlist))
	copy(out, a.credentialAllowlist)
	return out
}

// childSelection computes the child's loaded-server set: the parent's loaded set,
// optionally INTERSECTED with the model's allow_servers request. It can only
// subtract — any requested name the parent has NOT loaded is dropped, so a child
// can never reach a server the parent cannot. Returns the loaded-server map the
// child Agent starts with.
func (a *Agent) childSelection(allowServers []string) map[string]bool {
	a.mu.Lock()
	parentLoaded := make(map[string]bool, len(a.loadedServers))
	for name, ok := range a.loadedServers {
		if ok {
			parentLoaded[name] = true
		}
	}
	a.mu.Unlock()

	if len(allowServers) == 0 {
		return parentLoaded // inherit the full parent set
	}
	// Intersect: keep only requested servers the parent actually has. This is the
	// monotonic-privilege enforcement for the server axis — union is impossible.
	child := make(map[string]bool)
	for _, name := range allowServers {
		name = strings.TrimSpace(name)
		if parentLoaded[name] {
			child[name] = true
		}
	}
	return child
}

// buildChild constructs the CHILD Agent that the spawn runs through Execute. It
// inherits the parent's sandbox (→ same network-seal posture), MCP client (→ same
// brokered, host-side credentials), config, base prompt, persona, and notes
// wiring. Privilege only ever NARROWS: the credential allowlist is the parent's,
// the loaded-server selection is intersected with the request, and the budget is
// sliced (childCost/childTokens) by the caller. Depth is parent depth + 1; the
// child's own fan-out counter starts at 0. The child is given the SAME sub-agent
// config (so a grandchild may spawn, subject to the same caps), with depth
// advanced — this is what makes the caps compose across the tree.
func (a *Agent) buildChild(model fantasy.LanguageModel, allowlist agentcore.CredentialAllowlist, allowServers []string, childCost float64, childTokens int) *Agent {
	child := NewAgent(Options{
		Config:        a.config,
		Model:         model,
		FallbackModel: a.fallbackModel,
		MCPClient:     a.mcpClient,
		NativeTools:   a.nativeTools,
		SystemPrompt:  a.systemPrompt,
		Persona:       a.persona,
		MaxIterations: a.maxIterations,
		Sandbox:       a.sb,
		NotesProvider: a.notesProvider,
		NoteProposer:  a.noteProposer,
		// Monotonic privilege: the child carries the parent's (copied) credential
		// allowlist — Gate-3 enforced by the child's own agentcore.Run.
		CredentialAllowlist: allowlist,
		// A child does not itself run the phone-a-friend reviewer; that is a
		// root-run finish gate. Leaving it off avoids unbounded reviewer fan-out.
		PhoneAFriendEnabled: false,
		Subagent: SubagentOptions{
			Enabled:     a.subagent.enabled,
			MaxDepth:    a.subagent.maxDepth,
			MaxChildren: a.subagent.maxChildren,
			ModelSlug:   a.subagent.modelSlug,
			Resolver:    a.subagent.resolver,
		},
	})
	// Advance the child's depth (NewAgent resets it to 0). Done here rather than
	// via Options so the depth field stays a true internal invariant the model
	// cannot influence.
	child.subagent.depth = a.subagent.depth + 1
	// Narrow the child's loaded-server selection (intersection; never a superset).
	child.loadedServers = a.childSelection(allowServers)
	// Hand the child its SLICED budget ceiling. Execute applies these as the
	// child's MaxCostUSD/MaxTotalTokens, so the child's own agentcore.Run enforces
	// the slice via the SAME checkCeilings/budgetGuardedStep the parent uses. The
	// parent's config is untouched.
	child.costCeilingOverride = childCost
	child.tokenCeilingOverride = childTokens
	return child
}

// usageForParent snapshots the child run's accumulated usage in the RunUsage shape
// the parent charges back. Reads the child's session-log counters (the authoritative
// per-run totals agentcore.Run accumulated, including any nested grandchildren the
// child already charged into its own log).
func (a *Agent) usageForParent() agentcore.RunUsage {
	ls := a.logSession
	if ls == nil {
		return agentcore.RunUsage{}
	}
	return agentcore.RunUsage{
		PromptTokens:        ls.PromptTokens,
		CompletionTokens:    ls.CompletionTokens,
		CachedTokens:        ls.CachedTokens,
		CacheCreationTokens: ls.CacheCreationTokens,
		CostUSD:             ls.Cost,
	}
}

// formatChildResult renders the tool result the parent model sees: the child's
// answer plus a compact spend line, with an error note when the child failed.
func formatChildResult(answer string, usage agentcore.RunUsage, runErr error) string {
	var b strings.Builder
	if runErr != nil {
		fmt.Fprintf(&b, "[sub-agent ended with an error: %v]\n", runErr)
	}
	if answer == "" {
		b.WriteString("[sub-agent produced no final answer]")
	} else {
		b.WriteString(answer)
	}
	fmt.Fprintf(&b, "\n\n— sub-agent spend: $%.4f, %d prompt + %d completion tokens (charged to your budget)",
		usage.CostUSD, usage.PromptTokens, usage.CompletionTokens)
	return b.String()
}

func fmtCostCeiling(c float64) string {
	if c <= 0 {
		return "unlimited"
	}
	return fmt.Sprintf("$%.4f", c)
}

func fmtTokenCeiling(t int) string {
	if t <= 0 {
		return "unlimited"
	}
	return fmt.Sprintf("%d", t)
}
