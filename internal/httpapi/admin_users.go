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
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/ElcanoTek/fleet/internal/accountdelete"
	"github.com/ElcanoTek/fleet/internal/identity"
	"github.com/ElcanoTek/fleet/internal/logging"
	"github.com/ElcanoTek/fleet/internal/safe"
	"github.com/ElcanoTek/fleet/internal/sched/apikeys"
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
	if sameAccount(email, actor) {
		http.Error(w, "refusing to delete your own account", http.StatusBadRequest)
		return
	}
	// One deletion of an account at a time: a second one while the first
	// runs is refused (409) and changes nothing; tried again once the first
	// is over, it finds that one's outcome (404 when the account is gone).
	release, ok := s.accounts.claimDeletion(email)
	if !ok {
		http.Error(w, errAccountDeletionRunning, http.StatusConflict)
		return
	}
	defer release()
	// What the deletion would refuse is refused before the account's running
	// work is stopped; DeleteUser checks again in its own transaction.
	if pf, ok := s.store.(interface {
		DeleteUserPreflight(context.Context, string) error
	}); ok {
		if err := pf.DeleteUserPreflight(r.Context(), email); err != nil {
			if r.Context().Err() != nil {
				// This request was cancelled (its client went away, or the
				// administrator deleting is being deleted): nothing happened.
				http.Error(w, "the deletion was interrupted before it started; nothing was deleted", http.StatusServiceUnavailable)
				return
			}
			writeAccountDeleteRefusal(w, r, err)
			return
		}
	}
	// The account's requests and turns stop first, so none of them writes
	// after its rows are gone (account_gate.go, stopAccountWork). Requests
	// for it are refused until the deletion is over.
	reopen := s.accounts.close(email)
	defer reopen()
	stopped, err := s.stopAccountWork(r.Context(), email)
	if err != nil {
		// This request ended before the work stopped: its client went away,
		// or the administrator deleting is being deleted. Nothing is deleted,
		// though the account's requests in flight and its turns were already
		// cancelled.
		http.Error(w, "the deletion was interrupted before it started; nothing was deleted", http.StatusServiceUnavailable)
		return
	}
	if !stopped {
		// Some of the account's requests or turns had not ended within
		// accountDrainTimeout. The deletion waits for all of them, so nothing
		// is deleted: the gate reopens when this returns, and nothing was set
		// aside yet. What was cancelled stays cancelled, so a retry usually
		// finds it ended.
		logging.From(r.Context()).Warn("admin users: the account's requests or turns did not stop in time; nothing was deleted", "email", safe.LogSafe(email))
		w.Header().Set("Retry-After", "5")
		http.Error(w, errAccountWorkStillRunning, http.StatusServiceUnavailable)
		return
	}
	if s.afterAccountWorkStopped != nil {
		s.afterAccountWorkStopped()
	}
	// From here the deletion runs to its end even if the client goes away:
	// the account's work is already stopped and must not be left so with
	// the account still there. It is still bounded, so a stuck deletion
	// cannot hold the account's requests refused for ever.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), s.accountDeleteTimeout())
	defer cancel()
	// Read after the wait, so chats a stopped turn or request left in a
	// changed state are counted.
	affectedConvIDs := s.activeMultiplayerAndPendingConvIDsBestEffort(ctx)
	// The same deletion as `fleet admin rm`: the store cascade, the user API
	// keys that act as the account, and its upload and private workspace trees.
	// The account's in-memory approve/deny-all decisions go now, while its
	// work is stopped and the account still holds the address, and again
	// after the commit, whatever holds the address by then: they are keyed
	// by address, and forgetting one only means approving again.
	s.sessionApprovals.ForgetUser(email)
	opts := accountdelete.Options{
		Keys: s.apiKeys,
		// Before the rows go: move the account's private workspace trees
		// aside under the locks a running turn takes.
		ServerSetAside: s.setAsideParticipantStashes,
		// After: forget the account's in-memory approvals, end its part in
		// live multiplayer chats, and remove any private workspace tree still
		// there (each only while no account has its digest).
		ServerCleanup: func(email string, live func(digest string) (bool, error)) error {
			s.sessionApprovals.ForgetUser(email)
			s.reconcileAllMultiplayerAccess(ctx, affectedConvIDs...)
			return s.purgeParticipantPrivateStashes(email, live)
		},
	}
	if s.cfg != nil {
		opts.AttachmentDir = s.cfg.EmailAttachmentDir
	}
	deleted, err := accountdelete.Delete(ctx, s.store, email, opts)
	if err != nil {
		writeAccountDeleteRefusal(w, r, err)
		return
	}
	logAccountDeleteProblems(ctx, email, deleted.Problems)
	warnings := make([]string, 0, len(deleted.Problems))
	for _, p := range deleted.Problems {
		warnings = append(warnings, accountDeleteWarning(p).Error())
	}
	logging.From(ctx).Info("admin users: deleted", "email", safe.LogSafe(email), "actor", safe.LogSafe(actor), "api_keys_deleted", deleted.KeysDeleted)
	if len(warnings) > 0 {
		// 200, not an error status: the account IS deleted, and a client that
		// retried on an error would only meet a 404. The warnings say what is
		// left and how to remove it.
		writeJSONStatus(w, r, http.StatusOK, map[string]any{"warnings": warnings})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// errAccountDeletionRunning answers a deletion of an account another
// deletion of it is already running for (accountGate.claimDeletion).
const errAccountDeletionRunning = "a deletion of this account is already running"

// errAccountWorkStillRunning answers a deletion refused because some of the
// account's requests or turns had not ended within accountDrainTimeout.
const errAccountWorkStillRunning = "some of the account's requests or turns are still running; nothing was deleted; try again"

// accountDeleteWarning words a step the deletion left for the admin users
// page. Each wording is a fixed message the browser translates
// (backend-messages.json); the values in it are the account, a directory or
// an error.
func accountDeleteWarning(p accountdelete.Problem) error {
	switch p.Kind {
	case accountdelete.KeysUnavailable:
		return fmt.Errorf("the API-key store was not available, so the user API keys acting as %s were not deleted; list them with `fleet sched apikey list` and delete them with `fleet sched apikey delete <key_id>`", p.Email)
	case accountdelete.KeysFailed:
		return fmt.Errorf("deleting the user API keys acting as %s failed (%s); list them with `fleet sched apikey list` and delete them with `fleet sched apikey delete <key_id>`", p.Email, p.Err)
	case accountdelete.AttachmentDirUnreadable:
		return fmt.Errorf("the attachment directory %s could not be read (%s), so no upload or private workspace file was removed there; the server's maintenance pass removes them from the directory it uses", p.Path, p.Err)
	case accountdelete.UploadsFailed:
		return fmt.Errorf("removing the account's uploads failed: %s; the server's hourly maintenance pass tries again", p.Err)
	case accountdelete.PrivateWorkspacesFailed:
		return fmt.Errorf("removing the account's private workspace files failed: %s; the server's hourly maintenance pass tries again", p.Err)
	case accountdelete.FilesHeldByNewAccount:
		return fmt.Errorf("files under %s were left in place after the deletion: an account created under the address since holds them", p.Email)
	}
	return errors.New(p.String())
}

// writeAccountDeleteRefusal answers an account deletion the store refused:
// 404 for no such account, 409 for one that still owns team-shared projects,
// 500 for anything else.
func writeAccountDeleteRefusal(w http.ResponseWriter, r *http.Request, err error) {
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
	// The account's unbound user API keys could not be bound to it first, so
	// nothing was deleted (accountdelete.KeysNotBoundError): 503 with
	// Retry-After while the key store is busy, 500 otherwise.
	var notBound *accountdelete.KeysNotBoundError
	if errors.As(err, &notBound) {
		if errors.Is(err, apikeys.ErrStoreLocked) {
			w.Header().Set("Retry-After", "5")
			http.Error(w, errAccountDeleteKeysBusy, http.StatusServiceUnavailable)
			return
		}
		http.Error(w, fmt.Sprintf("the account's user API keys could not be updated before the deletion (%s); nothing was deleted", notBound.Err), http.StatusInternalServerError)
		return
	}
	// One of the account's trees could not be moved aside, so nothing was
	// deleted (accountdelete.FilesNotSetAsideError).
	var notMoved *accountdelete.FilesNotSetAsideError
	if errors.As(err, &notMoved) {
		http.Error(w, fmt.Sprintf("the account's files could not be moved aside before the deletion (%s), so nothing was deleted; fix that and try again", notMoved.Err), http.StatusInternalServerError)
		return
	}
	// Another account took the address between the read and the deletion.
	if errors.Is(err, store.ErrAccountChanged) {
		http.Error(w, "the account changed while it was being deleted; nothing was deleted", http.StatusConflict)
		return
	}
	http.Error(w, err.Error(), http.StatusInternalServerError)
}

// errAccountDeleteKeysBusy answers a deletion refused because the API-key
// store was busy when its keys had to be bound first.
const errAccountDeleteKeysBusy = "the API-key store is busy with another change, so nothing was deleted; try again in a few seconds"

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

// sameAccount reports whether a and b name the same account: equal once
// normalised the way the store keys accounts (identity.NormalizeEmail).
// strings.EqualFold would answer for a different notion of equality.
func sameAccount(a, b string) bool {
	return identity.NormalizeEmail(a) == identity.NormalizeEmail(b)
}
