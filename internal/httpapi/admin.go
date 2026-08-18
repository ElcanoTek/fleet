// Admin endpoints — operator-facing observability for the 10-20 user box.
//
// Security: gated by the ADMIN_EMAILS env allowlist. An authenticated user
// whose email isn't in that list gets 403 on /admin/*. Not meant as a
// proper RBAC layer — the server is single-tenant by design.

package httpapi

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/ElcanoTek/fleet/internal/store"
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
// allowlist. Case-insensitive. This is the out-of-band bootstrap gate: it
// works before any DB role is assigned (so the first operator can mint other
// admins) and complements — does not replace — the users.role = 'admin' path
// checked in adminMiddleware (#237).
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

// adminMiddleware rejects non-admin users with 403. Runs after membershipMiddleware
// (which enriches the request with the DB role), so admin is granted to either
// the ADMIN_EMAILS env allowlist OR a users.role = 'admin' account (#237) — the
// two are an OR, never a downgrade of the env bootstrap.
func (s *Server) adminMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		user := userFromCtx(ctx)
		if !s.isAdmin(user) && roleFromCtx(ctx) != store.RoleAdmin {
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

// adminUser is the JSON shape of one row in the admin Users tab (#237).
type adminUser struct {
	Email     string `json:"email"`
	Role      string `json:"role"`
	TeamID    string `json:"team_id"`
	CreatedAt int64  `json:"created_at"`
	UpdatedAt int64  `json:"updated_at"`
	// OpsCenterAdmin reports whether the same email also holds the
	// Operations Center (sched-plane) admin role — the two-plane view the
	// `fleet admin list` CLI prints. Always false when no orchestrator seam
	// is wired (WithOpsAdmins), so it never claims access that can't exist.
	// Kept alongside OpsCenterRole for older clients.
	OpsCenterAdmin bool `json:"ops_center_admin"`
	// OpsCenterRole is the sched-plane role for the same email:
	// "admin" | "client" | "readonly" | "" (no Operations Center access).
	OpsCenterRole string `json:"ops_center_role"`
}

func toAdminUser(u store.User) adminUser {
	return adminUser{
		Email:     u.Email,
		Role:      u.Role,
		TeamID:    u.TeamID,
		CreatedAt: u.CreatedAt,
		UpdatedAt: u.UpdatedAt,
	}
}

// handleAdminUsers serves the /admin/users collection (#237): GET lists every
// provisioned account with its role + team (+ the ops-center annotation when
// the orchestrator seam is wired), POST creates one. Admin-gated.
func (s *Server) handleAdminUsers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		// fallthrough to the list below
	case http.MethodPost:
		s.handleAdminUserCreate(w, r)
		return
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	users, err := s.store.ListUsers(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// One batched ops-center lookup for the whole table (never per-row).
	// Best-effort: a briefly unreachable sched DB must not blank the user list.
	opsRoles := map[string]string{}
	if s.opsAdmins != nil {
		if roles, err := s.opsAdmins.Roles(r.Context()); err == nil {
			opsRoles = roles
		}
	}
	out := make([]adminUser, 0, len(users))
	for _, u := range users {
		au := toAdminUser(u)
		au.OpsCenterRole = opsRoles[strings.ToLower(u.Email)]
		au.OpsCenterAdmin = au.OpsCenterRole == "admin"
		out = append(out, au)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"users": out})
}

// handleAdminUserItem dispatches the /admin/users/{email}[/password] item
// routes: PATCH (role/team, #237), DELETE (remove account), and
// PUT {email}/password (reset). Admin-gated.
func (s *Server) handleAdminUserItem(w http.ResponseWriter, r *http.Request) {
	email := strings.TrimPrefix(r.URL.Path, "/admin/users/")
	// The single sub-resource: /admin/users/{email}/password.
	if rest, ok := strings.CutSuffix(email, "/password"); ok && rest != "" && !strings.Contains(rest, "/") {
		if r.Method != http.MethodPut {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		s.handleAdminUserPassword(w, r, rest)
		return
	}
	if email == "" || strings.Contains(email, "/") {
		http.Error(w, "user email required", http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodPatch:
		s.handleAdminUserPatch(w, r, email)
	case http.MethodDelete:
		s.handleAdminUserDelete(w, r, email)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleAdminUserPatch serves PATCH /admin/users/{email} — assign a role and/or
// team to one account (#237). Admin-gated. Body (both fields optional; a nil
// field is left untouched, an empty team_id clears the team):
//
//	{ "role": "viewer", "team_id": "growth" }
//
// Role writes carry the two-plane semantics of `fleet admin add`: promoting to
// admin also ensures the Operations Center admin row; demoting removes it.
// Demoting YOURSELF is refused — the same lockout guard as self-deletion.
func (s *Server) handleAdminUserPatch(w http.ResponseWriter, r *http.Request, email string) {
	var body struct {
		Role   *string `json:"role"`
		TeamID *string `json:"team_id"`
		// OpsRole sets the Operations Center (sched-plane) role explicitly:
		// "none" | "readonly" | "client" | "admin". Independent of the chat
		// role except that chat admin implies ops admin (below).
		OpsRole *string `json:"ops_role"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if body.Role == nil && body.TeamID == nil && body.OpsRole == nil {
		http.Error(w, "nothing to update (provide role, team_id and/or ops_role)", http.StatusBadRequest)
		return
	}
	if body.OpsRole != nil {
		switch *body.OpsRole {
		case "none", "readonly", "client", "admin":
		default:
			http.Error(w, "invalid ops_role (want none|readonly|client|admin)", http.StatusBadRequest)
			return
		}
	}
	// Self-lockout guard. It fires on an ACTUAL demotion only: the Users tab
	// PATCHes role and team together, so editing your OWN team arrives carrying
	// your unchanged role — and for an ADMIN_EMAILS bootstrap admin that role is
	// the column default "member". A blanket "self + role != admin" refusal
	// therefore made an env-admin unable to set their own team at all (#1157).
	// The write only costs you access when it clears a users.role = 'admin' that
	// is your sole grant; the env allowlist survives any column write.
	if body.Role != nil && *body.Role != store.RoleAdmin &&
		strings.EqualFold(strings.TrimSpace(email), strings.TrimSpace(userFromCtx(r.Context()))) {
		cur, err := s.store.GetUser(r.Context(), email)
		if err != nil {
			if errors.Is(err, store.ErrUserNotFound) {
				http.Error(w, "user not found", http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if cur.Role == store.RoleAdmin && !s.isAdmin(cur.Email) {
			http.Error(w, "refusing to demote your own account", http.StatusBadRequest)
			return
		}
	}
	u, err := s.store.SetUserRoleTeam(r.Context(), email, body.Role, body.TeamID)
	if err != nil {
		switch {
		case strings.Contains(err.Error(), "invalid role"):
			http.Error(w, err.Error(), http.StatusBadRequest)
		case err.Error() == "user not found":
			http.Error(w, "user not found", http.StatusNotFound)
		default:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	if body.Role != nil {
		if *body.Role == store.RoleAdmin {
			s.ensureOpsAdmin(r, u.Email)
		} else if body.OpsRole == nil {
			// Chat demote: drop an ops-ADMIN row (the implied grant), but leave
			// an explicit client/readonly grant standing — those are granted
			// deliberately via ops_role, not implied by the chat role.
			if roles, rerr := s.opsRolesBestEffort(r); rerr == nil && roles[strings.ToLower(u.Email)] == "admin" {
				s.removeOpsAdmin(r, u.Email)
			}
		}
		//nolint:gosec // G706: %q escapes CR/LF, so request-supplied values cannot forge a log line; role is store-validated.
		log.Printf("admin users: set role of %q to %s by %q", u.Email, u.Role, userFromCtx(r.Context()))
	}
	if body.OpsRole != nil {
		s.setOpsRole(r, u.Email, *body.OpsRole)
		//nolint:gosec // G706: %q escapes CR/LF; ops_role is validated above.
		log.Printf("admin users: set ops role of %q to %s by %q", u.Email, *body.OpsRole, userFromCtx(r.Context()))
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(s.toAdminUserAnnotated(r, *u))
}
