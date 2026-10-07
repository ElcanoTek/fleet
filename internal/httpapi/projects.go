package httpapi

// Projects / Spaces HTTP surface (#509): CRUD + shared project memory + the
// auditable export. Membership = the #237 team trust-group (owner always;
// team_id match otherwise); the owner alone edits the definition. Chat httpapi
// is exempt from the orchestrator OpenAPI parity test — no openapi.yaml entries.

import (
	"container/heap"
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
	"time"

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
		case "my-state":
			s.projectMyState(w, r, p)
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
	// Each row says whether the viewer has branched it ("You branched this",
	// ADR-0079). The row itself still opens the owner's live chat.
	ids := make([]string, 0, len(list))
	for _, c := range list {
		ids = append(ids, c.ID)
	}
	branches, err := s.store.ViewerBranches(r.Context(), user, ids)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	type teamConversation struct {
		store.Conversation
		ViewerBranch *store.ViewerBranch `json:"viewer_branch"`
	}
	out := make([]teamConversation, 0, len(list))
	for _, c := range list {
		item := teamConversation{Conversation: c}
		if vb, ok := branches[c.ID]; ok {
			item.ViewerBranch = &vb
		}
		out = append(out, item)
	}
	writeJSON(w, map[string]any{"conversations": out})
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
	// The store re-checks the owner under the project's row lock: an owner's
	// request admitted above must not land after another transfer took the
	// project from them. An admin acts for no particular owner.
	actingOwner := user
	if admin {
		actingOwner = ""
	}
	updated, err := s.store.TransferProjectOwnership(r.Context(), projectID, req.ToEmail, actingOwner)
	if err != nil {
		if errors.Is(err, store.ErrNotProjectOwner) {
			http.Error(w, "project not found", http.StatusNotFound)
			return
		}
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

// projectFile is one entry in the LEGACY flat Sources list (`files`), kept
// for clients that predate the grouped shape.
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

// sourcesFile is one file in a Sources group.
type sourcesFile struct {
	Path       string `json:"path"`
	Name       string `json:"name"`
	Size       int64  `json:"size"`
	ModifiedAt int64  `json:"modified_at"`
	// Shared is "an output the owner has not unchecked". Always false for a
	// non-output, which is download-only and never shared.
	Shared bool `json:"shared"`
	// Output: presented in a reply (a file chip). Only outputs are shareable.
	Output bool `json:"output"`
	// YourCopy: copied in when the caller branched a teammate's chat.
	YourCopy bool `json:"your_copy"`
}

// sourcesGroup is one chat's files on the project home's Sources panel.
type sourcesGroup struct {
	ConversationID string `json:"conversation_id"`
	Title          string `json:"title"`
	OwnerEmail     string `json:"owner_email"`
	// Mine: the caller owns the chat ("Your chats"); otherwise it is a
	// teammate's shared chat ("From your team").
	Mine        bool `json:"mine"`
	TeamVisible bool `json:"team_visible"`
	// IsBranch: the caller's branch of a teammate's chat (its copied files
	// are labelled "Your copy · <date>").
	IsBranch     bool  `json:"is_branch"`
	BranchedAt   int64 `json:"branched_at,omitempty"`
	LastActiveAt int64 `json:"last_active_at"`
	// FileCount is len(Files): the rows the group lists, so the header and
	// the list always agree. SharedCount is the shared outputs among them.
	FileCount   int           `json:"file_count"`
	SharedCount int           `json:"shared_count"`
	Files       []sourcesFile `json:"files"`
}

// maxProjectFiles caps each Sources group (and the legacy flat list) — a
// runaway workspace (a build tree, node_modules the agent unpacked) must not
// stall the project home. Newest-first, so the cap drops the oldest entries.
const maxProjectFiles = 200

// Sources reads every chat it lists — a workspace walk plus output discovery
// per chat — so the number of chats is bounded too, per half: the
// maxSourcesGroups most recently active chats WITH files are listed, and at
// most maxSourcesChatsScanned chats are examined to find them (a project of
// hundreds of file-less chats must not cost hundreds of reads). Anything left
// over is reported through `truncated` (and the additive `groups_truncated`).
// Vars so tests can shrink them.
var (
	maxSourcesGroups       = 50
	maxSourcesChatsScanned = 200
)

// The per-half caps above multiply per-chat ceilings (a 4 MiB transcript
// parse, up to 500 output stats, a bounded workspace walk), so one request is
// also bounded as a whole: at most maxSourcesDiscoveries chats are examined
// across BOTH halves, and none is started once sourcesDiscoveryBudget has
// elapsed. The first half (the caller's own chats) may use at most half of
// each, so a project full of the caller's file-less chats cannot starve the
// team's shared files; the team half gets whatever is left. What the budget
// leaves out is reported as truncated, like the per-half cuts. The focused
// chat is the one exception (one more examination), so "Manage in Sources"
// still lands. Vars so tests can shrink them.
var (
	maxSourcesDiscoveries  = 100
	sourcesDiscoveryBudget = 4 * time.Second
	// sourcesFocusBudget is the focused chat's own window: it is examined
	// even past the budgets above, but never without a deadline.
	sourcesFocusBudget = 2 * time.Second
)

// sourcesBudget is one Sources request's shared discovery budget.
type sourcesBudget struct {
	left     int
	deadline time.Time
}

// newSourcesBudgets returns the first half's budget (half the chats, half
// the time) and a function giving the second half the rest once the first is
// done.
func newSourcesBudgets() (first *sourcesBudget, rest func() *sourcesBudget) {
	start := time.Now()
	half := maxSourcesDiscoveries / 2
	first = &sourcesBudget{left: half, deadline: start.Add(sourcesDiscoveryBudget / 2)}
	return first, func() *sourcesBudget {
		// At least half the time from NOW: a first half that overran its
		// own deadline (one examination finishing late) must not leave the
		// team half none.
		deadline := start.Add(sourcesDiscoveryBudget)
		if floor := time.Now().Add(sourcesDiscoveryBudget / 2); floor.After(deadline) {
			deadline = floor
		}
		return &sourcesBudget{
			left:     maxSourcesDiscoveries - (half - first.left),
			deadline: deadline,
		}
	}
}

// spend takes one chat examination from the budget; false once it is spent.
func (b *sourcesBudget) spend() bool {
	if b.left <= 0 || !time.Now().Before(b.deadline) {
		return false
	}
	b.left--
	return true
}

// maxWorkspaceWalkEntries bounds the directory entries one workspace walk
// visits. The heap below bounds what is KEPT; this bounds the work, so a tree
// with millions of entries cannot stall the project home either. Past it the
// listing stops and is reported truncated.
var maxWorkspaceWalkEntries = 20000 // a var so tests can shrink it

// newestFiles is a bounded min-heap of the newest files seen so far: its root
// is the WORST kept entry in newest-first order (oldest modtime; on a tie,
// the larger path), so a better candidate replaces it in O(log n).
type newestFiles []sourcesFile

func newerFile(a, b sourcesFile) bool {
	if a.ModifiedAt != b.ModifiedAt {
		return a.ModifiedAt > b.ModifiedAt
	}
	return a.Path < b.Path
}

func (h newestFiles) Len() int           { return len(h) }
func (h newestFiles) Less(i, j int) bool { return newerFile(h[j], h[i]) }
func (h newestFiles) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *newestFiles) Push(x any)        { *h = append(*h, x.(sourcesFile)) }
func (h *newestFiles) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

// walkWorkspaceFiles lists the newest `limit` regular, non-upload files in
// convID's workspace, newest first, and reports whether anything was left out
// (more files than the limit, or the walk hit maxWorkspaceWalkEntries).
// Only the bounded set is held during the walk — a runaway tree is never
// collected whole and sorted. Symlinks are skipped (WalkDir does not follow
// them): every workspace carries the bundle-mount symlinks (personas,
// protocols, shared, skills, system_prompts) pointing OUTSIDE the root, and a
// Sources entry must be a real file the user can open. The attachments/ and
// user-skills/ subtrees are skipped whole: uploads and the owner's
// materialized private skills are never listed in Sources (ADR-0079). So is
// every dot-named file or directory (hiddenWorkspaceName), .fleet/ included.
// walkWorkspaceFilesAbandonable runs the walk on its own goroutine and returns
// as soon as ctx is done — empty and truncated — even if a directory read is
// stuck in the kernel on a stalled filesystem (the walk stops at ctx between
// entries once that read returns, and its result is discarded).
func walkWorkspaceFilesAbandonable(ctx context.Context, convID string, limit int) ([]sourcesFile, bool) {
	type result struct {
		files     []sourcesFile
		truncated bool
	}
	release, ok := acquireFSWorker()
	if !ok {
		return []sourcesFile{}, true // a stalled filesystem holds every slot
	}
	done := make(chan result, 1)
	go func() {
		defer release()
		files, truncated := walkWorkspaceFiles(ctx, convID, limit)
		done <- result{files, truncated}
	}()
	select {
	case r := <-done:
		return r.files, r.truncated
	case <-ctx.Done():
		return []sourcesFile{}, true
	}
}

// hiddenWorkspaceName reports whether a workspace entry is hidden from the
// Sources walk: anything dot-named. That covers fleet's own .fleet/ tree (the
// tool-output recovery slots — .next-slot, .used, artifact-*.txt — and their
// .gitignore) as well as .git/, .cache/ and the like: internal state the
// chat never presented, which only made a group look full of files nobody
// made. An OUTPUT is still listed whatever its name — the agent presented
// it, and a shared one needs its row to be unshared by.
func hiddenWorkspaceName(name string) bool {
	return strings.HasPrefix(name, ".")
}

// workspaceWalkHook is a test seam run for each entry the walk visits.
var workspaceWalkHook func()

func walkWorkspaceFiles(ctx context.Context, convID string, limit int) (files []sourcesFile, truncated bool) {
	root, err := filepath.EvalSymlinks(tools.WorkspaceDirForConversation(convID))
	if err != nil {
		// Most conversations never touched a file — no workspace dir.
		return []sourcesFile{}, false
	}
	h := make(newestFiles, 0, limit)
	visited := 0
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, walkErr error) error {
		visited++
		if workspaceWalkHook != nil {
			workspaceWalkHook()
		}
		// The entry budget bounds the work; ctx bounds the time (Sources
		// gives each half a deadline, and a slow filesystem must not let one
		// chat's walk outlast it).
		if visited > maxWorkspaceWalkEntries || ctx.Err() != nil {
			truncated = true
			return filepath.SkipAll
		}
		// Per-entry errors (perms, vanished mid-walk) skip the entry,
		// never abort the whole listing.
		if walkErr != nil {
			return nil //nolint:nilerr // best-effort listing
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return nil //nolint:nilerr // best-effort listing
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			// attachments/ and user-skills/ (privateWorkspaceDirs) are the
			// owner's uploads and private skills fleet put there, not work
			// the chat produced; a dot-named directory (.fleet/, .git/, …)
			// is internal state, never listed either.
			if rel != "." && (isPrivateWorkspacePath(rel) || hiddenWorkspaceName(d.Name())) {
				return filepath.SkipDir
			}
			return nil
		}
		if hiddenWorkspaceName(d.Name()) {
			return nil
		}
		info, err := d.Info()
		if err != nil || !info.Mode().IsRegular() {
			return nil //nolint:nilerr // best-effort listing
		}
		f := sourcesFile{
			Path:       rel,
			Name:       d.Name(),
			Size:       info.Size(),
			ModifiedAt: info.ModTime().Unix(),
		}
		switch {
		case len(h) < limit:
			heap.Push(&h, f)
		case limit > 0 && newerFile(f, h[0]):
			truncated = true
			h[0] = f
			heap.Fix(&h, 0)
		default:
			truncated = true
		}
		return nil
	})
	files = make([]sourcesFile, len(h))
	for i := len(h) - 1; i >= 0; i-- {
		files[i] = heap.Pop(&h).(sourcesFile)
	}
	return files, truncated
}

// projectFiles handles GET /projects/{id}/files — the project home's Sources
// panel, grouped by chat (ADR-0079):
//
//   - the caller's OWN chats: every regular file in each workspace except
//     uploads and hidden (dot-named) entries, each flagged output / shared /
//     your_copy. Non-outputs are download-only; file_count counts every
//     listed row, shared_count the shared outputs;
//   - teammates' chats shared with the caller's team in THIS project (the
//     same gates as team-conversations): their SHARED outputs only, downloaded
//     through the team-files route, which re-checks every gate.
//
// Each half lists at most maxSourcesGroups chats (the most recently active
// with files), examining at most maxSourcesChatsScanned, and the request as a
// whole examines at most maxSourcesDiscoveries chats within
// sourcesDiscoveryBudget; the rest is reported as truncated.
//
// The optional ?focus=<conversation id> names the chat a "Manage in Sources"
// link sends the caller to: its group is included even past those caps, when
// that chat is in one of the two listings above (the same gates; an id the
// caller cannot see is ignored, indistinguishable from a chat with no files).
//
// Chats with no files are omitted. `files` is the legacy flat list of the
// caller's own files, kept for older clients. Another member's PRIVATE chat
// is never read here — the teammate half starts from the team listing.
func (s *Server) projectFiles(w http.ResponseWriter, r *http.Request, p *store.Project) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	ctx := r.Context()
	user := userFromCtx(ctx)
	// focus is the chat a "Manage in Sources" link is sending the caller to.
	// Its group is listed even past the caps below, but only if the chat
	// passes the same gates as any other group: it is found in the caller's
	// OWN project listing or in the team listing, never read by id alone.
	focus := r.URL.Query().Get("focus")
	if focus != "" && !validSourcesFocus(focus) {
		http.Error(w, "invalid focus", http.StatusBadRequest)
		return
	}
	convs, err := s.store.ListProjectConversationsForUser(ctx, user, p.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// Most recently active first (the listing's order, made explicit here
	// because the caps below keep the head of it).
	sortConversationsRecentFirst(convs)
	ids := make([]string, 0, min(len(convs), maxSourcesChatsScanned)+1)
	for i, c := range convs {
		// Past the scan bound only the focused chat — one of the caller's
		// own, being in this listing — may still be listed.
		if i < maxSourcesChatsScanned || c.ID == focus {
			ids = append(ids, c.ID)
		}
	}
	origins, err := s.store.BranchOriginsFor(ctx, ids)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	groups := []sourcesGroup{}
	flat := []projectFile{}
	truncated := false

	addMine := func(ctx context.Context, conv store.Conversation) (bool, error) {
		g, ok, gTruncated, err := s.ownSourcesGroup(ctx, conv, origins[conv.ID])
		if err != nil {
			return false, err
		}
		truncated = truncated || gTruncated
		if !ok {
			return false, nil
		}
		for _, f := range g.Files {
			flat = append(flat, projectFile{
				ConversationID: conv.ID, ConversationTitle: conv.Title,
				Path: f.Path, Name: f.Name, Size: f.Size, ModifiedAt: f.ModifiedAt,
			})
		}
		groups = append(groups, g)
		return true, nil
	}

	// focusDone is set once the focused chat has been examined (listed, or
	// found to have no files) so it is never listed twice.
	focusDone := focus == ""
	mineBudget, teamBudget := newSourcesBudgets()
	mineCut, err := listSourcesHalf(ctx, convs, focus, &focusDone, mineBudget, addMine)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	team, err := s.store.ListProjectTeamConversations(ctx, user, p.ID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sortConversationsRecentFirst(team)

	addTeam := func(ctx context.Context, conv store.Conversation) (bool, error) {
		g, ok, gTruncated, err := s.teamSourcesGroup(ctx, conv)
		if err != nil {
			return false, err
		}
		truncated = truncated || gTruncated
		if ok {
			groups = append(groups, g)
		}
		return ok, nil
	}

	teamCut, err := listSourcesHalf(ctx, team, focus, &focusDone, teamBudget(), addTeam)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	groupsTruncated := mineCut || teamCut

	sort.SliceStable(flat, func(i, j int) bool { return flat[i].ModifiedAt > flat[j].ModifiedAt })
	if len(flat) > maxProjectFiles {
		flat = flat[:maxProjectFiles]
		truncated = true
	}
	writeJSON(w, map[string]any{
		"groups": groups, "files": flat,
		"truncated":        truncated || groupsTruncated,
		"groups_truncated": groupsTruncated,
	})
}

// ownSourcesGroup builds the Sources group of one of the caller's OWN chats
// (origin is its teammate-branch origin, if any); ok is false for a chat with
// no files. truncated reports a bounded walk or discovery cut something.
func (s *Server) ownSourcesGroup(ctx context.Context, conv store.Conversation, origin *store.BranchOrigin) (g sourcesGroup, ok, truncated bool, err error) {
	all, walkTruncated := walkWorkspaceFilesAbandonable(ctx, conv.ID, maxProjectFiles)
	if walkTruncated {
		truncated = true
	}
	// Outputs are resolved independently of the walk: the walk is
	// bounded (file cap and visit budget), so an empty walk — e.g. the
	// budget spent on thousands of empty directories — does not mean the
	// chat has no current output. Skip the chat only when BOTH are empty.
	outs, outsTruncated, err := s.ownerOutputs(ctx, conv.ID)
	if err != nil {
		return g, false, false, err
	}
	if outsTruncated {
		truncated = true
	}
	if len(all) == 0 && len(outs) == 0 {
		return g, false, truncated, nil
	}
	copied := map[string]bool{}
	g = sourcesGroup{
		ConversationID: conv.ID,
		Title:          conv.Title,
		OwnerEmail:     conv.UserEmail,
		Mine:           true,
		TeamVisible:    conv.TeamVisible,
		LastActiveAt:   conv.UpdatedAt,
		SharedCount:    countShared(outs),
	}
	if origin != nil {
		g.IsBranch, g.BranchedAt = true, origin.BranchedAt
		for _, f := range origin.CopiedFiles {
			copied[f.Path] = true
		}
	}
	// Every current output first, whatever its age: SharedCount counts
	// them, and each needs its row (and toggle). The
	// bounded walk keeps only the newest files, so an older output can be
	// missing from `all` — listing from the walk alone left a shared
	// output counted but with no row to unshare it by. Then the newest
	// non-output files fill the group up to the cap.
	listed := make(map[string]bool, len(outs))
	for _, o := range outs {
		listed[o.Path] = true
		g.Files = append(g.Files, sourcesFile{
			Path: o.Path, Name: o.Name, Size: o.Size, ModifiedAt: o.ModifiedAt,
			Shared: o.Shared, Output: true, YourCopy: copied[o.Path],
		})
	}
	for _, f := range all {
		if listed[f.Path] {
			continue
		}
		if len(g.Files) >= maxProjectFiles {
			truncated = true
			break
		}
		f.YourCopy = copied[f.Path]
		g.Files = append(g.Files, f)
	}
	// FileCount is what the group lists — outputs and download-only files
	// alike — so the header never says "0 files" over a list of rows.
	g.FileCount = len(g.Files)
	sort.SliceStable(g.Files, func(i, j int) bool { return newerFile(g.Files[i], g.Files[j]) })
	return g, true, truncated, nil
}

// teamSourcesGroup builds the Sources group of a teammate's chat from the
// team listing: its SHARED outputs only; ok is false when it has none.
func (s *Server) teamSourcesGroup(ctx context.Context, conv store.Conversation) (g sourcesGroup, ok, truncated bool, err error) {
	outs, outsTruncated, err := s.ownerOutputs(ctx, conv.ID)
	if err != nil {
		return g, false, false, err
	}
	if outsTruncated {
		truncated = true
	}
	g = sourcesGroup{
		ConversationID: conv.ID,
		Title:          conv.Title,
		OwnerEmail:     conv.UserEmail,
		TeamVisible:    conv.TeamVisible,
		LastActiveAt:   conv.UpdatedAt,
		Files:          []sourcesFile{},
	}
	for _, o := range outs {
		if !o.Shared {
			continue // a teammate never sees a file the owner held back
		}
		g.Files = append(g.Files, sourcesFile{
			Path: o.Path, Name: o.Name, Size: o.Size, ModifiedAt: o.ModifiedAt,
			Shared: true, Output: true,
		})
	}
	if len(g.Files) == 0 {
		return g, false, truncated, nil
	}
	if len(g.Files) > maxProjectFiles {
		g.Files = g.Files[:maxProjectFiles]
		truncated = true
	}
	// Both counts after the cap: every listed row here is a shared output.
	g.FileCount, g.SharedCount = len(g.Files), len(g.Files)
	return g, true, truncated, nil
}

// listSourcesHalf adds the groups of one half of Sources (the caller's own
// chats, or the team's) through add, most recently active first, up to
// maxSourcesGroups groups out of at most maxSourcesChatsScanned chats, each
// examination spending one unit of the half's budget and running under the
// half's deadline — a slow chat is cut off at it (ctx reaches the history
// read and the workspace walk) rather than eating into the other half's
// time; cut reports that any bound left chats out. The focused chat (if not
// done yet) is added even past the bounds, on the request's own context —
// but only from this list, so focus never reaches a chat the listing's own
// gates did not return.
func listSourcesHalf(ctx context.Context, list []store.Conversation, focus string, focusDone *bool, budget *sourcesBudget, add func(context.Context, store.Conversation) (bool, error)) (cut bool, err error) {
	halfCtx, cancel := context.WithDeadline(ctx, budget.deadline)
	defer cancel()
	n := 0
	for i, conv := range list {
		if n >= maxSourcesGroups || i >= maxSourcesChatsScanned || !budget.spend() {
			cut = true
			break
		}
		added, err := add(halfCtx, conv)
		if err != nil {
			if (ctx.Err() == nil && halfCtx.Err() != nil) || errors.Is(err, errFilesystemBusy) {
				// The half's deadline (not the request's), or a stalled
				// filesystem holding every worker: stop here and say so,
				// like any other bound.
				cut = true
				break
			}
			return cut, err
		}
		if conv.ID == focus && halfCtx.Err() == nil {
			*focusDone = true
		}
		if added {
			n++
		}
	}
	if *focusDone {
		return cut, nil
	}
	for _, conv := range list {
		if conv.ID == focus {
			*focusDone = true
			// Past the count cap, not past a deadline: the focused chat gets
			// a window of its own, so "Manage in Sources" cannot hang on a
			// stalled filesystem either. Cut off, it is simply not listed.
			focusCtx, cancelFocus := context.WithTimeout(ctx, sourcesFocusBudget)
			defer cancelFocus()
			_, err := add(focusCtx, conv)
			if err != nil && ((ctx.Err() == nil && focusCtx.Err() != nil) || errors.Is(err, errFilesystemBusy)) {
				return true, nil
			}
			return cut, err
		}
	}
	return cut, nil
}

// validSourcesFocus bounds the ?focus id to the conversation-id alphabet
// (UUIDs in practice) before it reaches any lookup.
func validSourcesFocus(id string) bool {
	if len(id) == 0 || len(id) > 64 {
		return false
	}
	for _, c := range id {
		ok := c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '-' || c == '_'
		if !ok {
			return false
		}
	}
	return true
}

// sortConversationsRecentFirst orders chats most recently active first
// (updated_at desc, then id desc — the store listings' own order).
func sortConversationsRecentFirst(convs []store.Conversation) {
	sort.SliceStable(convs, func(i, j int) bool {
		if convs[i].UpdatedAt != convs[j].UpdatedAt {
			return convs[i].UpdatedAt > convs[j].UpdatedAt
		}
		return convs[i].ID > convs[j].ID
	})
}

// projectMyState handles GET/PUT /projects/{id}/my-state — the caller's own
// UI state for this project (ADR-0079): the getting-started card's "Keep
// personal" and has-shared-a-chat, and which Sources groups they left open.
// Stored per user so it follows them across devices. PUT takes any subset of
// {kept_personal, sources_open} (sources_open merges) and answers the full
// state; has_shared_chat is set server-side by a successful share only.
func (s *Server) projectMyState(w http.ResponseWriter, r *http.Request, p *store.Project) {
	user := userFromCtx(r.Context())
	switch r.Method {
	case http.MethodGet:
		st, err := s.store.GetProjectUserState(r.Context(), p.ID, user)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, st)
	case http.MethodPut:
		var body struct {
			KeptPersonal *bool           `json:"kept_personal"`
			SourcesOpen  map[string]bool `json:"sources_open"`
		}
		if !decodeJSONBody(w, r, &body) {
			return
		}
		st, err := s.store.UpdateProjectUserState(r.Context(), p.ID, user, body.KeptPersonal, body.SourcesOpen)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, st)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
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
		memory, err := s.store.UpdateProjectMemory(r.Context(), p.ID, memID, user, store.MemoryPatch{
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
		if err := s.store.DeleteProjectMemory(r.Context(), p.ID, memID, user); err != nil {
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
		if errors.Is(err, store.ErrProjectNotAccessible) {
			http.Error(w, "project not found", http.StatusNotFound)
			return nil, false
		}
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return nil, false
	}
	return conv, true
}
