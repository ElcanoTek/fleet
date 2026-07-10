package httpapi

// Projects / Spaces HTTP surface (#509): CRUD + shared project memory + the
// auditable export. Membership = the #237 team trust-group (owner always;
// team_id match otherwise); the owner alone edits the definition. Chat httpapi
// is exempt from the orchestrator OpenAPI parity test — no openapi.yaml entries.

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ElcanoTek/fleet/internal/store"
	"github.com/ElcanoTek/fleet/internal/tools"
)

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
				http.Error(w, "you are not in a team; ask an admin to set one before sharing a project", http.StatusBadRequest)
				return
			}
			p.TeamID = team
		}
		created, err := s.store.CreateProject(r.Context(), p)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
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
				team = s.resolveUserTeam(r, user)
				if team == "" {
					http.Error(w, "you are not in a team; ask an admin to set one before sharing a project", http.StatusBadRequest)
					return
				}
			}
			patch.TeamID = &team
		}
		updated, err := s.store.UpdateProject(r.Context(), user, id, patch)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		writeJSON(w, updated)
	case http.MethodDelete:
		if err := s.store.DeleteProject(r.Context(), user, id); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
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
	if list == nil {
		list = []store.Conversation{}
	}
	writeJSON(w, map[string]any{"conversations": list})
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

// projectMemories handles GET/POST /projects/{id}/memories and
// DELETE /projects/{id}/memories/{memID} — the SHARED memory scope every
// member reads and writes (distinct from personal memories, #515).
func (s *Server) projectMemories(w http.ResponseWriter, r *http.Request, p *store.Project, memID string) {
	user := userFromCtx(r.Context())
	switch {
	case memID != "" && r.Method == http.MethodDelete:
		if err := s.store.DeleteProjectMemory(r.Context(), p.ID, memID); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
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
		var req memoryRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		memory, err := s.store.CreateProjectMemory(r.Context(), p.ID, user, req.Content, req.Kind)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
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
func (s *Server) projectTurnContext(r *http.Request, conv *store.Conversation) (string, []string) {
	if conv.ProjectID == "" {
		return "", nil
	}
	proj, err := s.store.GetProject(r.Context(), conv.ProjectID)
	if err != nil || proj == nil {
		return "", nil
	}
	var bullets []string
	if pm, merr := s.store.ListProjectMemories(r.Context(), conv.ProjectID); merr == nil {
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
