package httpapi

import (
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

// chatToolName is the model-facing name a connector's tool registers under,
// the name interactive_hidden_tools suffixes are matched against.
func chatToolName(server, tool string) string {
	return "mcp_" + server + "_" + tool
}

// hideInteractiveHiddenTools applies the installed
// interactive_hidden_tools list to the chat picker's rows. It returns rows
// unchanged when the bundle hides nothing.
func (s *Server) hideInteractiveHiddenTools(rows []map[string]any) []map[string]any {
	if len(agentcore.InteractiveHiddenTools()) == 0 {
		return rows
	}
	// Always-on rows send a count but no names; their names come from the
	// same snapshot the count was taken from.
	alwaysOn := map[string][]string{}
	if p, ok := s.agent.(alwaysOnMCPServerCatalogProvider); ok {
		for _, info := range p.AlwaysOnMCPServerCatalog() {
			alwaysOn[info.Name] = info.Tools
		}
	}
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		name, _ := row["name"].(string)
		// A hosted connection lists no tools (they are only known once it is
		// opened for a turn); the run gate still applies to them.
		if remote, _ := row["remote"].(bool); remote || name == "" {
			out = append(out, row)
			continue
		}
		count, _ := row["tool_count"].(int)
		tools, _ := row["tools"].([]string)
		names := tools
		if alwaysOnRow, _ := row["always_on"].(bool); alwaysOnRow && len(tools) == 0 {
			names = alwaysOn[name]
		}
		visible := make([]string, 0, len(tools))
		hidden := 0
		for _, t := range names {
			if agentcore.InteractiveHiddenTool(chatToolName(name, t)) {
				hidden++
			} else if len(tools) > 0 {
				visible = append(visible, t)
			}
		}
		if hidden == 0 {
			out = append(out, row)
			continue
		}
		if count-hidden <= 0 {
			continue
		}
		cp := make(map[string]any, len(row))
		for k, v := range row {
			cp[k] = v
		}
		cp["tools"] = visible
		cp["tool_count"] = count - hidden
		out = append(out, cp)
	}
	return out
}
