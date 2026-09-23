package scheduledrun

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"charm.land/fantasy"
	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/mcp"
	"github.com/ElcanoTek/fleet/internal/sandbox"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// executionRequirements is a run's view of its task's optional, copyable
// handoff from a prompt producer: the declaration exactly as
// models.ParseExecutionRequirements reads it — the one grammar every task save
// path also validates with (#1601) — plus what dispatch resolves against the
// run's roster. Requirements never enable network, load credentials, or widen
// MCP scope. The one clause that relaxes anything is completion (#1602): a
// successful listed tool finishes the run without the end-of-run verifier and
// phone-a-friend review (ADR-0072).
type executionRequirements struct {
	models.ExecutionRequirements

	// completionRoster is Completion.AnySucceeded resolved against the run's
	// actual tool roster (full mcp_<server>_<tool> and native names), filled by
	// buildTaskRemoteOverlayChecked once the roster is known.
	completionRoster []string
}

// completionRequirement is the producer's deterministic completion predicate
// (#1602): the run is complete once ANY listed tool has a successful execution,
// judged by the same success classification the end-of-run verifier's tool
// summary uses. Like the rest of the declaration it is opaque to Fleet — a
// list of tool names, never a meaning assigned to them — and an empty or
// absent list declares no predicate, so the verifier runs as before.
type completionRequirement = models.ExecutionCompletion

// completionTools returns the resolved completion predicate names, nil when
// the run declared none.
func (r *executionRequirements) completionTools() []string {
	if r == nil {
		return nil
	}
	return r.completionRoster
}

// parseExecutionRequirements is the dispatch adapter over the one parser: a
// declaration refused here would have been refused when the task was saved,
// with the same message. nil when the prompt declares nothing.
func parseExecutionRequirements(prompt string) (*executionRequirements, error) {
	decl, err := models.ParseExecutionRequirements(prompt)
	if err != nil || decl == nil {
		return nil, err
	}
	return &executionRequirements{ExecutionRequirements: *decl}, nil
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

// rosterRequiredToolsOnly is the one roster narrowing a requirements block may
// request (#1603): the run registers only the MCP tools its required_tools
// names. The parser refuses any other value, so an unknown one fails dispatch
// like any malformed declaration.
const rosterRequiredToolsOnly = models.ExecutionRequirementsRosterRequiredToolsOnly

// rosterNarrowing is the declared narrowing, "" for none (no declaration, or
// no roster key).
func (r *executionRequirements) rosterNarrowing() string {
	if r == nil {
		return ""
	}
	return r.RosterNarrowing()
}

// narrowedAllowlist is the Gate-2 allowlist of a required_tools_only run
// (#1603): for every server in the run's MCP roster, the tools required_tools
// names — as the full mcp_<server>_<tool> name or the bare tool name, the forms
// checkTools resolves — that the base allowlist already permits. It only ever
// subtracts. Entries are keyed by the REGISTERED server name, and every other
// catalog server gets an explicit deny entry, so no server inherits another's
// narrowed list through the keying rule (a <server>_<account> seat keeps only
// the tools named in its own full form or by bare name). It is always non-nil and is paired with an EXHAUSTIVE
// Gate-2 (agent.Options.MCPRosterNarrowing), under which a server with no
// entry registers nothing. Native tools are not in the MCP roster and are
// untouched.
func (r *executionRequirements) narrowedAllowlist(catalog []mcp.ServerTool, base agentcore.MCPAllowlist) agentcore.MCPAllowlist {
	required := make(map[string]bool, len(r.Tools))
	for _, name := range r.Tools {
		required[name] = true
	}
	// A completion tool (#1602) the narrowing removed could never execute, so
	// the declared predicate would be unsatisfiable while looking declared:
	// keep every completion.any_succeeded name as if required_tools listed it.
	// Still intersected with the base allowlist below, so it never widens.
	if r.Completion != nil {
		for _, name := range r.Completion.AnySucceeded {
			required[name] = true
		}
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
	// Every other catalog server is denied EXPLICITLY. Leaving it without an
	// entry would let Gate-2's longest-prefix keying resolve it to another
	// server's narrowed entry — an account seat to its base server, but equally
	// an independent server that merely shares a prefix (foo_archive → foo) —
	// and register a same-named tool nothing required. The catalog cannot tell
	// the two apart, so no server inherits: a seat's tools are kept only when
	// required_tools names them in the seat's own full form or by bare name.
	for _, item := range catalog {
		if _, own := out[item.ServerName]; !own {
			out[item.ServerName] = []string{rosterNarrowingDeniesAll}
		}
	}
	return out
}

// rosterNarrowingDeniesAll is the entry of a catalog server none of whose tools
// a narrowed run requires: an EMPTY entry reads as "allow all" under the
// allowlist semantics, so denying needs one never-matching name.
const rosterNarrowingDeniesAll = "__roster_narrowing_denies_all_tools__"

func listHas(list []string, name string) bool {
	for _, item := range list {
		if item == name {
			return true
		}
	}
	return false
}

// checkTaskRequirementsAndRoster is the whole dispatch preflight of a task's
// EXECUTION REQUIREMENTS: the declaration (checkTaskRequirements) and the
// roster narrowing it declares, both before any MCP or model work.
func (r *Runner) checkTaskRequirementsAndRoster(task *models.Task) (*executionRequirements, string, error) {
	req, err := r.checkTaskRequirements(task)
	if err != nil {
		return nil, "", err
	}
	return req, req.rosterNarrowing(), nil
}

// taskRosterAllowlist is the run's Gate-2 allowlist: the manifest's, or — for a
// required_tools_only roster (#1603) — its narrowing to the tools
// required_tools names across the whole checked roster (the run's servers plus
// the owner's remote overlay). The narrowed allowlist is exhaustive for the run
// (agent.Options.MCPRosterNarrowing), so a selected server none of whose tools
// is required registers nothing. checkTools has already proved every required
// tool is in that roster, so narrowing never removes one the manifest
// allowlist permits.
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
