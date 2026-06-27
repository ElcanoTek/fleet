// Admin endpoints — operator-facing observability for the 10-20 user box.
//
// Security: gated by the ADMIN_EMAILS env allowlist. An authenticated user
// whose email isn't in that list gets 403 on /admin/*. Not meant as a
// proper RBAC layer — the server is single-tenant by design.

package httpapi

import (
	"encoding/json"
	"net/http"
	"strings"
)

// adminStats is the JSON shape the /admin page renders. Intentionally
// flat so it's easy to template on the frontend.
type adminStats struct {
	Users []userStat `json:"users"`
}

type userStat struct {
	Email                    string  `json:"email"`
	ConversationCount        int     `json:"conversation_count"`
	PinnedCount              int     `json:"pinned_count"`
	LastActivity             int64   `json:"last_activity"` // unix seconds
	TotalCostUSD             float64 `json:"total_cost_usd"`
	TotalTurns               int     `json:"total_turns"`
	TotalCachedTokens        int64   `json:"total_cached_tokens"`
	TotalCacheCreationTokens int64   `json:"total_cache_creation_tokens"`
	// CacheHitRatePct is cached_tokens / prompt_tokens * 100 — the share of
	// input tokens served from the prompt cache. 0 when no prompt tokens.
	CacheHitRatePct float64 `json:"cache_hit_rate_pct"`
}

// isAdmin returns true when the authenticated user is in the ADMIN_EMAILS
// allowlist. Case-insensitive.
func (s *Server) isAdmin(email string) bool {
	if len(s.cfg.AdminEmails) == 0 {
		return false
	}
	needle := strings.ToLower(strings.TrimSpace(email))
	for _, a := range s.cfg.AdminEmails {
		if a == needle {
			return true
		}
	}
	return false
}

// adminMiddleware rejects non-admin users with 403. Runs after authMiddleware.
func (s *Server) adminMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user := userFromCtx(r.Context())
		if !s.isAdmin(user) {
			http.Error(w, "forbidden — not an admin", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// handleAdminStats aggregates conversation + cost data per user.
// Kept as a single query-oriented endpoint so the frontend can render the
// whole table in one round trip.
func (s *Server) handleAdminStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	stats, err := s.store.AdminStats(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	users := make([]userStat, 0, len(stats))
	for _, row := range stats {
		var hitRate float64
		if row.TotalPromptTokens > 0 {
			hitRate = float64(row.TotalCachedTokens) / float64(row.TotalPromptTokens) * 100.0
		}
		users = append(users, userStat{
			Email:                    row.Email,
			ConversationCount:        row.ConversationCount,
			PinnedCount:              row.PinnedCount,
			LastActivity:             row.LastActivity,
			TotalCostUSD:             row.TotalCostUSD,
			TotalTurns:               row.TotalTurns,
			TotalCachedTokens:        row.TotalCachedTokens,
			TotalCacheCreationTokens: row.TotalCacheCreationTokens,
			CacheHitRatePct:          hitRate,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(adminStats{Users: users})
}
