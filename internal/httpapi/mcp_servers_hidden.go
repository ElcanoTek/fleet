package httpapi

import (
	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/agentcore"
)

// Interactive-hidden tools in the chat Tools picker
// (docs/INTERACTIVE-HIDDEN-TOOLS.md, ADR-0085).
//
// agent_policy.interactive_hidden_tools keeps an MCP tool out of every chat
// turn, but the picker (GET /mcp-servers and GET
// /conversations/{id}/mcp-servers, both chat-only; the task form reads the
// orchestrator's own catalog) listed each connector's tools from the boot
// catalog. A hidden write such as ox_create_deal still showed in a
// connector's tool list, its count, and the picker's search ("↳
// ox_create_deal"), as if a trader could use it in chat. The picker now shows
// what a chat turn can call:
//
//   - a hidden tool is dropped from the row's tools and tool_count (an
//     always-on row carries only the count, so its hidden tools are
//     subtracted by name on the server and never sent);
//   - a connector whose every tool is hidden is not listed at all: it has
//     nothing to offer in chat, and a switch that turns on nothing would
//     mislead. Its scheduled tasks still use it.
//
// Turning a connector on can never expose a hidden tool either: the run gate
// (agentcore buildFantasyToolsWithRoster) drops it from every chat turn
// whatever the conversation opted into. This filter only makes the picker
// say so.

// chatHiddenCount is how many of a connector's tools the hidden list keeps
// out of chat, matched by the name the run gate matches
// (agentcore.MCPToolName).
func chatHiddenCount(hidden []string, server string, tools []string) int {
	return len(tools) - len(chatVisibleTools(hidden, server, tools))
}

// chatVisibleTools is the one visibility rule every chat-facing listing uses
// (the picker rows, the always-on counts, the connector recommendation): the
// tools of server the hidden list does not select, matched by the name the
// run gate matches. tools itself is returned when nothing is hidden.
func chatVisibleTools(hidden []string, server string, tools []string) []string {
	if len(hidden) == 0 {
		return tools
	}
	visible := make([]string, 0, len(tools))
	for _, t := range tools {
		if !agentcore.InteractiveHiddenIn(hidden, agentcore.MCPToolName(server, t)) {
			visible = append(visible, t)
		}
	}
	return visible
}

// hideInteractiveHiddenTools applies the installed interactive_hidden_tools
// list to the rows that carry tool names (optional and needs-setup rows). An
// always-on row carries only a count; alwaysOnMCPServerCatalogEntries
// subtracts its hidden tools from the same snapshot it counts. Rows are
// returned unchanged when the bundle hides nothing.
func hideInteractiveHiddenTools(rows []map[string]any) []map[string]any {
	hidden := agentcore.InteractiveHiddenTools()
	if len(hidden) == 0 {
		return rows
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		name, _ := row["name"].(string)
		tools, _ := row["tools"].([]string)
		// A hosted connection lists no tools (they are only known once it is
		// opened for a turn); the run gate still applies to them.
		if remote, _ := row["remote"].(bool); remote || name == "" || len(tools) == 0 {
			out = append(out, row)
			continue
		}
		visible := chatVisibleTools(hidden, name, tools)
		if len(visible) == len(tools) {
			out = append(out, row)
			continue
		}
		if len(visible) == 0 {
			continue
		}
		cp := make(map[string]any, len(row))
		for k, v := range row {
			cp[k] = v
		}
		cp["tools"] = visible
		cp["tool_count"] = len(visible)
		out = append(out, cp)
	}
	return out
}

// chatVisibleConnectors drops the Optional connectors whose every tool the
// hidden list keeps out of chat, for the per-turn connector recommendation
// (connector_recommend.go): recommending one would steer the user to connect
// a connector that gives the chat nothing, which the picker does not list
// either. A connector with no tool names (not yet known) is kept, as the
// picker keeps it. The input is not modified.
func chatVisibleConnectors(catalog []agent.OptionalServerInfo, hidden []string) []agent.OptionalServerInfo {
	if len(hidden) == 0 {
		return catalog
	}
	out := make([]agent.OptionalServerInfo, 0, len(catalog))
	for _, srv := range catalog {
		if len(srv.Tools) > 0 && len(chatVisibleTools(hidden, srv.Name, srv.Tools)) == 0 {
			continue
		}
		out = append(out, srv)
	}
	return out
}
