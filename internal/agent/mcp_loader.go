package agent

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"charm.land/fantasy"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/creds"
)

// Load-on-demand MCP loader tools for the scheduled driver. mcp_list_servers
// renders the catalog; mcp_load_servers binds the chosen servers' credentialed
// subprocesses via agentcore.BindMCPSelection and marks the agent dirty so the
// SHARED loop (agentcore.Run) rebuilds the fantasy tool list before the next
// round. The dirty-rebuild itself is the loop's job (MCPServersDirty hook) — the
// loader only flips the flag and records what it loaded.

type mcpListServersInput struct{}

type mcpLoadServersInput struct {
	Names  []string `json:"names" description:"One or more MCP server names to load, as listed by mcp_list_servers. Use the bare server name (e.g. \"myserver\"). Already-loaded servers are silently skipped."`
	Client string   `json:"client" description:"Optional client/account suffix selecting a non-default credential set (e.g. \"reklaim\" picks env vars suffixed with _REKLAIM). Spawns a separate subprocess under \"<server>_<client>\"; tools surface as mcp_<server>_<client>_*. Omit for the default account. Stdio servers only — HTTP servers reject this."`
}

func (a *Agent) buildLoaderTools() []fantasy.AgentTool {
	return []fantasy.AgentTool{
		fantasy.NewAgentTool(
			"mcp_list_servers",
			"Lists every MCP server available to this agent with its tool count and whether it is currently loaded. Call this first when a task mentions an integration you don't already see in your tool list. Cheap and side-effect-free.",
			func(_ context.Context, _ mcpListServersInput, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
				return fantasy.NewTextResponse(a.renderMCPCatalog()), nil
			},
		),
		fantasy.NewAgentTool(
			"mcp_load_servers",
			"Loads one or more MCP servers so their tools become callable on your NEXT assistant turn. Call this ONLY for servers you actually need. Pass client=\"<name>\" to load a non-default credential variant; it spawns under <server>_<client> and surfaces as mcp_<server>_<client>_*.",
			func(ctx context.Context, input mcpLoadServersInput, _ fantasy.ToolCall) (fantasy.ToolResponse, error) {
				return a.loadMCPServers(ctx, input.Names, input.Client)
			},
		),
	}
}

// renderMCPCatalog lists the scheduled config's MCP servers grouped by loaded /
// available / disabled.
func (a *Agent) renderMCPCatalog() string {
	if a == nil || a.config == nil {
		return "[mcp_list_servers] agent state unavailable"
	}
	a.mu.Lock()
	loadedSet := make(map[string]bool, len(a.loadedServers))
	for k, v := range a.loadedServers {
		loadedSet[k] = v
	}
	a.mu.Unlock()

	toolsByServer := map[string]int{}
	if a.mcpClient != nil {
		for _, st := range a.mcpClient.GetAllTools() {
			toolsByServer[st.ServerName]++
		}
	}

	var loaded, available, disabled []string
	for name, sc := range a.config.MCPServers {
		switch {
		case !sc.Enabled:
			disabled = append(disabled, name)
		case loadedSet[name]:
			loaded = append(loaded, fmt.Sprintf("%s (%d tools)", name, toolsByServer[name]))
		default:
			available = append(available, name)
		}
	}
	sort.Strings(loaded)
	sort.Strings(available)
	sort.Strings(disabled)

	var b strings.Builder
	fmt.Fprintf(&b, "MCP server catalog (%d loaded, %d available, %d disabled)\n\n", len(loaded), len(available), len(disabled))
	if len(loaded) > 0 {
		b.WriteString("LOADED (tools already in your tool list; call directly):\n")
		for _, e := range loaded {
			fmt.Fprintf(&b, "  • %s\n", e)
		}
		b.WriteString("\n")
	}
	if len(available) > 0 {
		b.WriteString("AVAILABLE (configured but not yet loaded; call mcp_load_servers to enable):\n")
		for _, e := range available {
			fmt.Fprintf(&b, "  • %s\n", e)
		}
		names := make([]string, 0, len(available))
		for _, e := range available {
			names = append(names, fmt.Sprintf("%q", e))
		}
		fmt.Fprintf(&b, "\n  To load: mcp_load_servers(names=[%s])\n\n", strings.Join(names, ", "))
	}
	if len(disabled) > 0 {
		b.WriteString("DISABLED (missing credentials or explicitly off; NOT callable):\n")
		for _, e := range disabled {
			fmt.Fprintf(&b, "  • %s\n", e)
		}
	}
	return b.String()
}

// loadMCPServers binds the requested servers (optionally under a credential
// account variant) onto the MCP client and marks the agent dirty so the loop
// rebuilds the tool list. HTTP servers reject account variants. Newly-loaded
// servers end the current turn so their schemas reach the model next round.
func (a *Agent) loadMCPServers(ctx context.Context, names []string, account string) (fantasy.ToolResponse, error) {
	if a == nil || a.config == nil {
		return fantasy.NewTextErrorResponse("agent state unavailable"), nil
	}
	if a.mcpClient == nil {
		return fantasy.NewTextErrorResponse("no MCP client configured"), nil
	}

	bases := a.mcpBases()
	var loadedNow []string
	var errs []string
	for _, name := range names {
		sc, ok := a.config.MCPServers[name]
		if !ok || !sc.Enabled {
			errs = append(errs, fmt.Sprintf("%q: unknown or disabled server", name))
			continue
		}
		regName := name
		if account != "" {
			regName = name + "_" + account
		}
		a.mu.Lock()
		already := a.loadedServers[regName]
		a.mu.Unlock()
		if already {
			continue
		}

		sel := agentcore.MCPSelection{{Server: name, Account: account}}
		registered, err := agentcore.BindMCPSelection(ctx, a.mcpClient, sel, bases)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%q: %v", name, err))
			continue
		}
		for _, r := range registered {
			a.markServerLoaded(r)
			loadedNow = append(loadedNow, r)
		}
	}

	var b strings.Builder
	if len(loadedNow) > 0 {
		fmt.Fprintf(&b, "Loaded %d server(s): %s. Their tools are registered for your NEXT turn.\n", len(loadedNow), strings.Join(loadedNow, ", "))
	} else {
		b.WriteString("No new servers loaded.\n")
	}
	for _, e := range errs {
		fmt.Fprintf(&b, "  ✗ %s\n", e)
	}
	resp := fantasy.NewTextResponse(b.String())
	if len(loadedNow) > 0 {
		// End the current turn so the loop rebuilds the agent with new tools.
		resp.StopTurn = true
	}
	return resp, nil
}

// mcpBases maps each configured server name to the spawn spec + base env the
// binder needs (account overlays are applied by BindMCPSelection via
// creds.ApplyClientSuffix).
func (a *Agent) mcpBases() map[string]agentcore.MCPServerBase {
	bases := map[string]agentcore.MCPServerBase{}
	if a.config == nil {
		return bases
	}
	for name, sc := range a.config.MCPServers {
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

// ensure creds stays referenced (BindMCPSelection uses it transitively; this
// keeps the import meaningful for readers tracing the credential path).
var _ = creds.ApplyClientSuffix
