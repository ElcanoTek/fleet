// Package scheduledrun is the shared, governed scheduled-task driver. It builds
// an agent.Agent over an interactive Manager's model resolver + sandbox warm pool
// and runs ONE task to completion through agentcore.Run (Mode=Scheduled) — the
// single governed core (policy, cost/token ceilings, audit, the finish verifier)
// every fleet entrypoint shares.
//
// Two callers drive it: cmd/fleet's capped worker pool (the production scheduler)
// and `fleet task run`'s local one-shot harness. Both reach the SAME governed loop, so
// the harness is not a second, weaker execution path — it is the production
// driver with a CLI front-end instead of the orchestrator round-trip. This is why
// the logic lives here, in a shared internal package, rather than being copied:
// the "governance is one core" invariant (AGENTS.md) forbids a divergent fork.
package scheduledrun

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
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
	"github.com/ElcanoTek/fleet/internal/structuredoutput"
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

	// LearnedInstructions resolves a task's active distilled instruction (#516)
	// for run-start injection. nil = feature off (unchanged behavior).
	LearnedInstructions LearnedInstructionProvider

	// RemoteMCP resolves a task owner's OAuth-connected remote (hosted) MCP servers
	// and mints their bearer tokens, so a scheduled (headless) run can use the same
	// per-user servers a chat turn would (#443). nil = feature off. OwnerEmail maps
	// the task's creator UUID to the chat-side email the tokens are keyed by; nil
	// disables remote-MCP wiring for scheduled runs even when RemoteMCP is set.
	RemoteMCP  agent.RemoteMCPResolver
	OwnerEmail func(ctx context.Context, userID uuid.UUID) (string, error)
	// OpenRemoteMCPOverlay creates the task owner's remote-server overlay behind
	// an injected broker boundary. It takes precedence over RemoteMCP and lets
	// production keep token minting and remote clients out of this process.
	OpenRemoteMCPOverlay agent.RemoteMCPOverlayOpener

	// UserSkills returns the owner's ACTIVE builder skills for prompt
	// inlining; SkillProposerFor binds propose_skill staging to the owner
	// (docs/SKILLS.md). nil = capability off.
	UserSkills       func(ctx context.Context, email string) ([]UserSkillDoc, error)
	SkillProposerFor func(ownerEmail string) agentcore.SkillProposer

	// OpenTaskMCPScope creates one broker-owned MCP client for a scheduled run.
	// Nil preserves the in-process compatibility binder.
	OpenTaskMCPScope TaskMCPScopeOpener
	// MCPServerInventory returns the live public bundle-server inventory used to
	// expand an empty task selection and decide whether to mint a workspace. It
	// carries names and booleans only, so broker mode need not retain cfg secrets.
	MCPServerInventory TaskMCPServerInventoryProvider
}

// TaskMCPScopeOpener binds public server/account choices plus task/workspace
// identity inside the credential-owning process. Credential values never cross
// this seam.
type TaskMCPScopeOpener func(ctx context.Context, selection agentcore.MCPSelection, policy agent.MCPScopePolicy, taskID, workspace string) (*agent.MCPScope, error)

// TaskMCPServerInfo is the public per-server state scheduled scope construction
// needs. Credential-bearing env, headers, URLs, and commands are absent.
type TaskMCPServerInfo struct {
	UsesWorkspace bool
	// ToolAllowlist is the server's Gate-2 tool allowlist (empty = every
	// advertised tool). Carried here because broker mode scrubs the parent's
	// config.MCPServers after boot, leaving this inventory as the only public
	// copy a scheduled run can gate tool registration with.
	ToolAllowlist []string
}

// TaskMCPServerInventoryProvider returns a concurrency-safe snapshot of the
// currently enabled public bundle-server inventory.
type TaskMCPServerInventoryProvider func() map[string]TaskMCPServerInfo

// Runner executes claimed scheduled tasks in-process through the unified runtime
// (Mode=Scheduled). It reuses the model resolver + sandbox warm pool held on the
// interactive Manager — the SAME sandbox boundary interactive turns use.
//
// Per-task MCP credential-account isolation: an injected OpenTaskMCPScope gives
// every run a broker-owned client selected by public server/account names and
// task/workspace identity. The compatibility path retains the existing local
// behavior: an explicit selection gets a dedicated client bound via
// agentcore.BindMCPSelection, while an empty selection reuses the shared client.
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
	taskMemory          tools.TaskMemoryStore
	taskMemoryConfig    tools.TaskMemoryConfig
	learnedInstructions LearnedInstructionProvider

	// remoteMCP + ownerEmail wire a task owner's OAuth-connected remote (hosted)
	// MCP servers into a headless run (#443). nil = feature off.
	remoteMCP            agent.RemoteMCPResolver
	ownerEmail           func(ctx context.Context, userID uuid.UUID) (string, error)
	openRemoteMCPOverlay agent.RemoteMCPOverlayOpener

	// userSkills + skillProposerFor extend the same owner-resolution to the
	// skills arc (docs/SKILLS.md): userSkills returns the owner's ACTIVE
	// builder skills (inlined into the run's system prompt — a headless run has
	// no per-conversation workspace to materialize files into, and writing
	// per-user files into the SHARED workspace root would leak them across
	// users), and skillProposerFor binds propose_skill staging to the owner.
	// nil = capability off.
	userSkills       func(ctx context.Context, email string) ([]UserSkillDoc, error)
	skillProposerFor func(ownerEmail string) agentcore.SkillProposer

	openTaskMCPScope   TaskMCPScopeOpener
	mcpServerInventory TaskMCPServerInventoryProvider
}

// UserSkillDoc is one user-authored skill in the shape the scheduled prompt
// inlines (the runner is decoupled from the chat store's row type).
type UserSkillDoc struct {
	Name        string
	Description string
	Body        string
}

// IterationStore records per-iteration telemetry for a looped task (#179). It is
// the narrow subset of sched storage the loop runner needs; *storage.Storage
// satisfies it. nil = telemetry disabled (the loop still runs).
type IterationStore interface {
	AddTaskIteration(ctx context.Context, it *models.TaskIteration) error
}

// LearnedInstructionProvider is the narrow subset of sched storage the runner
// needs to inject a task's active learned instruction (#516). *storage.Storage
// satisfies it. nil = feature off.
type LearnedInstructionProvider interface {
	ActiveLearnedInstruction(ctx context.Context, taskID uuid.UUID) (*models.TaskLearnedInstruction, error)
}

// New builds a Runner. The base system prompt + persona are read once at
// construction (operators editing them in place take effect on the next process
// restart, matching the scheduled path's prior behaviour).
func New(opts Options) *Runner {
	r := &Runner{
		cfg:                  opts.Config,
		mgr:                  opts.Manager,
		notesProvider:        opts.NotesProvider,
		noteProposer:         opts.NoteProposer,
		personasDir:          opts.PersonasDir,
		systemPromptsDir:     opts.SystemPromptsDir,
		protocolsDir:         opts.ProtocolsDir,
		iterationStore:       opts.IterationStore,
		learnedInstructions:  opts.LearnedInstructions,
		taskEnqueuer:         opts.TaskEnqueuer,
		personaPolicies:      opts.PersonaPolicies,
		taskMemory:           opts.TaskMemory,
		taskMemoryConfig:     opts.TaskMemoryConfig,
		remoteMCP:            opts.RemoteMCP,
		ownerEmail:           opts.OwnerEmail,
		openRemoteMCPOverlay: opts.OpenRemoteMCPOverlay,
		userSkills:           opts.UserSkills,
		skillProposerFor:     opts.SkillProposerFor,
		openTaskMCPScope:     opts.OpenTaskMCPScope,
		mcpServerInventory:   opts.MCPServerInventory,
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
// activeLearnedInstruction resolves the task's active distilled instruction
// (#516) for run-start injection. Best-effort: any error (or no provider)
// yields "" so a run never fails on the self-improvement layer.
func (r *Runner) activeLearnedInstruction(ctx context.Context, taskID uuid.UUID) string {
	if r.learnedInstructions == nil {
		return ""
	}
	li, err := r.learnedInstructions.ActiveLearnedInstruction(ctx, taskID)
	if err != nil || li == nil {
		return ""
	}
	return li.Content
}

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
			ParentBudgetUSD:  r.cfg.LiveMaxCostUSD(),
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
	// TakeContainerWithEgress cold-starts a fresh sandbox in allowlisted egress
	// mode (#211), restricting outbound HTTP(S) to allowlist via the host proxy.
	TakeContainerWithEgress(ctx context.Context, ov sandbox.ResourceOverride, allowlist []string) (*sandbox.Sandbox, func(), error)
	// EgressDefault reports the fleet-wide network mode (#211) and the allowlist
	// for allowlisted mode. ("", nil) = open/current behavior.
	EgressDefault() (mode string, allowlist []string)
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
	// Fleet-wide egress mode (#211). lockdown is a kill-switch: it seals EVERY
	// sandbox, overriding a task's AllowNetwork. allowlisted routes a networked
	// task's egress through the host proxy (best-effort). open/"" = current.
	mode, allowlist := pool.EgressDefault()
	networked := task.AllowNetwork && mode != sandbox.NetworkModeLockdown
	allowlisted := networked && mode == sandbox.NetworkModeAllowlisted

	// Per-task sandbox limits (#205) require a cold start (a warm pooled container
	// is already running with the pool's ceilings), so route through the override
	// path — applying the task's own network posture (sealed unless AllowNetwork).
	if !task.SandboxLimits.IsZero() {
		ov := sandboxOverride(task.SandboxLimits)
		if allowlisted {
			sb, cleanup, err := pool.TakeContainerWithEgress(ctx, ov, allowlist)
			if errors.Is(err, sandbox.ErrContainerUnavailable) {
				return pool.Take()
			}
			return sb, cleanup, err
		}
		sb, cleanup, err := pool.TakeContainerWithOverrides(ctx, ov, !networked)
		if errors.Is(err, sandbox.ErrContainerUnavailable) {
			// Host/mock pool: no container to size — fall back to the host take.
			return pool.Take()
		}
		return sb, cleanup, err
	}
	if networked {
		if allowlisted {
			sb, cleanup, err := pool.TakeContainerWithEgress(ctx, sandbox.ResourceOverride{}, allowlist)
			if errors.Is(err, sandbox.ErrContainerUnavailable) {
				return pool.Take()
			}
			return sb, cleanup, err
		}
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
	// Thread the same effective root into every worker pass, including
	// non-worktree tasks and loop retries. Relative file-tool recovery paths must
	// resolve to the workspace the file browser reports, not the server cwd.
	ctx = tools.WithForcedWorkingDir(ctx, effectiveWorkspace)
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

// configureRunWorkspace binds every sandbox and in-process file resolver to
// the same effective root, then installs the governed output-artifact writer.
// Keeping this as one seam prevents non-worktree scheduled runs from returning
// a relative recovery path that view_file resolves against the server cwd.
func configureRunWorkspace(ctx context.Context, sb *sandbox.Sandbox, wtPath, sharedRoot string) (context.Context, func(), string, error) {
	if sb == nil {
		return ctx, func() {}, "", fmt.Errorf("configure scheduled workspace: nil sandbox")
	}
	effectiveRoot := tools.ForcedWorkingDirFromContext(ctx)
	if effectiveRoot == "" {
		effectiveRoot = wtPath
		if effectiveRoot == "" {
			effectiveRoot = sharedRoot
		}
	}
	if abs, err := filepath.Abs(effectiveRoot); err == nil {
		effectiveRoot = abs
	} else {
		return ctx, func() {}, "", fmt.Errorf("resolve scheduled workspace root: %w", err)
	}
	ctx = tools.WithForcedWorkingDir(ctx, effectiveRoot)
	sb.SetDefaultWorkingDir(effectiveRoot)
	if err := sb.BindFileOpRoot(ctx, effectiveRoot); err != nil {
		return ctx, func() {}, "", fmt.Errorf("bind scheduled file capability: %w", err)
	}
	// A non-worktree task shares effectiveRoot with other scheduled tasks. Fixed
	// artifact paths there could be replaced by another live run and leak its
	// governed result through this run's recovery hint. Keep the hard output cap
	// but disable retention unless the task owns an isolated worktree root.
	if wtPath == "" {
		return ctx, func() {}, effectiveRoot, nil
	}
	artifactCtx, release, err := tools.WithSandboxModelOutputArtifacts(ctx, sb, effectiveRoot)
	if err != nil {
		return ctx, func() {}, "", fmt.Errorf("install governed tool-output artifact scope: %w", err)
	}
	return artifactCtx, release, effectiveRoot, nil
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
		return nil, false, "", fmt.Errorf("no model configured for scheduled task (set FLEET_TASK_MODEL on the orchestrator, or pin the task's model)")
	}
	model, providerFallbacks, err := r.mgr.ResolveWithFallbacks(ctx, modelSlug)
	if err != nil {
		return nil, false, "", fmt.Errorf("resolve scheduled model %q: %w", modelSlug, err)
	}
	var fallback fantasy.LanguageModel
	if task.FallbackModel != nil && strings.TrimSpace(*task.FallbackModel) != "" {
		if fb, ferr := r.mgr.Resolve(ctx, strings.TrimSpace(*task.FallbackModel)); ferr == nil {
			fallback = fb
		}
	} else if r.cfg.TaskFallbackModel != "" {
		if fb, ferr := r.mgr.Resolve(ctx, r.cfg.TaskFallbackModel); ferr == nil {
			fallback = fb
		}
	}
	if fallback != nil {
		providerFallbacks = nil
	}

	// "Phone a friend" super-LLM reviewer (#175). Resolved only when the feature
	// is enabled; the reviewer slug comes from FLEET_PHONE_A_FRIEND_MODEL and
	// falls back to the run's fallback model when unset. A resolution failure
	// leaves the reviewer nil, which the agent treats as "skip the review" — the
	// feature degrades gracefully rather than failing the run.
	//
	// Read the live toggle ONCE for this run's whole setup: it also feeds
	// AgentOptions.PhoneAFriendEnabled below, and two separate reads could tear
	// around a concurrent admin toggle (enabled-but-reviewerless, or a resolved
	// reviewer the options then ignore). One read = one consistent run.
	phoneAFriend := r.cfg.LivePhoneAFriendEnabled()
	var reviewer fantasy.LanguageModel
	if phoneAFriend {
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

	// Scope every scheduled run to a concrete directory inside the bind-mounted
	// workspace. Worktree-enabled runs use their exact per-run worktree (#180);
	// ordinary runs use the configured workspace root. Previously the latter
	// left relative file tools resolving against Fleet's process cwd while the
	// container used the workspace mount, breaking parity and making a narrow
	// FileOp capability impossible (#784). The same root also scopes governed,
	// sandbox-readable model-output recovery (#793).
	// Two complementary seams cover the tool surface:
	//   - Sandbox.SetDefaultWorkingDir fills the cwd of any bash/run_python call
	//     that arrives with an empty WorkingDir, so the default applies host-side.
	//   - WithForcedWorkingDir scopes the IN-PROCESS tool layer (bash/run_python/
	//     file tools), whose resolvers otherwise default an empty working dir to
	//     the process cwd before the sandbox seam can fill it.
	// Run normally threads the absolute root in context; configureRunWorkspace
	// also derives it for focused runWorker/one-shot callers that bypass Run.
	artifactCtx, releaseArtifacts, toolRoot, artifactErr := configureRunWorkspace(ctx, sb, wtPath, r.workspaceRoot())
	if artifactErr != nil {
		return nil, false, "", artifactErr
	}
	ctx = artifactCtx
	defer releaseArtifacts()
	if wtPath != "" {
		log.Printf("scheduled task %s: git worktree isolation active; tool calls scoped to %s", task.ID, toolRoot)
	}

	turnTools := tools.NewTurnTools(sb)

	// Drop the interactive staging-card tools (preview_email, schedule_task,
	// suggest_advanced_model, propose_memory). Only the interactive
	// orchestration guard gives them behavior; headless they are a tripwire
	// that FAILS the whole run on first call (a model that finished a 16-minute
	// report and called preview_email to present it dead-lettered the task).
	// A scheduled task that needs to spawn tasks opts into create_task below.
	scheduledTools := tools.ExcludeInteractiveOnly(turnTools.Tools)

	// create_task (#277) is appended ONLY for a task that opted in. See
	// maybeAppendCreateTaskTool for the gate rationale.
	nativeTools := r.maybeAppendCreateTaskTool(scheduledTools, task)

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

	// ask / notify (#510): human-in-the-loop message types, SCHEDULED-only (a
	// human is present in interactive chat). notify is registered whenever a
	// notify handler is installed; ask ONLY when an ask handler is (the runner
	// installs both on the run context). Absent handlers → the tools aren't
	// registered, so the model never sees a capability it can't use.
	if tools.NotifyHandlerInstalled(ctx) {
		nativeTools = append(nativeTools, tools.NewNotifyTool())
	}
	if tools.AskHandlerInstalled(ctx) {
		nativeTools = append(nativeTools, tools.NewAskTool())
	}

	// self-wake (docs/SELF-WAKE.md): sleep / wake_on_event park the task and
	// let the scheduler re-queue it on a deadline or a named event. Same
	// registration contract as ask: handler installed by the runner → tools
	// registered; absent → the model never sees them.
	if tools.WakeHandlerInstalled(ctx) {
		nativeTools = append(nativeTools, tools.NewSleepTool(time.Now), tools.NewWakeOnEventTool(time.Now))
	}

	// publish_artifact (#204) lets a run mark workspace files as named, downloadable
	// deliverables (a curated manifest, distinct from the raw workspace the
	// file-browser endpoints already expose). Wired ONLY when the runner installed
	// an artifact collector on the run context (the production path); tests and the
	// cutlass one-shot leave it unset, so the tool is absent and behaviour is
	// unchanged. Scheduled-only and ungated — it only records files in the run's own
	// workspace, granting no access the operator didn't already have.
	if ac := ArtifactCollectorFromContext(ctx); ac != nil {
		// The workspace root is the non-worktree run's effective workspace (the
		// dir the #287 file browser serves); a worktree run's forced working dir
		// takes precedence inside the tool.
		nativeTools = append(nativeTools, tools.NewPublishArtifactTool(ac, r.workspaceRoot()))
	}

	maxIter := r.cfg.LiveMaxIterations()
	if task.MaxIterations != nil && *task.MaxIterations > 0 {
		maxIter = *task.MaxIterations
	}

	// Wire per-task MCP credential-account isolation. Broker mode opens one
	// child-owned scope per run; the compatibility path retains its dedicated
	// local client for explicit selections and shared client for an empty one.
	mcpBinding, err := r.bindTaskMCPRuntime(ctx, task)
	if err != nil {
		return nil, false, "", err
	}
	defer mcpBinding.cleanup()

	// Per-user remote (hosted) MCP overlay (#443): wire the task owner's
	// OAuth-connected servers via the SAME composite mechanism the chat path uses,
	// so a headless run reaches them without mutating the shared/per-run client.
	// Best-effort: a server that needs re-auth or whose owner can't be resolved is
	// skipped, never failing the run.
	remoteOverlay := r.buildTaskRemoteOverlay(ctx, task, mcpBinding.discoveryCatalog())
	defer remoteOverlay.Close()

	// The task owner's builder skills + propose_skill staging (docs/SKILLS.md):
	// resolve the owner once, mirror the remote-overlay best-effort posture —
	// an unresolvable owner just runs without the capability, never fails the
	// run.
	ownerSkillEmail := r.resolveOwnerEmail(ctx, task)

	// Per-task persona override (#221): a task may name a personas/<name>.yaml to
	// swap in specialized domain expertise; empty uses the runner's global persona.
	taskSystemPrompt, taskPersona := r.taskPromptAndPersona(task)

	// Resumed after ask (#510): this run follows a human answer to a question a
	// prior run posed. Inject the Q&A so the agent continues with the answer in
	// hand; the runner clears the pending columns once the run has started.
	if strings.TrimSpace(task.PendingAnswer) != "" {
		taskSystemPrompt += "\n\n## Resumed — Human Answer\n\n" +
			"A previous run of this task paused to ask a human a question. Continue the task using their answer.\n\n" +
			"Your question was: " + strings.TrimSpace(task.PendingQuestion) + "\n\n" +
			"The human answered: " + strings.TrimSpace(task.PendingAnswer) + "\n"
	}

	// Woken after self-wake (docs/SELF-WAKE.md): this run follows a sleep /
	// wake_on_event a prior run parked on. Inject WHY it woke and the note the
	// agent left itself; the runner clears the wake columns at the terminal
	// transition (like the Q&A columns, #582) so a retried woken run still
	// sees them.
	if strings.TrimSpace(task.WakeReason) != "" {
		taskSystemPrompt += "\n\n## Woken — Continue\n\n" +
			"A previous run of this task paused itself and scheduled this wake-up. Continue the task.\n\n" +
			"Why you woke: " + strings.TrimSpace(task.WakeReason) + "\n\n" +
			"Your note to yourself was: " + strings.TrimSpace(task.WakeNote) + "\n"
	}

	// Recurring context carry (#504): when the task opted into carry_context, the
	// runner installs a BOUNDED handoff from the prior run (its final answer,
	// clamped) — inject it as a "## Previous Run" section so this run continues
	// from where the last one left off. Deterministic + cheap: no whole-transcript
	// replay, no extra LLM call.
	if prior := PriorRunContextFromContext(ctx); prior != "" {
		taskSystemPrompt += "\n\n## Previous Run\n\n" +
			"This recurring task carries context across runs. The previous run's final output was:\n\n" +
			prior + "\n\nContinue from there; do not repeat work already done unless the schedule implies a fresh pass.\n"
	}

	// Structured-output mode (#244): when the task declares an output_schema, tell
	// the agent its final answer must be JSON conforming to that schema. The
	// runner Pool validates the produced output against the same schema after the
	// run and persists the result in the task's output_json.
	taskSystemPrompt += structuredoutput.PromptAugmentation(task.OutputSchema)

	taskSystemPrompt = r.appendOwnerSkills(ctx, taskSystemPrompt, ownerSkillEmail)

	// Sub-agent delegation (#1043): compute the composed gate once — it decides
	// BOTH the tool registration (SubagentOptions.Enabled below) and this
	// system-prompt policy section, so the advertised tool and the prompt
	// teaching stay in lockstep. Both flags default true; either is a kill
	// switch (fleet-wide Admin → Features / FLEET_SUBAGENTS_ENABLED, or the
	// task's allow_delegation).
	subagentsEnabled := r.cfg.LiveSubagentsEnabled() && task.AllowDelegation
	if subagentsEnabled {
		taskSystemPrompt += agent.DelegationPromptSection()
	}

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
	learnedInstruction := r.activeLearnedInstruction(ctx, task.ID)

	a := agent.NewAgent(agent.Options{
		Config:           r.cfg,
		Model:            model,
		FallbackModel:    fallback,
		FallbackModels:   providerFallbacks,
		MCPClient:        mcpBinding.client,
		MCPBroker:        mcpBinding.broker,
		MCPCatalog:       mcpBinding.catalog,
		MCPToolAllowlist: r.taskMCPToolAllowlist(),
		NativeTools:      nativeTools,
		SystemPrompt:     taskSystemPrompt,
		Persona:          taskPersona,
		MaxIterations:    maxIter,
		Sandbox:          sb,
		NotesProvider:    r.notesProvider,
		NoteProposer:     r.noteProposer,
		SkillProposer:    r.taskSkillProposer(ownerSkillEmail),
		// Captain's Log persistent memory (#285): nil unless the task opted in (above).
		TaskMemory:          taskMemory,
		TaskID:              task.ID,
		TaskMemoryConfig:    r.taskMemoryConfig,
		LearnedInstruction:  learnedInstruction,
		CredentialAllowlist: taskCredentialAllowlist(task),
		ThinkingBudget:      task.ThinkingBudgetTokens,
		PersonaPolicy:       r.personaPolicy(taskPersona),
		OutputSchema:        task.OutputSchema,
		Overlay:             remoteOverlay,
		PhoneAFriendEnabled: phoneAFriend,
		ReviewerModel:       reviewer,
		// Governed sub-agents / delegation (#175, #264, #1043): ON by default —
		// registered whenever the fleet-wide flag AND this task's allow_delegation
		// are both true (each defaults true; each is an independent kill switch,
		// composed as AND so either can turn the tool off). Registering the tool
		// is the feature: the PARENT AGENT decides whether to actually spawn.
		// The child model is resolved HOST-SIDE through the SAME Manager resolver the
		// parent's model came from (r.mgr), so a per-child model choice keeps
		// credentials host-side — never in the sandbox or model context.
		Subagent: agent.SubagentOptions{
			Enabled:        subagentsEnabled,
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
	// Start-of-run create reconciliation (#717): if the run's resolved MCP
	// workspace holds unresolved pre-POST create markers (a prior process died
	// between submitting a create and recording its outcome), append the
	// mandatory reconciliation sweep so this run verifies before it re-creates.
	// Appended to the TASK prompt, never the cached system prefix
	// (docs/PROMPT-CACHE-CONTRACT.md).
	prompt = agentcore.AugmentTaskWithCreateReconciliation(prompt, mcpBinding.workdir)
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
func (r *Runner) buildTaskRemoteOverlay(ctx context.Context, task *models.Task, baseCatalog []mcp.ServerTool) *agent.RemoteMCPOverlay {
	if (r.openRemoteMCPOverlay == nil && r.remoteMCP == nil) || r.ownerEmail == nil || task.CreatedBy == nil {
		return nil
	}
	// A deny-all task reaches no connector, and the owner's hosted connections are
	// connectors the task's own mcp_selection never names — an operator auditing
	// the task sees nothing, so they must not be wired behind its back (#979).
	// Gate-3 would refuse every call anyway; stopping here also avoids minting the
	// owner's bearer tokens and dialing third-party servers for a run that can
	// never use them.
	if taskCredentialAllowlist(task).DeniesAll() {
		log.Printf("scheduled task %s: credential allowlist denies all MCP, skipping the owner's remote connections", task.ID)
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
	for _, st := range baseCatalog {
		shadowed[st.ServerName] = true
	}
	// Scheduled runs have no interactive Tools picker, so all of the owner's
	// connected servers participate (nil opt-in set), bounded by the overlay cap.
	var overlay *agent.RemoteMCPOverlay
	if r.openRemoteMCPOverlay != nil {
		overlay, err = r.openRemoteMCPOverlay(ctx, email, shadowed, nil)
		if err == nil {
			err = overlay.Validate()
			if err != nil {
				overlay.Close()
			}
		}
	} else {
		overlay, err = agent.BuildRemoteMCPOverlay(ctx, r.remoteMCP, email, shadowed, nil)
	}
	if err != nil {
		log.Printf("scheduled task %s: remote-mcp overlay unavailable: %v", task.ID, err)
		return nil
	}
	if overlay.Active() {
		log.Printf("scheduled task %s: wired %d remote MCP server(s) for %s", task.ID, len(overlay.Servers), email)
	}
	return overlay
}

// taskMCPBinding is the scheduled Agent's transport-neutral per-run MCP wiring.
type taskMCPBinding struct {
	client  *mcp.Client
	broker  agentcore.MCPBroker
	catalog []mcp.ServerTool
	workdir string
	cleanup func()
}

func (b taskMCPBinding) discoveryCatalog() []mcp.ServerTool {
	if b.catalog != nil {
		return b.catalog
	}
	if b.client != nil {
		return b.client.GetAllTools()
	}
	return nil
}

const taskMCPScopeCloseTimeout = 5 * time.Second

func (r *Runner) bindTaskMCPRuntime(ctx context.Context, task *models.Task) (taskMCPBinding, error) {
	// "May call no MCP server" is wired as "has no MCP server" (#979). Gate-3
	// already refuses every call at the broker seam, but an empty selection means
	// the DEPLOYMENT DEFAULT SET everywhere else in this file, so a deny-all run
	// would otherwise spawn every bundle server and advertise its whole tool
	// roster to the model just to reject each call. Both paths below therefore
	// bind an empty selection rather than the default expansion.
	denyAll := taskCredentialAllowlist(task).DeniesAll()
	if r.openTaskMCPScope == nil {
		if r.mgr != nil && r.mgr.MCPClient() == nil && r.mgr.MCPBroker() != nil {
			return taskMCPBinding{}, errors.New("scheduled MCP broker requires a task scope opener")
		}
		client, cleanup, workdir, err := r.bindTaskMCP(ctx, task, denyAll)
		if err != nil {
			return taskMCPBinding{}, err
		}
		// Keep catalog nil so agentcore re-discovers the mutable local client on
		// every MCP-dirty rebuild after mcp_load_servers. discoveryCatalog still
		// snapshots it for remote-server shadowing before the run starts.
		return taskMCPBinding{client: client, workdir: workdir, cleanup: cleanup}, nil
	}

	selection := r.taskMCPSelection(task, !denyAll)
	workdir, err := r.prepareTaskMCPWorkspace(task, selection)
	if err != nil {
		return taskMCPBinding{}, err
	}
	// The credential owner enforces this run's gates itself (#167 residual 1):
	// the Gate-2 allowlist it can also re-derive from the bundle, plus the
	// per-task Gate-3 credential pairs, which are task data it has no other
	// source for.
	policy := agent.MCPScopePolicy{
		ToolAllowlist:       r.taskMCPToolAllowlist(),
		CredentialAllowlist: taskCredentialAllowlist(task),
	}
	scope, err := r.openTaskMCPScope(ctx, selection, policy, task.ID.String(), workdir)
	if err != nil {
		return taskMCPBinding{}, fmt.Errorf("open scheduled MCP scope: %w", err)
	}
	if scope == nil || scope.Close == nil {
		return taskMCPBinding{}, errors.New("open scheduled MCP scope: opener returned an incomplete scope")
	}
	cleanup := func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), taskMCPScopeCloseTimeout)
		defer cancel()
		if closeErr := scope.Close(closeCtx); closeErr != nil {
			log.Printf("scheduled task %s: close MCP scope: %v", task.ID, closeErr)
		}
	}
	if scope.Broker == nil {
		cleanup()
		return taskMCPBinding{}, errors.New("open scheduled MCP scope: opener returned an incomplete scope")
	}
	catalog := append([]mcp.ServerTool(nil), scope.Catalog...)
	if scope.Catalog != nil && catalog == nil {
		catalog = []mcp.ServerTool{}
	}
	return taskMCPBinding{broker: scope.Broker, catalog: catalog, workdir: workdir, cleanup: cleanup}, nil
}

func (r *Runner) taskMCPSelection(task *models.Task, allWhenEmpty bool) agentcore.MCPSelection {
	selection := make(agentcore.MCPSelection, 0, len(task.MCPSelection))
	for _, choice := range task.MCPSelection {
		selection = append(selection, agentcore.MCPChoice{Server: choice.Server, Account: choice.Account})
	}
	if len(selection) > 0 || !allWhenEmpty {
		return selection
	}
	for name := range r.taskMCPServerInventory() {
		selection = append(selection, agentcore.MCPChoice{Server: name})
	}
	sort.Slice(selection, func(i, j int) bool { return selection[i].Server < selection[j].Server })
	return selection
}

func (r *Runner) prepareTaskMCPWorkspace(task *models.Task, selection agentcore.MCPSelection) (string, error) {
	if r.openTaskMCPScope != nil {
		inventory := r.taskMCPServerInventory()
		for _, choice := range selection {
			if inventory[choice.Server].UsesWorkspace {
				workdir := agentcore.PerRunMCPWorkspaceDir("task-" + task.ID.String() + "-")
				if err := r.stageTaskInputs(task, filepath.Join(workdir, "inputs")); err != nil {
					return "", fmt.Errorf("stage task inputs: %w", err)
				}
				return workdir, nil
			}
		}
		return "", nil
	}
	bases := r.mcpBases()
	for _, choice := range selection {
		if base, ok := bases[choice.Server]; ok && agentcore.EnvReferencesWorkspace(base.BaseEnv) {
			workdir := agentcore.PerRunMCPWorkspaceDir("task-" + task.ID.String() + "-")
			if err := r.stageTaskInputs(task, filepath.Join(workdir, "inputs")); err != nil {
				return "", fmt.Errorf("stage task inputs: %w", err)
			}
			return workdir, nil
		}
	}
	return "", nil
}

func (r *Runner) taskMCPServerInventory() map[string]TaskMCPServerInfo {
	if r.mcpServerInventory != nil {
		return r.mcpServerInventory()
	}
	out := map[string]TaskMCPServerInfo{}
	if r.cfg == nil {
		return out
	}
	for name, server := range r.cfg.MCPServers {
		if server.Enabled {
			out[name] = TaskMCPServerInfo{
				UsesWorkspace: agentcore.EnvReferencesWorkspace(server.Env),
				ToolAllowlist: append([]string(nil), server.ToolAllowlist...),
			}
		}
	}
	return out
}

// taskMCPToolAllowlist projects the live public inventory onto the Gate-2
// allowlist shape the scheduled agent gates tool registration with. Always
// non-nil so the agent prefers it over config.MCPServers, which broker mode
// scrubs after boot.
func (r *Runner) taskMCPToolAllowlist() agentcore.MCPAllowlist {
	allow := agentcore.MCPAllowlist{}
	for name, server := range r.taskMCPServerInventory() {
		if len(server.ToolAllowlist) > 0 {
			allow[name] = append([]string(nil), server.ToolAllowlist...)
		}
	}
	return allow
}

// bindTaskMCP resolves the local compatibility client the scheduled run should
// use. bindTaskMCPRuntime selects this path or the injected broker scope above.
//
//   - Empty selection → the shared process-wide client (default seat), no-op
//     cleanup. This preserves the load-on-demand path (mcp_load_servers) for
//     tasks that don't pre-declare servers.
//   - Non-empty selection → a DEDICATED per-run client onto which the task's
//     {server, account} choices are bound via agentcore.BindMCPSelection. Named
//     accounts spawn <server>_<account> subprocesses whose env carries the
//     <VAR>_<ACCOUNT> overlay on cmd.Env only. Cleanup closes those subprocesses
//     at run end. A missing named-account credential is refused rather than
//     silently inheriting the default seat.
//
// A denyAll task (explicit empty credential allowlist) gets a DEDICATED EMPTY
// client on either branch: no bundle server, no http_tools, no shared-client
// fallback. See bindTaskMCPRuntime.
//
// The third return is the resolved ${FLEET_WORKSPACE} directory used for this
// task's connector ledger reconciliation ("" when no selected server references
// the token).
func (r *Runner) bindTaskMCP(ctx context.Context, task *models.Task, denyAll bool) (*mcp.Client, func(), string, error) {
	noop := func() {}
	if denyAll {
		// An empty per-run client, NOT the shared one: the shared client already
		// holds every boot-loaded bundle server, and handing it over would put that
		// whole roster in the model's tool list for a run permitted to call none of
		// it. Nothing is registered here, so there is no subprocess to reap — but
		// the client is still closed for symmetry with the dedicated path below.
		client := mcp.NewClient()
		return client, func() {
			if err := client.Close(); err != nil {
				log.Printf("scheduled task %s: error closing empty per-run MCP client: %v", task.ID, err)
			}
		}, "", nil
	}
	if len(task.MCPSelection) == 0 {
		// NO reconciliation workdir on this path, deliberately. The shared
		// client's workspace-armed servers were spawned at boot against the
		// stable PER-DEPLOYMENT dir (mcp_workspace.go), so its ledger holds the
		// markers of every task AND every chat conversation on the box, keyed
		// only by (ssp, deal_name) — nothing attributes a record to a run.
		// Replaying it would inject another task's half-finished creates into
		// this task's prompt as "the prior process stopped after submitting
		// these creates", and an abandoned marker is only ever cleared by a
		// matching resolution in the same file, so it would be replayed into
		// every future run forever. An unattributable ledger is not a resume
		// signal. A task that wants real per-run resume semantics declares an
		// mcp_selection and gets the dedicated client below, whose workdir and
		// ${FLEET_TASK_ID} identify exactly one run.
		if r.taskIdentityRequested() {
			log.Printf("scheduled task %s: no mcp_selection, so its MCP servers run on the shared "+
				"per-deployment client — the bundle's per-task ledgers (create idempotency, email "+
				"send-once) are inert for this run. Declare an mcp_selection for per-run identity.",
				task.ID)
		}
		return r.mgr.MCPClient(), noop, "", nil
	}

	selection := r.taskMCPSelection(task, false)

	client := mcp.NewClient()
	cleanup := func() {
		if err := client.Close(); err != nil {
			log.Printf("scheduled task %s: error closing per-run MCP client: %v", task.ID, err)
		}
	}

	// ${FLEET_WORKSPACE} (the reserved manifest-env token): a run with its OWN
	// client gets a fresh per-run workdir — cutlass-parity managed-run semantics
	// (per-run ledger, managed-run detection). Minted lazily: a catalog that
	// never uses the token creates nothing on disk.
	bases := r.mcpBases()
	for name, base := range bases {
		base.BaseEnv = agentcore.ExpandTaskIDEnv(base.BaseEnv, task.ID.String())
		bases[name] = base
	}
	workdir, err := r.prepareTaskMCPWorkspace(task, selection)
	if err != nil {
		cleanup()
		return nil, noop, "", err
	}

	registered, err := agentcore.BindMCPSelection(ctx, client, selection, bases, workdir)
	if err != nil {
		cleanup() // reap any subprocesses bound before the failure
		return nil, noop, "", fmt.Errorf("bind task mcp selection: %w", err)
	}
	// Inline http_tools (issue #261) are global manifest tools with no per-task
	// selection (like a non-optional server), so register them on this per-run
	// client too — otherwise a task that pins an MCP selection would silently lose
	// them. Same host-side credential path as the interactive Manager / broker.
	if r.cfg != nil {
		agent.RegisterHTTPTools(client, r.cfg.HTTPTools)
	}
	log.Printf("scheduled task %s: bound %d MCP server(s) on per-run client: %v", task.ID, len(registered), registered)
	return client, cleanup, workdir, nil
}

// stageTaskInputs copies collision-safe upload objects into the dedicated MCP
// workspace under the logical names referenced by the prompt. Source paths are
// server-owned names previously validated by the task-create handler; aliases
// are single path components and pair positionally with Files.
func (r *Runner) stageTaskInputs(task *models.Task, inputDir string) error {
	if len(task.Files) == 0 {
		return nil
	}
	if len(task.FileNames) > 0 && len(task.FileNames) != len(task.Files) {
		return fmt.Errorf("file_names must pair 1:1 with files")
	}
	if err := os.MkdirAll(inputDir, 0o750); err != nil {
		return err
	}
	for i, stored := range task.Files {
		logical := stored
		if len(task.FileNames) > 0 {
			logical = task.FileNames[i]
		}
		if logical == "" || logical != filepath.Base(logical) || !filepath.IsLocal(logical) {
			return fmt.Errorf("invalid logical file name %q", logical)
		}
		srcPath := filepath.Join(r.cfg.DataDir, "temp_uploads", stored)
		src, err := os.Open(srcPath) //nolint:gosec // stored names were validated at task creation.
		if err != nil {
			return fmt.Errorf("open %s: %w", stored, err)
		}
		dstPath := filepath.Join(inputDir, logical)
		dst, err := os.OpenFile(dstPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // logical is a validated local basename.
		if err != nil {
			_ = src.Close()
			return fmt.Errorf("create %s: %w", logical, err)
		}
		_, copyErr := io.Copy(dst, src)
		closeErr := errors.Join(src.Close(), dst.Close())
		if copyErr != nil {
			return fmt.Errorf("copy %s: %w", logical, copyErr)
		}
		if closeErr != nil {
			return fmt.Errorf("close %s: %w", logical, closeErr)
		}
	}
	return nil
}

// mcpBases maps each configured server name to the spawn spec + base env the
// binder needs. Account overlays are applied by agentcore.BindMCPSelection via
// creds.ApplyClientSuffix; this only supplies the default-seat env. Mirrors the
// interactive agent's mcpBases so both paths resolve identical specs.
// taskIdentityRequested reports whether the active catalog asks for a per-task
// identity — i.e. some server's manifest env references ${FLEET_TASK_ID}. Only
// the dedicated per-run client can supply one, so this is the signal that a
// selection-less run is losing a guarantee the bundle asked for (its ledgers go
// inert) rather than one it never wanted.
func (r *Runner) taskIdentityRequested() bool {
	for _, base := range r.mcpBases() {
		if agentcore.EnvReferencesTaskID(base.BaseEnv) {
			return true
		}
	}
	return false
}

func (r *Runner) mcpBases() map[string]agentcore.MCPServerBase {
	return BuildMCPBases(r.cfg)
}

// BuildMCPBases maps the configured catalog to the credential-bearing spawn
// definitions consumed by agentcore.BindMCPSelection. It is shared with the
// out-of-process broker so scheduled in-process binding and broker scopes cannot
// drift in account overlay, identity refusal, TLS, or required-server behavior.
func BuildMCPBases(cfg *config.Config) map[string]agentcore.MCPServerBase {
	bases := map[string]agentcore.MCPServerBase{}
	if cfg == nil {
		return bases
	}
	for name, sc := range cfg.MCPServers {
		if !sc.Enabled {
			continue
		}
		base := agentcore.MCPServerBase{
			BaseEnv:     sc.Env,
			Command:     sc.Command,
			Args:        sc.Args,
			Dir:         sc.Dir,
			HTTPHeaders: sc.Headers,
			IdentityEnv: sc.IdentityEnv,
		}
		if sc.Type == "http" {
			base.HTTPURL = sc.URL
			base.HTTPTLS = sc.TLS
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
		// Redacted like message content: output_json is persisted task output
		// and must never carry raw secret material. If redaction alters the
		// bytes, the runner fails the contract loudly rather than committing
		// corrupted-but-schema-shaped output.
		OutputJSON: agentcore.RedactSecrets(ls.SnapshotOutputJSON()),
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
			ToolName:    m.ToolName,
			IsError:     m.IsError,
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
// interactive engine) and `fleet task run` (the one-shot harness) so all callers
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
			TLS:           sc.TLS,
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
			DataSources:      append([]string(nil), sc.DataSources...),
		}
	}
	return out
}

// resolveOwnerEmail maps task.CreatedBy to the chat-side owner email, "" when
// unresolvable (capability off, API-key-created task, or lookup failure) —
// mirroring the remote-overlay best-effort posture.
func (r *Runner) resolveOwnerEmail(ctx context.Context, task *models.Task) string {
	if r.ownerEmail == nil || task.CreatedBy == nil {
		return ""
	}
	email, err := r.ownerEmail(ctx, *task.CreatedBy)
	if err != nil {
		return ""
	}
	return email
}

// appendOwnerSkills inlines the owner's ACTIVE builder skills into the run's
// system prompt (docs/SKILLS.md). Scheduled runs inline the full bodies
// instead of materializing files: there is no per-conversation workspace
// here, and per-user files in the shared workspace root would be readable by
// other users' runs. The section's total budget keeps a skill-heavy user from
// blowing up every task prompt; anything dropped is dropped LOUDLY inside the
// prompt so the agent knows.
func (r *Runner) appendOwnerSkills(ctx context.Context, prompt, ownerEmail string) string {
	if r.userSkills == nil || ownerEmail == "" {
		return prompt
	}
	docs, err := r.userSkills(ctx, ownerEmail)
	if err != nil || len(docs) == 0 {
		return prompt
	}
	return prompt + renderUserSkillsSection(docs)
}

// taskSkillProposer binds propose_skill staging to the resolved task owner;
// nil when the capability is off or the owner is unknown (the tool then stays
// unregistered for the run).
func (r *Runner) taskSkillProposer(ownerEmail string) agentcore.SkillProposer {
	if r.skillProposerFor == nil || ownerEmail == "" {
		return nil
	}
	return r.skillProposerFor(ownerEmail)
}

// userSkillsInlineBudget caps the total bytes of skill bodies inlined into a
// scheduled prompt (the interactive path materializes files instead and needs
// no cap).
const userSkillsInlineBudget = 24 * 1024

// renderUserSkillsSection renders the owner's builder skills as a prompt
// section, full bodies inline, dropping (loudly) past the budget.
func renderUserSkillsSection(docs []UserSkillDoc) string {
	var b strings.Builder
	b.WriteString("\n\n## Your user's skills\n\n")
	b.WriteString("Skills this task's owner authored for their own runs. Apply one when the task matches its description; ignore the rest.\n")
	used := 0
	dropped := 0
	for _, d := range docs {
		if used+len(d.Body) > userSkillsInlineBudget {
			dropped++
			continue
		}
		used += len(d.Body)
		b.WriteString("\n### ")
		b.WriteString(d.Name)
		b.WriteString("\n")
		b.WriteString(d.Description)
		b.WriteString("\n\n")
		b.WriteString(d.Body)
		if !strings.HasSuffix(d.Body, "\n") {
			b.WriteString("\n")
		}
	}
	if dropped > 0 {
		fmt.Fprintf(&b, "\n(%d more skill(s) were omitted for space.)\n", dropped)
	}
	return b.String()
}
