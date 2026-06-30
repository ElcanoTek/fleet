package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"charm.land/fantasy"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/agentcore"
)

// Governed sub-agents / agent delegation (#175 part b, completed by #264).
//
// A scheduled run may delegate a scoped piece of work to a CHILD run via the
// spawn_subagent native tool — the delegation primitive issue #264 asks for,
// realized as this one tool rather than a second `delegate_task` entrypoint (a
// second tool would be the forked, weaker path ADR-0001/ADR-0007 forbid). The
// hard rule (ADR-0001, "governance is one core", extended by ADR-0007): the child
// is NOT a new or weaker loop — it is another agentcore.Run, governed exactly
// like the parent. The tool body below only adapts I/O around a fresh
// agent.Agent.Execute (→ agentcore.Run). It creates no second control-flow trunk,
// no second policy path, and no privileged executor.
//
// #264 additions, all preserving the four properties below: the tool is marked
// PARALLEL (fantasy dispatches multiple spawn calls in one turn concurrently, so
// the parent fans out and collects before its next LLM call); it returns a
// machine-parseable JSON result {result, cost_usd, tokens, success}; each child's
// budget slice is capped at a configurable fraction (default 10%) of the parent's
// REMAINING budget and a request over that cap is refused; delegation is opt-in
// PER TASK (allow_delegation) as well as fleet-wide (FLEET_SUBAGENTS_ENABLED); a
// child does NOT get the spawn tool (depth default 1 — "parent → sub-agent only");
// and the child's run is linked back to its parent task (parent_task_id) for
// traceability. None of this is a new governance path — it is configuration of,
// and I/O adaptation around, the same agentcore.Run.
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
	enabled        bool
	depth          int // this run's depth in the spawn tree; root = 0
	maxDepth       int
	maxChildren    int
	budgetFraction float64 // each child's max slice of the parent's remaining budget (#264)
	modelSlug      string  // default child model slug; "" = inherit parent model
	resolver       ModelResolver
	// parentTaskID links a spawned child's run back to the scheduled task that
	// owns this run (#264). Stamped onto the child's session log for traceability.
	// Empty on a root run that has no owning task id.
	parentTaskID string

	// childrenSpawned counts how many children THIS run has spawned (fan-out). It
	// is guarded by the parent Agent's mu (reserveChildSlot/releaseChildSlot),
	// because tool calls within a run can execute concurrently (fantasy may run
	// parallel tools) and the fan-out cap must be a true invariant, not a racy
	// read-modify-write.
	childrenSpawned int

	// reservedCostUSD / reservedTokens track budget GRANTED to children that are
	// still in flight (spawned but not yet returned). Both are guarded by the
	// parent Agent's mu. The budget grant subtracts these from the parent's
	// remaining budget under the lock BEFORE computing a new grant, so even N
	// concurrent spawns can never collectively be granted more than the parent has
	// left — the atomic reservation, not the tool being sequential, is what makes
	// the parent ceiling a hard wall. Each child's reservation is released (under
	// the lock) when it returns; its ACTUAL spend is then folded into the parent
	// via ChargeChildUsage, so the budget the next spawn reads reflects real spend
	// (reservation gone) rather than the conservative grant.
	reservedCostUSD float64
	reservedTokens  int
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
	fraction := opts.BudgetFraction
	if fraction <= 0 || fraction > 1 {
		fraction = defaultSubagentBudgetFraction
	}
	return subagentConfig{
		enabled:        opts.Enabled,
		depth:          0,
		maxDepth:       maxDepth,
		maxChildren:    maxChildren,
		budgetFraction: fraction,
		modelSlug:      strings.TrimSpace(opts.ModelSlug),
		resolver:       opts.Resolver,
	}
}

// Sub-agent caps mirrored from config so the package has a self-contained default
// even when an Agent is constructed without a full config (tests, embedders). The
// depth default is 1 (#264, "parent → sub-agent only"): a child does not get the
// spawn tool. The fraction default is 0.10 (each child gets ≤10% of the parent's
// remaining budget). Keep these in sync with internal/config defaults.
const (
	defaultSubagentMaxDepth       = 1
	defaultSubagentMaxChildren    = 5
	defaultSubagentBudgetFraction = 0.10
)

// subagentMinChildCostUSD / subagentMinChildTokens are the floors below which a
// spawn is refused outright: a child handed a near-zero budget cannot make even
// one useful model call, so slicing it would waste a fan-out slot and confuse the
// model. They are deliberately tiny — large enough for one cheap completion.
const (
	subagentMinChildCostUSD = 0.01
	subagentMinChildTokens  = 1000
)

// spawnSubagentInput is the typed tool input. Field names retained from #175
// (task, allow_servers) plus the #264 additions (max_iterations, timeout_minutes).
type spawnSubagentInput struct {
	Task string `json:"task" description:"The self-contained task for the child agent to complete. It runs in a fresh context with NO access to this conversation's history, so include everything it needs (instructions AND any data/file contents)."`
	// MaxCostUSD / MaxTotalTokens REQUEST a budget slice; the actual ceiling is
	// capped at the per-child limit (a fraction of the parent's remaining budget)
	// AND at what the parent has genuinely left. A request ABOVE the per-child limit
	// is refused (#264). Omit (0) to get the default slice (the full per-child limit).
	MaxCostUSD     float64 `json:"max_cost_usd,omitempty" description:"Optional cost budget (USD) for the child. Must be ≤ the per-child limit (a fraction, default 10%, of your REMAINING budget) or the call is refused. Omit to use the default slice."`
	MaxTotalTokens int     `json:"max_total_tokens,omitempty" description:"Optional token budget for the child. Must be ≤ the per-child limit (the same fraction of your remaining token budget) or the call is refused. Omit to use the default slice."`
	// MaxIterations caps the child's agent steps (tool-call/model round-trips),
	// orthogonal to the token/cost wall (#264). Capped at the parent's own iteration
	// budget — a child can never run more steps than the parent is allowed.
	MaxIterations int `json:"max_iterations,omitempty" description:"Optional cap on the child's number of agent steps (tool-call/model round-trips). Capped at your own iteration budget; omit to inherit it."`
	// TimeoutMinutes bounds the child's wall-clock; on timeout the child stops, its
	// partial spend is still charged back to you, and the result reports success=false (#264).
	TimeoutMinutes int `json:"timeout_minutes,omitempty" description:"Optional wall-clock timeout (minutes) for the child. On timeout the child is stopped, its spend is still charged to your budget, and the result reports success=false."`
	// Model optionally overrides the child's model slug (resolved host-side). Omit
	// to inherit the parent's model (or the configured default child model).
	Model string `json:"model,omitempty" description:"Optional model slug for the child (e.g. a cheaper/faster model for a narrow subtask). Omit to inherit the parent's model."`
	// AllowServers optionally NARROWS the child's MCP servers to a subset of the
	// servers the parent has loaded. It can only subtract: any name not already
	// available to the parent is dropped. Omit to inherit the parent's full set.
	AllowServers []string `json:"allow_servers,omitempty" description:"Optional list of MCP server names to allow the child (a subset of the servers you have loaded). The child can NEVER use a server you cannot; omit to inherit your full set."`
}

// spawnSubagentOutput is the machine-parseable JSON result a delegation returns
// (#264): the child's final answer plus its spend and a success flag the parent
// can branch on. Every spawn outcome — success, refusal, timeout, child error —
// is reported through this shape so concurrent results stay deterministic to parse.
type spawnSubagentOutput struct {
	Result  string  `json:"result"`
	CostUSD float64 `json:"cost_usd"`
	Tokens  int     `json:"tokens"`
	Success bool    `json:"success"`
}

// budgetEpsilon absorbs float rounding when comparing a requested cost against
// the per-child cap so a request exactly at the limit is not spuriously refused.
const budgetEpsilon = 1e-9

// newSpawnSubagentTool builds the spawn_subagent native tool bound to this
// (parent) Agent. Registered only when the feature is enabled (see Execute).
func (a *Agent) newSpawnSubagentTool() fantasy.AgentTool {
	description := `Delegate a scoped subtask to a CHILD agent that runs to completion in its own fresh context, then returns its final answer to you as JSON {result, cost_usd, tokens, success}.

Use this to PARALLELIZE or isolate self-contained work (research a sub-question, analyze one of N files, draft one section). Call it MULTIPLE times in a single turn to fan out: the children run concurrently and all their results come back before your next step. The child is a FULL governed agent: it runs sandboxed under the same policy you do, with the same tools and credentials, and its spend counts against YOUR budget.

Hard limits (a spawn that violates one is refused with success=false, not silently downgraded):
- The child's budget is capped at a fraction of your REMAINING budget (default 10%); requesting max_cost_usd/max_total_tokens above that cap is refused. Its spend reduces yours.
- The number of children you may spawn is capped; an extra spawn is refused with "max concurrent sub-agents reached".
- A child cannot itself delegate (it does not get this tool).
- The child inherits your sandbox and MCP/credential permissions and may only have FEWER, never more (least privilege).

Give the child a complete, standalone task — it cannot see this conversation.`

	// Marked PARALLEL (#264): fantasy dispatches multiple spawn_subagent calls in a
	// single model turn concurrently (its parallel-tool semaphore bounds true
	// concurrency), so the parent fans out and collects all results before its next
	// LLM call. The fan-out / budget reservation is already atomic under a.mu, so
	// concurrent execution is safe — see reserveChildSlot / reserveChildBudget.
	return fantasy.NewParallelAgentTool("spawn_subagent", description,
		func(ctx context.Context, in spawnSubagentInput, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return a.spawn(ctx, in)
		})
}

// spawn is the spawn_subagent tool body. It performs ALL governance checks BEFORE
// building or running the child, then runs the child through the one governed core
// and charges its spend back to the parent. Every refusal path returns a JSON
// result with success=false — surfaced to the model as a tool result, never a
// transport error or a panic.
func (a *Agent) spawn(ctx context.Context, in spawnSubagentInput) (fantasy.ToolResponse, error) {
	if !a.subagent.enabled {
		// Defensive: the tool is not registered when disabled, so this is
		// unreachable in normal flow — but never silently allow if it is reached.
		return refuseSpawn("spawn_subagent is disabled."), nil
	}
	task := strings.TrimSpace(in.Task)
	if task == "" {
		return refuseSpawn("spawn_subagent requires a non-empty task."), nil
	}
	if in.TimeoutMinutes < 0 {
		return refuseSpawn("timeout_minutes must be non-negative."), nil
	}
	if in.MaxIterations < 0 {
		return refuseSpawn("max_iterations must be non-negative."), nil
	}

	// ── (4a) RECURSION CAP ──────────────────────────────────────────────────
	// Refuse the spawn once THIS run's own depth has reached maxDepth, BEFORE
	// reserving a fan-out slot or touching the budget. Root = depth 0. With the
	// default maxDepth=1 the root (0) may spawn but a child (1 >= 1) may not — and a
	// child does not even get this tool registered (see buildChild), so this body is
	// the belt-and-suspenders backstop for that structural non-registration.
	if a.subagent.depth >= a.subagent.maxDepth {
		return refuseSpawn(fmt.Sprintf(
			"SUBAGENT_DEPTH_EXCEEDED: this run is already at the maximum sub-agent depth (%d). "+
				"Do the work yourself instead of delegating further.", a.subagent.maxDepth)), nil
	}

	// ── (4b) FAN-OUT CAP ────────────────────────────────────────────────────
	// Atomically reserve a child slot under the parent's lock so concurrent tool
	// calls within this run cannot collectively exceed maxChildren. The slot is
	// released only if we refuse the spawn for a LATER reason (budget too small) —
	// a child that actually runs keeps its slot consumed. This per-parent cap is
	// what makes the (max+1)-th delegation in a turn fail with an error result
	// rather than blocking — #264's "max concurrent sub-agents" guarantee.
	if !a.reserveChildSlot() {
		return refuseSpawn(fmt.Sprintf(
			"max concurrent sub-agents reached: this run has already spawned the maximum of %d "+
				"sub-agents. Consolidate the remaining work or do it yourself.", a.subagent.maxChildren)), nil
	}

	// ── (3) HARD BUDGET SPLIT ───────────────────────────────────────────────
	// Reserve the child's budget ATOMICALLY under the parent's mutex. The grant is
	// computed against the parent's remaining budget MINUS the budget already
	// granted to other in-flight children (subagent.reservedCostUSD/Tokens), and
	// capped at the per-child limit (a fraction of remaining, #264). The grant is
	// added to that reservation before the lock is released. This is what makes the
	// parent ceiling a hard wall WITHOUT relying on spawns being sequential: even N
	// concurrent spawns each read-modify-write the reservation under the same lock,
	// so the sum of granted budgets can never exceed the parent's remaining budget.
	// -1 (unlimited parent) yields an unlimited child; the depth/fan-out caps remain
	// the bound there. A request ABOVE the per-child limit is refused outright.
	childCost, childTokens, refusal := a.reserveChildBudget(in.MaxCostUSD, in.MaxTotalTokens)
	if refusal != "" {
		a.releaseChildSlot()
		return refuseSpawn(refusal), nil
	}

	// ── (2) MONOTONIC PRIVILEGE ─────────────────────────────────────────────
	// Resolve the child model host-side (inherit the parent handle unless an
	// override slug resolves). The child's MCP selection + credential allowlist
	// are NARROWED from the parent's — never widened.
	childModel, modelDesc := a.resolveChildModel(ctx, in.Model)
	childAllowlist := a.narrowedCredentialAllowlist()
	childAgent := a.buildChild(childModel, childAllowlist, in.AllowServers, childCost, childTokens, in.MaxIterations)

	log.Printf("spawn_subagent: depth %d→%d, model=%s, cost_ceiling=%s, token_ceiling=%s, timeout=%s, parent_task=%s",
		a.subagent.depth, a.subagent.depth+1, modelDesc,
		fmtCostCeiling(childCost), fmtTokenCeiling(childTokens), fmtTimeout(in.TimeoutMinutes), a.subagent.parentTaskID)

	// Release this child's in-flight reservation and charge its ACTUAL spend back
	// into the parent's budget UNCONDITIONALLY (even on error / partial run / timeout
	// / panic): the child may have spent before stopping, and that spend must count
	// against the parent ceiling. The deferred release + charge are the bookend of
	// the atomic grant — after them, the budget the next spawn reads reflects this
	// child's REAL spend (its conservative reservation is gone), and the parent's own
	// checkCeilings accounts for it too.
	defer func() {
		a.releaseChildBudget(childCost, childTokens)
		if a.runtimePolicy != nil {
			a.runtimePolicy.ChargeChildUsage(childAgent.usageForParent())
		}
	}()

	// ── (1) ONE GOVERNED CORE ───────────────────────────────────────────────
	// The child runs through (*Agent).Execute → agentcore.Run. No second loop. The
	// child context is derived from the parent's, so a parent kill-switch / deadline
	// cancels the child too; an optional per-child timeout (#264) bounds it further.
	childCtx := ctx
	if in.TimeoutMinutes > 0 {
		var cancel context.CancelFunc
		childCtx, cancel = context.WithTimeout(ctx, time.Duration(in.TimeoutMinutes)*time.Minute)
		defer cancel()
	}
	runErr := childAgent.Execute(childCtx, task)
	// A per-child timeout (parent NOT itself cancelled) is reported as a failed
	// result rather than an error, but the spend is still charged back (defer above).
	timedOut := in.TimeoutMinutes > 0 && childCtx.Err() == context.DeadlineExceeded && ctx.Err() == nil

	childUsage := childAgent.usageForParent()
	answer := strings.TrimSpace(latestAssistantText(childAgent.LogSession()))

	// Record a structured linkage entry on the PARENT's (persisted) session log so a
	// spawned child is traceable to its parent task with its spend (#264). The
	// child's own session also carries parent_task_id (set in buildChild).
	a.recordSubagentSpawn(childAgent.LogSession().ID, childUsage, runErr == nil && !timedOut)

	return spawnResult(answer, childUsage, runErr, timedOut), nil
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

// reserveChildBudget atomically computes and reserves a child's cost/token
// ceiling under the parent's mutex (#175 hardening). It reads the parent's live
// remaining budget, subtracts the budget already granted to OTHER in-flight
// children (the reservation), slices a grant from what is genuinely available,
// and adds that grant to the reservation — all in one critical section. Because
// the read-modify-write of the reservation is serialized by a.mu, the sum of
// budgets granted to any number of concurrent spawns can never exceed the
// parent's remaining budget, independent of whether the tool runs sequentially.
//
// It returns (costCeiling, tokenCeiling, "") on success — a 0 ceiling on an axis
// means "unlimited" and is returned only when the parent itself is unlimited on
// that axis. On a request that exceeds the per-child limit it returns a refusal
// (#264); on too-little-budget it returns a refusal and reserves nothing. The
// caller must call releaseChildBudget with the SAME returned ceilings once the
// child has run.
//
// Per-child cap (#264): on a finite axis the grant is capped at
// fraction*remaining (the "≤10% of remaining" limit), AND at what is genuinely
// available (remaining − in-flight sibling reservations). A request above the
// per-child cap is REFUSED outright rather than silently clamped.
func (a *Agent) reserveChildBudget(reqCost float64, reqTokens int) (costCeiling float64, tokenCeiling int, refusal string) {
	var budget agentcore.BudgetState
	if a.runtimePolicy != nil {
		budget = a.runtimePolicy.Budget()
	}
	fraction := a.subagent.budgetFraction
	if fraction <= 0 || fraction > 1 {
		fraction = defaultSubagentBudgetFraction
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	// Cost axis. RemainingCostUSD() == -1 means the parent is unlimited; reserve
	// nothing and grant unlimited (0). Otherwise the per-child cap is a fraction of
	// the parent's remaining budget, and the grant is bounded by both that cap and
	// what is genuinely available after in-flight siblings.
	if rem := budget.RemainingCostUSD(); rem >= 0 {
		capUSD := rem * fraction
		if reqCost > capUSD+budgetEpsilon {
			return 0, 0, fmt.Sprintf("SUBAGENT_BUDGET_OVER_LIMIT: requested $%.4f exceeds the per-child "+
				"limit of $%.4f (%.0f%% of your $%.4f remaining). Request a smaller max_cost_usd, split "+
				"the work across more children, or do it yourself.", reqCost, capUSD, fraction*100, rem)
		}
		effectiveMax := min(capUSD, rem-a.subagent.reservedCostUSD)
		if effectiveMax < subagentMinChildCostUSD {
			return 0, 0, fmt.Sprintf("SUBAGENT_BUDGET_EXHAUSTED: only $%.4f can be granted to a child right "+
				"now (per-child limit $%.4f, after in-flight sub-agents) — too little to delegate; finish "+
				"the work yourself with what's left.", max(effectiveMax, 0), capUSD)
		}
		costCeiling = grantCostFrom(effectiveMax, reqCost)
	}

	// Token axis, mirrored.
	if rem := budget.RemainingTokens(); rem >= 0 {
		capTok := int(float64(rem) * fraction)
		if reqTokens > capTok {
			return 0, 0, fmt.Sprintf("SUBAGENT_BUDGET_OVER_LIMIT: requested %d tokens exceeds the per-child "+
				"limit of %d (%.0f%% of your %d remaining). Request fewer max_total_tokens, split the work "+
				"across more children, or do it yourself.", reqTokens, capTok, fraction*100, rem)
		}
		effectiveMax := min(capTok, rem-a.subagent.reservedTokens)
		if effectiveMax < subagentMinChildTokens {
			return 0, 0, fmt.Sprintf("SUBAGENT_BUDGET_EXHAUSTED: only %d tokens can be granted to a child "+
				"right now (per-child limit %d, after in-flight sub-agents) — too little to delegate; finish "+
				"the work yourself with what's left.", max(effectiveMax, 0), capTok)
		}
		tokenCeiling = grantTokensFrom(effectiveMax, reqTokens)
	}

	// Commit the reservation while still holding the lock.
	a.subagent.reservedCostUSD += costCeiling
	a.subagent.reservedTokens += tokenCeiling
	return costCeiling, tokenCeiling, ""
}

// releaseChildBudget returns an in-flight child's reservation under the parent
// mutex once the child has finished. The child's ACTUAL spend is folded into the
// parent separately via ChargeChildUsage, so after the release the next spawn
// sees real spend rather than this child's conservative grant. Clamped at 0 so a
// double release can never drive the reservation negative.
func (a *Agent) releaseChildBudget(costCeiling float64, tokenCeiling int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.subagent.reservedCostUSD -= costCeiling
	if a.subagent.reservedCostUSD < 0 {
		a.subagent.reservedCostUSD = 0
	}
	a.subagent.reservedTokens -= tokenCeiling
	if a.subagent.reservedTokens < 0 {
		a.subagent.reservedTokens = 0
	}
}

// grantCostFrom slices a child's cost ceiling from effectiveMax — the smaller of
// the per-child cap and what is genuinely available after in-flight siblings
// (computed by reserveChildBudget). An unspecified request (<=0) gets the FULL
// effectiveMax (the #264 default of one per-child slice of remaining). A request
// is honoured up to effectiveMax — it is already known to be ≤ the per-child cap
// (reserveChildBudget refused anything larger). Pure helper, no locking; the
// caller holds a.mu and guarantees effectiveMax >= subagentMinChildCostUSD.
func grantCostFrom(effectiveMax, requested float64) float64 {
	grant := requested
	if grant <= 0 || grant > effectiveMax {
		grant = effectiveMax // HARD CAP: never grant more than effectiveMax.
	}
	if grant < subagentMinChildCostUSD {
		grant = subagentMinChildCostUSD
	}
	return grant
}

// grantTokensFrom mirrors grantCostFrom for the token axis.
func grantTokensFrom(effectiveMax, requested int) int {
	grant := requested
	if grant <= 0 || grant > effectiveMax {
		grant = effectiveMax // HARD CAP
	}
	if grant < subagentMinChildTokens {
		grant = subagentMinChildTokens
	}
	return grant
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
// child's own fan-out counter starts at 0.
//
// Whether the child itself may delegate is decided HERE (#264): the child gets
// the spawn tool only when its depth is still below maxDepth. With the default
// maxDepth=1 the child's depth (1) is NOT below it, so childCanDelegate is false
// and the spawn tool is never even registered in the child — "parent → sub-agent
// only", enforced structurally (non-registration) rather than relying solely on
// the in-body depth check. Raising FLEET_SUBAGENTS_MAX_DEPTH re-enables deeper
// trees, and the caps compose because each level re-evaluates this.
//
// maxIter optionally caps the child's agent steps (#264), bounded at the parent's
// own iteration budget so a child can never run more steps than the parent may.
func (a *Agent) buildChild(model fantasy.LanguageModel, allowlist agentcore.CredentialAllowlist, allowServers []string, childCost float64, childTokens int, maxIter int) *Agent {
	childDepth := a.subagent.depth + 1
	childCanDelegate := a.subagent.enabled && childDepth < a.subagent.maxDepth

	childIter := a.maxIterations
	if maxIter > 0 && (childIter <= 0 || maxIter < childIter) {
		childIter = maxIter
	}

	child := NewAgent(Options{
		Config:        a.config,
		Model:         model,
		FallbackModel: a.fallbackModel,
		MCPClient:     a.mcpClient,
		NativeTools:   a.nativeTools,
		SystemPrompt:  a.systemPrompt,
		Persona:       a.persona,
		MaxIterations: childIter,
		Sandbox:       a.sb,
		NotesProvider: a.notesProvider,
		NoteProposer:  a.noteProposer,
		// Persistent task memory (#285) is inherited from the parent so a child of a
		// Captain's Log task shares the same task-scoped memory; nil for a
		// non-opted-in parent (unchanged default).
		TaskMemory:       a.taskMemory,
		TaskID:           a.taskID,
		TaskMemoryConfig: a.taskMemoryConfig,
		// Monotonic privilege: the child carries the parent's (copied) credential
		// allowlist — Gate-3 enforced by the child's own agentcore.Run.
		CredentialAllowlist: allowlist,
		// A child does not itself run the phone-a-friend reviewer; that is a
		// root-run finish gate. Leaving it off avoids unbounded reviewer fan-out.
		PhoneAFriendEnabled: false,
		Subagent: SubagentOptions{
			// Off unless the child is still below the depth cap (#264): a depth-limit
			// child gets NO spawn tool, so it structurally cannot delegate further.
			Enabled:        childCanDelegate,
			MaxDepth:       a.subagent.maxDepth,
			MaxChildren:    a.subagent.maxChildren,
			BudgetFraction: a.subagent.budgetFraction,
			ModelSlug:      a.subagent.modelSlug,
			Resolver:       a.subagent.resolver,
		},
	})
	// Advance the child's depth (NewAgent resets it to 0). Done here rather than
	// via Options so the depth field stays a true internal invariant the model
	// cannot influence.
	child.subagent.depth = childDepth
	// Link the child's run back to the parent task for traceability (#264): a fresh,
	// unique session ID (concurrent children must not collide on the timestamp-based
	// default), the parent task id, and a descriptive title. The child writes to its
	// OWN log file (derived below) so parallel children never clobber one another.
	child.subagent.parentTaskID = a.subagent.parentTaskID
	child.logSession.ID = "subagent-" + uuid.NewString()
	child.logSession.ParentTaskID = a.subagent.parentTaskID
	if a.subagent.parentTaskID != "" {
		child.logSession.Title = "Sub-agent of task " + a.subagent.parentTaskID
	} else {
		child.logSession.Title = "Sub-agent"
	}
	child.logFile = childLogFilePath(a.logFile, child.logSession.ID)
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

// childLogFilePath derives a UNIQUE per-child log-file path from the parent's
// effective log path so concurrently-running children (the tool is parallel, #264)
// never clobber one another's file — and never the parent's. It mirrors
// writeLogFile's default-path resolution, then inserts the child's session id
// before the extension.
func childLogFilePath(parentLogFile, childID string) string {
	base := parentLogFile
	if base == "" {
		base = os.Getenv("FLEET_LOG_FILE")
	}
	if base == "" {
		base = os.Getenv("CUTLASS_LOG_FILE")
	}
	if base == "" {
		base = "fleet-session.json"
	}
	ext := filepath.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	return fmt.Sprintf("%s.%s%s", stem, childID, ext)
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

// spawnResult builds the JSON tool result the parent model sees (#264):
// {result, cost_usd, tokens, success}. `result` carries the child's final answer
// (with a leading note when the child errored or timed out); success is false on
// any error / timeout / empty answer so the parent can branch deterministically
// even when several results return at once. tokens is prompt+completion.
func spawnResult(answer string, usage agentcore.RunUsage, runErr error, timedOut bool) fantasy.ToolResponse {
	success := true
	var note string
	switch {
	case timedOut:
		success = false
		note = "[sub-agent timed out before finishing; partial work below, spend charged to your budget]\n"
	case runErr != nil:
		success = false
		note = fmt.Sprintf("[sub-agent ended with an error: %v]\n", runErr)
	}
	result := note + answer
	if answer == "" {
		success = false
		result = note + "[sub-agent produced no final answer]"
	}
	return marshalSpawnOutput(spawnSubagentOutput{
		Result:  result,
		CostUSD: usage.CostUSD,
		Tokens:  usage.PromptTokens + usage.CompletionTokens,
		Success: success,
	})
}

// refuseSpawn builds the JSON result for a refused spawn (disabled / bad input /
// depth / fan-out / budget): success=false, zero spend, reason in `result`. It is
// a tool RESULT the model reads (#264 "error result"), not a transport error or a
// panic.
func refuseSpawn(reason string) fantasy.ToolResponse {
	return marshalSpawnOutput(spawnSubagentOutput{Result: reason, Success: false})
}

// marshalSpawnOutput renders a spawnSubagentOutput as the tool response. Marshal
// cannot fail for this flat struct, but on the impossible error we fall back to a
// plain text error rather than dropping the result.
func marshalSpawnOutput(out spawnSubagentOutput) fantasy.ToolResponse {
	data, err := json.Marshal(out)
	if err != nil {
		return fantasy.NewTextErrorResponse(out.Result)
	}
	return fantasy.NewTextResponse(string(data))
}

// recordSubagentSpawn appends a structured linkage entry to the PARENT's session
// log (#264) so a spawned child is traceable to the parent task with its spend.
// The parent's log is the one persisted for the scheduled run, so this is the
// queryable record; the child's own session additionally carries parent_task_id.
func (a *Agent) recordSubagentSpawn(childSessionID string, usage agentcore.RunUsage, success bool) {
	if a.logSession == nil {
		return
	}
	payload, err := json.Marshal(struct {
		ParentTaskID   string  `json:"parent_task_id,omitempty"`
		ChildSessionID string  `json:"child_session_id"`
		CostUSD        float64 `json:"cost_usd"`
		Tokens         int     `json:"tokens"`
		Success        bool    `json:"success"`
	}{
		ParentTaskID:   a.subagent.parentTaskID,
		ChildSessionID: childSessionID,
		CostUSD:        usage.CostUSD,
		Tokens:         usage.PromptTokens + usage.CompletionTokens,
		Success:        success,
	})
	if err != nil {
		return
	}
	t := "subagent_spawned"
	a.logSession.AddMessageWithMetadata(roleTool, string(payload), nil, nil, &t, nil, nil, "")
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

func fmtTimeout(minutes int) string {
	if minutes <= 0 {
		return "none"
	}
	return fmt.Sprintf("%dm", minutes)
}
