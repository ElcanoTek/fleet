package httpapi

// Self-serve team membership (#1157). Until now users.team_id was writable only
// through the admin Users tab (PATCH /admin/users/{email}), so a box whose first
// operator was an ADMIN_EMAILS bootstrap admin had no way to get anyone — including
// that operator — into a team, and every "Share with my team" project write failed
// with "you are not in a team". This is the member-facing half:
//
//	GET /me         → who am I: email, role, team, admin flag, saved first name
//	PUT /me/team    → set MY OWN team ({"team_id": "platform"}; "" leaves)
//	PUT /me/name    → set MY OWN first name ({"given_name": "Elyse"}; "" clears)
//
// The privacy gate is in store.SetOwnTeam, not here: a member may CREATE a team
// or LEAVE one, but joining a team that already has members (or that owns a
// team-shared project) is refused with ErrTeamExists → 409, because a shared
// team_id is what exposes team-visible conversations and team-shared projects
// (ADR-0013 / ADR-0047). Admins keep the unrestricted path — both this route
// (allowExisting) and the admin Users tab.
//
// Both routes sit behind auth+membership, and /me/team behind rejectViewerWrites,
// so a read-only viewer can see its team but not change it.

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"

	"github.com/ElcanoTek/fleet/internal/agentcore"
	"github.com/ElcanoTek/fleet/internal/store"
)

// meResponse is the shape both /me and /me/team return, so a client that just
// wrote a team does not need a second round trip to re-read it.
type meResponse struct {
	Email  string `json:"email"`
	Role   string `json:"role"`
	TeamID string `json:"team_id"`
	// Admin is the effective admin flag (ADMIN_EMAILS env allowlist OR
	// users.role = 'admin'), the same OR adminMiddleware applies. Clients use
	// it to phrase the "ask an admin" path — authorization stays server-side.
	Admin bool `json:"admin"`
	// GivenName is the first name the user saved for themselves (PUT
	// /me/name), "" when unset. It is what ${FLEET_USER_NAME} resolves to
	// (ADR-0087); fleet never fills it from the email.
	GivenName string `json:"given_name"`
}

func (s *Server) meResponseFor(u *store.User) meResponse {
	return meResponse{
		Email:     u.Email,
		Role:      u.Role,
		TeamID:    u.TeamID,
		Admin:     s.isAdmin(u.Email) || u.Role == store.RoleAdmin,
		GivenName: u.GivenName,
	}
}

// handleMe serves GET /me — the caller's own account record.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	u, err := s.store.GetUser(r.Context(), userFromCtx(r.Context()))
	if err != nil {
		s.writeUserLookupError(w, err)
		return
	}
	writeJSON(w, s.meResponseFor(u))
}

// teamResponse is meResponse plus what LEAVING would cost — the two counts the
// Leave confirm quotes ("you'll lose access to N team-shared projects; M chats
// you shared there stop being shared"). Only GET /me/team computes them: /me is
// read on every page and must stay one query.
type teamResponse struct {
	meResponse
	store.LeaveTeamImpact
}

// handleMyTeamGet serves GET /me/team — the caller's own team plus the leave
// impact, so Settings → Team can state consequences BEFORE acting rather than
// reporting them afterwards (Item A4).
func (s *Server) handleMyTeamGet(w http.ResponseWriter, r *http.Request) {
	u, err := s.store.GetUser(r.Context(), userFromCtx(r.Context()))
	if err != nil {
		s.writeUserLookupError(w, err)
		return
	}
	out := teamResponse{meResponse: s.meResponseFor(u)}
	// Best-effort: the counts are confirm-dialog copy, so a failure leaves them
	// ABSENT rather than breaking the page that shows which team you are in.
	// Absent, not zero — the counts are pointers precisely so the confirm can
	// tell "nothing to lose" from "we couldn't check", and quoting a zero it
	// never computed is how a user agrees to lose work they were told was not
	// there.
	if impact, ierr := s.store.LeaveTeamImpact(r.Context(), u.Email, u.TeamID); ierr == nil {
		out.LeaveTeamImpact = impact
	}
	writeJSON(w, out)
}

// handleMyTeam serves PUT /me/team — set the caller's own team.
// Body: {"team_id": "platform"} (empty string leaves the current team).
func (s *Server) handleMyTeam(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		s.handleMyTeamGet(w, r)
		return
	}
	if r.Method != http.MethodPut && r.Method != http.MethodPatch {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		TeamID *string `json:"team_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if body.TeamID == nil {
		http.Error(w, `team_id required (use "" to leave your team)`, http.StatusBadRequest)
		return
	}
	caller := userFromCtx(r.Context())
	// Admins may join any team — they already have that power through the admin
	// Users tab, and gating their own row differently would just push them
	// there for no gain.
	admin := s.isAdmin(caller) || roleFromCtx(r.Context()) == store.RoleAdmin
	// A team is part of the account-events state, so a self-serve move is
	// published like an admin's (the web UI's source, the caller as actor).
	defer s.beginAccountChange(r, caller, store.AccountEventSourceAdminUI, caller)()
	u, err := s.store.SetOwnTeam(r.Context(), caller, *body.TeamID, admin)
	if err != nil {
		if errors.Is(err, store.ErrTeamExists) {
			http.Error(w, "that team already exists — ask an admin to add you to it", http.StatusConflict)
			return
		}
		s.writeUserLookupError(w, err)
		return
	}
	// %q escapes CR/LF, so the request-supplied team cannot forge a log line.
	log.Printf("teams: %q set own team to %q", u.Email, u.TeamID)
	writeJSON(w, s.meResponseFor(u))
}

// handleMyName serves PUT /me/name — set the caller's own first name, the
// value the bundle's ${FLEET_USER_NAME} token resolves to on their chat turns
// (ADR-0087), e.g. the "Hi Elyse," of a deal-sheet email. Body:
// {"given_name": "Elyse"}; "" clears it, and a connector then uses its no-name
// wording ("Hi team,"). GET returns the same /me record.
//
// This is the only way a name reaches fleet: every sign-in path (password,
// OIDC from the Elcano Auth service, the elcano_auth cookie) carries an email
// and no name claim, and fleet does not guess one from the email. The value is
// held to agentcore.ValidMCPUserName here — the same rule the turn applies
// before handing it to a connector — so a saved name is never silently dropped
// later, and an address or handle cannot be saved as a name.
func (s *Server) handleMyName(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		s.handleMe(w, r)
		return
	}
	if r.Method != http.MethodPut && r.Method != http.MethodPatch {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		GivenName *string `json:"given_name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&body); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	if body.GivenName == nil {
		http.Error(w, `given_name required (use "" to clear your name)`, http.StatusBadRequest)
		return
	}
	name := strings.TrimSpace(*body.GivenName)
	if name != "" && !agentcore.ValidMCPUserName(name) {
		http.Error(w, "use letters, single spaces, apostrophes, hyphens or periods (at most 64 characters) — the name you want to be greeted by, not an email address", http.StatusBadRequest)
		return
	}
	u, err := s.store.SetOwnGivenName(r.Context(), userFromCtx(r.Context()), name)
	if err != nil {
		s.writeUserLookupError(w, err)
		return
	}
	writeJSON(w, s.meResponseFor(u))
}

// mcpUserFor builds the ${FLEET_USER_*} identity (ADR-0087) for a signed-in
// user who sent a turn or approved a card: their authenticated email plus the
// first name THEY saved (PUT /me/name). A user who saved none — or a lookup
// that fails — gets no name, so the token is unset and a connector uses its
// no-name wording. The name is never derived from the email.
func (s *Server) mcpUserFor(ctx context.Context, email string) agentcore.MCPUserIdentity {
	name, err := s.store.UserGivenName(ctx, email)
	if err != nil {
		if !errors.Is(err, store.ErrUserNotFound) {
			// No address or name in the line: the cause is enough to debug a
			// missing greeting, and the turn goes ahead without the name.
			log.Printf("mcp user: first-name lookup failed, the turn proceeds without one: %v", err)
		}
		name = ""
	}
	return agentcore.NewMCPUserIdentity(email, name)
}

// writeUserLookupError maps the store's user errors onto the two statuses this
// surface can produce; anything else is a 500 with the store's own message.
func (s *Server) writeUserLookupError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrUserNotFound):
		http.Error(w, "user not found", http.StatusNotFound)
	case store.IsInputError(err), strings.Contains(err.Error(), "too long"):
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
