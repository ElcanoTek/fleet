package httpapi

// Self-serve team membership (#1157). Until now users.team_id was writable only
// through the admin Users tab (PATCH /admin/users/{email}), so a box whose first
// operator was an ADMIN_EMAILS bootstrap admin had no way to get anyone — including
// that operator — into a team, and every "Share with my team" project write failed
// with "you are not in a team". This is the member-facing half:
//
//	GET /me         → who am I: email, role, team, admin flag
//	PUT /me/team    → set MY OWN team ({"team_id": "platform"}; "" leaves)
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
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strings"

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
}

func (s *Server) meResponseFor(u *store.User) meResponse {
	return meResponse{
		Email:  u.Email,
		Role:   u.Role,
		TeamID: u.TeamID,
		Admin:  s.isAdmin(u.Email) || u.Role == store.RoleAdmin,
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

// writeUserLookupError maps the store's user errors onto the two statuses this
// surface can produce; anything else is a 500 with the store's own message.
func (s *Server) writeUserLookupError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrUserNotFound):
		http.Error(w, "user not found", http.StatusNotFound)
	case strings.Contains(err.Error(), "too long"):
		http.Error(w, err.Error(), http.StatusBadRequest)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
