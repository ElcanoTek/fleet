package acpingress

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// stagedToolRunner resolves an APPROVED staged tool one-shot, outside the agent
// loop, through the SAME governed primitives the web approval handler uses
// (internal/httpapi/approvals.go: runStagedTool / runStagedBash). It is the
// reusable execution kernel behind the ingress approval surface, so an approved
// critical action over ACP runs through the identical path it would on the web —
// NOT a second, weaker one:
//
//   - native bash → the warm sandbox pool (same container boundary, hard-blocked
//     patterns still rejected inside RunBashForApproval);
//   - MCP tools (send_email and friends) → the prefixed, host-credentialed MCP
//     client (creds stay host-side, never on the wire);
//   - preview_email → a no-op dismissal (preview has no send path).
//
// It cannot import the agent package's private helpers (store imports agent, so
// agent cannot import store — the import-cycle reason the web copy lives in
// httpapi), so it is implemented here over the narrow Toolbox seam.
type stagedToolRunner struct {
	tb Toolbox
}

var _ StagedToolRunner = (*stagedToolRunner)(nil)

// NewStagedToolRunner builds the runner over a Toolbox (production: *agent.Manager).
func NewStagedToolRunner(tb Toolbox) StagedToolRunner {
	return &stagedToolRunner{tb: tb}
}

// RunStagedTool executes the approved tool described by the approval row and
// returns the flattened result text (or an execution error). Mirrors the web
// approval handler so both surfaces resolve an approval identically.
func (r *stagedToolRunner) RunStagedTool(ctx context.Context, approval *store.Approval) (string, error) {
	if approval == nil {
		return "", errors.New("staged tool: nil approval")
	}
	if approval.ToolName == "bash" {
		return r.runBash(ctx, approval)
	}
	if approval.ToolName == "preview_email" {
		return "Preview dismissed by user. No email was sent.", nil
	}
	client := r.tb.MCPClient()
	if client == nil {
		return "", errors.New("MCP client not initialized (mock mode?)")
	}
	// Internal naming is mcp_<server>_<tool>; route by the full prefixed name so
	// the call lands on the server that staged it (bare names collide across
	// servers — sendgrid and mailbux both export send_email).
	if !strings.HasPrefix(approval.ToolName, "mcp_") {
		return "", fmt.Errorf("unsupported tool for approval: %s", approval.ToolName)
	}
	var args map[string]any
	if err := json.Unmarshal([]byte(approval.ArgsJSON), &args); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	result, err := client.CallToolPrefixed(ctx, approval.ToolName, args)
	if err != nil {
		return "", err
	}
	var sb strings.Builder
	for _, block := range result.Content {
		if block.Type == "text" {
			sb.WriteString(block.Text)
			sb.WriteString("\n")
		}
	}
	return strings.TrimSpace(sb.String()), nil
}

// runBash executes an approved native bash command through a warm sandbox so it
// crosses the SAME container boundary an agent-driven call would. A nil pool is
// a boot misconfiguration, surfaced rather than silently dropping to host exec.
func (r *stagedToolRunner) runBash(ctx context.Context, approval *store.Approval) (string, error) {
	var params tools.BashParams
	if err := json.Unmarshal([]byte(approval.ArgsJSON), &params); err != nil {
		return "", fmt.Errorf("parse bash args: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	pool := r.tb.SandboxPool()
	if pool == nil {
		return "", fmt.Errorf("staged bash: no sandbox pool wired (boot misconfigured?)")
	}
	sb, cleanup, err := pool.Take()
	if err != nil {
		return "", fmt.Errorf("take sandbox: %w", err)
	}
	defer cleanup()
	return tools.RunBashForApproval(ctx, sb, params)
}
