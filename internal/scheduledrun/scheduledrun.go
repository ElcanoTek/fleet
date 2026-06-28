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
	"strings"
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
	}
	r.baseSystemPrompt = r.buildBaseSystemPrompt()
	return r
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
	if task.AllowNetwork {
		return pool.Take()
	}
	sb, cleanup, err := pool.TakeContainer(ctx)
	if errors.Is(err, sandbox.ErrContainerUnavailable) {
		return pool.Take()
	}
	return sb, cleanup, err
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

	// Per-task persona override (#221): a task may name a personas/<name>.yaml to
	// swap in specialized domain expertise; empty uses the runner's global persona.
	taskSystemPrompt, taskPersona := r.taskPromptAndPersona(task)

	a := agent.NewAgent(agent.Options{
		Config:              r.cfg,
		Model:               model,
		FallbackModel:       fallback,
		MCPClient:           mcpClient,
		NativeTools:         turnTools.Tools,
		SystemPrompt:        taskSystemPrompt,
		Persona:             taskPersona,
		MaxIterations:       maxIter,
		Sandbox:             sb,
		NotesProvider:       r.notesProvider,
		NoteProposer:        r.noteProposer,
		CredentialAllowlist: taskCredentialAllowlist(task),
		PhoneAFriendEnabled: r.cfg.PhoneAFriendEnabled,
		ReviewerModel:       reviewer,
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
		}
	}
	return out
}
