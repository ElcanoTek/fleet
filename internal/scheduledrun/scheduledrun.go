// Package scheduledrun is the shared, governed scheduled-task driver. It builds
// an agent.Agent over an interactive Manager's model resolver + sandbox warm pool
// and runs ONE task to completion through agentcore.Run (Mode=Scheduled) — the
// single governed core (policy, cost/token ceilings, audit, the finish verifier)
// every fleet entrypoint shares.
//
// Two callers drive it: cmd/fleet's capped worker pool (the production scheduler)
// and cmd/cutlass's local one-shot harness. Both reach the SAME governed loop, so
// the harness is not a second, weaker execution path — it is the production
// driver with a CLI front-end instead of the orchestrator round-trip. This is why
// the logic lives here, in a shared internal package, rather than being copied:
// the "governance is one core" invariant (AGENTS.md) forbids a divergent fork.
package scheduledrun

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"charm.land/fantasy"
	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/sandbox"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// Options configures a Runner. Manager and Config are required; the rest mirror
// the bundle-resolved scheduled-runtime selection.
type Options struct {
	Config           *config.Config
	Manager          *agent.Manager
	NotesProvider    agentcore.NotesProvider
	NoteProposer     agentcore.NoteProposer
	PersonasDir      string
	SystemPromptsDir string
	ProtocolsDir     string

	// IterationStore records per-iteration telemetry for looped tasks (#179). nil
	// disables telemetry (the loop still runs); production wires the sched storage.
	IterationStore IterationStore

	// TaskEnqueuer backs the built-in create_task tool (#277): it lets a SCHEDULED
	// run of a task that opted in (allow_task_creation) enqueue follow-up tasks.
	// nil disables the tool entirely (it is never registered) — the secure default.
	// Production wires the sched storage; *storage.Storage satisfies the seam.
	TaskEnqueuer tools.TaskEnqueuer

	// PersonaPolicies is the per-persona tool allowlist (Gate-4, #294), keyed by
	// persona basename, translated from the bundle manifest's personas: block.
	// nil/empty = no narrowing for any persona (defaults unchanged). cmd/fleet
	// builds it once from the bundle and hands the SAME map to both drivers.
	PersonaPolicies map[string]agentcore.PersonaToolPermissions

	// ── agent self-improvement (#285), gated per-task by instruction_self_improve
	// ("Captain's Log"). These seams are wired ONCE here and handed to a run only
	// when its task opted in (runWorker), so non-opted-in tasks behave exactly as
	// before. nil disables the respective capability entirely. ──
	//
	// TaskMemory backs the remember/recall tools + run-start memory injection
	// (#198); TaskMemoryConfig caps how much a single task may accumulate.
	TaskMemory       tools.TaskMemoryStore
	TaskMemoryConfig tools.TaskMemoryConfig

	// RemoteMCP resolves a task owner's OAuth-connected remote (hosted) MCP servers
	// and mints their bearer tokens, so a scheduled (headless) run can use the same
	// per-user servers a chat turn would (#443). nil = feature off. OwnerEmail maps
	// the task's creator UUID to the chat-side email the tokens are keyed by; nil
	// disables remote-MCP wiring for scheduled runs even when RemoteMCP is set.
	RemoteMCP  agent.RemoteMCPResolver
	OwnerEmail func(ctx context.Context, userID uuid.UUID) (string, error)
}

// Runner executes claimed scheduled tasks in-process through the unified runtime
// (Mode=Scheduled). It reuses the model resolver + sandbox warm pool held on the
// interactive Manager — the SAME sandbox boundary interactive turns use.
//
// Per-task MCP credential-account isolation: when a task carries an mcp_selection
// with named accounts, the run gets its OWN MCP client onto which the selection's
// account-variant subprocesses are bound via agentcore.BindMCPSelection (which
// overlays <VAR>_<ACCOUNT> via creds.ApplyClientSuffix onto the subprocess cmd.Env
// — never argv, never the sandbox). That per-run client is Closed at run end so no
// credentialed subprocess leaks across runs or into a concurrent task's client.
// Tasks with no selection (or a default-account-only selection) reuse the shared
// process-wide client.
type Runner struct {
	cfg           *config.Config
	mgr           *agent.Manager
	notesProvider agentcore.NotesProvider
	noteProposer  agentcore.NoteProposer

	personasDir      string
	systemPromptsDir string
	protocolsDir     string

	baseSystemPrompt string

	iterationStore IterationStore
	taskEnqueuer   tools.TaskEnqueuer

	// personaPolicies is the per-persona tool allowlist (Gate-4, #294), keyed by
	// persona basename. nil/empty = no narrowing. Resolved per task at dispatch.
	personaPolicies map[string]agentcore.PersonaToolPermissions

	// Captain's Log persistent task memory (#285), handed to a run only when the
	// task opted in (instruction_self_improve). nil = capability disabled.
	taskMemory       tools.TaskMemoryStore
	taskMemoryConfig tools.TaskMemoryConfig

	// remoteMCP + ownerEmail wire a task owner's OAuth-connected remote (hosted)
	// MCP servers into a headless run (#443). nil = feature off.
	remoteMCP  agent.RemoteMCPResolver
	ownerEmail func(ctx context.Context, userID uuid.UUID) (string, error)
}

// IterationStore records per-iteration telemetry for a looped task (#179). It is
// the narrow subset of sched storage the loop runner needs; *storage.Storage
// satisfies it. nil = telemetry disabled (the loop still runs).
type IterationStore interface {
	AddTaskIteration(ctx context.Context, it *models.TaskIteration) error
}

// New builds a Runner. The base system prompt + persona are read once at
// construction (operators editing them in place take effect on the next process
// restart, matching the scheduled path's prior behaviour).
func New(opts Options) *Runner {
	r := &Runner{
		cfg:              opts.Config,
		mgr:              opts.Manager,
		notesProvider:    opts.NotesProvider,
		noteProposer:     opts.NoteProposer,
		personasDir:      opts.PersonasDir,
		systemPromptsDir: opts.SystemPromptsDir,
		protocolsDir:     opts.ProtocolsDir,
		iterationStore:   opts.IterationStore,
		taskEnqueuer:     opts.TaskEnqueuer,
		personaPolicies:  opts.PersonaPolicies,
		taskMemory:       opts.TaskMemory,
		taskMemoryConfig: opts.TaskMemoryConfig,
		remoteMCP:        opts.RemoteMCP,
		ownerEmail:       opts.OwnerEmail,
	}
	r.baseSystemPrompt = r.buildBaseSystemPrompt()
	return r
}

// personaPolicy resolves the per-persona tool allowlist (#294) for a resolved
// scheduled persona filename (e.g. "code-reviewer.yaml" or the global default).
// It returns nil when there is no manifest entry or the entry's policy is empty
// — both mean "no narrowing". The persona filename is normalized to its bare
// basename to match the manifest keys.
func (r *Runner) personaPolicy(persona string) *agentcore.PersonaToolPermissions {
	if len(r.personaPolicies) == 0 {
		return nil
	}
	base := filepath.Base(strings.TrimSpace(persona))
	base = strings.TrimSuffix(base, filepath.Ext(base))
	p, ok := r.personaPolicies[base]
	if !ok || (len(p.Allow) == 0 && len(p.Deny) == 0) {
		return nil
	}
	return &p
}

// buildBaseSystemPrompt composes the scheduled base prompt: the default system
// prompt + the configured GLOBAL persona's domain expertise. Baked once at
// startup and used for tasks with no per-task persona override (#221).
func (r *Runner) buildBaseSystemPrompt() string {
	personaPath := r.cfg.Persona
	if personaPath == "" {
		personaPath = "assistant.yaml"
	}
	return r.composeSystemPrompt(personaPath)
}

// composeSystemPrompt builds the scheduled system prompt = the default system
// prompt + the named persona's domain expertise block. personaFile is a
// personas/ filename (e.g. "assistant.yaml"); a missing persona file just omits
// the expertise block. Failures degrade to a partial prompt rather than blocking.
func (r *Runner) composeSystemPrompt(personaFile string) string {
	var sb strings.Builder
	spName := r.cfg.SystemPrompt
	if spName == "" {
		spName = "default.md"
	}
	if content, err := os.ReadFile(filepath.Join(r.systemPromptsDir, filepath.Base(spName))); err == nil {
		sb.Write(content)
	}
	if content, err := os.ReadFile(filepath.Join(r.personasDir, filepath.Base(personaFile))); err == nil && len(content) > 0 {
		name := strings.TrimSuffix(filepath.Base(personaFile), filepath.Ext(personaFile))
		fmt.Fprintf(&sb, "\n\n---\n\n# %s Domain Expertise & Context\n\n", name)
		sb.Write(content)
	}
	return sb.String()
}

// taskPromptAndPersona resolves the system prompt + effective persona filename
// for a task (#221). With no per-task persona it returns the pre-baked base
// prompt + global persona; with a valid override it rebuilds the prompt with
// that persona; an unknown override logs and falls back to the global default.
func (r *Runner) taskPromptAndPersona(task *models.Task) (systemPrompt, persona string) {
	override := strings.TrimSpace(task.Persona)
	if override == "" {
		return r.baseSystemPrompt, r.cfg.Persona
	}
	personaFile := filepath.Base(override) + ".yaml"
	if _, err := os.Stat(filepath.Join(r.personasDir, personaFile)); err != nil {
		log.Printf("scheduled task %s: persona %q not found in bundle; using global default", task.ID, override)
		return r.baseSystemPrompt, r.cfg.Persona
	}
	return r.composeSystemPrompt(personaFile), personaFile
}

// maybeAppendCreateTaskTool returns base with the built-in create_task tool
// appended ONLY when the run is authorized to spawn follow-up tasks (#277):
//   - an enqueuer must be wired (nil = the feature is disabled process-wide), and
//   - the task must have opted in via allow_task_creation (default false).
//
// This is the authority gate, evaluated at tool-list construction time, mirroring
// how the scheduled confirm_audit tool is conditionally appended. Because this
// driver is scheduled-only, an INTERACTIVE turn never reaches this code at all,
// and a scheduled run whose task did not opt in never gets the tool — so there is
// no privilege-escalation path and the model literally cannot see create_task
// unless its task granted the capability. When the gate is closed, base is
// returned unchanged (defaults are byte-identical to the prior behaviour).
//
// The deeper limits (per-run spawn cap, per-child budget fraction, the stricter
// recurrence grant) are enforced INSIDE the tool as defence in depth; this gate
// only decides whether the tool exists for the run. The per-run spawn counter is
// allocated here so it is scoped to exactly one task run.
// selfImproveTaskMemory is the per-task Captain's Log (#285) opt-in gate: it
// returns the persistent task-memory store for a task ONLY when the task set
// instruction_self_improve, and nil otherwise. Centralizing the gate here keeps
// runWorker readable and makes the opt-in boundary unit-testable. A nil return
// (a runner built without the seam, or a task that did not opt in) cleanly
// disables the capability — the agent registers remember/recall only when the
// seam is non-nil, so non-opted-in tasks behave exactly as before.
func (r *Runner) selfImproveTaskMemory(task *models.Task) tools.TaskMemoryStore {
	if task == nil || !task.InstructionSelfImprove {
		return nil
	}
	return r.taskMemory
}

func (r *Runner) maybeAppendCreateTaskTool(base []fantasy.AgentTool, task *models.Task) []fantasy.AgentTool {
	if r.taskEnqueuer == nil || !task.AllowTaskCreation {
		return base
	}
	counter := &atomic.Int32{}
	out := append(append([]fantasy.AgentTool{}, base...),
		tools.NewCreateTaskTool(tools.CreateTaskConfig{
			Enqueuer:         r.taskEnqueuer,
			CreatingTaskID:   task.ID,
			ParentModel:      task.Model,
			ParentBudgetUSD:  r.cfg.MaxCostUSD,
			RecurringAllowed: task.AllowRecurringTaskCreation,
			MaxCreations:     tools.DefaultMaxTaskCreations,
			Counter:          counter,
		}))
	log.Printf("scheduled task %s: create_task tool enabled (recurring=%v)", task.ID, task.AllowRecurringTaskCreation)
	return out
}

// SystemPromptForPersona returns the assembled scheduled system prompt for a
// persona override, using the SAME composition the runner applies at dispatch:
// the global base prompt for an empty/unknown persona, or the default prompt
// plus that persona's domain-expertise block for a known override. It exists so
// the cost forecast (#233) can count the exact system prompt a real run would
// send. Read-only: it reads bundle files already on disk and assembles a string;
// it dispatches nothing.
func (r *Runner) SystemPromptForPersona(persona string) string {
	override := strings.TrimSpace(persona)
	if override == "" {
		return r.baseSystemPrompt
	}
	personaFile := filepath.Base(override) + ".yaml"
	if _, err := os.Stat(filepath.Join(r.personasDir, personaFile)); err != nil {
		return r.baseSystemPrompt
	}
	return r.composeSystemPrompt(personaFile)
}

// sandboxTaker is the subset of *sandbox.Pool that a scheduled run uses to
// acquire an execution sandbox. It is an interface so the take-decision
// (sealed-by-default vs. egress opt-in) is unit-testable without spinning a
// real podman container.
type sandboxTaker interface {
	// Take returns a warm, network-ENABLED sandbox (the interactive default).
	Take() (*sandbox.Sandbox, func(), error)
	// TakeContainer cold-starts a fresh sandbox with egress SEALED
	// (--network=none) — the lockdown boundary.
	TakeContainer(ctx context.Context) (*sandbox.Sandbox, func(), error)
	// TakeContainerWithOverrides cold-starts a fresh sandbox applying per-task
	// resource overrides (#205), with the caller's chosen network posture.
	TakeContainerWithOverrides(ctx context.Context, ov sandbox.ResourceOverride, noNetwork bool) (*sandbox.Sandbox, func(), error)
}

// takeTaskSandbox acquires the bash/run_python execution sandbox for a
// scheduled task. By default (task.AllowNetwork == false) it seals outbound
// egress via TakeContainer (--network=none), matching the interactive lockdown
// path — an unattended task can otherwise fetch arbitrary URLs, pip install,
// SSRF host-local services, or exfiltrate with no human on the loop. A task
// opts into egress by setting AllowNetwork, which draws from the shared warm
// pool (Take).
//
// The sealed path fails CLOSED on a real container error — it does not silently
// downgrade to egress-on. The single exception is ErrContainerUnavailable,
// which means there is no container backend at all (a host-mode / mock pool —
// e.g. the cutlass dev one-shot or tests without podman): a host sandbox has no
// network namespace to seal, so sealing is not applicable and we fall back to
// the host take. This is not a production downgrade — buildSandboxPool requires
// a container image outside mock mode, so a real deployment always seals here.
func takeTaskSandbox(ctx context.Context, pool sandboxTaker, task *models.Task) (*sandbox.Sandbox, func(), error) {
	// Per-task sandbox limits (#205) require a cold start (a warm pooled container
	// is already running with the pool's ceilings), so route through the override
	// path — applying the task's own network posture (sealed unless AllowNetwork).
	if !task.SandboxLimits.IsZero() {
		sb, cleanup, err := pool.TakeContainerWithOverrides(ctx, sandboxOverride(task.SandboxLimits), !task.AllowNetwork)
		if errors.Is(err, sandbox.ErrContainerUnavailable) {
			// Host/mock pool: no container to size — fall back to the host take.
			return pool.Take()
		}
		return sb, cleanup, err
	}
	if task.AllowNetwork {
		return pool.Take()
	}
	sb, cleanup, err := pool.TakeContainer(ctx)
	if errors.Is(err, sandbox.ErrContainerUnavailable) {
		return pool.Take()
	}
	return sb, cleanup, err
}

// sandboxOverride converts a task's optional per-task limits (#205) into the
// sandbox package's podman-ready override shape. A zero field maps to "" / 0,
// which the pool leaves at its default. memory is emitted in MiB ("Nm"), cpus
// with two decimals to match the global SandboxCPUs string format.
func sandboxOverride(l *models.TaskSandboxLimits) sandbox.ResourceOverride {
	if l == nil {
		return sandbox.ResourceOverride{}
	}
	ov := sandbox.ResourceOverride{PidsLimit: l.Pids}
	if l.MemoryMB > 0 {
		ov.MemoryLimit = fmt.Sprintf("%dm", l.MemoryMB)
	}
	if l.CPUs > 0 {
		ov.CPULimit = strconv.FormatFloat(l.CPUs, 'f', 2, 64)
	}
	return ov
}

// Run executes one task and returns the converted session log. It satisfies
// runner.TaskRunner. A task with no LoopConfig is a single worker pass (the
// prior behaviour, byte-identical); a task WITH a LoopConfig (#179) runs the
// worker+verify loop instead — see runWithLoop.
func (r *Runner) Run(ctx context.Context, task *models.Task) (*models.LogSession, error) {
	// Git worktree isolation (#180): prepare the worktree ONCE per task — before
	// the (possibly looped) execution — so a looped task's iterations share one
	// worktree and accumulate filesystem state, rather than each iteration
	// starting from a fresh empty checkout. wtPath is "" when worktree isolation
	// is disabled (the common case), leaving the sandbox working dir untouched.
	runID := uuid.NewString()[:8]
	wtPath, _, wtCleanup, err := prepareWorktree(ctx, r.workspaceRoot(), task, runID)
	if err != nil {
		return nil, fmt.Errorf("prepare worktree: %w", err)
	}

	// Record the effective per-run workspace path for the file browser (#287): the
	// per-run git worktree subdir when isolation is enabled, otherwise the shared
	// workspace root the sandbox bind-mounts. Reported once, before the agent runs;
	// the reporter (installed by the runner pool) persists it to the task row. A
	// nil reporter (tests / cutlass one-shot) makes this a no-op.
	effectiveWorkspace := wtPath
	if effectiveWorkspace == "" {
		effectiveWorkspace = r.workspaceRoot()
	}
	if abs, aerr := filepath.Abs(effectiveWorkspace); aerr == nil {
		effectiveWorkspace = abs
	}
	reportWorkspacePath(ctx, effectiveWorkspace)
	// Cleanup is scheduled in a defer so its clock starts at run COMPLETION, never
	// run start: the agent executes synchronously below (a loop can run for many
	// minutes), and arming the delay timer up-front would let a delay shorter than
	// the run delete the worktree out from under the live agent. With the defer,
	// cleanup_delay_seconds is the post-run inspection window the docs promise.
	// (A process crash before this defer leaves an orphan, reclaimed by
	// `fleet-admin worktree prune`.)
	if wc := task.WorktreeConfig; wc != nil && wc.Enabled && wc.AutoCleanup {
		defer func() {
			if wc.CleanupDelaySeconds > 0 {
				time.AfterFunc(time.Duration(wc.CleanupDelaySeconds)*time.Second, wtCleanup)
			} else {
				wtCleanup()
			}
		}()
	}

	if task.LoopConfig != nil {
		return r.runWithLoop(ctx, task, wtPath)
	}
	session, _, _, err := r.runWorker(ctx, task, "", nil, wtPath)
	return session, err
}

// workspaceRoot resolves the host workspace root the same way the agent manager
// does (config, then ./workspace), so worktree operations target the same dir
// the sandbox bind-mounts.
func (r *Runner) workspaceRoot() string {
	if r.cfg != nil && strings.TrimSpace(r.cfg.WorkspaceRoot) != "" {
		return r.cfg.WorkspaceRoot
	}
	if abs, err := filepath.Abs("workspace"); err == nil {
		return abs
	}
	return "workspace"
}

// runWorker executes ONE worker pass: it resolves the model, acquires the
// sandbox + MCP, runs the agent to completion, and (when lc != nil) evaluates
// the loop exit condition while the sandbox is still live. extraPrompt carries a
// prior iteration's output forward as additional context (empty on the first /
// only pass). It returns the session, whether the exit condition passed (always
// true / unused when lc == nil), the exit-condition result label, and any run
// error.
func (r *Runner) runWorker(ctx context.Context, task *models.Task, extraPrompt string, lc *models.LoopConfig, wtPath string) (*models.LogSession, bool, string, error) {
	// Resolve the task's model (falls back to the configured task model).
	modelSlug := r.cfg.TaskModel
	if task.Model != nil && strings.TrimSpace(*task.Model) != "" {
		modelSlug = strings.TrimSpace(*task.Model)
	}
	if modelSlug == "" {
		return nil, false, "", fmt.Errorf("no model configured for scheduled task (set CUTLASS_TASK_MODEL or the task's model)")
	}
	model, err := r.mgr.Resolve(ctx, modelSlug)
	if err != nil {
		return nil, false, "", fmt.Errorf("resolve scheduled model %q: %w", modelSlug, err)
	}
	var fallback = model
	if task.FallbackModel != nil && strings.TrimSpace(*task.FallbackModel) != "" {
		if fb, ferr := r.mgr.Resolve(ctx, strings.TrimSpace(*task.FallbackModel)); ferr == nil {
			fallback = fb
		}
	} else if r.cfg.TaskFallbackModel != "" {
		if fb, ferr := r.mgr.Resolve(ctx, r.cfg.TaskFallbackModel); ferr == nil {
			fallback = fb
		}
	}

	// "Phone a friend" super-LLM reviewer (#175). Resolved only when the feature
	// is enabled; the reviewer slug comes from FLEET_PHONE_A_FRIEND_MODEL and
	// falls back to the run's fallback model when unset. A resolution failure
	// leaves the reviewer nil, which the agent treats as "skip the review" — the
	// feature degrades gracefully rather than failing the run.
	var reviewer fantasy.LanguageModel
	if r.cfg.PhoneAFriendEnabled {
		reviewer = fallback
		if slug := strings.TrimSpace(r.cfg.PhoneAFriendModel); slug != "" {
			if rv, rerr := r.mgr.Resolve(ctx, slug); rerr == nil {
				reviewer = rv
			} else {
				log.Printf("scheduled task %s: phone_a_friend reviewer %q unresolved (%v); falling back to the run's fallback model", task.ID, slug, rerr)
			}
		}
	}

	// Acquire the execution sandbox for this task. Scheduled runs are
	// network-SEALED by default (--network=none, same as interactive lockdown)
	// because unattended runs have no human on the loop; a task opts into
	// outbound egress only via its AllowNetwork field. See takeTaskSandbox.
	sb, cleanup, err := takeTaskSandbox(ctx, r.mgr.SandboxPool(), task)
	if err != nil {
		return nil, false, "", fmt.Errorf("take sandbox: %w", err)
	}
	defer cleanup()

	// Git worktree isolation (#180): scope this run's tool calls into the per-run
	// worktree (prepared once per task in Run; a subdir of the bind-mounted
	// workspace root, reachable at the same absolute path inside the sandbox).
	// Two complementary seams cover the tool surface:
	//   - Sandbox.SetDefaultWorkingDir fills the cwd of any bash/run_python call
	//     that arrives with an empty WorkingDir, so the default applies host-side.
	//   - WithForcedWorkingDir scopes the IN-PROCESS tool layer (bash/run_python/
	//     file tools), whose resolvers otherwise default an empty working dir to
	//     the process cwd before the sandbox seam can fill it.
	// Both are no-ops when wtPath == "" (non-worktree task), so behaviour there is
	// unchanged.
	if wtPath != "" {
		sb.SetDefaultWorkingDir(wtPath)
		ctx = tools.WithForcedWorkingDir(ctx, wtPath)
		log.Printf("scheduled task %s: git worktree isolation active; tool calls scoped to %s", task.ID, wtPath)
	}

	turnTools := tools.NewTurnTools(sb)

	// create_task (#277) is appended ONLY for a task that opted in. See
	// maybeAppendCreateTaskTool for the gate rationale.
	nativeTools := r.maybeAppendCreateTaskTool(turnTools.Tools, task)

	// #191 git-metadata tools (suggest_branch_name / suggest_commit_message /
	// suggest_pr_description) are wired into the SCHEDULED native set only:
	// autonomous agents that produce branches/commits/PRs are the use case, and
	// a task's MCP selection is narrow, so the 3 extra tools stay well clear of
	// the 128-tool ceiling that the interactive chat turn runs near (#433/#449).
	// They resolve through the SAME host-side Manager resolver the run uses for
	// its main model (r.mgr), so the operator's key never enters the sandbox.
	// MetadataModel defaults to the title model in config.Load; an empty slug
	// (only reachable via a test double) makes the tool return a clear error.
	nativeTools = append(nativeTools, tools.MetadataTools(r.mgr, r.cfg.MetadataModel)...)

	maxIter := r.cfg.MaxIterations
	if task.MaxIterations != nil && *task.MaxIterations > 0 {
		maxIter = *task.MaxIterations
	}

	// Wire per-task MCP credential-account isolation. When the task names
	// accounts, bind its account-variant subprocesses onto a DEDICATED per-run
	// client and Close them at run end so credentials never leak across runs or to
	// a concurrent task. A default-only / empty selection reuses the shared client.
	mcpClient, mcpCleanup, err := r.bindTaskMCP(ctx, task)
	if err != nil {
		return nil, false, "", err
	}
	defer mcpCleanup()

	// Per-user remote (hosted) MCP overlay (#443): wire the task owner's
	// OAuth-connected servers via the SAME composite mechanism the chat path uses,
	// so a headless run reaches them without mutating the shared/per-run client.
	// Best-effort: a server that needs re-auth or whose owner can't be resolved is
	// skipped, never failing the run.
	remoteOverlay := r.buildTaskRemoteOverlay(ctx, task, mcpClient)
	defer remoteOverlay.Close()

	// Per-task persona override (#221): a task may name a personas/<name>.yaml to
	// swap in specialized domain expertise; empty uses the runner's global persona.
	taskSystemPrompt, taskPersona := r.taskPromptAndPersona(task)

	// Captain's Log (#285): instruction_self_improve is the per-task opt-in gate
	// that finally gives the flag runtime effect (#322). Only when it is set does
	// the run get persistent task memory — the remember/recall tools + run-start
	// injection (#198). OFF (the default) reproduces pre-#285 behaviour exactly.
	// The seam is nil-checked here so a runner built without it (or a non-opted-in
	// task) wires nothing. Agent-authored client-bundle skills are intentionally
	// out of scope (#255 closed): skills stay operator-authored so the bundle
	// remains a reproducible git artifact — fleet never writes the bundle.
	//
	// NOTE: propose_note is deliberately NOT gated by this flag — it stays
	// unconditionally available in scheduled mode (it is wired separately, above,
	// via NoteProposer) so opting OUT of Captain's Log does not regress the
	// pre-existing note-proposal behaviour, and it remains fleet's DB-backed path
	// for agents to improve the shared knowledge base. The flag gates only the NEW
	// capability — persistent task memory. "Off = today's behaviour" is the rule.
	taskMemory := r.selfImproveTaskMemory(task)

	a := agent.NewAgent(agent.Options{
		Config:        r.cfg,
		Model:         model,
		FallbackModel: fallback,
		MCPClient:     mcpClient,
		NativeTools:   nativeTools,
		SystemPrompt:  taskSystemPrompt,
		Persona:       taskPersona,
		MaxIterations: maxIter,
		Sandbox:       sb,
		NotesProvider: r.notesProvider,
		NoteProposer:  r.noteProposer,
		// Captain's Log persistent memory (#285): nil unless the task opted in (above).
		TaskMemory:          taskMemory,
		TaskID:              task.ID,
		TaskMemoryConfig:    r.taskMemoryConfig,
		CredentialAllowlist: taskCredentialAllowlist(task),
		PersonaPolicy:       r.personaPolicy(taskPersona),
		Overlay:             remoteOverlay,
		PhoneAFriendEnabled: r.cfg.PhoneAFriendEnabled,
		ReviewerModel:       reviewer,
		// Governed sub-agents / delegation (#175, #264): enabled when the fleet-wide
		// FLEET_SUBAGENTS_ENABLED operator flag is on OR THIS task opted in via
		// allow_delegation — the per-task opt-in is sufficient on its own (#264),
		// while the env flag stays a fleet-wide override (#175). Either way the tool
		// is registered ONLY here in the scheduled driver, never in interactive chat.
		// The child model is resolved HOST-SIDE through the SAME Manager resolver the
		// parent's model came from (r.mgr), so a per-child model choice keeps
		// credentials host-side — never in the sandbox or model context.
		Subagent: agent.SubagentOptions{
			Enabled:        r.cfg.SubagentsEnabled || task.AllowDelegation,
			MaxDepth:       r.cfg.SubagentsMaxDepth,
			MaxChildren:    r.cfg.SubagentsMaxChildren,
			BudgetFraction: r.cfg.SubagentsBudgetFraction,
			ModelSlug:      r.cfg.SubagentsModel,
			Resolver:       r.mgr,
		},
	})

	// On a retry (a prior attempt failed transiently and was re-queued), warn the
	// agent so it can guard non-idempotent external side-effects: a counter alone
	// can't prevent a re-run from re-sending an email / re-charging / re-mutating
	// state, so the agent must verify before repeating. Only the integer attempt
	// number is injected — no prior error text (which could carry leaked context).
	prompt := task.Prompt
	if task.AttemptCount > 0 {
		prompt = fmt.Sprintf(
			"[retry] This is attempt %d of a previously-failed run. Before repeating any external "+
				"side-effect (sending email, payments, creating/mutating records), VERIFY it was not "+
				"already performed by an earlier attempt; do not duplicate it.\n\n%s",
			task.AttemptCount+1, task.Prompt)
	}
	// Owner-visible degradation notice (#443): a headless run can't re-prompt the
	// user to log in, so a connected remote MCP server whose token needs re-auth is
	// skipped. Surface it in the run transcript so the owner sees the task did less
	// than expected, and tell the agent so it doesn't silently rely on missing tools.
	if remoteOverlay != nil && len(remoteOverlay.Skipped) > 0 {
		log.Printf("scheduled task %s: skipped remote MCP server(s) needing re-auth: %v", task.ID, remoteOverlay.Skipped)
		prompt = fmt.Sprintf(
			"[notice] These remote MCP connectors were unavailable this run because their "+
				"login expired (the task owner must reconnect them in Settings → Connections): %s. "+
				"Proceed without them; if the task depends on one, say so in your result rather than "+
				"guessing.\n\n%s",
			strings.Join(remoteOverlay.Skipped, ", "), prompt)
	}
	// Loop context (#179): a prior iteration's output is fed forward so the worker
	// can improve on it. Empty on the first / only pass.
	if strings.TrimSpace(extraPrompt) != "" {
		prompt = fmt.Sprintf(
			"%s\n\n---\nA previous attempt did NOT pass verification. Its output follows; "+
				"diagnose why it failed and produce a corrected result:\n---\n%s",
			prompt, extraPrompt)
	}
	runErr := a.Execute(ctx, prompt)
	session := convertLogSession(task, a.LogSession())
	if runErr != nil {
		return session, false, "", runErr
	}
	if lc == nil {
		// One-shot task: no exit condition to evaluate. "passed" is unused.
		return session, true, "", nil
	}
	// Evaluate the loop exit condition while the sandbox is still live (the
	// shell: form runs a command in it). model/fallback back the llm: form.
	passed, result := r.evaluateExitCondition(ctx, lc, sb, session, fallback)
	return session, passed, result, nil
}

// taskCredentialAllowlist converts the task's persisted credential allowlist
// (#184) into the agentcore form the run loop's Gate-3 enforces. nil → nil
// (inherit global); the nil-vs-empty distinction is preserved so an empty list
// still denies all MCP calls.
func taskCredentialAllowlist(task *models.Task) agentcore.CredentialAllowlist {
	if task.CredentialAllowlist == nil {
		return nil
	}
	al := make(agentcore.CredentialAllowlist, 0, len(task.CredentialAllowlist))
	for _, e := range task.CredentialAllowlist {
		al = append(al, agentcore.CredentialAllowlistEntry{Server: e.Server, Account: e.Account})
	}
	return al
}

// buildTaskRemoteOverlay resolves the task owner's email (creator UUID →
// chat-side email) and builds a per-user remote-MCP overlay (#443) for the run.
// Returns nil (a no-op overlay) when the feature is off, the owner can't be
// resolved, or no server is connected — all best-effort, never fatal.
func (r *Runner) buildTaskRemoteOverlay(ctx context.Context, task *models.Task, base *mcp.Client) *agent.RemoteMCPOverlay {
	if r.remoteMCP == nil || r.ownerEmail == nil || task.CreatedBy == nil {
		return nil
	}
	email, err := r.ownerEmail(ctx, *task.CreatedBy)
	if err != nil {
		log.Printf("scheduled task %s: cannot resolve owner email for remote MCP: %v", task.ID, err)
		return nil
	}
	if email == "" {
		return nil
	}
	shadowed := make(map[string]bool)
	for _, st := range base.GetAllTools() {
		shadowed[st.ServerName] = true
	}
	// Scheduled runs have no interactive Tools picker, so all of the owner's
	// connected servers participate (nil opt-in set), bounded by the overlay cap.
	overlay, err := agent.BuildRemoteMCPOverlay(ctx, r.remoteMCP, email, shadowed, nil)
	if err != nil {
		log.Printf("scheduled task %s: remote-mcp overlay unavailable: %v", task.ID, err)
		return nil
	}
	if overlay.Active() {
		log.Printf("scheduled task %s: wired %d remote MCP server(s) for %s", task.ID, len(overlay.Servers), email)
	}
	return overlay
}

// bindTaskMCP resolves the MCP client the scheduled run should use.
//
//   - Empty selection → the shared process-wide client (default seat), no-op
//     cleanup. This preserves the load-on-demand path (mcp_load_servers) for
//     tasks that don't pre-declare servers.
//   - Non-empty selection → a DEDICATED per-run client onto which the task's
//     {server, account} choices are bound via agentcore.BindMCPSelection. Named
//     accounts spawn <server>_<account> subprocesses whose env carries the
//     <VAR>_<ACCOUNT> overlay (creds.ApplyClientSuffix) on cmd.Env only. The
//     cleanup Closes those subprocesses at run end so credentials never leak
//     across runs or into a concurrent task's client. A named account with no
//     matching <VAR>_<ACCOUNT> creds is REFUSED by BindMCPSelection rather than
//     silently inheriting the default seat.
func (r *Runner) bindTaskMCP(ctx context.Context, task *models.Task) (*mcp.Client, func(), error) {
	noop := func() {}
	if len(task.MCPSelection) == 0 {
		return r.mgr.MCPClient(), noop, nil
	}

	selection := make(agentcore.MCPSelection, 0, len(task.MCPSelection))
	for _, c := range task.MCPSelection {
		selection = append(selection, agentcore.MCPChoice{Server: c.Server, Account: c.Account})
	}

	client := mcp.NewClient()
	cleanup := func() {
		if err := client.Close(); err != nil {
			log.Printf("scheduled task %s: error closing per-run MCP client: %v", task.ID, err)
		}
	}

	registered, err := agentcore.BindMCPSelection(ctx, client, selection, r.mcpBases())
	if err != nil {
		cleanup() // reap any subprocesses bound before the failure
		return nil, noop, fmt.Errorf("bind task mcp selection: %w", err)
	}
	// Inline http_tools (issue #261) are global manifest tools with no per-task
	// selection (like a non-optional server), so register them on this per-run
	// client too — otherwise a task that pins an MCP selection would silently lose
	// them. Same host-side credential path as the interactive Manager / broker.
	if r.cfg != nil {
		agent.RegisterHTTPTools(client, r.cfg.HTTPTools)
	}
	log.Printf("scheduled task %s: bound %d MCP server(s) on per-run client: %v", task.ID, len(registered), registered)
	return client, cleanup, nil
}

// mcpBases maps each configured server name to the spawn spec + base env the
// binder needs. Account overlays are applied by agentcore.BindMCPSelection via
// creds.ApplyClientSuffix; this only supplies the default-seat env. Mirrors the
// interactive agent's mcpBases so both paths resolve identical specs.
func (r *Runner) mcpBases() map[string]agentcore.MCPServerBase {
	bases := map[string]agentcore.MCPServerBase{}
	if r.cfg == nil {
		return bases
	}
	for name, sc := range r.cfg.MCPServers {
		base := agentcore.MCPServerBase{
			BaseEnv:     sc.Env,
			Command:     sc.Command,
			Args:        sc.Args,
			Dir:         sc.Dir,
			HTTPHeaders: sc.Headers,
		}
		if sc.Type == "http" {
			base.HTTPURL = sc.URL
		}
		bases[name] = base
	}
	return bases
}

// convertLogSession maps the agentcore session log to the sched models log shape
// the orchestrator persists + renders. Secrets are scrubbed defensively.
func convertLogSession(_ *models.Task, ls *agent.LogSession) *models.LogSession {
	if ls == nil {
		return nil
	}
	msgs := ls.SnapshotMessages()
	out := &models.LogSession{
		ID:                  ls.ID,
		Title:               ls.Title,
		PromptTokens:        ls.PromptTokens,
		CompletionTokens:    ls.CompletionTokens,
		CachedTokens:        ls.CachedTokens,
		CacheCreationTokens: ls.CacheCreationTokens,
		Cost:                ls.Cost,
		CreatedAt:           ls.CreatedAt,
		UpdatedAt:           ls.UpdatedAt,
		Messages:            make([]models.LogMessage, 0, len(msgs)),
	}
	for _, m := range msgs {
		mm := models.LogMessage{
			ID:          m.ID,
			Role:        m.Role,
			Content:     agentcore.RedactSecrets(m.Content),
			Reasoning:   agentcore.RedactSecrets(m.Reasoning),
			Model:       m.Model,
			Provider:    m.Provider,
			CreatedAt:   m.CreatedAt,
			FinishedAt:  m.FinishedAt,
			MessageType: m.MessageType,
			ToolCallID:  m.ToolCallID,
		}
		for _, tc := range m.ToolCalls {
			mm.ToolCalls = append(mm.ToolCalls, models.LogToolCall{
				ID:        tc.ID,
				Name:      tc.Name,
				Arguments: agentcore.RedactSecrets(tc.Arguments),
			})
		}
		out.Messages = append(out.Messages, mm)
	}
	return out
}

// BuildMCPSpecs converts config.MCPServers into the agent.MCPServerSpec map the
// interactive Manager connects at construction. Shared by cmd/fleet (the
// interactive engine) and cmd/cutlass (the one-shot harness) so all callers
// resolve identical MCP specs.
func BuildMCPSpecs(cfg *config.Config) map[string]agent.MCPServerSpec {
	out := make(map[string]agent.MCPServerSpec, len(cfg.MCPServers))
	for name, sc := range cfg.MCPServers {
		out[name] = agent.MCPServerSpec{
			Enabled:       sc.Enabled,
			Command:       sc.Command,
			Args:          sc.Args,
			Env:           sc.Env,
			Dir:           sc.Dir,
			URL:           sc.URL,
			Headers:       sc.Headers,
			ToolAllowlist: sc.ToolAllowlist,
			AccountVars:   sc.AccountVars,
			// Optional-server metadata — without these the chat Manager's
			// optional-set is empty, Gate-1 never skips, and every connector's
			// tools register on every turn (the 128-tool ceiling overflow).
			Optional:         sc.Optional,
			DisplayName:      sc.DisplayName,
			Description:      sc.Description,
			Beta:             sc.Beta,
			EnabledByDefault: sc.EnabledByDefault,
		}
	}
	return out
}
