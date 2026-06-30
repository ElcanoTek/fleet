// Read-only public conversation sharing (#226).
//
// An authenticated owner issues an unguessable share token for one of their
// conversations (POST /conversations/{id}/share) and hands the link to anyone.
// The public GET /shared/{token} returns a read-only snapshot — title, model,
// and the message thread — with the conversation id and author email
// deliberately omitted. The token is 256 bits of crypto/rand: token entropy is
// the confidentiality guarantee, and a per-token rate limit is the abuse gate.
//
// The public endpoint is token-gated (shared secret, so only the trusted Next
// proxy reaches it) but identity-less — the share token in the path is the
// authorization. Revoking (DELETE) NULLs the token; expiry is enforced
// server-side in the store lookup.

package httpapi

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// sharedReadsPerMinutePerToken bounds GET /shared/{token} per share token —
// generous for real viewers of a popular link, a hard ceiling against scraping
// or DDoS amplification of a single share. Keyed by token (not IP) because the
// endpoint sits behind the Next proxy.
const sharedReadsPerMinutePerToken = 120

// shareTokenBytes is the entropy of a share token before base64url encoding.
// 32 bytes = 256 bits, brute-force infeasible.
const shareTokenBytes = 32

// handleConversationShare issues (or rotates) the public read-only share token
// for a conversation the caller owns. Body is optional:
//
//	{ "expires_at": <unix seconds> }   // omitted / null = never expires
//
// Returns 201 with {"url": "/shared/<token>", "token": "<token>"}.
func (s *Server) handleConversationShare(w http.ResponseWriter, r *http.Request, convID, user string) {
	// Confirm the conversation exists and belongs to the caller before minting a
	// token (Get is user-scoped, so a foreign/unknown id yields nil → 404).
	conv, err := s.store.Get(r.Context(), user, convID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if conv == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}

	var body struct {
		ExpiresAt *int64 `json:"expires_at"`
	}
	// Body is optional; an empty body decodes to the zero value.
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	if body.ExpiresAt != nil && *body.ExpiresAt <= time.Now().Unix() {
		http.Error(w, "expires_at must be a unix timestamp in the future", http.StatusBadRequest)
		return
	}

	raw := make([]byte, shareTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		http.Error(w, "token generation failed", http.StatusInternalServerError)
		return
	}
	token := base64.RawURLEncoding.EncodeToString(raw)

	if err := s.store.SetShareToken(r.Context(), user, convID, token, body.ExpiresAt); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSONStatus(w, http.StatusCreated, map[string]any{
		"url":   "/shared/" + token,
		"token": token,
	})
}

// handleConversationUnshare revokes sharing for a conversation the caller owns.
// A non-owned/unknown id is 404 (the ownership pre-check via Get, mirroring the
// share path); for an owned conversation it is idempotent — revoking when
// already unshared still returns 204, so the UI can call it without checking
// current state.
func (s *Server) handleConversationUnshare(w http.ResponseWriter, r *http.Request, convID, user string) {
	conv, err := s.store.Get(r.Context(), user, convID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if conv == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	if err := s.store.RevokeShareToken(r.Context(), user, convID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleConversationShareWithTeam toggles a conversation's team-visibility flag
// (#237) — the OWNER opting their thread into (or out of) read-only visibility
// for same-team members. This is distinct from public share tokens (#226):
// team visibility is gated by shared team_id, never mints a public link, and is
// the only path by which one teammate's conversation becomes readable by
// another. Body: { "visible": bool } (default false = un-share). The store
// gates on ownership, so a non-owned/unknown id is 404.
func (s *Server) handleConversationShareWithTeam(w http.ResponseWriter, r *http.Request, convID, user string) {
	var body struct {
		Visible bool `json:"visible"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	err := s.store.SetConversationTeamVisible(r.Context(), user, convID, body.Visible)
	if err != nil {
		// The store returns a plain "conversation not found" when the caller
		// doesn't own a conversation with this id → 404, matching the share path.
		if strings.Contains(err.Error(), "not found") {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"team_visible": body.Visible})
}

// handleSharedConversation serves the public read-only snapshot for a share
// token. Token-gated (shared secret) but identity-less; the token in the path
// is the authorization. Per-token rate-limited; returns 404 for an unknown,
// revoked, or expired token (indistinguishable, so a probe can't tell which).
func (s *Server) handleSharedConversation(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	token := strings.Trim(strings.TrimPrefix(r.URL.Path, "/shared/"), "/")
	if token == "" {
		http.Error(w, "missing token", http.StatusBadRequest)
		return
	}
	if ok, _ := s.shareRL.Allow(token); !ok {
		http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
		return
	}
	conv, err := s.store.GetConversationByShareToken(r.Context(), token, time.Now().Unix())
	if err != nil {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	if conv == nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	writeJSON(w, conv)
}
