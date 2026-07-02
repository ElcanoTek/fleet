// Package taskrun is fleet's local one-shot harness: it runs a SINGLE task
// YAML to completion in an isolated workspace through the SAME governed
// scheduled runtime the production scheduler uses — with no orchestrator
// round-trip, no HTTP server, no scheduler ticker, and no database.
//
// It exists so an operator can iterate on a task/persona/MCP setup locally and
// watch it run end to end (`fleet task run <task.yaml>`). The execution is NOT
// a debug shortcut: it builds the interactive Manager (model resolver + sandbox
// warm pool) exactly as the fleet server does, then drives
// internal/scheduledrun — which calls agentcore.Run (Mode=Scheduled), the
// single governed core (policy, cost/token ceilings, audit, the finish
// verifier). Tool calls still execute inside the rootless-Podman sandbox; MCP
// credentials are still brokered host-side. Only the orchestrator's
// persistence/dispatch is swapped for a CLI front-end.
//
// The task YAML is a thin mirror of the scheduled-task create shape:
//
//	prompt: "Summarize today's news and email it to me."
//	model: anthropic/claude-opus-4.8              # optional; else CUTLASS_TASK_MODEL/config
//	fallback_model: anthropic/claude-sonnet-4-6   # optional
//	max_iterations: 20                            # optional
//	mcp_selection:                                # optional
//	  - server: sendgrid
//	    account: client_a
//
// On exit it writes the run's session log (the full transcript + token/cost
// accounting, secrets redacted) as JSON to --log (default
// <workspace>/session.json).
package taskrun

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/goccy/go-yaml"
	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/clientconfig"
	"github.com/ElcanoTek/fleet/internal/config"
	"github.com/ElcanoTek/fleet/internal/sandbox"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/scheduledrun"
)

// Run executes one task YAML to completion and returns the process exit code.
// progName labels messages/usage ("fleet task run" from the unified binary;
// "cutlass" from the deprecated shim).
func Run(argv []string, progName string) int {
	if err := run(argv, progName); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", progName, err)
		return 1
	}
	return 0
}

// taskYAML is the on-disk task schema cutlass parses — a thin YAML mirror of the
// fields models.TaskCreate carries that matter for a one-shot local run.
type taskYAML struct {
	Prompt        string          `yaml:"prompt"`
	Model         string          `yaml:"model"`
	FallbackModel string          `yaml:"fallback_model"`
	MaxIterations *int            `yaml:"max_iterations"`
	MCPSelection  []mcpChoiceYAML `yaml:"mcp_selection"`
}

type mcpChoiceYAML struct {
	Server  string `yaml:"server"`
	Account string `yaml:"account"`
}

func run(argv []string, progName string) error {
	fs := flag.NewFlagSet(progName, flag.ContinueOnError)
	logPath := fs.String("log", "", "path to write the JSON session log (default <workspace>/session.json)")
	workspace := fs.String("workspace", "", "workspace dir for this run (default: a fresh per-run dir)")
	if err := fs.Parse(argv); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("usage: %s [--log FILE] [--workspace DIR] <task.yaml>", progName)
	}
	taskFile := fs.Arg(0)

	task, err := loadTaskYAML(taskFile)
	if err != nil {
		return err
	}

	// Load the client bundle FIRST: it supplies the MCP catalog, the
	// supporting-doc dirs, branding, and the connector env-var names config.Load
	// must admit — exactly as cmd/fleet does at boot.
	bundle, err := clientconfig.Load(clientconfig.Dir())
	if err != nil {
		return fmt.Errorf("load client config bundle: %w", err)
	}
	config.RegisterAllowedEnvVars(bundle.EnvVarNames()...)

	cfg, err := config.Load(os.Getenv("FLEET_ENV_FILE"))
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	cfg.MCPServers = bundle.MCPServerConfigs()
	cfg.HTTPTools = bundle.HTTPToolConfigs()
	if strings.TrimSpace(cfg.SandboxImage) == "" {
		if ref := bundle.Sandbox().ResolvedImageRef(); ref != "" {
			cfg.SandboxImage = ref
		}
	}
	// Honor the bundle's sandbox.runtime (#217) with the same precedence the fleet
	// boot path uses — env FLEET_SANDBOX_RUNTIME wins, else the manifest — so a
	// kata/libkrun bundle gets the fail-closed KVM preflight under cutlass too.
	// buildSandboxPool (via agent.New below) runs PreflightRuntime on this value.
	cfg.SandboxRuntime = sandbox.ResolveRuntime(cfg.SandboxRuntime, bundle.Sandbox().Runtime)

	// Install the bundle's agent tool-behavior policy before any turn runs.
	bp := bundle.AgentPolicy()
	agentcore.ConfigureAgentPolicy(agentcore.AgentPolicy{
		ParallelSafeTools:       bp.ParallelSafeTools,
		CriticalToolSuffixes:    bp.CriticalToolSuffixes,
		CriticalToolSubstitutes: bp.CriticalToolSubstitutes,
	})

	// Install the bundle's custom model-pricing overrides (#297) before any turn
	// runs. The generic bundle ships none, so cost accounting stays on the
	// OpenRouter-returned price.
	agentcore.ConfigurePricing(toAgentcorePricing(bundle.Pricing()))

	// Fresh, isolated workspace for this run so it never collides with the
	// server's shared workspace/ — set BEFORE agent.New so the sandbox pool
	// bind-mounts the isolated root.
	wsDir, err := resolveWorkspace(*workspace, cfg)
	if err != nil {
		return err
	}
	cfg.WorkspaceRoot = wsDir
	fmt.Fprintf(os.Stderr, "%s: workspace=%s\n", progName, wsDir)

	mgr, err := agent.New(agent.ManagerOptions{
		Config:               cfg,
		ServerSpecs:          scheduledrun.BuildMCPSpecs(cfg),
		PersonasDir:          bundle.PersonasDir,
		ProtocolsDir:         bundle.ProtocolsDir,
		SkillsDir:            bundle.SkillsDir,
		SystemPromptsDir:     bundle.SystemPromptsDir,
		ChatSystemPromptFile: "chat.md",
		// No NotesProvider/NoteProposer: the one-shot harness has no sched DB, so
		// admin notes + propose_note are simply unavailable (honest: the tool
		// reports UNAVAILABLE rather than silently dropping).
	})
	if err != nil {
		return fmt.Errorf("build agent runtime: %w", err)
	}
	defer mgr.Close()

	// This is a LOCAL debug harness that drives the governed in-process native
	// loop (agentcore.Run, Mode=Scheduled) — the same and only runtime fleet ships.
	runner := scheduledrun.New(scheduledrun.Options{
		Config:           cfg,
		Manager:          mgr,
		PersonasDir:      bundle.PersonasDir,
		SystemPromptsDir: bundle.SystemPromptsDir,
		ProtocolsDir:     bundle.ProtocolsDir,
	})

	ctx := context.Background()
	session, runErr := runner.Run(ctx, task)

	// Always write whatever session we got (a failed run still has a useful
	// partial transcript), then surface the run error.
	out := *logPath
	if out == "" {
		out = filepath.Join(wsDir, "session.json")
	}
	if werr := writeSessionLog(out, session); werr != nil {
		fmt.Fprintf(os.Stderr, "%s: warning: write session log: %v\n", progName, werr)
	} else {
		fmt.Fprintf(os.Stderr, "%s: session log → %s\n", progName, out)
	}
	if runErr != nil {
		return fmt.Errorf("run task: %w", runErr)
	}
	return nil
}

// loadTaskYAML parses the task file into a models.Task ready for the runner.
func loadTaskYAML(path string) (*models.Task, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: operator-supplied task file path — reading it is the whole point of the CLI.
	if err != nil {
		return nil, fmt.Errorf("read task file: %w", err)
	}
	var y taskYAML
	if err := yaml.Unmarshal(raw, &y); err != nil {
		return nil, fmt.Errorf("parse task YAML: %w", err)
	}
	if strings.TrimSpace(y.Prompt) == "" {
		return nil, fmt.Errorf("task YAML: prompt is required")
	}
	task := &models.Task{
		ID:        uuid.New(),
		Prompt:    y.Prompt,
		Status:    models.TaskStatusRunning,
		CreatedAt: time.Now().UTC(),
	}
	if s := strings.TrimSpace(y.Model); s != "" {
		task.Model = &s
	}
	if s := strings.TrimSpace(y.FallbackModel); s != "" {
		task.FallbackModel = &s
	}
	if y.MaxIterations != nil {
		task.MaxIterations = y.MaxIterations
	}
	for _, c := range y.MCPSelection {
		if strings.TrimSpace(c.Server) == "" {
			return nil, fmt.Errorf("task YAML: mcp_selection entry has no server")
		}
		task.MCPSelection = append(task.MCPSelection, models.MCPChoice{Server: c.Server, Account: c.Account})
	}
	return task, nil
}

// resolveWorkspace returns the workspace dir for this run. An explicit
// --workspace is used as-is (created if needed); otherwise a fresh per-run dir is
// minted under cfg.WorkspaceRoot, else cfg.DataDir, else the OS temp dir.
func resolveWorkspace(override string, cfg *config.Config) (string, error) {
	if strings.TrimSpace(override) != "" {
		if err := os.MkdirAll(override, 0o750); err != nil {
			return "", fmt.Errorf("create workspace %s: %w", override, err)
		}
		abs, err := filepath.Abs(override)
		if err != nil {
			return "", err
		}
		return abs, nil
	}
	base := firstNonEmpty(cfg.WorkspaceRoot, cfg.DataDir, os.TempDir())
	//nolint:gosec // G703: base is operator config (FLEET_WORKSPACE_ROOT / DataDir / temp), not request/LLM input.
	if err := os.MkdirAll(base, 0o750); err != nil {
		return "", fmt.Errorf("create workspace base %s: %w", base, err)
	}
	dir, err := os.MkdirTemp(base, "task-run-")
	if err != nil {
		return "", fmt.Errorf("create per-run workspace: %w", err)
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	return abs, nil
}

// writeSessionLog writes the session log as indented JSON. A nil session (the
// run produced no log) writes an explicit null so the file always exists.
func writeSessionLog(path string, session *models.LogSession) error {
	data, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o600)
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// toAgentcorePricing translates the bundle's pricing config into the
// agentcore.PricingConfig the accounting path consumes (#297). Mirrors the
// fleet-server helper; the harness installs the same overrides so a one-shot run
// accounts cost (and enforces its ceiling) at the operator's negotiated rates.
func toAgentcorePricing(p clientconfig.PricingConfig) agentcore.PricingConfig {
	out := agentcore.PricingConfig{Fallback: agentcore.PricingFallback(p.Fallback)}
	if len(p.Overrides) > 0 {
		out.Overrides = make([]agentcore.PricingOverride, 0, len(p.Overrides))
		for _, o := range p.Overrides {
			out.Overrides = append(out.Overrides, agentcore.PricingOverride{
				Model:                          o.Model,
				InputCostPerMillionTokens:      o.InputCostPerMillionTokens,
				OutputCostPerMillionTokens:     o.OutputCostPerMillionTokens,
				CacheReadCostPerMillionTokens:  o.CacheReadCostPerMillionTokens,
				CacheWriteCostPerMillionTokens: o.CacheWriteCostPerMillionTokens,
			})
		}
	}
	return out
}
