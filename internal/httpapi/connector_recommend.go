package httpapi

import (
	"fmt"
	"strings"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// Connector auto-recommendation (#512): when a chat message is relevant to an
// Optional MCP connector the user has NOT enabled, add a note to the turn context
// so the agent can suggest connecting it — via the EXISTING /settings/connections
// OAuth flow, never auto-connecting. Detection reuses the #506 BM25 index over the
// connector catalog's {name, display, description}; it is deterministic, needs no
// network, and is DEFAULT OFF (FLEET_CONNECTOR_RECOMMENDATIONS_ENABLED).

const maxConnectorRecommendations = 2

// recommendConnectors ranks the Optional MCP catalog against the user's message
// (BM25) and returns up to `limit` connectors that are RELEVANT but NOT already
// enabled for this turn. An already-enabled connector is never recommended; a
// message with no term overlap yields nothing (BM25 excludes zero-score hits).
func recommendConnectors(userMessage string, catalog []agent.OptionalServerInfo, enabled map[string]bool, limit int) []agent.OptionalServerInfo {
	msg := strings.TrimSpace(userMessage)
	if msg == "" || len(catalog) == 0 || limit <= 0 {
		return nil
	}
	byName := make(map[string]agent.OptionalServerInfo, len(catalog))
	docs := make([]tools.BM25Doc, 0, len(catalog))
	for _, srv := range catalog {
		if enabled[srv.Name] {
			continue // already enabled/connected — nothing to recommend
		}
		byName[srv.Name] = srv
		docs = append(docs, tools.BM25Doc{
			ID:   srv.Name,
			Text: srv.Name + " " + srv.DisplayName + " " + srv.Description,
		})
	}
	if len(docs) == 0 {
		return nil
	}
	out := make([]agent.OptionalServerInfo, 0, limit)
	for _, hit := range tools.NewBM25Index(docs).Search(msg, limit) {
		if srv, ok := byName[hit.ID]; ok {
			out = append(out, srv)
		}
	}
	return out
}

// appendConnectorRecommendationBlock appends a note listing the
// relevant-but-unconnected connectors so the agent can suggest the user connect
// them. The note is guidance/DATA for the model — it explicitly states the tools
// are NOT available yet, so the agent can't hallucinate access. No-op when empty.
func appendConnectorRecommendationBlock(message string, recs []agent.OptionalServerInfo) string {
	if len(recs) == 0 {
		return message
	}
	var b strings.Builder
	b.WriteString(message)
	b.WriteString("\n\n---\n**Possibly-relevant connectors (NOT currently connected):**\n")
	for _, srv := range recs {
		name := strings.TrimSpace(srv.DisplayName)
		if name == "" {
			name = srv.Name
		}
		if desc := strings.TrimSpace(srv.Description); desc != "" {
			fmt.Fprintf(&b, "- **%s** — %s\n", name, desc)
		} else {
			fmt.Fprintf(&b, "- **%s**\n", name)
		}
	}
	b.WriteString("\nThese connectors are NOT enabled for this chat, so you do NOT have their tools. " +
		"If the request needs one, tell the user it's available and suggest they connect it from the " +
		"Connections settings page (/settings/connections) — do NOT claim you can use it until they do.\n")
	return b.String()
}

// applyConnectorRecommendations appends connector recommendations to userMessage
// when the feature is enabled (#512), else returns it unchanged. A method so
// postChat stays a single statement (no added complexity).
func (s *Server) applyConnectorRecommendations(userMessage, rawMessage string, enabledOptional []string) string {
	if !s.cfg.ConnectorRecommendationsEnabled || s.agent == nil {
		return userMessage
	}
	enabled := make(map[string]bool, len(enabledOptional))
	for _, n := range enabledOptional {
		enabled[n] = true
	}
	recs := recommendConnectors(rawMessage, s.agent.MCPServerCatalog(), enabled, maxConnectorRecommendations)
	return appendConnectorRecommendationBlock(userMessage, recs)
}
