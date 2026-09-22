package scheduledrun

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"charm.land/fantasy"
	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/agentcore"
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

// rosterRequiredToolsOnly is the one roster narrowing a requirements block may
// request (#1603): the run registers only the MCP tools its required_tools
// names. An unknown value fails dispatch like any malformed declaration.
const rosterRequiredToolsOnly = "required_tools_only"

// parseRequirementsRoster reads the optional "roster" key of the task's
// EXECUTION REQUIREMENTS object (#1603). It reads the same line
// parseExecutionRequirements validated (one bounded JSON object after the one
// marker), so it runs only after that preflight has passed; the key lives
// beside executionRequirements rather than in it only so the two can evolve in
// parallel changes. "" = no narrowing (the key absent or null). Any other value
// than rosterRequiredToolsOnly — including a non-string — is a dispatch error,
// before any model or MCP work.
func parseRequirementsRoster(prompt string) (string, error) {
	lines := strings.Split(prompt, "\n")
	for i, line := range lines {
		if strings.TrimSpace(line) != executionRequirementsMarker || i+1 == len(lines) {
			continue
		}
		var req struct {
			Roster *string `json:"roster"`
		}
		if err := json.Unmarshal([]byte(lines[i+1]), &req); err != nil {
			return "", fmt.Errorf("execution requirements: roster must be a string")
		}
		if req.Roster == nil || *req.Roster == "" {
			return "", nil
		}
		if *req.Roster != rosterRequiredToolsOnly {
			return "", fmt.Errorf("execution requirements: unknown roster %q (supported: %q)", *req.Roster, rosterRequiredToolsOnly)
		}
		return rosterRequiredToolsOnly, nil
	}
	return "", nil
}

// narrowedAllowlist is the Gate-2 allowlist of a required_tools_only run
// (#1603): for every server in the run's MCP roster, the tools required_tools
// names — as the full mcp_<server>_<tool> name or the bare tool name, the forms
// checkTools resolves — that the base allowlist already permits. It only ever
// subtracts. Entries are keyed by the REGISTERED server name; a
// <server>_<account> seat whose tools are not named in its own full form gets
// no entry and falls back to its base server's entry by the one keying rule, so
// it narrows the same way. It is always non-nil and is paired with an EXHAUSTIVE
// Gate-2 (agent.Options.MCPRosterNarrowing), under which a server with no
// entry registers nothing. Native tools are not in the MCP roster and are
// untouched.
func (r *executionRequirements) narrowedAllowlist(catalog []mcp.ServerTool, base agentcore.MCPAllowlist) agentcore.MCPAllowlist {
	required := make(map[string]bool, len(r.Tools))
	for _, name := range r.Tools {
		required[name] = true
	}
	out := agentcore.MCPAllowlist{}
	for _, item := range catalog {
		full := "mcp_" + item.ServerName + "_" + item.Tool.Name
		if !required[full] && !required[item.Tool.Name] {
			continue
		}
		if list := agentcore.AllowlistToolsFor(base, item.ServerName); len(list) > 0 && !listHas(list, item.Tool.Name) {
			continue // Gate-2 already removes it; narrowing never widens
		}
		if !listHas(out[item.ServerName], item.Tool.Name) {
			out[item.ServerName] = append(out[item.ServerName], item.Tool.Name)
		}
	}
	return out
}

func listHas(list []string, name string) bool {
	for _, item := range list {
		if item == name {
			return true
		}
	}
	return false
}

// checkTaskRequirementsAndRoster is the whole dispatch preflight of a task's
// EXECUTION REQUIREMENTS: the declaration (checkTaskRequirements) and its
// roster narrowing (parseRequirementsRoster), both before any MCP or model
// work.
func (r *Runner) checkTaskRequirementsAndRoster(task *models.Task) (*executionRequirements, string, error) {
	req, err := r.checkTaskRequirements(task)
	if err != nil {
		return nil, "", err
	}
	roster, err := parseRequirementsRoster(task.Prompt)
	if err != nil {
		return nil, "", err
	}
	return req, roster, nil
}

// taskRosterAllowlist is the run's Gate-2 allowlist: the manifest's, or — for a
// required_tools_only roster (#1603) — its narrowing to the tools
// required_tools names across the whole checked roster (the run's servers plus
// the owner's remote overlay). The narrowed allowlist is exhaustive for the run
// (agent.Options.MCPRosterNarrowing), so a selected server none of whose tools
// is required registers nothing; checkTools has already proved every required
// tool is in that roster, so none can be narrowed away.
func (r *Runner) taskRosterAllowlist(req *executionRequirements, roster string, binding taskMCPBinding, overlay *agent.RemoteMCPOverlay) agentcore.MCPAllowlist {
	allow := r.taskMCPToolAllowlist()
	if roster == "" || req == nil {
		return allow
	}
	catalog := append([]mcp.ServerTool(nil), binding.discoveryCatalog()...)
	if overlay != nil {
		catalog = append(catalog, overlay.Catalog...)
	}
	return req.narrowedAllowlist(catalog, allow)
}
