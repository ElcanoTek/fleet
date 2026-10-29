package httpapi

import (
	"net/http"
	"strings"

	"github.com/ElcanoTek/fleet/internal/clientconfig"
	"github.com/ElcanoTek/fleet/internal/mcpoauth"
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
	Name             string   `json:"name"`
	DisplayName      string   `json:"display_name,omitempty"`
	Description      string   `json:"description"`
	ToolCount        int      `json:"tool_count"`
	Beta             bool     `json:"beta,omitempty"`
	Optional         bool     `json:"optional"`
	Accounts         []string `json:"accounts,omitempty"` // credential-account seat names (never values)
	EnabledByDefault bool     `json:"enabled_by_default"`
	DataSources      []string `json:"data_sources,omitempty"`
	Category         string   `json:"category"`
	// NeedsSetup marks a manifest connector whose credentials are not set on
	// this box: listed so the chat picker's "Set up in Settings" link lands on
	// its card, with no toggles (it never launched).
	NeedsSetup bool   `json:"needs_setup,omitempty"`
	Trust      string `json:"trust"` // always "bundled"
}

type mcpCatalogURLVariant struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	URL   string `json:"url"`
}

type mcpCatalogThirdPartyEntry struct {
	Name        string   `json:"name"`
	DisplayName string   `json:"display_name"`
	Description string   `json:"description"`
	URL         string   `json:"url"`
	Vendor      string   `json:"vendor,omitempty"`
	DocsURL     string   `json:"docs_url,omitempty"`
	RepoURL     string   `json:"repo_url,omitempty"`
	Category    string   `json:"category,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	Provenance  string   `json:"provenance"` // official | third_party | community (hosting-operator trust tier)
	Auth        string   `json:"auth,omitempty"`
	// Onboarding guidance (connector-directory onboarding): a visible hint on
	// where the URL/key comes from, the vendor's connect walkthrough, and — for
	// api_key entries — the header name the key must be sent under.
	SetupHint    string `json:"setup_hint,omitempty"`
	SetupURL     string `json:"setup_url,omitempty"`
	APIKeyHeader string `json:"api_key_header,omitempty"`
	APIKeyQuery  string `json:"api_key_query,omitempty"`
	// The scheme sent in front of the key under api_key_header ("Token
	// token="); the form shows it so the user pastes only the key.
	APIKeyPrefix string `json:"api_key_prefix,omitempty"`
	// Alternative endpoints (regional hosts) the card offers as a select; the
	// chosen URL is what the add posts.
	URLVariants []mcpCatalogURLVariant `json:"url_variants,omitempty"`
	// "manual" = the vendor's AS has no dynamic client registration; the UI
	// collects a bring-your-own OAuth client ID (+ optional secret) up front.
	ClientRegistration string `json:"client_registration,omitempty"`
	// "required" = the AS accepts no public clients, so the manual client's
	// secret field is mandatory in the form rather than "optional".
	ClientSecret string `json:"client_secret,omitempty"`
	Featured     bool   `json:"featured,omitempty"` // curated Featured-shelf pick
	Trust        string `json:"trust"`              // always "third_party" (the CLASS, vs "bundled"; distinct from Provenance)
}

type mcpCatalogResponse struct {
	Bundled          []mcpCatalogBundledEntry    `json:"bundled"`
	ThirdParty       []mcpCatalogThirdPartyEntry `json:"third_party"`
	RemoteMCPEnabled bool                        `json:"remote_mcp_enabled"`
	// OAuthRedirectURI is the stable callback URL manual client registrations
	// (GitHub, Google Workspace, …) must be created with. Setup hints tell the
	// user to find it "in your Fleet instance's OAuth settings" — this field is
	// what the settings UI shows for that. Empty when the flow is unconfigured.
	OAuthRedirectURI string `json:"oauth_redirect_uri,omitempty"`
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
	if resp.RemoteMCPEnabled {
		resp.OAuthRedirectURI = s.remoteMCP.RedirectURI()
	}
	cats := s.connectorCategories()
	if s.agent != nil {
		for _, info := range s.agent.MCPServerCatalog() {
			resp.Bundled = append(resp.Bundled, mcpCatalogBundledEntry{
				Name:             info.Name,
				DisplayName:      info.DisplayName,
				Description:      info.Description,
				ToolCount:        info.ToolCount,
				Beta:             info.Beta,
				Optional:         true,
				Accounts:         info.Accounts,
				EnabledByDefault: info.EnabledByDefault,
				DataSources:      append([]string(nil), info.DataSources...),
				Category:         categoryFor(cats, info.Name),
				Trust:            "bundled",
			})
		}
	}
	if s.clientConfig != nil {
		// Always-on (non-Optional) connectors render as visible-but-locked
		// rows: no toggle, no opt-in — they are wired into every turn, and
		// users should be able to see that.
		for _, a := range s.alwaysOnDirectoryRows() {
			resp.Bundled = append(resp.Bundled, mcpCatalogBundledEntry{
				Name:        a.Name,
				DisplayName: a.DisplayName,
				Description: a.Description,
				Optional:    false,
				Category:    categoryFor(cats, a.Name),
				Trust:       "bundled",
			})
		}
		listed := make(map[string]bool, len(resp.Bundled))
		for _, e := range resp.Bundled {
			listed[e.Name] = true
		}
		for _, m := range s.connectorMeta() {
			// A connector that is running is configured, whatever the snapshot says.
			if m.Configured || listed[m.Name] {
				continue
			}
			resp.Bundled = append(resp.Bundled, mcpCatalogBundledEntry{
				Name:        m.Name,
				DisplayName: m.DisplayName,
				Description: m.Description,
				ToolCount:   len(m.Tools),
				Optional:    m.Optional,
				Category:    m.Category,
				NeedsSetup:  true,
				Trust:       "bundled",
			})
		}
		for _, e := range s.clientConfig.RemoteMCPCatalog {
			resp.ThirdParty = append(resp.ThirdParty, thirdPartyCatalogEntry(e))
		}
	}
	writeJSON(w, resp)
}

// alwaysOnDirectoryRows lists the enabled always-on connectors for the
// directory from the same snapshot as its needs-setup rows: the Manager's
// reload-fresh connector metadata when it has one, so a connector that a
// reload turned on or off is listed once, as a card or as needs-setup, never
// both or neither. Without it, the boot bundle's list.
func (s *Server) alwaysOnDirectoryRows() []clientconfig.AlwaysOnServer {
	if p, ok := s.agent.(connectorMetaProvider); ok {
		if meta := p.ConnectorMeta(); meta != nil {
			var out []clientconfig.AlwaysOnServer
			for _, m := range meta {
				if !m.Optional && m.Configured {
					out = append(out, clientconfig.AlwaysOnServer{Name: m.Name, DisplayName: m.DisplayName, Description: m.Description})
				}
			}
			return out
		}
	}
	return s.clientConfig.AlwaysOnServers()
}

// thirdPartyCatalogEntry maps a manifest catalog entry to the wire shape,
// trimming whitespace once at the edge so the UI never renders stray spaces.
// When an operator has provisioned a shared OAuth client for a
// client_registration: manual entry (in the manifest or via the process
// environment), the wire projection clears ClientRegistration, ClientSecret,
// and SetupHint so the directory card renders a one-click OAuth add instead of
// asking each user to register their own OAuth app.
func thirdPartyCatalogEntry(e clientconfig.RemoteMCPCatalogEntry) mcpCatalogThirdPartyEntry {
	clientReg := strings.TrimSpace(e.ClientRegistration)
	clientSec := strings.TrimSpace(e.ClientSecret)
	setupHint := strings.TrimSpace(e.SetupHint)
	if _, _, ok := e.EffectiveOAuthClient(); ok {
		clientReg = ""
		clientSec = ""
		if e.Auth != "tenant" && !strings.Contains(e.URL, "{") {
			setupHint = ""
		}
	}
	return mcpCatalogThirdPartyEntry{
		Name:               strings.TrimSpace(e.Name),
		DisplayName:        strings.TrimSpace(e.DisplayName),
		Description:        strings.TrimSpace(e.Description),
		URL:                strings.TrimSpace(e.URL),
		Vendor:             strings.TrimSpace(e.Vendor),
		DocsURL:            strings.TrimSpace(e.DocsURL),
		RepoURL:            strings.TrimSpace(e.RepoURL),
		Category:           strings.TrimSpace(e.Category),
		Tags:               e.Tags,
		Provenance:         e.Provenance,
		Auth:               e.Auth,
		SetupHint:          setupHint,
		SetupURL:           strings.TrimSpace(e.SetupURL),
		APIKeyHeader:       strings.TrimSpace(e.APIKeyHeader),
		APIKeyQuery:        strings.TrimSpace(e.APIKeyQuery),
		APIKeyPrefix:       e.APIKeyPrefix,
		URLVariants:        catalogURLVariants(e.URLVariants),
		ClientRegistration: clientReg,
		ClientSecret:       clientSec,
		Featured:           e.Featured,
		Trust:              "third_party",
	}
}

// catalogURLVariants projects an entry's url_variants for the card, each URL
// in the canonical form AddServer stores (lowercased host, default port
// dropped). The card posts the variant URL as is and later recognises a saved
// seat's endpoint by comparing the stored URL to it, so a bundle spelling such
// as an uppercase host or an explicit :443 must not reach the page unmatched —
// a regional key would then be offered the default host. A URL that does not
// canonicalise is passed through trimmed (the loader has already required
// https).
func catalogURLVariants(in []clientconfig.RemoteMCPURLVariant) []mcpCatalogURLVariant {
	if len(in) == 0 {
		return nil
	}
	out := make([]mcpCatalogURLVariant, 0, len(in))
	for _, v := range in {
		u := strings.TrimSpace(v.URL)
		if canon, err := mcpoauth.CanonicalResourceURI(u); err == nil {
			u = canon
		}
		out = append(out, mcpCatalogURLVariant{ID: v.ID, Label: strings.TrimSpace(v.Label), URL: u})
	}
	return out
}
