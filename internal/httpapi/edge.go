// Copyright (c) 2026 ElcanoTek
// SPDX-License-Identifier: MIT

package httpapi

import (
	"context"
	"net/http"
	"strings"

	"github.com/ElcanoTek/fleet/internal/principal"
	"github.com/ElcanoTek/fleet/internal/sched/apikeys"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// keyValidator is the API-key store the chat listener's edge checks a
// presented key against (apikeys.Manager, the orchestrator's store). Deleting
// an account deletes the user keys that act as it through the same store
// (internal/accountdelete).
type keyValidator interface {
	ValidateKeyOn(ctx context.Context, s apikeys.Surface, rawKey string, requiredPermission *models.Permission, ipAddress, userAgent *string) (bool, *apikeys.APIKey, string)
	BindUnboundUserKeys(ctx context.Context, email, accountID string) (int, error)
	DeleteUserKeys(ctx context.Context, email, accountID string) (int, error)
	RecordUserKeyAccount(ctx context.Context, keyID, accountID string) error
}

// WithAPIKeys lets the chat listener's own port accept user API keys
// (ADR-0108). Without it only the gateway's in-process calls authenticate.
func WithAPIKeys(v keyValidator) Option {
	return func(s *Server) { s.apiKeys = v }
}

// retiredServiceTokenHeader is the header of the internal service-token
// channel ADR-0108 removed; nothing honours it.
const retiredServiceTokenHeader = "X-Chat-Server-Token" //nolint:gosec // G101: a header NAME, not a credential.

// edgePrincipal resolves the credential a request presented on the chat
// listener's own port, for a request the gateway did not attach a principal
// to — `fleet chat`, `fleet acp` and fleet-bench on the box. It writes the
// refusal itself and reports false.
//
// The credential is a user API key (apikeys.KeyTypeUser), as X-API-Key or
// Authorization: Bearer. The principal is the account the key names; the
// membership gate resolves that account on every request, so disabling it
// ends the key's access too, and admits it only for the account it is bound
// to (principal.Principal.KeyAccountID; membershipMiddleware binds a key
// minted before keys named their account on its first use). No header names
// a person: a caller still presenting the retired service token gets a 401
// that says what replaced it, never a silent fall-through.
func (s *Server) edgePrincipal(w http.ResponseWriter, r *http.Request) (principal.Principal, bool) {
	if r.Header.Get(retiredServiceTokenHeader) != "" {
		writeJSONStatus(w, r, http.StatusUnauthorized, map[string]string{
			"error":   "service_token_retired",
			"message": "the internal service token (X-Chat-Server-Token, FLEET_SERVER_TOKEN) was removed (ADR-0108): authenticate with a user API key in X-API-Key (fleet sched apikey create <name> --type user --user <email>)",
		})
		return principal.Principal{}, false
	}
	raw, method := strings.TrimSpace(r.Header.Get("X-API-Key")), principal.MethodAPIKey
	if raw == "" {
		raw, method = bearerCredential(r), principal.MethodBearer
	}
	if raw == "" || s.apiKeys == nil {
		writeJSONStatus(w, r, http.StatusUnauthorized, map[string]string{
			"error":   "authentication_required",
			"message": "send a user API key in X-API-Key (fleet sched apikey create <name> --type user --user <email>)",
		})
		return principal.Principal{}, false
	}
	// SurfaceChat admits user keys only (apikeys.KeyType.AllowsSurface).
	ok, key, reason := s.apiKeys.ValidateKeyOn(r.Context(), apikeys.SurfaceChat, raw, nil, nil, nil)
	if (!ok && reason == apikeys.ReasonWrongSurface) || (ok && key != nil && strings.TrimSpace(key.UserEmail) == "") {
		writeJSONStatus(w, r, http.StatusForbidden, map[string]string{
			"error":   "not_a_user_key",
			"message": "this key names no user; the chat API takes a user key (fleet sched apikey create <name> --type user --user <email>)",
		})
		return principal.Principal{}, false
	}
	if !ok || key == nil {
		writeJSONStatus(w, r, http.StatusUnauthorized, map[string]string{"error": "invalid_api_key", "message": reason})
		return principal.Principal{}, false
	}
	// A key minted before keys named their account, whose email had no
	// account when it was bound, acts as no one (apikeys.APIKey.AccountMissing).
	if key.AccountMissing {
		writeKeyPredatesAccount(w)
		return principal.Principal{}, false
	}
	return principal.Principal{
		Email:        strings.ToLower(strings.TrimSpace(key.UserEmail)),
		KeyID:        key.KeyID,
		KeyAccountID: key.AccountID,
		AuthMethod:   method,
	}, true
}

// bearerCredential is an `Authorization: Bearer` credential, or "".
func bearerCredential(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if len(auth) < 7 || !strings.EqualFold(auth[:7], "bearer ") {
		return ""
	}
	return strings.TrimSpace(auth[7:])
}
