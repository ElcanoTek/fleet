package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/ElcanoTek/fleet/internal/remotemcp"
	"github.com/ElcanoTek/fleet/internal/store"
)

// Per-user remote (hosted) MCP server + OAuth endpoints (#443). Every handler
// is behind auth+membership, so userFromCtx is a provisioned user's email. The
// service enforces per-user scoping and never returns secrets in any response.

// remoteMCPEnabled reports whether the feature is usable; handlers short-circuit
// with a clear 503 otherwise so the UI can render "not configured".
func (s *Server) remoteMCPReady(w http.ResponseWriter) bool {
	if s.remoteMCP == nil || !s.remoteMCP.Enabled() {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"remote_mcp_disabled","detail":"remote MCP OAuth is not configured on this server (set FLEET_MCP_OAUTH_ENCRYPTION_KEY and FLEET_PUBLIC_BASE_URL)"}`))
		return false
	}
	return true
}

type addRemoteMCPRequest struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	// Optional manual client credentials for an AS without dynamic registration.
	ClientID     string `json:"client_id,omitempty"`
	ClientSecret string `json:"client_secret,omitempty"`
}

// remoteMCPServers handles GET (list) and POST (add) on /remote-mcp-servers.
func (s *Server) remoteMCPServers(w http.ResponseWriter, r *http.Request) {
	if !s.remoteMCPReady(w) {
		return
	}
	user := userFromCtx(r.Context())
	switch r.Method {
	case http.MethodGet:
		servers, err := s.remoteMCP.ListServers(r.Context(), user)
		if err != nil {
			s.remoteMCPError(w, err)
			return
		}
		if servers == nil {
			servers = []store.RemoteMCPServer{}
		}
		writeJSON(w, map[string]any{"servers": servers})
	case http.MethodPost:
		var req addRemoteMCPRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		server, err := s.remoteMCP.AddServer(r.Context(), remotemcp.AddServerInput{
			Email:        user,
			Name:         req.Name,
			URL:          req.URL,
			ClientID:     req.ClientID,
			ClientSecret: req.ClientSecret,
		})
		if err != nil {
			s.remoteMCPError(w, err)
			return
		}
		writeJSON(w, server)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// remoteMCPServerByID handles DELETE /remote-mcp-servers/{id} and
// POST /remote-mcp-servers/{id}/authorize.
func (s *Server) remoteMCPServerByID(w http.ResponseWriter, r *http.Request) {
	if !s.remoteMCPReady(w) {
		return
	}
	user := userFromCtx(r.Context())
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/remote-mcp-servers/"), "/")
	if rest == "" {
		http.Error(w, "server id required", http.StatusBadRequest)
		return
	}
	parts := strings.SplitN(rest, "/", 2)
	id := parts[0]
	sub := ""
	if len(parts) == 2 {
		sub = parts[1]
	}

	switch {
	case sub == "authorize" && r.Method == http.MethodPost:
		authURL, err := s.remoteMCP.Authorize(r.Context(), user, id)
		if err != nil {
			s.remoteMCPError(w, err)
			return
		}
		writeJSON(w, map[string]any{"redirect_url": authURL})
	case sub == "" && r.Method == http.MethodDelete:
		if err := s.remoteMCP.Disconnect(r.Context(), user, id); err != nil {
			s.remoteMCPError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

type oauthCallbackRequest struct {
	State string `json:"state"`
	Code  string `json:"code"`
	// Error carries an OAuth error code (e.g. access_denied) the AS returned to
	// the redirect instead of a code — the user declined or the AS rejected.
	Error string `json:"error,omitempty"`
}

// remoteMCPOAuthCallback completes an OAuth flow. The browser-facing Next.js
// callback route relays the AS's redirect (code + state, or an error) here; we
// never expose chat-server to the browser directly. The completing user
// (X-User-Email) must equal the user who initiated the flow — enforced in the
// service via the stored, single-use state.
func (s *Server) remoteMCPOAuthCallback(w http.ResponseWriter, r *http.Request) {
	if !s.remoteMCPReady(w) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	user := userFromCtx(r.Context())
	var req oauthCallbackRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if req.Error != "" {
		// The user declined or the AS errored; surface it without treating it as a
		// server failure.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "authorization_failed", "detail": req.Error})
		return
	}
	if req.State == "" || req.Code == "" {
		http.Error(w, "missing state or code", http.StatusBadRequest)
		return
	}
	server, err := s.remoteMCP.Complete(r.Context(), user, req.State, req.Code)
	if err != nil {
		s.remoteMCPError(w, err)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "server_id": server.ID, "name": server.Name, "status": server.Status})
}

// remoteMCPError maps service/store errors to HTTP statuses. The error text is
// non-secret (the service is careful never to embed tokens in errors).
func (s *Server) remoteMCPError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrRemoteMCPNotFound):
		http.Error(w, "remote MCP server not found", http.StatusNotFound)
	case errors.Is(err, store.ErrOAuthFlowNotFound):
		http.Error(w, "authorization session expired or already used — start the connection again", http.StatusConflict)
	case errors.Is(err, store.ErrRemoteMCPNeedsReauth):
		http.Error(w, "this connection needs to be re-authorized", http.StatusConflict)
	case errors.Is(err, remotemcp.ErrManualClientRequired):
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
	case errors.Is(err, remotemcp.ErrDisabled):
		http.Error(w, "remote MCP OAuth is not configured", http.StatusServiceUnavailable)
	default:
		// Discovery / DCR / network / bad-URL failures: a 400 keeps it
		// actionable (the user can fix the URL or try a different server)
		// without leaking internal detail.
		http.Error(w, err.Error(), http.StatusBadRequest)
	}
}
