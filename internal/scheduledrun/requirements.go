package scheduledrun

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"charm.land/fantasy"
	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/sandbox"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

const executionRequirementsMarker = "EXECUTION REQUIREMENTS (JSON):"

// Optional, copyable handoff from a prompt producer. Requirements only narrow
// execution: they never enable network, load credentials, or widen MCP scope.
type executionRequirements struct {
	Servers []string `json:"mcp_servers"`
	Tools   []string `json:"required_tools"`
	Network bool     `json:"network"`
}

var requirementName = regexp.MustCompile(`^[a-zA-Z0-9_.-]{1,200}$`)

func parseExecutionRequirements(prompt string) (*executionRequirements, error) {
	var found *executionRequirements
	lines := strings.Split(prompt, "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) != executionRequirementsMarker {
			continue
		}
		if found != nil || i+1 == len(lines) || len(lines[i+1]) > 16384 {
			return nil, fmt.Errorf("execution requirements: expected one bounded JSON object after the marker")
		}
		var req *executionRequirements
		if err := json.Unmarshal([]byte(lines[i+1]), &req); err != nil || req == nil {
			return nil, fmt.Errorf("execution requirements: invalid JSON object")
		}
		if len(req.Servers) > 100 || len(req.Tools) > 200 {
			return nil, fmt.Errorf("execution requirements: too many servers or tools")
		}
		for _, name := range append(append([]string{}, req.Servers...), req.Tools...) {
			if !requirementName.MatchString(name) {
				return nil, fmt.Errorf("execution requirements: invalid server or tool identifier")
			}
		}
		found = req
	}
	return found, nil
}

func (r *executionRequirements) checkNetwork(networked bool) error {
	if r != nil && r.Network && !networked {
		return fmt.Errorf("execution requirements: sandbox network access is required; enable Allow network egress for this task and check the administrator's egress policy")
	}
	return nil
}

func (r *executionRequirements) checkTools(catalog []mcp.ServerTool, native []fantasy.AgentTool) error {
	if r == nil {
		return nil
	}
	servers := make(map[string]bool)
	tools := make(map[string]bool)
	for _, item := range catalog {
		servers[item.ServerName] = true
		tools["mcp_"+item.ServerName+"_"+item.Tool.Name] = true
		tools[item.Tool.Name] = true
	}
	for _, tool := range native {
		tools[tool.Info().Name] = true
	}
	var missing []string
	for _, server := range r.Servers {
		if !servers[server] {
			missing = append(missing, "server "+server)
		}
	}
	for _, tool := range r.Tools {
		if !tools[tool] {
			missing = append(missing, "tool "+tool)
		}
	}
	if len(missing) != 0 {
		return fmt.Errorf("execution requirements: unavailable in the task's MCP/native tool roster: %s; check selected servers, tool permissions and connected accounts", strings.Join(missing, ", "))
	}
	return nil
}

func (r *Runner) checkTaskRequirements(task *models.Task) (*executionRequirements, error) {
	req, err := parseExecutionRequirements(task.Prompt)
	if err != nil {
		return nil, err
	}
	if err := req.checkNetwork(task.AllowNetwork); err != nil {
		return nil, err
	}
	if req != nil && req.Network {
		mode, _ := r.mgr.SandboxPool().EgressDefault()
		if err := req.checkNetwork(mode != sandbox.NetworkModeLockdown); err != nil {
			return nil, err
		}
	}
	return req, nil
}

func (r *Runner) buildTaskRemoteOverlayChecked(ctx context.Context, task *models.Task, binding taskMCPBinding, req *executionRequirements, native []fantasy.AgentTool) (*agent.RemoteMCPOverlay, error) {
	catalog := binding.discoveryCatalog()
	overlay, err := r.buildTaskRemoteOverlay(ctx, task, catalog)
	if err != nil {
		return nil, err
	}
	if req != nil {
		catalog = append([]mcp.ServerTool(nil), catalog...)
		if overlay != nil {
			catalog = append(catalog, overlay.Catalog...)
		}
		if err := req.checkTools(catalog, native); err != nil {
			overlay.Close()
			return nil, err
		}
	}
	return overlay, nil
}
