package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// User-authored Agent Skills (the skills builder, docs/SKILLS.md phase 2).
// CRUD over the caller's own skills plus the per-turn materialization that
// makes them usable: before a turn runs, the user's ACTIVE skills are written
// into the conversation workspace (user-skills/<name>/SKILL.md — writable,
// per-conversation, already inside the sandbox mount surface) and their
// Level-1 metadata rides TurnInput.UserSkills into the prompt roster. Only
// the author's own runs ever see them; graduating a skill to the whole
// deployment stays an operator action (copy it into the bundle's skills/).

// userSkillsRoot is the workspace subdir user skills materialize into. It is
// deliberately distinct from the read-only "skills/" symlink so a user skill
// can never shadow (or be confused with) bundle/built-in content.
const userSkillsRoot = "user-skills"

type userSkillRequest struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Body        string `json:"body"`
	Status      string `json:"status,omitempty"`
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
		writeJSON(w, map[string]any{"skills": skills})
	case http.MethodPost:
		var req userSkillRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		sk, err := s.store.CreateUserSkill(r.Context(), user, req.Name, req.Description, req.Body)
		if err != nil {
			s.userSkillError(w, err)
			return
		}
		writeJSON(w, sk)
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
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		if req.Status == "" {
			req.Status = store.UserSkillStatusActive
		}
		sk, err := s.store.UpdateUserSkill(r.Context(), user, id, req.Name, req.Description, req.Body, req.Status)
		if err != nil {
			s.userSkillError(w, err)
			return
		}
		writeJSON(w, sk)
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
	case errors.Is(err, store.ErrUserSkillInvalid):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, store.ErrUserSkillNotFound):
		http.Error(w, "skill not found", http.StatusNotFound)
	default:
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// materializeUserSkills syncs the user's ACTIVE skills into the conversation
// workspace and returns the prompt roster entries. Best-effort by contract: a
// failure returns nil (the turn runs without user skills) — skills are a
// capability, not a turn invariant. Stale dirs from renamed/disabled/deleted
// skills are removed so the on-disk set always mirrors the DB.
func (s *Server) materializeUserSkills(ctx context.Context, user, conversationID string) []agent.UserSkillPromptEntry {
	skills, err := s.store.ListUserSkills(ctx, user)
	if err != nil || len(skills) == 0 {
		return nil
	}
	wsDir, err := tools.EnsureWorkspaceDir(conversationID)
	if err != nil {
		return nil
	}
	root := filepath.Join(wsDir, userSkillsRoot)
	want := map[string]bool{}
	var entries []agent.UserSkillPromptEntry
	for _, sk := range skills {
		if sk.Status != store.UserSkillStatusActive {
			continue
		}
		want[sk.Name] = true
		dir := filepath.Join(root, sk.Name)
		if err := os.MkdirAll(dir, 0o755); err != nil { //nolint:gosec // workspace content, sandbox-readable by design (see tools/workspace.go)
			continue
		}
		md := store.RenderUserSkillMarkdown(&sk)
		path := filepath.Join(dir, "SKILL.md")
		if cur, rerr := os.ReadFile(path); rerr != nil || string(cur) != md { // #nosec G304 — path built from validated skill name
			if werr := os.WriteFile(path, []byte(md), 0o644); werr != nil { // #nosec G306 — non-secret instructions
				continue
			}
		}
		entries = append(entries, agent.UserSkillPromptEntry{
			Name:        sk.Name,
			Description: sk.Description,
			Path:        userSkillsRoot + "/" + sk.Name + "/SKILL.md",
		})
	}
	// Remove stale materializations (renamed/disabled/deleted skills).
	if dirs, err := os.ReadDir(root); err == nil {
		for _, d := range dirs {
			if !want[d.Name()] {
				_ = os.RemoveAll(filepath.Join(root, d.Name()))
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
		return "\n\n[Skill invoked: " + sk.Name + "]\nThe user explicitly invoked their own skill \"" + sk.Name +
			"\". Read `" + userSkillsRoot + "/" + sk.Name + "/SKILL.md` now and follow its instructions for this request."
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
