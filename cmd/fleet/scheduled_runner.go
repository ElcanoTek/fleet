package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// scheduledRunner is the runner.TaskRunner that executes a claimed scheduled
// task in-process through the unified runtime (Mode=Scheduled). It reuses the
// process-wide MCP client + model resolver (held on the interactive Manager) and
// takes a sandbox from the SHARED warm pool per task — the SAME sandbox boundary
// interactive turns use. The task's mcp_selection gates which servers' tools
// register for the run (per-account credential isolation via BindMCPSelection is
// the P8 hardening step; here the shared host-side client is reused).
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
		personaPath = "victoria.yaml"
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

	a := agent.NewAgent(agent.AgentOptions{
		Config:        r.cfg,
		Model:         model,
		FallbackModel: fallback,
		MCPClient:     r.mgr.MCPClient(),
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

// convertLogSession maps the agentcore session log to the sched models log shape
// the orchestrator persists + renders. Secrets are scrubbed defensively.
func convertLogSession(task *models.Task, ls *agent.LogSession) *models.LogSession {
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
