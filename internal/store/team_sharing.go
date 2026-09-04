package store

// Team-visible chats inside team-shared projects (ADR-0057), plus the counts
// the destructive confirms need.
//
// ADR-0013 gave a conversation owner a per-chat `team_visible` opt-in and a
// team-scoped LIST endpoint; nothing ever read ONE such conversation, and
// nothing tied the flag to a place where teammates would look for it. The
// invariant this file adds is that a team-shared chat always has a home: it
// lives in a project that is itself shared with the team. The write paths that
// could break that pairing — moving a chat out of its project, unsharing the
// project, deleting the project, leaving the team — clear the flag instead of
// leaving a chat visible to a team with no surface listing it.
//
// The mirror-image rule, which those same paths get wrong in the other
// direction, lives in projects.go: a chat's project_id must never point at a
// project ITS OWNER cannot see, so every revocation of access unfiles the
// affected chats (never deletes them). Unsharing a team-shared chat that stays
// filed in a project only somebody else can see hides it from its own owner.
//
// The read path (GetTeamVisibleConversation) is the SECOND cross-user
// conversation read in the store, after ListTeamConversations, and carries the
// same two gates: a shared users.team_id AND the owner's explicit per-chat
// opt-in. Like the public share snapshot it exposes user/assistant TEXT only —
// never tool calls, tool results, or reasoning, which can carry command output
// and API responses the owner never meant to hand over.

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"

	"github.com/ElcanoTek/fleet/internal/agent"
)

// TeamSharedConversation is the read-only view a teammate gets of a
// conversation its owner shared with the team. It carries the owner's email
// (the viewer is told whose chat this is) and the team it was shared with,
// which the anonymous share snapshot deliberately omits.
//
// Unlike SharedConversation the message entries KEEP their persisted ids: a
// teammate's only way to build on the chat is Branch, and a branch point is a
// messages.id. The ids are already visible to every member of the team through
// their own branches, so this leaks nothing the feature does not require.
//
// What is SERIALIZED is what the viewer needs: whose chat, called what, shared
// with whom, and the transcript. The owner's working state is read but not
// sent — see the unexported block.
type TeamSharedConversation struct {
	ID         string `json:"id"`
	OwnerEmail string `json:"owner_email"`
	Title      string `json:"title"`
	TeamID     string `json:"team_id"`
	UpdatedAt  int64  `json:"updated_at"`

	// Read for BranchConversation, which inherits them, and json:"-" because
	// the viewer has no use for them: the fork's project and settings are
	// decided entirely server-side from the parent row, so sending them would
	// be the owner's per-chat state going out for nothing. Lockdown is the
	// load-bearing one — a fork of a network-isolated chat must be isolated.
	Persona   string `json:"-"`
	Model     string `json:"-"`
	ProjectID string `json:"-"`
	Lockdown  bool   `json:"-"`

	Messages []agent.HistoryEntry `json:"messages"`
}

// GetTeamVisibleConversation returns the read-only transcript of convID when
// the caller may read it as a teammate — the owner opted the chat into team
// visibility AND the caller's non-empty users.team_id is the audience the
// owner named — or (nil, nil) when they may not (unknown id, not opted in,
// different team, archived, deleted). The owner reading their own chat
// resolves too, so one endpoint serves the "what does my team see?" preview
// without a second code path.
//
// ARCHIVED is a refusal here on purpose, matching both listings: archiving
// removes the chat from the project's Team section and from ?scope=team, so a
// teammate who kept the id must not keep the transcript either. Archive is the
// owner's "put this away", and the read gates agree on what that means.
//
// The audience is the team the OWNER NAMED when they opted in
// (team_shared_with), not whichever team they happen to be in now — see
// migration 054. Membership state is never leaked: every refusal is the same
// nil.
func (s *Store) GetTeamVisibleConversation(ctx context.Context, callerEmail, convID string) (*TeamSharedConversation, error) {
	callerEmail = normalizeEmail(callerEmail)
	if convID == "" {
		return nil, nil
	}
	caller, err := s.GetUser(ctx, callerEmail)
	if err != nil {
		if errors.Is(err, ErrUserNotFound) {
			return nil, nil
		}
		return nil, err
	}
	team := strings.TrimSpace(caller.TeamID)

	var out TeamSharedConversation
	err = s.db.QueryRowContext(ctx, `
		SELECT c.id, c.user_email, c.title, c.persona, c.model,
		       COALESCE(c.team_shared_with, ''), COALESCE(c.project_id, ''),
		       c.lockdown, c.updated_at
		FROM conversations c
		WHERE c.id = $1
		  AND c.deleted_at IS NULL
		  AND c.archived_at IS NULL
		  AND c.team_visible = TRUE
		  AND (c.user_email = $2 OR ($3 <> '' AND c.team_shared_with = $3))`,
		convID, callerEmail, team,
	).Scan(&out.ID, &out.OwnerEmail, &out.Title, &out.Persona, &out.Model,
		&out.TeamID, &out.ProjectID, &out.Lockdown, &out.UpdatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}

	msgs, err := s.LoadHistory(ctx, out.ID)
	if err != nil {
		return nil, err
	}
	// Transcript only — the same filter the public snapshot applies, and for
	// the same reason: the full history carries tool_call / tool_result /
	// reasoning entries whose content can include command output and API
	// responses that were never part of what the owner shared.
	out.Messages = make([]agent.HistoryEntry, 0, len(msgs))
	for _, m := range msgs {
		if m.Type == "text" && (m.Role == "user" || m.Role == "assistant") {
			out.Messages = append(out.Messages, m)
		}
	}
	return &out, nil
}

// ListProjectTeamConversations returns the team-shared chats OTHER members
// contributed to this project — the project home's Team section. The caller's
// own chats are excluded: they already render in "your chats" (with a team
// badge when shared), and listing them twice would read as two copies of the
// same conversation.
//
// Gated exactly like ListTeamConversations — each owner's per-chat opt-in plus
// the audience they named (team_shared_with, not their current team) — and
// additionally narrowed to this project, which is what makes the section a
// place a team-shared chat can actually live.
// Returns an empty list (never an error) for a caller with no team.
func (s *Store) ListProjectTeamConversations(ctx context.Context, callerEmail, projectID string) ([]Conversation, error) {
	callerEmail = normalizeEmail(callerEmail)
	caller, err := s.GetUser(ctx, callerEmail)
	if err != nil {
		if errors.Is(err, ErrUserNotFound) {
			return []Conversation{}, nil
		}
		return nil, err
	}
	team := strings.TrimSpace(caller.TeamID)
	if team == "" {
		return []Conversation{}, nil
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+teamConversationColumns+`
		FROM conversations c
		WHERE c.deleted_at IS NULL
		  AND c.archived_at IS NULL
		  AND c.team_visible = TRUE
		  AND c.project_id = $1
		  AND c.user_email <> $2
		  AND c.team_shared_with = $3
		ORDER BY c.updated_at DESC, c.id DESC`,
		projectID, callerEmail, team,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	list, err := scanConversationRows(rows)
	if err != nil {
		return nil, err
	}
	// Belt-and-suspenders, as in ListTeamConversations: a teammate listing
	// must never carry the owner's public share capability URL.
	for i := range list {
		list[i].ShareToken = ""
	}
	if list == nil {
		list = []Conversation{}
	}
	return list, nil
}

// UnshareTeamVisibleChatsInTeam clears team_visible on every conversation
// ownerEmail has shared into a project belonging to teamID. Called when the
// owner leaves that team: the projects stop being visible to them, so a chat
// they shared there would otherwise stay readable by a group they are no
// longer part of, with no surface on their side to revoke it. Returns the
// number of chats unshared so the caller can report it.
func (s *Store) UnshareTeamVisibleChatsInTeam(ctx context.Context, ownerEmail, teamID string) (int, error) {
	teamID = strings.TrimSpace(teamID)
	if teamID == "" {
		return 0, nil
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE conversations SET team_visible = FALSE, team_shared_with = NULL, updated_at = $1
		WHERE user_email = $2 AND team_visible = TRUE AND team_shared_with = $3`,
		time.Now().Unix(), normalizeEmail(ownerEmail), teamID,
	)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// LeaveTeamImpact is what a user loses by leaving their team — the numbers the
// Leave confirm states instead of acting first and reporting afterwards.
type LeaveTeamImpact struct {
	// SharedProjects is the count of team-shared projects the user would stop
	// seeing: those shared with the team that they do NOT own (an owner keeps
	// their own projects, still shared with the team they left).
	//
	// A POINTER, and omitted when nil, because this is confirm-dialog copy:
	// the difference between "leaving costs you nothing" and "we could not
	// work out what leaving costs you" must survive serialization. As a plain
	// int a failed count serialized as 0 and the dialog cheerfully told the
	// user there was nothing to lose.
	SharedProjects *int `json:"shared_projects,omitempty"`
	// SharedChats is the count of the user's OWN chats currently shared with
	// that team — all of which are unshared by leaving. Nil for the same
	// reason as SharedProjects.
	SharedChats *int `json:"shared_chats,omitempty"`
}

// LeaveTeamImpact computes the two counts the Leave-team confirm quotes. A
// caller with no team gets a zeroed struct with BOTH counts nil — there is
// nothing to leave, so there is nothing to state. On error the counts stay nil
// so the confirm can say it doesn't know, rather than reporting zero.
func (s *Store) LeaveTeamImpact(ctx context.Context, email, teamID string) (LeaveTeamImpact, error) {
	var out LeaveTeamImpact
	teamID = strings.TrimSpace(teamID)
	if teamID == "" {
		return out, nil
	}
	email = normalizeEmail(email)
	var projects, chats int
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM projects WHERE team_id = $1 AND owner_email <> $2`,
		teamID, email,
	).Scan(&projects); err != nil {
		return LeaveTeamImpact{}, err
	}
	if err := s.db.QueryRowContext(ctx, `
		SELECT count(*) FROM conversations
		WHERE user_email = $1 AND team_visible = TRUE AND deleted_at IS NULL
		  AND team_shared_with = $2`,
		email, teamID,
	).Scan(&chats); err != nil {
		return LeaveTeamImpact{}, err
	}
	out.SharedProjects, out.SharedChats = &projects, &chats
	return out, nil
}

// ProjectImpact is what deleting a project destroys or detaches — the numbers
// the delete confirm states so an owner can see what members lose before they
// answer, and decide to export first.
type ProjectImpact struct {
	// Memories is the count of team learnings that die with the project.
	Memories int `json:"memories"`
	// Chats is the count of conversations that leave the project (across all
	// members), and Members how many distinct people own them.
	Chats   int `json:"chats"`
	Members int `json:"members"`
	// TeamSharedChats is how many of those chats are currently team-visible;
	// detaching unshares them.
	TeamSharedChats int `json:"team_shared_chats"`
	// ChatsFromTeammates / TeammatesWithChats are the same numbers restricted
	// to chats somebody OTHER than the project owner filed here — what
	// "Share with my team" being unticked costs, as opposed to what deleting
	// the project costs. Making a project personal leaves the owner's own
	// chats exactly where they are and unfiles every other member's (they can
	// no longer see the project), so the untick confirm quotes these two:
	// "{N} chats from teammates will move to their unfiled chats."
	//
	// Plain ints, unlike LeaveTeamImpact's pointers, because the whole struct
	// comes from one query that either succeeds or fails the request — there
	// is no half-known state to distinguish from zero.
	ChatsFromTeammates int `json:"chats_from_teammates"`
	TeammatesWithChats int `json:"teammates_with_chats"`
}

// ErrNotAProjectMember is returned when a transfer names someone who is not in
// the project's team — handing a team-shared project to an outsider would
// leave it shared with a team its owner is not in, which nobody can reason
// about.
var ErrNotAProjectMember = errors.New("the new owner must be a member of the project's team")

// OwnsSharedProjectsError is returned by DeleteUser when the account still
// owns team-shared projects. Deleting it would take those projects — and every
// team learning in them — away from people who are still here, so the delete
// fails closed and names what to transfer first.
type OwnsSharedProjectsError struct{ Projects []string }

func (e *OwnsSharedProjectsError) Error() string {
	return "account still owns team-shared projects: " + strings.Join(e.Projects, ", ")
}

// TeamSharedProjectsOwnedBy lists the NAMES of team-shared projects the user
// owns — what a delete would destroy, and what an admin must transfer first.
// Personal projects are excluded: nobody else can see them, so they belong
// with the account.
func (s *Store) TeamSharedProjectsOwnedBy(ctx context.Context, email string) ([]string, error) {
	return teamSharedProjectsOwnedBy(ctx, s.db, normalizeEmail(email), "")
}

// teamSharedProjectsOwnedByTx is the same read taken INSIDE a transaction with
// the matching rows locked — what DeleteUser's fail-closed guard needs so a
// concurrent "share this project" cannot slip past the check and be deleted by
// the very statement the check was protecting against.
func teamSharedProjectsOwnedByTx(ctx context.Context, tx *sql.Tx, email string) ([]string, error) {
	return teamSharedProjectsOwnedBy(ctx, tx, normalizeEmail(email), " FOR UPDATE")
}

// queryer is the subset of *sql.DB / *sql.Tx the shared read needs.
type queryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

func teamSharedProjectsOwnedBy(ctx context.Context, q queryer, email, lock string) ([]string, error) {
	rows, err := q.QueryContext(ctx,
		`SELECT name FROM projects WHERE owner_email = $1 AND team_id <> '' ORDER BY name`+lock,
		email)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// ProjectMemberEmails lists who a project can be handed to: everyone in its
// team, plus the current owner (who is always a member, team or not). Sorted,
// deduplicated, emails only — the transfer picker's options.
//
// This enumerates every account in the team, including people who have never
// shared anything, which is more than a plain member could learn from the
// project's own surfaces. The handler therefore serves it to the OWNER and
// admins only — the only callers with a transfer to make.
func (s *Store) ProjectMemberEmails(ctx context.Context, projectID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT email FROM (
			SELECT p.owner_email AS email FROM projects p WHERE p.id = $1
			UNION
			SELECT u.email FROM users u
			JOIN projects p ON p.id = $1
			WHERE p.team_id <> '' AND u.team_id = p.team_id
		) m ORDER BY email`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var e string
		if err := rows.Scan(&e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// TransferProjectOwnership hands a project to newOwnerEmail. It changes ONLY
// who may edit and delete the definition: the team it is shared with, its
// team learnings, its chats and every member's access are untouched, because
// none of those are keyed on the owner.
//
// A project could not change hands at all, which made "the owner left" an
// unrecoverable state: the definition was frozen (every mutation is
// owner-scoped) and deleting the departing account destroyed the project and
// its team learnings outright. This is the missing move.
//
// WHO may call it is the handler's gate (the owner, or an admin — the admin
// path is the whole point, since a departed owner cannot act). What the store
// enforces is that the result makes sense: the project is TEAM-SHARED and the
// target is in that team. Every refusal is the same ErrNotAProjectMember, so
// the route says nothing about which addresses have accounts.
func (s *Store) TransferProjectOwnership(ctx context.Context, projectID, newOwnerEmail string) (*Project, error) {
	newOwner := normalizeEmail(newOwnerEmail)
	if newOwner == "" {
		return nil, errors.New("new owner email required")
	}
	p, err := s.GetProject(ctx, projectID)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, errors.New("project not found")
	}
	if p.OwnerEmail == newOwner {
		return p, nil // already theirs — idempotent, not an error
	}
	// A PERSONAL project has no membership to check against, so "transfer" it
	// and you are pushing a project — with instructions of your choosing, which
	// get injected into every chat started in it — into a stranger's rail,
	// without asking them. Transfer exists to keep a TEAM's shared work alive
	// when its owner leaves; a personal project has no such second party.
	if p.TeamID == "" {
		return nil, ErrNotAProjectMember
	}
	// The project row is read BEFORE the account, and both failures return the
	// same sentinel, so this route cannot be used to test whether an arbitrary
	// address has an account here (see the handler's single 400).
	target, err := s.GetUser(ctx, newOwner)
	if err != nil {
		if errors.Is(err, ErrUserNotFound) {
			return nil, ErrNotAProjectMember
		}
		return nil, err
	}
	// Exact compare, matching every other team gate in the store. EqualFold
	// here would have been the one loose comparison, letting a `Quant` member
	// take over a `quant` project.
	if strings.TrimSpace(target.TeamID) != p.TeamID {
		return nil, ErrNotAProjectMember
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }() // no-op after a successful Commit
	row := tx.QueryRowContext(ctx,
		`UPDATE projects SET owner_email = $1, updated_at = $2 WHERE id = $3
		 RETURNING `+projectColumns,
		newOwner, time.Now().Unix(), projectID)
	updated, err := scanProject(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errors.New("project not found")
		}
		return nil, err
	}
	// Everyone's access is untouched — with one exception the sentence above
	// used to gloss over: the OUTGOING owner saw the project through
	// owner_email, and after this statement they see it only if they are in its
	// team. An owner who has left the team (the case the admin arm exists for)
	// therefore loses access to a project their own chats may still be filed
	// in, which the rail would render nowhere. Unfile them, in this
	// transaction, per the filing invariant in projects.go.
	if err := unfileChatsWithoutProjectAccessTx(ctx, tx, projectID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return updated, nil
}

// ProjectImpact counts what a delete would take with it, and what making the
// project personal would unfile. Best-effort display data: the counts are read
// outside the transaction that acts on them, so a chat filed a moment later is
// simply not reflected.
func (s *Store) ProjectImpact(ctx context.Context, projectID string) (ProjectImpact, error) {
	var out ProjectImpact
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM memories WHERE project_id = $1`, projectID,
	).Scan(&out.Memories); err != nil {
		return out, err
	}
	// The teammate counts compare each chat's owner against the project's
	// owner_email — the definition of "loses the project when it goes
	// personal", since a personal project is visible to its owner and nobody
	// else. A missing project makes the scalar subquery NULL, so the FILTERs
	// match nothing and the counts are 0 rather than an error.
	err := s.db.QueryRowContext(ctx, `
		WITH owner AS (SELECT owner_email FROM projects WHERE id = $1)
		SELECT count(*), count(DISTINCT user_email),
		       count(*) FILTER (WHERE team_visible),
		       count(*) FILTER (WHERE user_email <> (SELECT owner_email FROM owner)),
		       count(DISTINCT user_email) FILTER (WHERE user_email <> (SELECT owner_email FROM owner))
		FROM conversations WHERE project_id = $1 AND deleted_at IS NULL`,
		projectID,
	).Scan(&out.Chats, &out.Members, &out.TeamSharedChats, &out.ChatsFromTeammates, &out.TeammatesWithChats)
	if err != nil {
		return out, err
	}
	return out, nil
}
