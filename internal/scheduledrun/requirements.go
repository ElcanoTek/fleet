package scheduledrun

import (
	"context"
	"errors"
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
	// blockedRoster is Completion.BlockedWhen.Tool resolved the same way (the
	// parser guarantees any_succeeded lists it, so checkTools has already
	// proved it resolves).
	blockedRoster []string
}

// completionBlockedWhen returns the resolved blocked rule for the agent, nil
// when the run declared none.
func (r *executionRequirements) completionBlockedWhen() *agent.CompletionBlockedWhen {
	if r == nil || r.Completion == nil || r.Completion.BlockedWhen == nil || len(r.blockedRoster) == 0 {
		return nil
	}
	bw := r.Completion.BlockedWhen
	return &agent.CompletionBlockedWhen{
		Tools:          append([]string(nil), r.blockedRoster...),
		Argument:       bw.Argument,
		In:             append([]string(nil), bw.In...),
		DetailArgument: bw.DetailArgument,
	}
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
	return resolveRosterNames(r.Completion.AnySucceeded, catalog, native)
}

// resolveBlockedWhen resolves the blocked rule's tool against the roster.
func (r *executionRequirements) resolveBlockedWhen(catalog []mcp.ServerTool, native []fantasy.AgentTool) []string {
	if r == nil || r.Completion == nil || r.Completion.BlockedWhen == nil {
		return nil
	}
	return resolveRosterNames([]string{r.Completion.BlockedWhen.Tool}, catalog, native)
}

// resolveRosterNames maps declared names (bare or full) to the deduplicated,
// sorted full roster names they denote.
func resolveRosterNames(names []string, catalog []mcp.ServerTool, native []fantasy.AgentTool) []string {
	_, tools := rosterNames(catalog, native)
	seen := make(map[string]bool)
	var out []string
	for _, name := range names {
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

// checkTools is checkToolsAgainst a run in which every selected server
// registered.
func (r *executionRequirements) checkTools(catalog []mcp.ServerTool, native []fantasy.AgentTool) error {
	return r.checkToolsAgainst(catalog, native, nil)
}

// missingRequirement is one declared name the roster lacks: kind is "server",
// "tool" or "completion tool", name the declared identifier.
type missingRequirement struct {
	kind string
	name string
}

func (m missingRequirement) String() string { return m.kind + " " + m.name }

// checkToolsAgainst checks the declaration against the run's roster. failures
// are the selected servers that failed to register in this run (bundle and
// hosted). When every missing name is explained by a server that failed to
// connect TRANSIENTLY, the error wraps agentcore.ErrConnectorUnavailable — a
// connector outage the scheduler re-runs later instead of dead-lettering the
// task on a DNS blip (production: "server pages" missing after
// "lookup pages.elcanotek.com: no such host"). Anything else — a server that
// is not configured or not selected, a tool name no server provides, a
// connect failure that is not transient — stays the terminal roster error.
func (r *executionRequirements) checkToolsAgainst(catalog []mcp.ServerTool, native []fantasy.AgentTool, failures []agentcore.MCPConnectFailure) error {
	if r == nil {
		return nil
	}
	servers, tools := rosterNames(catalog, native)
	var missing []missingRequirement
	for _, server := range r.Servers {
		if !servers[server] {
			missing = append(missing, missingRequirement{"server", server})
		}
	}
	for _, tool := range r.Tools {
		if len(tools[tool]) == 0 {
			missing = append(missing, missingRequirement{"tool", tool})
		}
	}
	// A completion tool the run cannot call would make the predicate
	// unsatisfiable while looking declared: the same dispatch error as an
	// unavailable required tool, before any model execution.
	if r.Completion != nil {
		for _, tool := range r.Completion.AnySucceeded {
			if len(tools[tool]) == 0 {
				missing = append(missing, missingRequirement{"completion tool", tool})
			}
		}
	}
	if len(missing) == 0 {
		return nil
	}
	names := make([]string, len(missing))
	for i, m := range missing {
		names[i] = m.String()
	}
	msg := "execution requirements: unavailable in the task's MCP/native tool roster: " + strings.Join(names, ", ")
	explaining, outage := explainByConnectFailures(missing, failures)
	if len(explaining) > 0 {
		msg += "; " + describeConnectFailures(explaining)
	}
	if outage {
		return &agentcore.ConnectorUnavailableError{Message: msg + " — a transient connector outage, not a roster problem", Failures: explaining}
	}
	return errors.New(msg + "; check selected servers, tool permissions and connected accounts")
}

// explainByConnectFailures attributes each missing name to a server that
// failed to register in this run: a missing server by its name; a missing
// tool by its full mcp_<server>_<tool> name (the longest failed server name
// that prefixes it). A BARE tool name cannot be traced to a server whose
// catalog was never fetched, so it is attributed to the run's transient
// failures as a whole when there are any — the tool may live on a server
// that never connected, and the re-run is bounded. It returns the failures
// that explain at least one missing name, and whether EVERY missing name is
// explained by a transient failure (the connector-outage verdict).
func explainByConnectFailures(missing []missingRequirement, failures []agentcore.MCPConnectFailure) ([]agentcore.MCPConnectFailure, bool) {
	if len(failures) == 0 {
		return nil, false
	}
	byServer := make(map[string]agentcore.MCPConnectFailure, len(failures))
	var transient []agentcore.MCPConnectFailure
	for _, f := range failures {
		byServer[f.Server] = f
		if f.Transient {
			transient = append(transient, f)
		}
	}
	used := map[string]bool{}
	var explaining []agentcore.MCPConnectFailure
	use := func(f agentcore.MCPConnectFailure) {
		if !used[f.Server] {
			used[f.Server] = true
			explaining = append(explaining, f)
		}
	}
	outage := true
	for _, m := range missing {
		if m.kind == "server" {
			f, ok := byServer[m.name]
			if !ok {
				outage = false
				continue
			}
			use(f)
			outage = outage && f.Transient
			continue
		}
		if rest, full := strings.CutPrefix(m.name, "mcp_"); full {
			var best agentcore.MCPConnectFailure
			for _, f := range failures {
				if strings.HasPrefix(rest, f.Server+"_") && len(f.Server) > len(best.Server) {
					best = f
				}
			}
			if best.Server == "" {
				outage = false
				continue
			}
			use(best)
			outage = outage && best.Transient
			continue
		}
		if len(transient) == 0 {
			outage = false
			continue
		}
		for _, f := range transient {
			use(f)
		}
	}
	sort.Slice(explaining, func(i, j int) bool { return explaining[i].Server < explaining[j].Server })
	return explaining, outage
}

// describeConnectFailures renders "server pages failed to connect this run
// (DNS lookup failed (no such host))", joined with "; ".
func describeConnectFailures(failures []agentcore.MCPConnectFailure) string {
	parts := make([]string, 0, len(failures))
	for _, f := range failures {
		detail := f.Detail
		if detail == "" {
			detail = "failed to connect"
		}
		parts = append(parts, fmt.Sprintf("server %s failed to connect this run (%s)", f.Server, detail))
	}
	return strings.Join(parts, "; ")
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
		failures := append([]agentcore.MCPConnectFailure(nil), binding.connectFailures...)
		if overlay != nil {
			catalog = append(catalog, overlay.Catalog...)
			failures = append(failures, overlay.ConnectFailures...)
		}
		if err := req.checkToolsAgainst(catalog, native, failures); err != nil {
			overlay.Close()
			return nil, err
		}
		req.completionRoster = req.resolveCompletion(catalog, native)
		req.blockedRoster = req.resolveBlockedWhen(catalog, native)
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
