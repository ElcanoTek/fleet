// Admin Users tab — full user management over HTTP (create / delete / reset
// password), completing the list + role/team PATCH that shipped with #237, so
// admins manage accounts from Settings → Admin instead of the box CLI.
//
// An account is ONE row of the identity table (#2049 M4, ADR-0105): its role,
// team and Operations Center access (ops_scope; admin is full access) are
// written together, in one transaction. There is no second plane to keep in
// step and no seam to reach it through.

package httpapi

import (
	"errors"
	"net/http"
	"strings"

	"github.com/ElcanoTek/fleet/internal/identity"
	"github.com/ElcanoTek/fleet/internal/logging"
	"github.com/ElcanoTek/fleet/internal/safe"
	"github.com/ElcanoTek/fleet/internal/store"
)

// The refusals Settings → Admin → Users gets on a Fleet whose accounts central
// Auth manages (config.Config.AccountsManagedByAuth). The page is view-only
// there; these are for a caller that does not know it.
const (
	errAddUsersInAuth   = "This Fleet's accounts are managed in Auth. Add people in Auth: granting them Fleet there creates their account here."
	errResetInAuth      = "This Fleet's accounts are managed in Auth. Reset the password in Auth."
	errRolesTeamsInAuth = "This Fleet's accounts are managed in Auth. Change roles and teams there; Auth sends them here."
	errDeleteInAuth     = "This Fleet's accounts are managed in Auth. To end someone's access, remove Fleet from their access in Auth (their Fleet data is kept). To erase their Fleet data, run `fleet admin rm <email>` on the server."
	errTeamRenameInAuth = "This Fleet's teams are managed in Auth. Rename the team there: what is shared with it stays shared."
	errOwnTeamInAuth    = "Your team is set in Auth by an administrator."
)

// handleAdminUserCreate serves POST /admin/users — provision a new account.
// Body: {"email": ..., "password": ..., "role": "member|viewer|admin"?,
// "ops_scope": "none|view|run"?, "ops_role": "none|readonly|client|admin"?,
// "team_id": ...?}. role defaults to member; admin is full Operations Center
// access and wins over a narrower ops_scope/ops_role.
func (s *Server) handleAdminUserCreate(w http.ResponseWriter, r *http.Request) {
	// Auth's Fleet grant creates the account (ADR-0074); one made here would
	// have no Auth account to sign in with.
	if s.accountsManagedByAuth() {
		http.Error(w, errAddUsersInAuth, http.StatusConflict)
		return
	}
	var body struct {
		Email    string  `json:"email"`
		Password string  `json:"password"`
		Role     *string `json:"role"`
		OpsRole  *string `json:"ops_role"`
		OpsScope *string `json:"ops_scope"`
		TeamID   *string `json:"team_id"`
	}
	if !decodeJSONBody(w, r, &body) {
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
	opsGrant, ok := opsGrantFromBody(w, body.OpsRole, body.OpsScope)
	if !ok {
		return
	}
	// Chat Admin is the unified grant and wins over a narrower ops grant an
	// older client may send next to it.
	if body.Role != nil && *body.Role == store.RoleAdmin {
		opsGrant = nil
	}
	// ops_role "admin" is the Fleet admin role: a create asking for it with
	// a non-admin (or default) role is the split state ADR-0074 refuses.
	if opsGrant != nil {
		if _, admin, _ := identity.ParseOpsRole(*opsGrant); admin {
			http.Error(w, "ops_role admin is the Fleet admin role; send role admin instead", http.StatusBadRequest)
			return
		}
	}
	// Checked before the account exists: a team refused after CreateUser would
	// leave a created account and a 500 behind.
	if body.TeamID != nil {
		if err := store.ValidateTeamName(*body.TeamID); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	u, err := s.store.CreateUser(r.Context(), email, body.Password)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrUserExists):
			http.Error(w, err.Error(), http.StatusConflict)
		case store.IsInputError(err):
			http.Error(w, err.Error(), http.StatusBadRequest)
		default:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	// Role, team and Operations Center access are one second write (CreateUser
	// inserts with the column defaults: member, scope none). A failure here
	// leaves a valid member account, which the admin can still PATCH — report
	// it rather than unwinding the create.
	if body.Role != nil || body.TeamID != nil || opsGrant != nil {
		if u, err = s.store.SetUserAccess(r.Context(), email, body.Role, body.TeamID, opsGrant); err != nil {
			http.Error(w, "user created, but assigning role/team/Operations Center access failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	logging.From(r.Context()).Info("admin users: created", "email", safe.LogSafe(u.Email), "role", safe.LogSafe(u.Role), "ops_scope", safe.LogSafe(u.OpsScope), "actor", safe.LogSafe(userFromCtx(r.Context())))
	writeJSONStatus(w, r, http.StatusCreated, toAdminUser(*u))
}

// handleAdminUserDelete serves DELETE /admin/users/{email} — remove the account
// and its owned data (conversations, memories, remote MCP, projects; the store
// cascade), and with it the account's Operations Center access (the same row). Self-deletion is refused so an
// admin cannot lock themselves out mid-session.
func (s *Server) handleAdminUserDelete(w http.ResponseWriter, r *http.Request, email string) {
	actor := userFromCtx(r.Context())
	if strings.EqualFold(strings.TrimSpace(email), strings.TrimSpace(actor)) {
		http.Error(w, "refusing to delete your own account", http.StatusBadRequest)
		return
	}
	affectedConvIDs := s.activeMultiplayerAndPendingConvIDsBestEffort(r.Context())
	if err := s.store.DeleteUser(r.Context(), email); err != nil {
		if errors.Is(err, store.ErrUserNotFound) {
			http.Error(w, "user not found", http.StatusNotFound)
			return
		}
		// The account still owns team-shared projects. Deleting it would take
		// those projects — and every team learning in them — from people who
		// are still here, so the delete fails closed and names what to transfer
		// first (ADR-0057). 409, not 500: nothing is wrong, there is a step
		// missing.
		var owns *store.OwnsSharedProjectsError
		if errors.As(err, &owns) {
			// JSON, not http.Error's plain text, because the sentence alone was
			// a dead end: it named the projects and gave the admin no route to
			// the transfer control that resolves it, and the id cannot be
			// recovered client-side (GET /projects is scoped to the caller's
			// own and team-visible projects, and an admin is usually neither).
			// `error` keeps the exact prose an older client already renders;
			// `owns_shared_projects` carries {id, name} so the refusal can link
			// straight at each project's transfer control (QA #21).
			writeJSONStatus(w, r, http.StatusConflict, map[string]any{
				"error": "this account still owns team-shared projects (" +
					strings.Join(owns.ProjectNames(), ", ") +
					") — transfer them to another member first, then delete the account",
				"owns_shared_projects": owns.Projects,
			})
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.sessionApprovals.ForgetUser(email)
	s.reconcileAllMultiplayerAccess(r.Context(), affectedConvIDs...)
	s.purgeParticipantPrivateStashes(email)
	logging.From(r.Context()).Info("admin users: deleted", "email", safe.LogSafe(email), "actor", safe.LogSafe(actor))
	w.WriteHeader(http.StatusNoContent)
}

// handleAdminUserPassword serves PUT /admin/users/{email}/password — reset an
// account's password. Body: {"password": ...}. The new password is never
// logged; the audit line records only actor + target.
func (s *Server) handleAdminUserPassword(w http.ResponseWriter, r *http.Request, email string) {
	if s.accountsManagedByAuth() {
		http.Error(w, errResetInAuth, http.StatusConflict)
		return
	}
	var body struct {
		Password string `json:"password"`
	}
	if !decodeJSONBody(w, r, &body) {
		return
	}
	if err := s.store.UpdatePassword(r.Context(), email, body.Password); err != nil {
		switch {
		case errors.Is(err, store.ErrUserNotFound):
			http.Error(w, "user not found", http.StatusNotFound)
		case store.IsInputError(err):
			http.Error(w, err.Error(), http.StatusBadRequest)
		default:
			http.Error(w, err.Error(), http.StatusInternalServerError)
		}
		return
	}
	logging.From(r.Context()).Info("admin users: password reset", "email", safe.LogSafe(email), "actor", safe.LogSafe(userFromCtx(r.Context())))
	w.WriteHeader(http.StatusNoContent)
}

// handleAdminTeamRename serves POST /admin/teams/rename — relabel a team
// everywhere it appears (users + team-shared projects, one transaction).
// Body: {"from": "devops", "to": "platform"}. Admin-gated at the route.
func (s *Server) handleAdminTeamRename(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.accountsManagedByAuth() {
		http.Error(w, errTeamRenameInAuth, http.StatusConflict)
		return
	}
	var body struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	if !decodeJSONBody(w, r, &body) {
		return
	}
	affectedConvIDs := s.activeMultiplayerAndPendingConvIDsBestEffort(r.Context())
	usersN, projectsN, err := s.store.RenameTeam(r.Context(), body.From, body.To)
	if err != nil {
		// Only the store's typed input failures (blank/equal names, an
		// unknown team) are the caller's fault; a failed transaction is the
		// server's and must not be reported as the admin's own mistake.
		if store.IsInputError(err) {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.reconcileAllMultiplayerAccess(r.Context(), affectedConvIDs...)
	logging.From(r.Context()).Info("admin teams: team renamed", "from", body.From, "to", body.To, "users", usersN, "projects", projectsN, "actor", userFromCtx(r.Context()))
	writeJSON(w, r, map[string]any{"users_updated": usersN, "projects_updated": projectsN})
}
