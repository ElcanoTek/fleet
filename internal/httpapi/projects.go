package httpapi

// Projects / Spaces HTTP surface (#509): CRUD + shared project memory + the
// auditable export. Membership = the #237 team trust-group (owner always;
// team_id match otherwise); the owner alone edits the definition. Chat httpapi
// is exempt from the orchestrator OpenAPI parity test — no openapi.yaml entries.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/tools"
)

// errNoTeamForShare is the 400 a team_shared write gets from a caller with no
// team. It names the self-serve fix (PUT /me/team, Settings → Team) rather than
// only "ask an admin", which was a dead end on a box whose only admin came from
// the ADMIN_EMAILS env allowlist (#1157).
const errNoTeamForShare = "you are not in a team yet — create one in Settings → Team (or ask an admin to add you to an existing team), then share this project"

// resolveUserTeam returns the requester's team_id ("" when unset/unknown).
func (s *Server) resolveUserTeam(r *http.Request, user string) string {
	u, err := s.store.GetUser(r.Context(), user)
	if err != nil || u == nil {
		return ""
	}
	return u.TeamID
}

// projectForMember loads a project and enforces membership; nil = already
// responded (404 for both missing and non-member, so project ids don't leak
// membership state).
func (s *Server) projectForMember(w http.ResponseWriter, r *http.Request, user, id string) *store.Project {
	p, err := s.store.GetProject(r.Context(), id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return nil
	}
	if p == nil || !p.MemberOf(user, s.resolveUserTeam(r, user)) {
		http.Error(w, "project not found", http.StatusNotFound)
		return nil
	}
	return p
}

type projectRequest struct {
	Name           *string  `json:"name"`
	Instructions   *string  `json:"instructions"`
	DefaultPersona *string  `json:"default_persona"`
	DefaultModel   *string  `json:"default_model"`
	MCPServers     []string `json:"mcp_servers"`
	// TeamShared true shares the project with the creator's CURRENT team (the
	// server resolves the team — a caller can never name an arbitrary team);
	// false makes/keeps it personal.
	TeamShared *bool `json:"team_shared"`
	// Pinned floats the project to the top of the rail's Projects section
	// (PATCH only; a new project starts unpinned). Owner-only, enforced by
	// the store's owner-scoped UPDATE.
	Pinned *bool `json:"pinned"`
}

// projectMemoryRequest is the POST /projects/{id}/memories body: either new
// content (the normal write) or FromMemoryID, which promotes one of the
// caller's existing PERSONAL memories into this project's team learnings.
type projectMemoryRequest struct {
	Content      string `json:"content"`
	Kind         string `json:"kind"`
	FromMemoryID string `json:"from_memory_id"`
}

// projects handles GET/POST /projects.
func (s *Server) projects(w http.ResponseWriter, r *http.Request) {
	user := userFromCtx(r.Context())
	switch r.Method {
	case http.MethodGet:
		list, err := s.store.ListProjectsForUser(r.Context(), user, s.resolveUserTeam(r, user))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if list == nil {
			list = []store.Project{}
		}
		writeJSON(w, map[string]any{"projects": list})
	case http.MethodPost:
		var req projectRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		p := &store.Project{OwnerEmail: user, MCPServers: req.MCPServers}
		if req.Name != nil {
			p.Name = *req.Name
		}
		if req.Instructions != nil {
			p.Instructions = *req.Instructions
		}
		if req.DefaultPersona != nil {
			p.DefaultPersona = *req.DefaultPersona
		}
		if req.DefaultModel != nil {
			p.DefaultModel = *req.DefaultModel
		}
		if req.TeamShared != nil && *req.TeamShared {
			team := s.resolveUserTeam(r, user)
			if team == "" {
				http.Error(w, errNoTeamForShare, http.StatusBadRequest)
				return
			}
			p.TeamID = team
		}
		created, err := s.store.CreateProject(r.Context(), p)
		if err != nil {
			writeMemoryStoreError(w, err)
			return
		}
		writeJSON(w, created)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// projectByID handles /projects/{id}[/memories[/{memID}]|/export].
func (s *Server) projectByID(w http.ResponseWriter, r *http.Request) {
	user := userFromCtx(r.Context())
	rest := strings.Trim(strings.TrimPrefix(r.URL.Path, "/projects/"), "/")
	if rest == "" {
		http.Error(w, "project id required", http.StatusBadRequest)
		return
	}
	parts := strings.SplitN(rest, "/", 3)
	id := parts[0]

	// Transfer, and the picker that feeds it, are dispatched BEFORE the
	// membership gate: the case they exist for is an admin cleaning up after an
	// owner who left, and an admin is usually not a member of the project.
	// Their own authorization (owner or admin) lives in the handlers.
	//
	// `members` used to sit behind the gate while carrying its own owner-or-
	// admin check — so the check was unreachable for the one caller it names,
	// and an admin got the same 404 a stranger does. That made the transfer
	// only half-reachable: an admin could POST the handover but could not ask
	// who to hand it to, which is the whole content of the decision.
	if len(parts) == 2 {
		switch parts[1] {
		case "transfer":
			s.projectTransfer(w, r, user, id)
			return
		case "members":
			s.projectMembersByID(w, r, user, id)
			return
		}
	}

	p := s.projectForMember(w, r, user, id)
	if p == nil {
		return
	}

	if len(parts) >= 2 {
		switch parts[1] {
		case "memories":
			memID := ""
			if len(parts) == 3 {
				memID = parts[2]
			}
			s.projectMemories(w, r, p, memID)
		case "conversations":
			s.projectConversations(w, r, p)
		case "team-conversations":
			s.projectTeamConversations(w, r, p)
		case "impact":
			s.projectImpact(w, r, p)
		case "files":
			s.projectFiles(w, r, p)
		case "export":
			s.projectExport(w, r, p)
		default:
			http.Error(w, "unknown project subresource", http.StatusNotFound)
		}
		return
	}

	switch r.Method {
	case http.MethodGet:
		writeJSON(w, p)
	case http.MethodPatch:
		var req projectRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		patch := store.ProjectPatch{
			Name:           req.Name,
			Instructions:   req.Instructions,
			DefaultPersona: req.DefaultPersona,
			DefaultModel:   req.DefaultModel,
			MCPServers:     req.MCPServers,
			Pinned:         req.Pinned,
		}
		if req.TeamShared != nil {
			team := ""
			if *req.TeamShared {
				// A project ALREADY shared with a team keeps that audience.
				// Resolving "shared: true" to the owner's CURRENT team every
				// time meant an unrelated edit re-pointed the project — an
				// owner moved from `quant` to `ops` who then renamed the
				// project handed `ops` every team learning `quant` had written
				// and locked `quant` out, with no dialog and no trace. This is
				// the same "stamped, not inferred" rule migration 054 applies
				// to conversations. Re-pointing a shared project at a new team
				// is a deliberate act: unshare it, then share it again.
				team = p.TeamID
				if team == "" {
					team = s.resolveUserTeam(r, user)
				}
				if team == "" {
					http.Error(w, errNoTeamForShare, http.StatusBadRequest)
					return
				}
			}
			patch.TeamID = &team
		}
		updated, err := s.store.UpdateProject(r.Context(), user, id, patch)
		if err != nil {
			writeMemoryStoreError(w, err)
			return
		}
		writeJSON(w, updated)
	case http.MethodDelete:
		if err := s.store.DeleteProject(r.Context(), user, id); err != nil {
			writeMemoryStoreError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// projectConversations handles GET /projects/{id}/conversations — the project
// home's chat list. Scoped to the CALLER'S OWN conversations: members share
// the project definition, but conversations stay private to their creators
// (#237's rule), so this must never enumerate another member's chats.
func (s *Server) projectConversations(w http.ResponseWriter, r *http.Request, p *store.Project) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	user := userFromCtx(r.Context())
	list, err := s.store.ListProjectConversationsForUser(r.Context(), user, p.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Previews are display sugar — a failure degrades to titles-only rather
	// than failing the home.
	previews, err := s.store.ListProjectConversationPreviews(r.Context(), user, p.ID)
	if err != nil {
		previews = nil
	}
	type convWithPreview struct {
		store.Conversation
		// Preview is the last text message's snippet ("You: …" when the
		// user spoke last) — the home's 1–2 line history per chat.
		Preview string `json:"preview,omitempty"`
	}
	out := make([]convWithPreview, 0, len(list))
	for _, c := range list {
		out = append(out, convWithPreview{Conversation: c, Preview: previews[c.ID]})
	}
	writeJSON(w, map[string]any{"conversations": out})
}

// projectTeamConversations handles GET /projects/{id}/team-conversations —
// the project home's Team section (Item C3): the chats OTHER members of the
// team have explicitly shared into THIS project. Two gates, neither
// sufficient alone: a shared users.team_id and each owner's per-chat opt-in
// (ADR-0013). Membership of the project itself is already established by
// projectForMember before this runs.
//
// A personal project can never produce rows here (its chats cannot be
// team-shared, ADR-0057), so the section renders its empty state and says how
// to share — it does not pretend to be a different feature.
func (s *Server) projectTeamConversations(w http.ResponseWriter, r *http.Request, p *store.Project) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	user := userFromCtx(r.Context())
	list, err := s.store.ListProjectTeamConversations(r.Context(), user, p.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if list == nil {
		list = []store.Conversation{}
	}
	writeJSON(w, map[string]any{"conversations": list})
}

// projectImpact handles GET /projects/{id}/impact — the counts the project's
// destructive confirms quote so an owner sees what members lose BEFORE
// answering, rather than after (Item A6).
//
// It serves two confirms, because both take access away from the same people:
//
//   - DELETE the project: team learnings die with it, and every member's
//     chats leave it and become temporary (`memories`, `chats`, `members`,
//     `team_shared_chats`);
//   - untick "Share with my team": the owner's own chats stay put, while every
//     OTHER member's chats in the project are unfiled into their own Temporary
//     list, because a personal project is visible to its owner alone
//     (`chats_from_teammates`, `teammates_with_chats` — the untick confirm
//     quotes "{N} chats from teammates will move to their unfiled chats.").
//
// One read, one shape: the make-personal counts are a strict subset of the
// delete counts, and a second endpoint answering the same question about the
// same project would be a second thing to keep in sync.
func (s *Server) projectImpact(w http.ResponseWriter, r *http.Request, p *store.Project) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	impact, err := s.store.ProjectImpact(r.Context(), p.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, impact)
}

// projectTransfer handles POST /projects/{id}/transfer — hand the project to
// another member. Body: {"to_email": "..."}.
//
// A project could not change hands at all, which made "the owner left" an
// unrecoverable state: every mutation is owner-scoped, so the definition
// froze, and deleting the departing account destroyed the project and its
// team learnings outright (ADR-0057). Two callers may fix that:
//
//   - the OWNER, handing it over deliberately, and
//   - an ADMIN, because a departed owner cannot act — which is the whole
//     point, and why this route sits before the membership gate.
//
// It changes only who may edit and delete: the team, the team learnings, the
// chats and every member's access are untouched. A caller who is neither gets
// the same 404 a non-member gets for any project subresource, so the route
// leaks nothing about which projects exist.
func (s *Server) projectTransfer(w http.ResponseWriter, r *http.Request, user, projectID string) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	p, err := s.store.GetProject(r.Context(), projectID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	admin := s.isAdmin(user) || roleFromCtx(r.Context()) == store.RoleAdmin
	if p == nil || (!strings.EqualFold(p.OwnerEmail, user) && !admin) {
		http.Error(w, "project not found", http.StatusNotFound)
		return
	}
	var req struct {
		ToEmail string `json:"to_email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	updated, err := s.store.TransferProjectOwnership(r.Context(), projectID, req.ToEmail)
	if err != nil {
		// ONE message for every "that target won't do" case. Splitting it into
		// "no such user" vs "not a member of this team" turned the route into
		// an account-existence oracle over arbitrary addresses — exactly the
		// disclosure the login form's constant-time dummy hash goes out of its
		// way to deny. Anything else is a server fault, not a bad request.
		if errors.Is(err, store.ErrNotAProjectMember) || errors.Is(err, store.ErrUserNotFound) {
			http.Error(w, store.ErrNotAProjectMember.Error(), http.StatusBadRequest)
			return
		}
		if strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "required") {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		http.Error(w, "transfer failed", http.StatusInternalServerError)
		return
	}
	//nolint:gosec // G706: %q escapes CR/LF, so the path-supplied project id cannot forge a log line; the two emails are an authenticated caller and a normalized DB value.
	log.Printf("projects: %q transferred project %q to %q", user, projectID, updated.OwnerEmail)
	writeJSON(w, updated)
}

// projectMembers handles GET /projects/{id}/members — the accounts a project
// can be transferred to: everyone in its team, plus the current owner. Emails
// only.
//
// OWNER (or admin) only, not every member. It enumerates the whole team,
// including people who have never shared a chat or written a learning, which
// is a directory read the project's own surfaces do not otherwise give a plain
// member. The only caller that needs it is the transfer picker, and only the
// owner and admins can transfer.
func (s *Server) projectMembersByID(w http.ResponseWriter, r *http.Request, user, projectID string) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	p, err := s.store.GetProject(r.Context(), projectID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Same shape as projectTransfer's gate, and the same 404 for everyone
	// else: this list enumerates every account in a team, which is more than a
	// plain member can learn from the project's own surfaces.
	admin := s.isAdmin(user) || roleFromCtx(r.Context()) == store.RoleAdmin
	if p == nil || (!strings.EqualFold(p.OwnerEmail, user) && !admin) {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	emails, err := s.store.ProjectMemberEmails(r.Context(), p.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if emails == nil {
		emails = []string{}
	}
	writeJSON(w, map[string]any{"members": emails})
}

// projectFile is one entry in the project home's Sources list.
type projectFile struct {
	ConversationID    string `json:"conversation_id"`
	ConversationTitle string `json:"conversation_title"`
	// Path is relative to the conversation's workspace root — the exact
	// segment GET /conversations/{id}/workspace/{path} streams.
	Path       string `json:"path"`
	Name       string `json:"name"`
	Size       int64  `json:"size"`
	ModifiedAt int64  `json:"modified_at"`
}

// maxProjectFiles caps the Sources listing — a runaway workspace (a build
// tree, node_modules the agent unpacked) must not stall the project home.
// Newest-first, so the cap drops the oldest entries.
const maxProjectFiles = 200

// projectFiles handles GET /projects/{id}/files — the project home's Sources
// panel: every workspace file (uploads, generated CSVs/plots, …) across the
// CALLER'S OWN conversations in the project, newest first. Same privacy rule
// as projectConversations: another member's files are never listed. Download
// goes through the existing per-conversation workspace streamer, which owns
// the path-traversal guards.
func (s *Server) projectFiles(w http.ResponseWriter, r *http.Request, p *store.Project) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	user := userFromCtx(r.Context())
	convs, err := s.store.ListProjectConversationsForUser(r.Context(), user, p.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	files := []projectFile{}
	for _, conv := range convs {
		wsDir := tools.WorkspaceDirForConversation(conv.ID)
		root, err := filepath.EvalSymlinks(wsDir)
		if err != nil {
			// Most conversations never touched a file — no workspace dir.
			continue
		}
		title := conv.Title
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
			// Per-entry errors (perms, vanished mid-walk) skip the entry,
			// never abort the whole listing.
			if walkErr != nil || d.IsDir() {
				return nil //nolint:nilerr // best-effort listing
			}
			info, err := d.Info()
			if err != nil {
				return nil //nolint:nilerr // best-effort listing
			}
			// Regular files only. Every conversation workspace carries the
			// bundle-mount symlinks (personas, protocols, shared, skills,
			// system_prompts) pointing OUTSIDE the workspace root; WalkDir
			// does not follow them, so each surfaced as a "file" whose size
			// was the length of its target path — and whose download
			// correctly tripped the workspace path-traversal guard, dumping
			// "path escapes workspace" into a tab. They are plumbing, not
			// sources: a Sources entry must be a real file the user can open.
			if !info.Mode().IsRegular() {
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return nil //nolint:nilerr // best-effort listing
			}
			files = append(files, projectFile{
				ConversationID:    conv.ID,
				ConversationTitle: title,
				Path:              filepath.ToSlash(rel),
				Name:              d.Name(),
				Size:              info.Size(),
				ModifiedAt:        info.ModTime().Unix(),
			})
			return nil
		})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].ModifiedAt > files[j].ModifiedAt })
	truncated := false
	if len(files) > maxProjectFiles {
		files = files[:maxProjectFiles]
		truncated = true
	}
	writeJSON(w, map[string]any{"files": files, "truncated": truncated})
}

// mayManageProjectMemory reports whether user may mutate this team learning:
// its writer manages their own entries, and the project owner manages all of
// them. Members are peers otherwise — a shared learning nobody can correct is
// worse than one anybody can, but "anybody edits anybody" is not a model a
// team can reason about, so the rule is one line: yours, or yours to own.
func mayManageProjectMemory(p *store.Project, m *store.Memory, user string) bool {
	return strings.EqualFold(m.UserEmail, user) || strings.EqualFold(p.OwnerEmail, user)
}

// projectMemoryPermitted resolves memID within the project and enforces
// mayManageProjectMemory. nil = already responded (404 for an id that is not
// this project's, 403 for another member's entry).
func (s *Server) projectMemoryPermitted(w http.ResponseWriter, r *http.Request, p *store.Project, memID, user string) *store.Memory {
	m, err := s.store.GetProjectMemory(r.Context(), p.ID, memID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return nil
	}
	if m == nil {
		http.Error(w, "memory not found", http.StatusNotFound)
		return nil
	}
	if !mayManageProjectMemory(p, m, user) {
		http.Error(w, "only the author or the project owner can change this team learning", http.StatusForbidden)
		return nil
	}
	return m
}

// projectMemories handles GET/POST /projects/{id}/memories and
// PATCH/DELETE /projects/{id}/memories/{memID} — the SHARED memory scope every
// member reads and writes (distinct from personal memories, #515), surfaced to
// users as the project's "team learnings".
//
// Reads and writes are open to every member (that is what shared memory is);
// CHANGING an existing entry is narrowed to its author or the project owner,
// so one member cannot quietly rewrite another's contribution. Retire (a
// PATCH) is the default remove — it drops the entry from injection and keeps
// the record of who wrote what.
func (s *Server) projectMemories(w http.ResponseWriter, r *http.Request, p *store.Project, memID string) {
	user := userFromCtx(r.Context())
	switch {
	case memID != "" && r.Method == http.MethodPatch:
		if s.projectMemoryPermitted(w, r, p, memID, user) == nil {
			return
		}
		var req memoryPatchRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		// The full patch, validity window included: the request type carries
		// valid_from/valid_to and the personal-memory PATCH honors them, so
		// dropping them here answered 200 to a team-learning window change
		// that never happened.
		memory, err := s.store.UpdateProjectMemory(r.Context(), p.ID, memID, store.MemoryPatch{
			Content:   req.Content,
			Kind:      req.Kind,
			Pinned:    req.Pinned,
			Retired:   req.Retired,
			ValidFrom: req.ValidFrom,
			ValidTo:   req.ValidTo,
		})
		if err != nil {
			writeMemoryStoreError(w, err)
			return
		}
		writeJSON(w, memory)
	case memID != "" && r.Method == http.MethodDelete:
		if s.projectMemoryPermitted(w, r, p, memID, user) == nil {
			return
		}
		if err := s.store.DeleteProjectMemory(r.Context(), p.ID, memID); err != nil {
			writeMemoryStoreError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	case memID == "" && r.Method == http.MethodGet:
		memories, err := s.store.ListProjectMemories(r.Context(), p.ID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if memories == nil {
			memories = []store.Memory{}
		}
		writeJSON(w, map[string]any{"memories": memories})
	case memID == "" && r.Method == http.MethodPost:
		var req projectMemoryRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		// Promotion path (Item D5): {"from_memory_id": ...} MOVES one of the
		// caller's own personal memories into this project instead of writing
		// new content — the migration for team facts saved personally before
		// the destination picker existed. It moves rather than copies, so the
		// same fact is not injected twice in every project chat.
		if id := strings.TrimSpace(req.FromMemoryID); id != "" {
			memory, err := s.store.MoveMemoryToProject(r.Context(), user, id, p.ID)
			if err != nil {
				writeMemoryStoreError(w, err)
				return
			}
			writeJSON(w, memory)
			return
		}
		memory, err := s.store.CreateProjectMemory(r.Context(), p.ID, user, req.Content, req.Kind)
		if err != nil {
			writeMemoryStoreError(w, err)
			return
		}
		writeJSON(w, memory)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// projectExport handles GET /projects/{id}/export: the project's full config
// plus references to its DB runtime state (shared memories verbatim,
// conversation ids) — auditable/exportable without writing client content
// into fleet core.
func (s *Server) projectExport(w http.ResponseWriter, r *http.Request, p *store.Project) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	// OWNER (or admin) only, like the member list. `conversation_ids` covers
	// EVERY member's chats in the project, so a plain member could learn how
	// many chats each colleague has here and collect a valid id set — neither
	// of which any other project surface gives them. An id alone unlocks
	// nothing (team-view needs the owner's opt-in; the transcript and
	// workspace routes are owner-scoped), but the export exists for the one
	// person who can destroy the project, and that is who should have it.
	user := userFromCtx(r.Context())
	if !strings.EqualFold(p.OwnerEmail, user) && !s.isAdmin(user) && roleFromCtx(r.Context()) != store.RoleAdmin {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	memories, err := s.store.ListProjectMemories(r.Context(), p.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	convIDs, err := s.store.ListProjectConversationIDs(r.Context(), p.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if memories == nil {
		memories = []store.Memory{}
	}
	if convIDs == nil {
		convIDs = []string{}
	}
	// Own the download filename here, like every other export endpoint
	// (conversation export, dataset CSV, prompts, adoption). The web proxy used
	// to synthesize `project-<uuid>.json` itself, which meant the saved file was
	// named after an opaque id; exportFilename gives the project's own name plus
	// a short id, and one owner of the filename means the proxy just forwards it.
	// Content-Type is left to writeJSON below, which sets it.
	w.Header().Set(
		"Content-Disposition",
		fmt.Sprintf(`attachment; filename="%s"`, exportFilename(p.Name, p.ID, "json", "project")),
	)
	writeJSON(w, map[string]any{
		"version":          "1",
		"project":          p,
		"memories":         memories,
		"conversation_ids": convIDs,
	})
}

// projectMemoryContents renders a project's ACTIVE shared memories as
// injectable bullets, each tagged so the model (and the user reading the
// prompt) can tell shared context from personal memory.
func projectMemoryContents(memories []store.Memory) []string {
	out := make([]string, 0, len(memories))
	for _, m := range memories {
		if m.Source == "proposed" || m.Retired() {
			continue
		}
		if len(out) >= 50 {
			break
		}
		content := strings.TrimSpace(m.Content)
		if content == "" {
			continue
		}
		if note := memoryAnnotation(&m); note != "" {
			content += " (" + note + ")"
		}
		out = append(out, "[project] "+content)
	}
	return out
}

// projectTurnContext resolves the turn-time project injection (#509): the
// standing instructions plus the shared memories as tagged bullets. Empty for
// non-project conversations; best-effort (a load failure degrades to no
// project context rather than failing the turn).
func (s *Server) projectTurnContext(ctx context.Context, conv *store.Conversation) (string, []string) {
	if conv.ProjectID == "" {
		return "", nil
	}
	proj, err := s.store.GetProject(ctx, conv.ProjectID)
	if err != nil || proj == nil {
		return "", nil
	}
	var bullets []string
	if pm, merr := s.store.ListProjectMemories(ctx, conv.ProjectID); merr == nil {
		bullets = projectMemoryContents(pm)
	}
	return proj.Instructions, bullets
}

// createConversationForRequest is the create-path split (#509): project-bound
// creation validates membership + inherits the project's defaults where the
// request left them blank; otherwise the plain create. false = already
// responded.
func (s *Server) createConversationForRequest(w http.ResponseWriter, r *http.Request, user, projectID, title, persona, model string, lockdown bool) (*store.Conversation, bool) {
	var (
		conv *store.Conversation
		err  error
	)
	if projectID != "" {
		p := s.projectForMember(w, r, user, projectID)
		if p == nil {
			return nil, false
		}
		if persona == "" {
			persona = p.DefaultPersona
		}
		if model == "" {
			model = p.DefaultModel
		}
		if lockdown && model != "" && !s.cfg.LockdownAllows(model) {
			http.Error(w, "project default model not allowed in lockdown mode", http.StatusBadRequest)
			return nil, false
		}
		conv, err = s.store.CreateProjectConversation(r.Context(), user, title, persona, model, lockdown, p.ID, p.MCPServers)
	} else {
		conv, err = s.store.CreateConversation(r.Context(), user, title, persona, model, lockdown)
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return nil, false
	}
	return conv, true
}
