package httpapi

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/pgtext"
	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// User-authored Agent Skills (the skills builder, docs/SKILLS.md phase 2).
// CRUD over the caller's own skills plus the per-turn materialization that
// makes them usable: before a turn runs, the user's ACTIVE skills are written
// into the conversation workspace (user-skills/<name>/SKILL.md — writable,
// per-conversation, already inside the sandbox mount surface) and their
// Level-1 metadata rides TurnInput.UserSkills into the prompt roster. A skill
// reaches its author's turns only, with one exception: a workspace-shared skill
// an admin turned on for everyone (enabled_for_all) is loaded into every
// user's chat turns from the admin's row (docs/SKILLS.md "On for everyone").
// Putting a skill into the bundle itself stays an operator action (copy it
// into the bundle's skills/).

// userSkillsRoot is the workspace subdir user skills materialize into. It is
// deliberately distinct from the read-only "skills/" symlink so a user skill
// can never shadow (or be confused with) bundle/built-in content.
const userSkillsRoot = "user-skills"

type userSkillRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Body        string `json:"body"`
	Status      string `json:"status,omitempty"`
	Visibility  string `json:"visibility,omitempty"`
	// EnabledForAll turns a workspace-shared skill on for everyone (true, admins
	// only) or back off (false). Absent leaves it as it is.
	EnabledForAll *bool `json:"enabled_for_all,omitempty"`
}

// userSkillsCollection handles GET (list) and POST (create) /user-skills.
func (s *Server) userSkillsCollection(w http.ResponseWriter, r *http.Request) {
	user := userFromCtx(r.Context())
	switch r.Method {
	case http.MethodGet:
		skills, err := s.store.ListUserSkills(r.Context(), user)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, r, map[string]any{"skills": skills})
	case http.MethodPost:
		var req userSkillRequest
		if !decodeJSONBody(w, r, &req) {
			return
		}
		var (
			sk  *store.UserSkill
			err error
		)
		if strings.TrimSpace(req.Visibility) == "" {
			sk, err = s.store.CreateUserSkill(r.Context(), user, req.Name, req.Description, req.Body)
		} else {
			sk, err = s.store.CreateUserSkillWithVisibility(r.Context(), user, req.Name, req.Description, req.Body, req.Visibility)
		}
		if err != nil {
			s.userSkillError(w, err)
			return
		}
		writeJSON(w, r, sk)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// userSkillByID handles PUT and DELETE /user-skills/{id}.
func (s *Server) userSkillByID(w http.ResponseWriter, r *http.Request) {
	user := userFromCtx(r.Context())
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/user-skills/"), "/")
	if id == "" {
		http.Error(w, "skill id required", http.StatusBadRequest)
		return
	}
	switch r.Method {
	case http.MethodPut:
		var req userSkillRequest
		if !decodeJSONBody(w, r, &req) {
			return
		}
		if req.Status == "" {
			req.Status = store.UserSkillStatusActive
		}
		// A skill that is on for everyone is instructions in every user's
		// turns, so only an admin may switch it on. Refused before anything is
		// written, so a refused request changes nothing. Switching it off is
		// the owner's to do whatever their role is now.
		if req.EnabledForAll != nil && *req.EnabledForAll && !s.callerIsAdmin(r.Context()) {
			http.Error(w, "forbidden — only an admin can turn a skill on for everyone", http.StatusForbidden)
			return
		}
		var (
			sk  *store.UserSkill
			err error
		)
		if strings.TrimSpace(req.Visibility) == "" {
			sk, err = s.store.UpdateUserSkill(r.Context(), user, id, req.Name, req.Description, req.Body, req.Status)
		} else {
			sk, err = s.store.UpdateUserSkillWithVisibility(r.Context(), user, id, req.Name, req.Description, req.Body, req.Status, req.Visibility)
		}
		if err != nil {
			s.userSkillError(w, err)
			return
		}
		if req.EnabledForAll != nil {
			// sk.ID, not the path id: approving a staged "<name>-proposed"
			// revision returns the base skill it was merged into.
			sk, err = s.store.SetUserSkillEnabledForAll(r.Context(), user, sk.ID, *req.EnabledForAll)
			if err != nil {
				s.userSkillError(w, err)
				return
			}
		}
		writeJSON(w, r, sk)
	case http.MethodDelete:
		if err := s.store.DeleteUserSkill(r.Context(), user, id); err != nil {
			s.userSkillError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) userSkillError(w http.ResponseWriter, err error) {
	switch {
	case writeAccountGone(w, err):
	case errors.Is(err, store.ErrUserSkillInvalid):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, store.ErrUserSkillNotFound):
		http.Error(w, "skill not found", http.StatusNotFound)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// effectiveUserSkills returns the skills that take part in the caller's chat
// turns, for materialization and explicit "/<name>" invocation: the caller's
// own, plus the workspace-shared skills an admin turned on for everyone
// (enabledForAllSkills). Every other teammate-shared skill stays opt-in (via
// "Add to my skills" or "Clone & edit" in Settings → Skills) before entering
// the caller's prompt roster. An active skill the caller owns wins over a
// skill of the same name that is on for everyone, the same way it customizes
// (overrides) a bundle/plugin/built-in skill's SKILL.md for the caller's runs.
func (s *Server) effectiveUserSkills(ctx context.Context, user string) ([]store.UserSkill, error) {
	own, err := s.store.ListUserSkills(ctx, user)
	if err != nil {
		return nil, err
	}
	ownActive := map[string]bool{}
	for _, sk := range own {
		if sk.Status == store.UserSkillStatusActive {
			ownActive[sk.Name] = true
		}
	}
	userKey := pgtext.EmailKey(user)
	for _, sk := range s.enabledForAllSkills(ctx) {
		if ownActive[sk.Name] || pgtext.EmailKey(sk.UserEmail) == userKey {
			continue
		}
		own = append(own, sk)
	}
	return own, nil
}

// enabledForAllSkills returns the workspace-shared skills that are on for
// everyone right now, one per name. The store row says an admin switched it
// on; this also requires that the row's owner is an admin NOW, so a skill
// stops reaching other people's turns the moment its owner loses the role
// (or the account is disabled), without anyone having to remember to switch it
// off. Where two admins each have one of the same name, the most recently
// updated wins. Best-effort like the rest of the skill roster: a store error
// yields none, and the turn runs with the caller's own skills.
func (s *Server) enabledForAllSkills(ctx context.Context) []store.UserSkill {
	rows, err := s.store.ListEnabledForAllUserSkills(ctx)
	if err != nil || len(rows) == 0 {
		return nil
	}
	ownerIsAdmin := map[string]bool{}
	seen := map[string]bool{}
	out := make([]store.UserSkill, 0, len(rows))
	for _, sk := range rows {
		if seen[sk.Name] {
			continue
		}
		ok, known := ownerIsAdmin[sk.UserEmail]
		if !known {
			// GetUser answers only for an enabled account, so a disabled
			// owner is out even where ADMIN_EMAILS still names them.
			if u, uerr := s.store.GetUser(ctx, sk.UserEmail); uerr == nil && u != nil {
				ok = u.Role == store.RoleAdmin || s.isAdmin(sk.UserEmail)
			}
			ownerIsAdmin[sk.UserEmail] = ok
		}
		if !ok {
			continue
		}
		seen[sk.Name] = true
		out = append(out, sk)
	}
	return out
}

// materializeUserSkills syncs the user's ACTIVE skills into the conversation
// workspace and returns the prompt roster entries.
// Best-effort by contract: a failure returns nil (the turn runs without user
// skills) — skills are a capability, not a turn invariant. Stale dirs from
// renamed/disabled/deleted skills are removed so the on-disk set always mirrors
// the DB, including when the effective roster becomes empty.
func (s *Server) materializeUserSkills(ctx context.Context, user, conversationID string) []agent.UserSkillPromptEntry {
	skills, err := s.effectiveUserSkills(ctx, user)
	if err != nil {
		return nil
	}
	// Every write and removal goes through the conversation directory opened
	// without following a symbolic link, and through os.Root below it. A
	// conversation directory that is a link is refused, and a link at
	// user-skills/ or a skill's directory that leads out of the workspace is
	// refused too. A link that stays inside the workspace (user-skills ->
	// out) is followed, so the skills land in, and stale entries are removed
	// from, that directory of the sandbox's own: nothing the sandbox could
	// not already write or delete itself.
	ws, err := tools.EnsureConversationDir(ctx, conversationID)
	if err != nil {
		return nil
	}
	defer ws.Close()
	for _, sk := range skills {
		if sk.Status == store.UserSkillStatusActive {
			if err := ws.MkdirAll(userSkillsRoot, 0o755); err != nil {
				return nil
			}
			break
		}
	}
	root, err := ws.OpenRoot(userSkillsRoot)
	if err != nil {
		return nil // no active skill and nothing materialized earlier
	}
	defer root.Close()
	want := map[string]bool{}
	var entries []agent.UserSkillPromptEntry
	for _, sk := range skills {
		if sk.Status != store.UserSkillStatusActive {
			continue
		}
		want[sk.Name] = true
		if err := root.MkdirAll(sk.Name, 0o755); err != nil { // workspace content, sandbox-readable by design (see tools/workspace.go)
			continue
		}
		md := store.RenderUserSkillMarkdown(&sk)
		path := filepath.Join(sk.Name, "SKILL.md")
		if cur, rerr := root.ReadFile(path); rerr != nil || string(cur) != md {
			if werr := root.WriteFile(path, []byte(md), 0o644); werr != nil { // non-secret instructions, sandbox-readable
				continue
			}
		}
		entries = append(entries, agent.UserSkillPromptEntry{
			Name:        sk.Name,
			Description: sk.Description,
			Path:        userSkillsRoot + "/" + sk.Name + "/SKILL.md",
		})
	}
	// Remove stale materializations (renamed/disabled/deleted skills). The
	// names come from listing the opened user-skills/ root itself, and each
	// removal goes through it, so no entry can reach outside it.
	if dirs, err := fs.ReadDir(root.FS(), "."); err == nil {
		for _, d := range dirs {
			if !want[d.Name()] {
				_ = root.RemoveAll(d.Name())
			}
		}
	}
	return entries
}

// matchUserSkillInvocation is the user-skill counterpart of
// matchSkillInvocation: an exact "/<name>" match against the caller's ACTIVE
// skills, pointing the agent at the workspace materialization path.
func matchUserSkillInvocation(message string, skills []store.UserSkill) string {
	if len(skills) == 0 || !strings.HasPrefix(message, "/") {
		return ""
	}
	token := strings.TrimPrefix(message, "/")
	if i := strings.IndexAny(token, " \t\n"); i >= 0 {
		token = token[:i]
	}
	for _, sk := range skills {
		if sk.Status != store.UserSkillStatusActive || sk.Name != token {
			continue
		}
		whose := "their own skill"
		if sk.SharedBy != "" {
			whose = "the workspace skill"
		}
		return "\n\n[Skill invoked: " + sk.Name + "]\nThe user explicitly invoked " + whose + " \"" + sk.Name +
			"\". Read `" + userSkillsRoot + "/" + sk.Name + "/SKILL.md` now and follow its instructions for this request (any supporting reference files or scripts under `skills/" + sk.Name + "/` remain available on disk)."
	}
	return ""
}

// skillProposer implements agentcore.SkillProposer for interactive turns: a
// propose_skill call stages a PROPOSED row for the turn's user, who reviews it
// on the Skills page. Mirrors memoryProposer's shape (approvals.go).
type skillProposer struct {
	ctx   context.Context
	store chatStore
	user  string
}

func (p *skillProposer) Propose(name, description, body, _ string) (string, error) {
	sk, err := p.store.CreateUserSkillProposal(p.ctx, p.user, name, description, body)
	if err != nil {
		return "", err
	}
	return sk.ID, nil
}
