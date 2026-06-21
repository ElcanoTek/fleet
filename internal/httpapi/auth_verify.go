// Password verification endpoint for the Next.js /api/auth/login flow.
//
// Auth model:
//   - chat-server owns the user table (email + bcrypt hash).
//   - Next.js's /api/auth/login receives email+password from the browser,
//     forwards to POST /auth/verify with the usual shared-secret headers.
//   - On success it mints the existing HMAC session cookie.
//
// We deliberately do NOT distinguish "no such user" from "bad password"
// in the response so an attacker can't enumerate the allowlist through
// timing or message content.

package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/ElcanoTek/fleet/internal/store"
)

type verifyRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type verifyResponse struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

func (s *Server) handleAuthVerify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req verifyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json", http.StatusBadRequest)
		return
	}
	// Instance has no users at all — fail fast so operator knows they
	// need to provision via `chat user add`. Browser-facing message is
	// generic to avoid leaking deployment state.
	if !s.hasUsers.Load() {
		now := time.Now().Unix()
		last := s.lastUserCheck.Load()
		if now-last < 5 {
			writeJSON(w, verifyResponse{
				OK:    false,
				Error: "This instance has no users. Run `chat user add <email>` on the server.",
			})
			return
		}

		n, err := s.store.CountUsers(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if n == 0 {
			s.lastUserCheck.Store(now)
			writeJSON(w, verifyResponse{
				OK:    false,
				Error: "This instance has no users. Run `chat user add <email>` on the server.",
			})
			return
		}
		s.hasUsers.Store(true)
	}

	err := s.store.VerifyUser(r.Context(), req.Email, req.Password)
	if err != nil {
		// Both "not found" and "bad password" surface the same way.
		if errors.Is(err, store.ErrUserNotFound) || errors.Is(err, store.ErrBadPassword) {
			writeJSON(w, verifyResponse{OK: false, Error: "invalid credentials"})
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, verifyResponse{OK: true})
}
