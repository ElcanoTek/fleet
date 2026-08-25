package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
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
	Name     string `json:"name"`
	URL      string `json:"url"`
	AuthMode string `json:"auth,omitempty"`
	// Account is the seat label for a further login under an existing name
	// (#988); empty adds the unlabeled seat.
	Account string `json:"account,omitempty"`
	// Optional manual client credentials for an AS without dynamic registration.
	ClientID     string `json:"client_id,omitempty"`
	ClientSecret string `json:"client_secret,omitempty"`
	// api_key mode: the key is write-only (sealed at rest, never in a response);
	// the header is the NAME to send it under ("" = Authorization: Bearer).
	APIKey       string `json:"api_key,omitempty"`
	APIKeyHeader string `json:"api_key_header,omitempty"`
	APIKeyQuery  string `json:"api_key_query,omitempty"`
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
		// Sharing decorations: the grants on the user's own servers (owner-only
		// management surface) and the servers others shared WITH them (usable in
		// their runs; read-only here, with the owner named for attribution).
		shares, err := s.remoteMCP.SharesByOwner(r.Context(), user)
		if err != nil {
			s.remoteMCPError(w, err)
			return
		}
		sharedRows, err := s.remoteMCP.SharedWithMe(r.Context(), user)
		if err != nil {
			s.remoteMCPError(w, err)
			return
		}
		sharedWithMe := make([]sharedServerView, 0, len(sharedRows))
		for _, m := range sharedRows {
			sharedWithMe = append(sharedWithMe, sharedServerView{RemoteMCPServer: m, Owner: m.UserEmail})
		}
		writeJSON(w, map[string]any{
			"servers":        servers,
			"shares":         shares,
			"shared_with_me": sharedWithMe,
		})
	case http.MethodPost:
		var req addRemoteMCPRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		server, toolCount, err := s.remoteMCP.AddServer(r.Context(), remotemcp.AddServerInput{
			Email:        user,
			Name:         req.Name,
			Account:      req.Account,
			URL:          req.URL,
			AuthMode:     req.AuthMode,
			ClientID:     req.ClientID,
			ClientSecret: req.ClientSecret,
			APIKey:       req.APIKey,
			APIKeyHeader: req.APIKeyHeader,
			APIKeyQuery:  req.APIKeyQuery,
		})
		if err != nil {
			s.remoteMCPError(w, err)
			return
		}
		// Probed adds (open/api_key) validated the connection with a real MCP
		// handshake; surface the observed tool count so the UI can confirm
		// "connected — N tools" instead of a bare "added".
		if toolCount >= 0 {
			writeJSON(w, struct {
				*store.RemoteMCPServer
				ToolCount int `json:"tool_count"`
			}{server, toolCount})
			return
		}
		writeJSON(w, server)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// remoteMCPServerByID handles DELETE /remote-mcp-servers/{id},
// POST /remote-mcp-servers/{id}/authorize, POST .../{id}/default and
// PUT .../{id}/account (#988 seats), plus the share/key/signout sub-routes.
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
	case sub == "shares" && r.Method == http.MethodPost:
		var req shareRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.remoteMCP.ShareServer(r.Context(), user, id, req.Grantee); err != nil {
			s.remoteMCPError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case strings.HasPrefix(sub, "shares/") && r.Method == http.MethodDelete:
		grantee, err := url.PathUnescape(strings.TrimPrefix(sub, "shares/"))
		if err != nil || grantee == "" {
			http.Error(w, "grantee required", http.StatusBadRequest)
			return
		}
		if err := s.remoteMCP.UnshareServer(r.Context(), user, id, grantee); err != nil {
			s.remoteMCPError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case sub == "key" && r.Method == http.MethodPut:
		// Rotate / correct an api_key connection's key. Write-only: the key is
		// sealed at rest and never appears in any response. The new key is
		// validated with a real MCP handshake before it replaces the old one;
		// the probed tool count comes back for the UI's confirmation notice.
		var req struct {
			APIKey string `json:"api_key"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		toolCount, err := s.remoteMCP.SetAPIKey(r.Context(), user, id, req.APIKey)
		if err != nil {
			s.remoteMCPError(w, err)
			return
		}
		writeJSON(w, map[string]any{"ok": true, "tool_count": toolCount})
	case sub == "default" && r.Method == http.MethodPost:
		// Make this seat the default among the caller's seats of the same
		// connection name (#988). Owner-only; idempotent.
		if err := s.remoteMCP.SetDefaultSeat(r.Context(), user, id); err != nil {
			s.remoteMCPError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case sub == "account" && r.Method == http.MethodPut:
		// Rename a seat's public label (#988). The credential is untouched.
		var req struct {
			Account string `json:"account"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		if err := s.remoteMCP.RenameSeat(r.Context(), user, id, req.Account); err != nil {
			s.remoteMCPError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case sub == "signout" && r.Method == http.MethodPost:
		if err := s.remoteMCP.SignOut(r.Context(), user, id); err != nil {
			s.remoteMCPError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
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

// shareRequest grants use of a connection to another user on this box
// ("grantee" is a user email, or "*" for everyone).
type shareRequest struct {
	Grantee string `json:"grantee"`
}

// sharedServerView is a server another user shared with the caller: the normal
// row plus the owner's email for attribution (UserEmail itself never
// serializes).
type sharedServerView struct {
	store.RemoteMCPServer
	Owner string `json:"owner"`
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
	case errors.Is(err, store.ErrRemoteMCPSeatExists):
		http.Error(w, err.Error(), http.StatusConflict)
	case errors.Is(err, store.ErrRemoteMCPAccountInvalid):
		http.Error(w, err.Error(), http.StatusBadRequest)
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
