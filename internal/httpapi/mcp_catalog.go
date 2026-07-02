package httpapi

import (
	"net/http"
	"strings"

	"github.com/ElcanoTek/fleet/internal/clientconfig"
)

// GET /mcp-catalog (#538) — the trust-labeled MCP directory the settings UI
// renders. It returns BOTH connector classes side by side, each explicitly
// tagged, so a user can see what they are opting into before enabling
// anything:
//
//   - "bundled": servers from the operator's client bundle (mcp_servers).
//     These run inside the mandatory sandbox on this box; their credentials
//     are brokered host-side and never leave the deployment.
//   - "third_party": curated entries from the bundle's remote_mcp_catalog
//     section — services HOSTED BY AN EXTERNAL VENDOR. Connecting one sends
//     tool calls (which can carry conversation content) to that vendor under
//     its own terms. Nothing connects from this endpoint; the user must add
//     the server through the per-user remote-MCP OAuth flow (#443).
//
// remote_mcp_enabled reports whether that OAuth flow is configured on this
// server, so the UI can render one-click Connect vs. a disabled hint.

type mcpCatalogBundledEntry struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name,omitempty"`
	Description string `json:"description"`
	ToolCount   int    `json:"tool_count"`
	Beta        bool   `json:"beta,omitempty"`
	Optional    bool   `json:"optional"`
	Trust       string `json:"trust"` // always "bundled"
}

type mcpCatalogThirdPartyEntry struct {
	Name        string `json:"name"`
	DisplayName string `json:"display_name"`
	Description string `json:"description"`
	URL         string `json:"url"`
	Vendor      string `json:"vendor,omitempty"`
	DocsURL     string `json:"docs_url,omitempty"`
	Trust       string `json:"trust"` // always "third_party"
}

type mcpCatalogResponse struct {
	Bundled          []mcpCatalogBundledEntry    `json:"bundled"`
	ThirdParty       []mcpCatalogThirdPartyEntry `json:"third_party"`
	RemoteMCPEnabled bool                        `json:"remote_mcp_enabled"`
}

// mcpCatalog handles GET /mcp-catalog. Auth+member like every settings read.
// The bundled list is the Optional-server catalog snapshot (the same source
// /mcp-servers uses — always-on servers are not listed there and need no
// opt-in decision); the third-party list is static manifest content.
func (s *Server) mcpCatalog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	resp := mcpCatalogResponse{
		Bundled:          []mcpCatalogBundledEntry{},
		ThirdParty:       []mcpCatalogThirdPartyEntry{},
		RemoteMCPEnabled: s.remoteMCP != nil && s.remoteMCP.Enabled(),
	}
	if s.agent != nil {
		for _, info := range s.agent.MCPServerCatalog() {
			resp.Bundled = append(resp.Bundled, mcpCatalogBundledEntry{
				Name:        info.Name,
				DisplayName: info.DisplayName,
				Description: info.Description,
				ToolCount:   info.ToolCount,
				Beta:        info.Beta,
				Optional:    true,
				Trust:       "bundled",
			})
		}
	}
	if s.clientConfig != nil {
		for _, e := range s.clientConfig.RemoteMCPCatalog {
			resp.ThirdParty = append(resp.ThirdParty, thirdPartyCatalogEntry(e))
		}
	}
	writeJSON(w, resp)
}

// thirdPartyCatalogEntry maps a manifest catalog entry to the wire shape,
// trimming whitespace once at the edge so the UI never renders stray spaces.
func thirdPartyCatalogEntry(e clientconfig.RemoteMCPCatalogEntry) mcpCatalogThirdPartyEntry {
	return mcpCatalogThirdPartyEntry{
		Name:        strings.TrimSpace(e.Name),
		DisplayName: strings.TrimSpace(e.DisplayName),
		Description: strings.TrimSpace(e.Description),
		URL:         strings.TrimSpace(e.URL),
		Vendor:      strings.TrimSpace(e.Vendor),
		DocsURL:     strings.TrimSpace(e.DocsURL),
		Trust:       "third_party",
	}
}
