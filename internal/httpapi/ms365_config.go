package httpapi

import (
	"context"
	"net/http"
	"regexp"
	"strings"

	"github.com/ElcanoTek/fleet/internal/config/envreg"
	"github.com/ElcanoTek/fleet/internal/ms365mcp"
	"github.com/ElcanoTek/fleet/internal/store"
)

type ms365ConfigStore interface {
	MS365MCPConfig(ctx context.Context) (clientID, tenantID string, ok bool, err error)
	SetMS365MCPConfig(ctx context.Context, clientID, tenantID, updatedBy string) error
	DeleteMS365MCPConfig(ctx context.Context) error
}

var _ ms365ConfigStore = (*store.Store)(nil)

var ms365IdentifierRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,128}$`)

type ms365ConfigResponse struct {
	Configured           bool   `json:"configured"`
	Source               string `json:"source"` // "env" | "db" | "none"
	ClientID             string `json:"client_id,omitempty"`
	TenantID             string `json:"tenant_id,omitempty"`
	AuthorizeRedirectURI string `json:"authorize_redirect_uri"`
	MCPURL               string `json:"mcp_url,omitempty"`
	CanEdit              bool   `json:"can_edit"`
}

func (s *Server) resolveMS365Config(ctx context.Context) ms365ConfigResponse {
	publicURL := ""
	if s.cfg != nil && strings.TrimSpace(s.cfg.PublicBaseURL) != "" {
		publicURL = strings.TrimRight(strings.TrimSpace(s.cfg.PublicBaseURL), "/")
	}
	if envPub := strings.TrimRight(strings.TrimSpace(envreg.Get("MS365_MCP_PUBLIC_URL")), "/"); envPub != "" {
		publicURL = envPub
	}
	redirectURI := ""
	if s.remoteMCP != nil && s.remoteMCP.Enabled() {
		redirectURI = s.remoteMCP.RedirectURI()
	} else if publicURL != "" {
		redirectURI = publicURL + "/api/oauth/mcp/callback"
	}
	mcpURL := ""
	if publicURL != "" {
		mcpURL = publicURL + "/mcp"
	}
	resp := ms365ConfigResponse{
		Source:               "none",
		AuthorizeRedirectURI: redirectURI,
		MCPURL:               mcpURL,
		CanEdit:              s.callerIsAdmin(ctx),
	}

	if st, ok := s.store.(ms365ConfigStore); ok && st != nil {
		if cid, tid, found, err := st.MS365MCPConfig(ctx); err == nil && found && strings.TrimSpace(cid) != "" {
			if strings.TrimSpace(tid) == "" {
				tid = "common"
			}
			resp.Configured = true
			resp.Source = "db"
			resp.ClientID = strings.TrimSpace(cid)
			resp.TenantID = strings.TrimSpace(tid)
			return resp
		}
	}

	ms365Cfg := ms365mcp.ConfigFromEnv(s.cfg)
	if cid, tid, src, ok := ms365Cfg.EffectiveCredentials(ctx); ok && cid != "" {
		resp.Configured = true
		resp.Source = src
		resp.ClientID = cid
		resp.TenantID = tid
		return resp
	}
	return resp
}

// handleMS365Config serves GET/PUT/DELETE /mcp/ms365-config.
// GET is member-authed (so the Settings -> Connections directory knows whether
// the workspace Entra app is configured and what Redirect URI to display).
// PUT and DELETE are admin-gated via requireAdmin.
func (s *Server) handleMS365Config(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, r, s.resolveMS365Config(r.Context()))

	case http.MethodPut:
		if !s.requireAdmin(w, r) {
			return
		}
		st, ok := s.store.(ms365ConfigStore)
		if !ok || st == nil {
			http.Error(w, "workspace settings store unavailable", http.StatusNotImplemented)
			return
		}
		var body struct {
			ClientID string `json:"client_id"`
			TenantID string `json:"tenant_id"`
		}
		if !decodeJSONBody(w, r, &body) {
			return
		}
		clientID := strings.TrimSpace(body.ClientID)
		tenantID := strings.TrimSpace(body.TenantID)
		if tenantID == "" {
			tenantID = "common"
		}
		if !ms365IdentifierRe.MatchString(clientID) {
			http.Error(w, "client_id must be a valid Microsoft Entra Application (client) ID", http.StatusBadRequest)
			return
		}
		if !ms365IdentifierRe.MatchString(tenantID) {
			http.Error(w, "tenant_id must be a valid Microsoft Entra Directory (tenant) ID, domain, or 'organizations'/'common'", http.StatusBadRequest)
			return
		}
		if err := st.SetMS365MCPConfig(r.Context(), clientID, tenantID, userFromCtx(r.Context())); err != nil {
			http.Error(w, "save Microsoft 365 configuration: "+err.Error(), http.StatusInternalServerError)
			return
		}
		ms365mcp.SetCachedCredentials(clientID, tenantID)
		writeJSON(w, r, s.resolveMS365Config(r.Context()))

	case http.MethodDelete:
		if !s.requireAdmin(w, r) {
			return
		}
		st, ok := s.store.(ms365ConfigStore)
		if !ok || st == nil {
			http.Error(w, "workspace settings store unavailable", http.StatusNotImplemented)
			return
		}
		if err := st.DeleteMS365MCPConfig(r.Context()); err != nil {
			http.Error(w, "reset Microsoft 365 configuration: "+err.Error(), http.StatusInternalServerError)
			return
		}
		ms365mcp.SetCachedCredentials("", "")
		writeJSON(w, r, s.resolveMS365Config(r.Context()))

	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
