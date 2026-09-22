package scheduledrun

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
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
	Servers    []string               `json:"mcp_servers"`
	Tools      []string               `json:"required_tools"`
	Network    bool                   `json:"network"`
	Completion *completionRequirement `json:"completion"`

	// completionRoster is Completion.AnySucceeded resolved against the run's
	// actual tool roster (full mcp_<server>_<tool> and native names), filled by
	// buildTaskRemoteOverlayChecked once the roster is known.
	completionRoster []string
}

// completionRequirement is the producer's deterministic completion predicate
// (#1602): the run is complete once ANY listed tool has a successful execution,
// judged by the same success classification the end-of-run verifier's tool
// summary uses. Names take the forms required_tools accepts. Like the rest of
// the declaration it is opaque to Fleet — a list of tool names, never a meaning
// assigned to them — and an empty or absent list declares no predicate, so the
// verifier runs as before. Unknown sibling keys are ignored for forward
// compatibility, like unknown top-level keys.
type completionRequirement struct {
	AnySucceeded []string `json:"any_succeeded"`
}

// completionTools returns the resolved completion predicate names, nil when
// the run declared none.
func (r *executionRequirements) completionTools() []string {
	if r == nil {
		return nil
	}
	return r.completionRoster
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
		var completion []string
		if req.Completion != nil {
			completion = req.Completion.AnySucceeded
		}
		if len(req.Servers) > 100 || len(req.Tools) > 200 || len(completion) > 200 {
			return nil, fmt.Errorf("execution requirements: too many servers or tools")
		}
		for _, name := range append(append(append([]string{}, req.Servers...), req.Tools...), completion...) {
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

// rosterNames indexes a run's tool roster by every identifier a declaration may
// use — native name, bare server tool name, or full mcp_<server>_<tool> name —
// mapping each to the full roster names it denotes (a bare name shared by two
// servers denotes both), plus the set of servers present.
func rosterNames(catalog []mcp.ServerTool, native []fantasy.AgentTool) (servers map[string]bool, tools map[string][]string) {
	servers = make(map[string]bool)
	tools = make(map[string][]string)
	for _, item := range catalog {
		full := "mcp_" + item.ServerName + "_" + item.Tool.Name
		servers[item.ServerName] = true
		tools[full] = append(tools[full], full)
		tools[item.Tool.Name] = append(tools[item.Tool.Name], full)
	}
	for _, tool := range native {
		name := tool.Info().Name
		tools[name] = append(tools[name], name)
	}
	return servers, tools
}

// resolveCompletion resolves the completion predicate against the roster into
// the deduplicated, sorted full names a successful execution may carry. A name
// that resolves to nothing is reported by checkTools, which runs first.
func (r *executionRequirements) resolveCompletion(catalog []mcp.ServerTool, native []fantasy.AgentTool) []string {
	if r == nil || r.Completion == nil || len(r.Completion.AnySucceeded) == 0 {
		return nil
	}
	_, tools := rosterNames(catalog, native)
	seen := make(map[string]bool)
	var out []string
	for _, name := range r.Completion.AnySucceeded {
		for _, full := range tools[name] {
			if !seen[full] {
				seen[full] = true
				out = append(out, full)
			}
		}
	}
	sort.Strings(out)
	return out
}

func (r *executionRequirements) checkTools(catalog []mcp.ServerTool, native []fantasy.AgentTool) error {
	if r == nil {
		return nil
	}
	servers, tools := rosterNames(catalog, native)
	var missing []string
	for _, server := range r.Servers {
		if !servers[server] {
			missing = append(missing, "server "+server)
		}
	}
	for _, tool := range r.Tools {
		if len(tools[tool]) == 0 {
			missing = append(missing, "tool "+tool)
		}
	}
	// A completion tool the run cannot call would make the predicate
	// unsatisfiable while looking declared: the same dispatch error as an
	// unavailable required tool, before any model execution.
	if r.Completion != nil {
		for _, tool := range r.Completion.AnySucceeded {
			if len(tools[tool]) == 0 {
				missing = append(missing, "completion tool "+tool)
			}
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
		req.completionRoster = req.resolveCompletion(catalog, native)
	}
	return overlay, nil
}
