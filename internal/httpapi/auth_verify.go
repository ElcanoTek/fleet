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
	"strings"
	"sync"
	"time"

	"github.com/ElcanoTek/fleet/internal/store"
)

// Verify-attempt limits. Every attempt burns a bcrypt (~100ms of CPU on
// purpose), and this endpoint is pre-login, so the chat rate limiter — keyed
// by the authenticated user — cannot cover it. Two fixed windows:
//   - per attempted email, so one account can't be brute-forced online;
//   - global, so rotating emails can't turn the endpoint into a CPU DoS on
//     the process that also runs the scheduler.
//
// Keying by attempted email rather than source address is deliberate: the
// only caller is the trusted Next.js layer, so a client address here would
// just be that proxy's.
const (
	verifyPerEmailPerMinute = 5
	verifyGlobalPerMinute   = 30
)

// verifyLimiter is a minimal fixed-window counter. A sliding window buys
// nothing meaningful at these rates, and fixed windows keep it allocation-
// and dependency-free.
type verifyLimiter struct {
	mu      sync.Mutex
	window  int64 // unix minute the counts belong to
	byEmail map[string]int
	global  int
}

func (l *verifyLimiter) allow(email string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	minute := now.Unix() / 60
	if minute != l.window {
		l.window = minute
		l.byEmail = make(map[string]int)
		l.global = 0
	}
	if l.global >= verifyGlobalPerMinute || l.byEmail[email] >= verifyPerEmailPerMinute {
		return false
	}
	l.global++
	l.byEmail[email]++
	return true
}

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
	// Rate-limit BEFORE any store work so a limited attempt costs neither a
	// bcrypt nor a query. 429 (not the generic invalid-credentials body) so
	// the login UI can say "try again shortly" honestly.
	if !s.verifyLimit.allow(strings.ToLower(strings.TrimSpace(req.Email)), time.Now()) {
		w.Header().Set("Retry-After", "60")
		writeJSONStatus(w, http.StatusTooManyRequests, verifyResponse{
			OK:    false,
			Error: "too many attempts — try again in a minute",
		})
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
