package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// scheduledRunner is the runner.TaskRunner that executes a claimed scheduled
// task in-process through the unified runtime (Mode=Scheduled). It reuses the
// model resolver + sandbox warm pool (held on the interactive Manager) — the
// SAME sandbox boundary interactive turns use.
//
// Per-task MCP credential-account isolation (plan §6.3): when a task carries an
// mcp_selection with named accounts, the run gets its OWN MCP client onto which
// the selection's account-variant subprocesses are bound via
// agentcore.BindMCPSelection (which overlays <VAR>_<ACCOUNT> via
// creds.ApplyClientSuffix onto the subprocess cmd.Env — never argv, never the
// sandbox). That per-run client is Closed at run end so no credentialed
// subprocess leaks across runs or into a concurrent task's client. Tasks with no
// selection (or a default-account-only selection that binds cleanly) reuse the
// shared process-wide client; only credentialed account variants force an
// isolated per-run client.
type scheduledRunner struct {
	cfg           *config.Config
	mgr           *agent.Manager
	storage       *storage.Storage
	notesProvider agentcore.NotesProvider
	noteProposer  agentcore.NoteProposer

	personasDir      string
	systemPromptsDir string
	protocolsDir     string

	baseSystemPrompt string
}

// newScheduledRunner builds the scheduled TaskRunner. The base system prompt +
// persona are read once at boot (operators editing them in place take effect on
// the next process restart, matching the scheduled path's prior behaviour).
func newScheduledRunner(cfg *config.Config, mgr *agent.Manager, st *storage.Storage, notes *notesAdapter, personasDir, systemPromptsDir, protocolsDir string) *scheduledRunner {
	r := &scheduledRunner{
		cfg:              cfg,
		mgr:              mgr,
		storage:          st,
		notesProvider:    notes,
		noteProposer:     notes,
		personasDir:      personasDir,
		systemPromptsDir: systemPromptsDir,
		protocolsDir:     protocolsDir,
	}
	r.baseSystemPrompt = r.buildBaseSystemPrompt()
	return r
}

// buildBaseSystemPrompt composes the scheduled base prompt: the default system
// prompt + the configured persona domain expertise. Failures degrade to an
// empty/partial prompt with a log line rather than blocking the runner.
func (r *scheduledRunner) buildBaseSystemPrompt() string {
	var sb strings.Builder
	spName := r.cfg.SystemPrompt
	if spName == "" {
		spName = "default.md"
	}
	if content, err := os.ReadFile(filepath.Join(r.systemPromptsDir, filepath.Base(spName))); err == nil {
		sb.Write(content)
	}
	personaPath := r.cfg.Persona
	if personaPath == "" {
		personaPath = "assistant.yaml"
	}
	if content, err := os.ReadFile(filepath.Join(r.personasDir, filepath.Base(personaPath))); err == nil && len(content) > 0 {
		name := strings.TrimSuffix(filepath.Base(personaPath), filepath.Ext(personaPath))
		fmt.Fprintf(&sb, "\n\n---\n\n# %s Domain Expertise & Context\n\n", name)
		sb.Write(content)
	}
	return sb.String()
}

// Run executes one task to completion and returns the converted session log.
func (r *scheduledRunner) Run(ctx context.Context, task *models.Task) (*models.LogSession, error) {
	// Resolve the task's model (falls back to the configured task model).
	modelSlug := r.cfg.TaskModel
	if task.Model != nil && strings.TrimSpace(*task.Model) != "" {
		modelSlug = strings.TrimSpace(*task.Model)
	}
	if modelSlug == "" {
		return nil, fmt.Errorf("no model configured for scheduled task (set CUTLASS_TASK_MODEL or the task's model)")
	}
	model, err := r.mgr.Resolve(ctx, modelSlug)
	if err != nil {
		return nil, fmt.Errorf("resolve scheduled model %q: %w", modelSlug, err)
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

	// Take a sandbox from the shared warm pool (the SAME boundary interactive
	// turns use). Scheduled runs are not lockdown; a per-exec-burst container.
	sb, cleanup, err := r.mgr.SandboxPool().Take()
	if err != nil {
		return nil, fmt.Errorf("take sandbox: %w", err)
	}
	defer cleanup()

	turnTools := tools.NewTurnTools(sb)

	maxIter := r.cfg.MaxIterations
	if task.MaxIterations != nil && *task.MaxIterations > 0 {
		maxIter = *task.MaxIterations
	}

	// Wire per-task MCP credential-account isolation. When the task names
	// accounts, bind its account-variant subprocesses onto a DEDICATED per-run
	// client and Close them at run end (plan §6.3) so credentials never leak
	// across runs or to a concurrent task. A default-only / empty selection
	// reuses the shared process-wide client.
	mcpClient, mcpCleanup, err := r.bindTaskMCP(ctx, task)
	if err != nil {
		return nil, err
	}
	defer mcpCleanup()

	a := agent.NewAgent(agent.Options{
		Config:        r.cfg,
		Model:         model,
		FallbackModel: fallback,
		MCPClient:     mcpClient,
		NativeTools:   turnTools.Tools,
		SystemPrompt:  r.baseSystemPrompt,
		Persona:       r.cfg.Persona,
		MaxIterations: maxIter,
		Sandbox:       sb,
		NotesProvider: r.notesProvider,
		NoteProposer:  r.noteProposer,
	})

	runErr := a.Execute(ctx, task.Prompt)
	session := convertLogSession(task, a.LogSession())
	if runErr != nil {
		return session, runErr
	}
	return session, nil
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
func (r *scheduledRunner) bindTaskMCP(ctx context.Context, task *models.Task) (*mcp.Client, func(), error) {
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
func (r *scheduledRunner) mcpBases() map[string]agentcore.MCPServerBase {
	bases := map[string]agentcore.MCPServerBase{}
	if r.cfg == nil {
		return bases
	}
	for name, sc := range r.cfg.MCPServers {
		base := agentcore.MCPServerBase{
			BaseEnv:     sc.Env,
			Command:     sc.Command,
			Args:        sc.Args,
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
