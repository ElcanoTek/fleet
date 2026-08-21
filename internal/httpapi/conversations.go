// The /conversations collection (list / create / bulk ops) and the
// /conversations/{id} sub-route dispatcher, split out of server.go (#1127).
// The dispatcher's per-action bodies live in conversation_actions.go (and
// mcp_servers.go for the Tools-picker pair); withOwnedConversation carries the
// ownership-check framing the old switch repeated per branch.

package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/store"
)

// ── /conversations ─────────────────────────────────────────────────────────

type createConversationRequest struct {
	Title   string `json:"title"`
	Persona string `json:"persona"`
	Model   string `json:"model"`
	// Lockdown, when true, marks this conversation as locked-down: the
	// agent loop forces a per-turn container sandbox and the model slug
	// is restricted to CHAT_LOCKDOWN_ALLOWED_MODELS. Frontend exposes
	// this as a "New lockdown chat" affordance. Server rejects when
	// CHAT_LOCKDOWN_ENABLED is false.
	Lockdown bool `json:"lockdown,omitempty"`
	// ProjectID binds the conversation to a project/space (#509): membership
	// is validated and the project's default persona/model + curated
	// connector selection are inherited (explicit request values win).
	ProjectID string `json:"project_id,omitempty"`
	// Seed, when non-empty, is appended as the conversation's first user
	// message WITHOUT running a turn — pre-loaded context the user's first
	// real message will build on. The orchestrator's "Discuss this run"
	// bridge uses it to open a chat seeded with a scheduled run's
	// transcript digest (docs/DISCUSS-RUN.md). Clamped server-side to
	// seedMaxChars; the tail is kept (the end of a transcript — the
	// result — matters more than the beginning).
	Seed string `json:"seed,omitempty"`
}

// seedMaxChars bounds a seeded first message so a pathological caller can't
// preload a conversation past any sane context budget. The proactive context
// compaction in agentcore handles the rest.
const seedMaxChars = 48_000

// clampSeed enforces seedMaxChars, keeping the TAIL: transcripts end with the
// outcome, and the outcome is what a discussion seed is for.
func clampSeed(seed string) string {
	if len(seed) <= seedMaxChars {
		return seed
	}
	return "[…seed truncated…]\n" + seed[len(seed)-seedMaxChars:]
}

func (s *Server) listOrCreateConversations(w http.ResponseWriter, r *http.Request) {
	user := userFromCtx(r.Context())
	switch r.Method {
	case http.MethodGet:
		// ?archived=true returns the archived conversations (the collapsed
		// "Archived" sidebar section, #282); default returns active ones.
		// ?label= (repeatable, AND semantics) restricts to conversations
		// carrying every listed label (#258).
		q := r.URL.Query()
		// ?scope=team returns the conversations same-team members have shared
		// (team_visible), read-only (#237) — the manager/teammate view. It is a
		// distinct, opt-in read path: it never mixes the caller's private
		// conversations in, and a caller with no team gets 400.
		if q.Get("scope") == "team" {
			list, err := s.store.ListTeamConversations(r.Context(), user)
			if errors.Is(err, store.ErrNoTeam) {
				http.Error(w, "no team", http.StatusBadRequest)
				return
			}
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			writeJSON(w, map[string]any{"conversations": list})
			return
		}
		list, err := s.store.ListFiltered(r.Context(), user, store.ListFilter{
			ArchivedOnly: q.Get("archived") == "true",
			Labels:       q["label"],
		})
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]any{"conversations": list})
	case http.MethodDelete:
		// Bulk delete (#279). Two modes, mutually exclusive:
		//
		//   • targeted:  { "conversation_ids": [...], "confirm": true }
		//     deletes exactly those IDs (max 100). A foreign or unknown ID
		//     aborts the whole request with 403 and deletes nothing.
		//
		//   • all_matching: { "all_matching": true, "confirm": true }
		//     with an optional ?label= query filter. Requires confirm:true so
		//     an accidental bulk wipe can't fire. With no filter this
		//     replicates the legacy "nuke all unpinned" behavior.
		//
		// A request with neither conversation_ids nor all_matching falls
		// back to the legacy DeleteAllUnpinned path (back-compat for older
		// clients that issued a bare DELETE /conversations), preserving the
		// historical affordance.
		var req struct {
			ConversationIDs []string `json:"conversation_ids"`
			AllMatching     bool     `json:"all_matching"`
			Confirm         bool     `json:"confirm"`
		}
		if r.Body != nil {
			// Empty body (io.EOF) is the legacy bare DELETE → DeleteAllUnpinned.
			// Any other decode error is a client that *intended* a targeted
			// bulk delete but sent malformed / truncated JSON — refuse with
			// 400 rather than wipe every unpinned conversation (#1110).
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
				http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
				return
			}
		}

		switch {
		case req.AllMatching:
			// Filter-based bulk delete. confirm:true is mandatory so a stray
			// request can't wipe a user's history.
			if !req.Confirm {
				http.Error(w, `"confirm": true required for all_matching bulk delete`, http.StatusBadRequest)
				return
			}
			if len(req.ConversationIDs) > 0 {
				http.Error(w, "conversation_ids is mutually exclusive with all_matching", http.StatusBadRequest)
				return
			}
			n, err := s.store.DeleteAllMatching(r.Context(), user, r.URL.Query().Get("label"))
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			writeJSON(w, map[string]any{"deleted": n})
		case len(req.ConversationIDs) > 0:
			// Targeted bulk delete by explicit ID list.
			if len(req.ConversationIDs) > 100 {
				http.Error(w, "max 100 conversation_ids per request", http.StatusBadRequest)
				return
			}
			n, err := s.store.DeleteByIDs(r.Context(), user, req.ConversationIDs)
			if errors.Is(err, store.ErrForeignConversation) {
				http.Error(w, "one or more conversation IDs not owned by caller", http.StatusForbidden)
				return
			}
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			// Reclaim each deleted conversation's persistent run_python sandbox
			// (#213). The filter-based bulk paths (DeleteAllMatching /
			// DeleteAllUnpinned) return only a count, not IDs, so those rely on
			// the pool's idle reaper to reclaim instead.
			for _, cid := range req.ConversationIDs {
				s.releasePersistentSandbox(cid)
			}
			writeJSON(w, map[string]any{"deleted": n})
		default:
			// Legacy bare DELETE /conversations → DeleteAllUnpinned.
			n, err := s.store.DeleteAllUnpinned(r.Context(), user)
			if err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			writeJSON(w, map[string]any{"deleted": n})
		}
	case http.MethodPost:
		var req createConversationRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
			return
		}
		persona := strings.TrimSpace(req.Persona)
		if persona == "" {
			persona = s.cfg.PersonaDefault
		}
		title := strings.TrimSpace(req.Title)
		if title == "" {
			title = "New conversation"
		}
		lockdown := req.Lockdown || s.cfg.LockdownOnly
		if lockdown && !s.cfg.LockdownAvailable() {
			http.Error(w, "lockdown is unavailable on this server (no sandbox image configured)", http.StatusBadRequest)
			return
		}
		model := strings.TrimSpace(req.Model)
		if lockdown && model != "" && !s.cfg.LockdownAllows(model) {
			http.Error(w, "model not allowed in lockdown mode", http.StatusBadRequest)
			return
		}
		conv, ok := s.createConversationForRequest(w, r, user, req.ProjectID, title, persona, model, lockdown)
		if !ok {
			return
		}
		// Optional seed (docs/DISCUSS-RUN.md): persist one user text message
		// without running a turn, so the next real turn sees it as history.
		// Best-effort ordering is NOT acceptable here — a discussion opened
		// on a missing seed is an empty chat with a misleading title — so a
		// failed append fails the request (the empty conversation row is
		// harmless and reachable from the sidebar).
		if seed := strings.TrimSpace(req.Seed); seed != "" {
			content, err := json.Marshal(map[string]any{"text": clampSeed(seed)})
			if err == nil {
				_, err = s.store.AppendHistory(r.Context(), conv.ID,
					[]agent.HistoryEntry{{Role: "user", Type: "text", Content: content}})
			}
			if err != nil {
				http.Error(w, "failed to seed conversation: "+err.Error(), http.StatusInternalServerError)
				return
			}
		}
		writeJSON(w, conv)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// bulkPatchConversations handles PATCH /conversations/bulk (#279): applies the
// same additive mutation (pinned / labels) to up to 100 conversations in a
// single transaction. A nil pointer in `changes` (an omitted field) means
// "leave untouched"; a non-nil pointer — including an empty labels slice —
// overwrites the stored value. A foreign or unknown ID returns 403 and rolls
// the whole transaction back so nothing is partially mutated.
func (s *Server) bulkPatchConversations(w http.ResponseWriter, r *http.Request) {
	user := userFromCtx(r.Context())
	var req struct {
		ConversationIDs []string `json:"conversation_ids"`
		Changes         struct {
			Pinned *bool    `json:"pinned"`
			Labels []string `json:"labels"`
		} `json:"changes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(req.ConversationIDs) == 0 || len(req.ConversationIDs) > 100 {
		http.Error(w, "conversation_ids must be 1–100 items", http.StatusBadRequest)
		return
	}
	// At least one change must be supplied — a bare PATCH with an empty changes
	// object is a no-op the caller almost certainly didn't intend.
	if req.Changes.Pinned == nil && req.Changes.Labels == nil {
		http.Error(w, "changes must include at least one of pinned, labels", http.StatusBadRequest)
		return
	}
	// Bound + normalize the label metadata (#258) before persisting.
	if err := normalizeAndValidateLabels(req.Changes.Labels); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	n, err := s.store.BulkPatch(r.Context(), user, req.ConversationIDs, req.Changes.Pinned, req.Changes.Labels)
	if errors.Is(err, store.ErrForeignConversation) {
		http.Error(w, "one or more conversation IDs not owned by caller", http.StatusForbidden)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"updated": n})
}

// /conversations/{id}
// /conversations/{id}/pin
// /conversations/{id}/messages//
func (s *Server) conversationByID(w http.ResponseWriter, r *http.Request) {
	user := userFromCtx(r.Context())
	rest := strings.TrimPrefix(r.URL.Path, "/conversations/")
	if rest == "" {
		http.Error(w, "conversation id required", http.StatusBadRequest)
		return
	}
	parts := strings.SplitN(rest, "/", 3)
	id := parts[0]
	sub := ""
	subArg := ""
	if len(parts) >= 2 {
		sub = parts[1]
	}
	if len(parts) == 3 {
		subArg = parts[2]
	}

	// Bulk patch (#279): PATCH /conversations/bulk applies the same additive
	// mutation (pinned / labels) to multiple conversations in a single
	// transaction. Routed here because /conversations/bulk falls under the
	// /conversations/ prefix; "bulk" is a reserved pseudo-id, never a UUID, so
	// it can't collide with a real conversation.
	if rest == "bulk" && r.Method == http.MethodPatch {
		s.bulkPatchConversations(w, r)
		return
	}

	// Approval resolution lives at /conversations/{id}/approvals/{approvalId}.
	if sub == "approvals" && subArg != "" {
		s.handleApproval(w, r, id, subArg)
		return
	}

	// Sub-agent child transcript (#1043) —
	// GET /conversations/{id}/subagents/{childSessionID}. Same ownership gate
	// as every other conversation sub-route, plus a history-linkage check.
	if sub == "subagents" && subArg != "" && r.Method == http.MethodGet {
		s.handleSubagentLog(w, r, id, subArg)
		return
	}

	// Stream reattach + inflight probe — see handleStream/handleInflight.
	if sub == "stream" && r.Method == http.MethodGet {
		s.handleStream(w, r, id)
		return
	}
	if sub == "inflight" && r.Method == http.MethodGet {
		s.handleInflight(w, r, id)
		return
	}

	// Cursor-paginated turn-event read path (#189) — see handleTurnEventsPage.
	if sub == "events" && r.Method == http.MethodGet {
		s.handleTurnEventsPage(w, r, id)
		return
	}

	// Workspace file fetch — `GET /conversations/{id}/workspace/<path>`
	// streams a file from the conversation's per-turn workspace dir so
	// markdown image references like `![](spend_chart.png)` written by
	// the agent during run_python actually render in the chat UI.
	if sub == "workspace" && r.Method == http.MethodGet {
		s.handleWorkspaceFile(w, r, id, subArg)
		return
	}

	// Tool-call audit log — `GET /conversations/{id}/audit` returns the
	// persistent, queryable history of every tool the agent ran in this
	// conversation (#224). Membership-scoped: 404 for a conversation the
	// caller doesn't own.
	if sub == "audit" && r.Method == http.MethodGet {
		s.handleConversationAudit(w, r, id)
		return
	}

	// Input queue (#785): GET snapshot, DELETE {inputID}, POST
	// {inputID}/send-now. Ownership is validated inside.
	if sub == "queue" {
		s.handleQueueRoutes(w, r, user, id, subArg)
		return
	}

	// Everything else is a (sub, method)-keyed action. The table replaces the
	// former ~630-line switch: its cases were mutually exclusive on exactly
	// these pairs, so a lookup preserves the match behavior — including the
	// 405 for an unmatched pair — while keeping this dispatcher's complexity
	// flat as sub-routes accumulate.
	if h, ok := conversationSubroutes[conversationSubroute{sub: sub, method: r.Method}]; ok {
		h(s, w, r, user, id)
		return
	}
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}

// conversationSubroute keys one /conversations/{id}/<sub> action by its path
// segment and HTTP method. sub "" is the bare item route.
type conversationSubroute struct {
	sub    string
	method string
}

// conversationSubrouteHandler is the uniform signature conversationByID's
// dispatch table calls every action through. Handlers that predate the table
// and take (convID, user) in another order are adapted by small closures in
// the table itself.
type conversationSubrouteHandler func(s *Server, w http.ResponseWriter, r *http.Request, user, id string)

// ownedConversationHandler is an action body that runs only after
// withOwnedConversation resolved the caller's conversation: the ownership
// gate has already passed and conv is non-nil.
type ownedConversationHandler func(s *Server, w http.ResponseWriter, r *http.Request, user, id string, conv *store.Conversation)

// withOwnedConversation adapts an ownedConversationHandler by prepending the
// ownership-check framing the old switch repeated per branch: resolve the
// conversation for THIS caller (store.Get is scoped by user_email, so a
// foreign or unknown id resolves nil), answer 500 on a store error or 404
// when it doesn't resolve, and only then run handler. notFound is the 404
// body — the historical branches answered either "not found" or
// "conversation not found", and each table entry preserves its branch's
// original text verbatim.
func withOwnedConversation(notFound string, handler ownedConversationHandler) conversationSubrouteHandler {
	return func(s *Server, w http.ResponseWriter, r *http.Request, user, id string) {
		conv, err := s.store.Get(r.Context(), user, id)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if conv == nil {
			http.Error(w, notFound, http.StatusNotFound)
			return
		}
		handler(s, w, r, user, id, conv)
	}
}

// decodeJSONBody decodes r's JSON body into dst, answering the shared
// "bad json" 400 on failure. Reports whether decoding succeeded. Only for
// handlers that REQUIRE a well-formed body — branches with lenient decode
// semantics (the cancel scope, the legacy bare DELETE /conversations) keep
// their own decode inline.
func decodeJSONBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		http.Error(w, "bad json: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

// conversationSubroutes maps each (sub, method) pair the old conversationByID
// switch matched to its action. Entries reference only functions (method
// expressions or literals), so initialization is file-order-independent.
var conversationSubroutes = map[conversationSubroute]conversationSubrouteHandler{
	{sub: "", method: http.MethodGet}:                   withOwnedConversation("not found", (*Server).handleConversationGet),
	{sub: "", method: http.MethodDelete}:                (*Server).handleConversationDelete,
	{sub: "truncate", method: http.MethodPost}:          (*Server).handleConversationTruncate,
	{sub: "pin", method: http.MethodPost}:               (*Server).handleConversationPin,
	{sub: "archive", method: http.MethodPost}:           (*Server).handleConversationArchive,
	{sub: "project", method: http.MethodPost}:           (*Server).handleConversationRefile,
	{sub: "rename", method: http.MethodPost}:            (*Server).handleConversationRename,
	{sub: "model", method: http.MethodPost}:             (*Server).handleConversationModel,
	{sub: "approval-timeout", method: http.MethodGet}:   withOwnedConversation("not found", (*Server).handleApprovalTimeoutGet),
	{sub: "approval-timeout", method: http.MethodPost}:  (*Server).handleApprovalTimeoutSet,
	{sub: "thinking_config", method: http.MethodGet}:    withOwnedConversation("not found", (*Server).handleThinkingConfigGet),
	{sub: "thinking_config", method: http.MethodPut}:    (*Server).handleThinkingConfigPut,
	{sub: "thinking_config", method: http.MethodDelete}: (*Server).handleThinkingConfigDelete,
	// Fork this conversation at a chosen message into a new conversation (#454).
	{sub: "branch", method: http.MethodPost}: func(s *Server, w http.ResponseWriter, r *http.Request, user, id string) {
		s.handleConversationBranch(w, r, id, user)
	},
	// Synthesize a recurring-task proposal from this chat + stage it as a
	// schedule_task approval card (#455).
	{sub: "promote-to-task", method: http.MethodPost}: func(s *Server, w http.ResponseWriter, r *http.Request, user, id string) {
		s.handlePromoteToTask(w, r, id, user)
	},
	// Distill this chat into a prompt-library draft the user reviews and
	// saves client-side via the orchestrator's POST /prompts.
	{sub: "suggest-prompt", method: http.MethodPost}: func(s *Server, w http.ResponseWriter, r *http.Request, user, id string) {
		s.handleSuggestPrompt(w, r, id, user)
	},
	// Issue (or rotate) a public read-only share token (#226).
	{sub: "share", method: http.MethodPost}: func(s *Server, w http.ResponseWriter, r *http.Request, user, id string) {
		s.handleConversationShare(w, r, id, user)
	},
	// Revoke sharing for this conversation (#226).
	{sub: "share", method: http.MethodDelete}: func(s *Server, w http.ResponseWriter, r *http.Request, user, id string) {
		s.handleConversationUnshare(w, r, id, user)
	},
	// Opt this conversation in/out of team visibility (#237). Owner-only.
	{sub: "share-with-team", method: http.MethodPost}: func(s *Server, w http.ResponseWriter, r *http.Request, user, id string) {
		s.handleConversationShareWithTeam(w, r, id, user)
	},
	{sub: "mcp-servers", method: http.MethodGet}:  withOwnedConversation("not found", (*Server).handleConversationMCPServersGet),
	{sub: "mcp-servers", method: http.MethodPost}: (*Server).handleConversationMCPServersSet,
	{sub: "export", method: http.MethodGet}:       withOwnedConversation("not found", (*Server).handleConversationExport),
	{sub: "cancel", method: http.MethodPost}:      withOwnedConversation("conversation not found", (*Server).handleConversationCancel),
	{sub: "summarize", method: http.MethodPost}: func(s *Server, w http.ResponseWriter, r *http.Request, user, id string) {
		s.handleSummarize(w, r, user, id)
	},
}
