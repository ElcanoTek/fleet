// Admin Users tab — full user management over HTTP (create / delete / reset
// password), completing the list + role/team PATCH that shipped with #237, so
// admins manage accounts from Settings → Admin instead of the box CLI.
//
// Two-plane admin semantics mirror `fleet admin add`: granting the chat role
// "admin" ALSO ensures the Operations Center admin row (sched DB, matched by
// email — the #458 cookie-bridge convention), and demoting/deleting removes it.
// The sched plane is reached through the injected opsAdmins seam only —
// httpapi stays sched-agnostic like the WorkerStats/TaskScheduler seams.

package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/ElcanoTek/fleet/internal/store"
)

// OpsAdmins is the Operations-Center admin seam, implemented in cmd/fleet by
// closures over the sched storage. All methods are keyed by (lowercased) email.
// Ensure is the same idempotent create-or-promote the
// FLEET_ORCHESTRATOR_BOOTSTRAP_ADMINS boot seed uses; Remove deletes the row
// (revoking Operations Center access entirely); List returns the emails that
// currently hold the sched-side admin role.
type OpsAdmins interface {
	Ensure(ctx context.Context, email string) error
	Remove(ctx context.Context, email string) error
	List(ctx context.Context) ([]string, error)
	// SetRole grants email the sched-plane role (admin|client|readonly);
	// role "none" (or "") removes the row entirely. The generalized form of
	// Ensure/Remove for the non-admin Operations Center roles.
	SetRole(ctx context.Context, email, role string) error
	// Roles returns every provisioned sched-plane account's role, keyed by
	// lowercased email — the batched lookup the users table renders from.
	Roles(ctx context.Context) (map[string]string, error)
}

// WithOpsAdmins injects the Operations-Center admin service. nil (the default)
// leaves the chat plane fully functional and simply skips the second plane —
// role writes then affect chat RBAC only, and the users list reports no
// ops-center annotation.
func WithOpsAdmins(svc OpsAdmins) Option {
	return func(s *Server) { s.opsAdmins = svc }
}

// handleAdminUserCreate serves POST /admin/users — provision a new account.
// Body: {"email": ..., "password": ..., "role": "member|viewer|admin"?,
// "team_id": ...?}. role defaults to member; role "admin" also ensures the
// Operations Center admin row (two-plane, like `fleet admin add`).
func (s *Server) handleAdminUserCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email    string  `json:"email"`
		Password string  `json:"password"`
		Role     *string `json:"role"`
		TeamID   *string `json:"team_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	email := strings.TrimSpace(body.Email)
	if email == "" {
		http.Error(w, "email required", http.StatusBadRequest)
		return
	}
	if body.Role != nil && !store.ValidRole(*body.Role) {
		http.Error(w, "invalid role (want member|viewer|admin)", http.StatusBadRequest)
		return
	}

	u, err := s.store.CreateUser(r.Context(), email, body.Password)
	if err != nil {
		switch {
		case strings.Contains(err.Error(), "already exists"):
			http.Error(w, err.Error(), http.StatusConflict)
		case strings.Contains(err.Error(), "at least 8 characters"),
			strings.Contains(err.Error(), "email required"):
			http.Error(w, err.Error(), http.StatusBadRequest)
		default:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	// Role/team are a second write (CreateUser inserts with the column-default
	// member role). A failure here leaves a valid member account, which the
	// admin can still PATCH — report it rather than unwinding the create.
	if body.Role != nil || body.TeamID != nil {
		if u, err = s.store.SetUserRoleTeam(r.Context(), email, body.Role, body.TeamID); err != nil {
			http.Error(w, "user created, but assigning role/team failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	if body.Role != nil && *body.Role == store.RoleAdmin {
		s.ensureOpsAdmin(r, u.Email)
	}
	// %q escapes CR/LF, so request-supplied values cannot forge a log line.
	log.Printf("admin users: created %q (role=%s) by %q", u.Email, u.Role, userFromCtx(r.Context()))
	writeJSONStatus(w, http.StatusCreated, s.toAdminUserAnnotated(r, *u))
}

// handleAdminUserDelete serves DELETE /admin/users/{email} — remove the account
// and its owned data (conversations, memories, remote MCP, projects; the store
// cascade), plus the Operations Center row. Self-deletion is refused so an
// admin cannot lock themselves out mid-session.
func (s *Server) handleAdminUserDelete(w http.ResponseWriter, r *http.Request, email string) {
	actor := userFromCtx(r.Context())
	if strings.EqualFold(strings.TrimSpace(email), strings.TrimSpace(actor)) {
		http.Error(w, "refusing to delete your own account", http.StatusBadRequest)
		return
	}
	if err := s.store.DeleteUser(r.Context(), email); err != nil {
		if errors.Is(err, store.ErrUserNotFound) {
			http.Error(w, "user not found", http.StatusNotFound)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.removeOpsAdmin(r, email)
	//nolint:gosec // G706: %q escapes CR/LF, so the path-supplied email cannot forge a log line.
	log.Printf("admin users: deleted %q by %q", email, actor)
	w.WriteHeader(http.StatusNoContent)
}

// handleAdminUserPassword serves PUT /admin/users/{email}/password — reset an
// account's password. Body: {"password": ...}. The new password is never
// logged; the audit line records only actor + target.
func (s *Server) handleAdminUserPassword(w http.ResponseWriter, r *http.Request, email string) {
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if err := s.store.UpdatePassword(r.Context(), email, body.Password); err != nil {
		switch {
		case errors.Is(err, store.ErrUserNotFound):
			http.Error(w, "user not found", http.StatusNotFound)
		case strings.Contains(err.Error(), "at least 8 characters"):
			http.Error(w, err.Error(), http.StatusBadRequest)
		default:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	//nolint:gosec // G706: %q escapes CR/LF, so the path-supplied email cannot forge a log line.
	log.Printf("admin users: password reset for %q by %q", email, userFromCtx(r.Context()))
	w.WriteHeader(http.StatusNoContent)
}

// setOpsRole applies an explicit Operations Center role ("none" removes the
// row). Best-effort like ensureOpsAdmin — the chat plane is untouched either
// way; failures are logged, never unwound.
func (s *Server) setOpsRole(r *http.Request, email, role string) {
	if s.opsAdmins == nil {
		return
	}
	if err := s.opsAdmins.SetRole(r.Context(), email, role); err != nil {
		//nolint:gosec // G706: %q escapes CR/LF; role is validated before this call.
		log.Printf("WARNING: admin users: set ops role %q for %q failed: %v", role, email, err)
	}
}

// opsRolesBestEffort returns the sched-plane role map, or an error when the
// seam is absent/unreachable (callers then skip conditional ops writes).
func (s *Server) opsRolesBestEffort(r *http.Request) (map[string]string, error) {
	if s.opsAdmins == nil {
		return nil, errors.New("no ops seam")
	}
	return s.opsAdmins.Roles(r.Context())
}

// ensureOpsAdmin / removeOpsAdmin apply the second (Operations Center) plane
// best-effort: the chat-plane write already succeeded, so a sched-plane
// failure is reported in the log — never by unwinding the chat write. nil seam
// (no orchestrator wired) is a silent no-op.
func (s *Server) ensureOpsAdmin(r *http.Request, email string) {
	if s.opsAdmins == nil {
		return
	}
	if err := s.opsAdmins.Ensure(r.Context(), email); err != nil {
		//nolint:gosec // G706: %q escapes CR/LF, so the request-supplied email cannot forge a log line.
		log.Printf("admin users: WARNING ops-center admin grant for %q failed: %v", email, err)
	}
}

func (s *Server) removeOpsAdmin(r *http.Request, email string) {
	if s.opsAdmins == nil {
		return
	}
	if err := s.opsAdmins.Remove(r.Context(), email); err != nil {
		//nolint:gosec // G706: %q escapes CR/LF, so the request-supplied email cannot forge a log line.
		log.Printf("admin users: WARNING ops-center admin removal for %q failed: %v", email, err)
	}
}

// toAdminUserAnnotated is toAdminUser plus the ops-center flag for one user
// (used on single-user responses; the list endpoint batches the lookup).
func (s *Server) toAdminUserAnnotated(r *http.Request, u store.User) adminUser {
	out := toAdminUser(u)
	if s.opsAdmins != nil {
		if roles, err := s.opsAdmins.Roles(r.Context()); err == nil {
			out.OpsCenterRole = roles[strings.ToLower(u.Email)]
			out.OpsCenterAdmin = out.OpsCenterRole == "admin"
		}
	}
	return out
}

// handleAdminTeamRename serves POST /admin/teams/rename — relabel a team
// everywhere it appears (users + team-shared projects, one transaction).
// Body: {"from": "devops", "to": "platform"}. Admin-gated at the route.
func (s *Server) handleAdminTeamRename(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	usersN, projectsN, err := s.store.RenameTeam(r.Context(), body.From, body.To)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	//nolint:gosec // G706: %q escapes CR/LF, so request-supplied values cannot forge a log line.
	log.Printf("admin teams: renamed %q -> %q (%d users, %d projects) by %q",
		body.From, body.To, usersN, projectsN, userFromCtx(r.Context()))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"users_updated": usersN, "projects_updated": projectsN})
}
