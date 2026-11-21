// Patrol Launchpad — the "Draft it with AI" half (docs/PATROL-LAUNCHPAD.md).
//
// The Launchpad wizard walks a person from nothing to a running patrol:
// connectors, non-negotiables (memories), what to watch, how often, how much
// it may do on its own. The browser composes a template draft of the patrol's
// instructions by itself; this endpoint is the optional upgrade that asks a
// model to write them from the same answers. It RETURNS the draft and persists
// nothing — the person edits it, and the wizard then creates the patrol
// through the ordinary conversation and schedule endpoints, which own every
// rule about patrols (cadence floor, auto-approve, ownership).

package httpapi

import (
	"io"
	"net/http"
	"strings"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/store"
)

// patrolDraftMaxBody bounds the request: a dozen goals, a notes box, a few
// dozen connector descriptions and memories fit comfortably.
const patrolDraftMaxBody = 64 << 10

// patrolDraftMemoryRunes caps each memory's content in the draft prompt (its
// kind/validity annotation is added after the cut, so it always survives).
const patrolDraftMemoryRunes = 400

// patrolDraftRequest is the wizard's answers. Connector descriptions come
// from the caller's own /mcp-servers catalog and are context for the draft
// only; they grant nothing — the patrol chat's connector set is persisted
// separately through /conversations/{id}/mcp-servers, which intersects it
// with the server's catalog.
type patrolDraftRequest struct {
	Goals      []string `json:"goals"`
	Notes      string   `json:"notes"`
	Connectors []struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	} `json:"connectors"`
	Memories []string `json:"memories"`
	Cadence  string   `json:"cadence"`
	Mode     string   `json:"mode"`
	Persona  string   `json:"persona"`
}

// handlePatrolDraft backs POST /patrol-draft. 400 on a malformed body or one
// with nothing to patrol, 502 if synthesis fails (the wizard keeps its own
// template draft and says the assistant could not help this time).
func (s *Server) handlePatrolDraft(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, patrolDraftMaxBody)
	var req patrolDraftRequest
	if !decodeJSONBody(w, r, &req) {
		return
	}
	// Drain anything after the JSON value so a trailing payload cannot sit
	// unread under the size cap.
	_, _ = io.Copy(io.Discard, r.Body)

	goals := make([]string, 0, len(req.Goals))
	for _, g := range req.Goals {
		if g = strings.TrimSpace(g); g != "" {
			goals = append(goals, g)
		}
	}
	if len(goals) == 0 && strings.TrimSpace(req.Notes) == "" {
		http.Error(w, "pick at least one thing for the patrol to watch", http.StatusBadRequest)
		return
	}
	in := agent.PatrolDraftInput{
		Goals:    goals,
		Notes:    req.Notes,
		Memories: req.Memories,
		Cadence:  req.Cadence,
		Mode:     req.Mode,
		Persona:  req.Persona,
	}
	for _, c := range req.Connectors {
		in.Connectors = append(in.Connectors, agent.PatrolDraftConnector{Name: c.Name, Description: c.Description})
	}

	user := strings.ToLower(userFromCtx(r.Context()))
	// The memories the draft must respect are the ones a patrol turn will
	// actually receive: read them here and render them as the turn does
	// (memoryContents — active only, pinned first, capped at 50, each with its
	// kind and validity window), so a time-scoped fact is not baked into the
	// standing instructions as timeless. Each memory's content is shortened
	// BEFORE its annotation is added, which keeps the prompt bounded without
	// losing the annotation. If they cannot be read there is no draft: one
	// written without them could contradict what every patrol turn is told.
	// (The request's own memories are read only by a Server with no store,
	// i.e. in unit tests; the wizard no longer sends them.)
	if s.store != nil {
		mems, err := s.store.ListMemories(r.Context(), user)
		if err != nil {
			http.Error(w, "could not read your memories to draft with; try again", http.StatusServiceUnavailable)
			return
		}
		in.Memories = memoryContentsCapped(mems, patrolDraftMemoryRunes)
	}
	if _, busy := s.patrolDraftInFlight.LoadOrStore(user, struct{}{}); busy {
		http.Error(w, "a draft is already being written; wait for it to finish", http.StatusTooManyRequests)
		return
	}
	defer s.patrolDraftInFlight.Delete(user)

	draft, err := s.agent.SuggestPatrolDraft(r.Context(), in)
	if err != nil || draft == nil {
		http.Error(w, "the assistant could not draft this patrol; try again or edit the draft yourself", http.StatusBadGateway)
		return
	}
	writeJSON(w, r, map[string]any{
		"title":   draft.Title,
		"prompt":  draft.Prompt,
		"summary": draft.Summary,
	})
}

// patrolsIndexMax bounds each half of the Patrols index — the caller's own
// patrols, and teammates' patrols shared with them (a person with more live
// patrols still finds the rest in Chat); patrolsIndexMaxPages bounds how far
// it reads past rows the caller can no longer open.
const (
	patrolsIndexMax      = 100
	patrolsIndexMaxPages = 10
)

// handlePatrols backs GET /patrols: the index of patrols for the Patrols page
// and the sidebar's row marks (docs/SIDEBAR.md). It lists the caller's own
// active and paused recurring patrols across their chats ("mine": true),
// then the patrols teammates run in chats the caller can open ("mine":
// false, with "owner_email"). A patrol lives in its chat; this is only the
// index of them, so it carries no prompt.
func (s *Server) handlePatrols(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	user := userFromCtx(r.Context())
	// A patrol in a teammate's multiplayer chat is listed only while the
	// caller can still open that chat — the same rule the schedule routes
	// apply (getOwnedOrMultiplayerConversation), so the index never links to
	// a chat that would answer 404. The check runs per row, after the query,
	// so the index pages until it has its fill of rows the caller can open:
	// revoked rows never crowd out ones they still can.
	//
	// The check is the multiplayer one alone (the row already names the
	// owner, so a teammate's chat cannot be the caller's own), and it runs
	// once per distinct chat per request: patrols sharing a chat share the
	// answer. Rows in the caller's own chats need no check at all. It stays
	// per chat, not one batched query, because the participation rule
	// (store.canParticipateInMultiplayer: participants, exclusions, team
	// membership) lives in one place and must not be re-implemented in SQL.
	canOpen := map[string]bool{}
	out := make([]store.UserPatrol, 0, patrolsIndexMax)
	for page := 0; page < patrolsIndexMaxPages && len(out) < patrolsIndexMax; page++ {
		items, err := s.store.ListUserPatrols(r.Context(), user, patrolsIndexMax, page*patrolsIndexMax)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		for _, p := range items {
			if len(out) == patrolsIndexMax {
				break
			}
			if !strings.EqualFold(p.ConversationOwner, user) {
				ok, seen := canOpen[p.ConversationID]
				if !seen {
					conv, err := s.store.GetMultiplayerConversation(r.Context(), user, p.ConversationID)
					if err != nil {
						// An error is not "no access": answer 5xx so the page
						// shows its load-failure state, never a false empty list.
						http.Error(w, err.Error(), http.StatusInternalServerError)
						return
					}
					ok = conv != nil
					canOpen[p.ConversationID] = ok
				}
				if !ok {
					continue // access revoked
				}
			}
			out = append(out, p)
		}
		if len(items) < patrolsIndexMax {
			break // no more rows
		}
	}
	// Which of the caller's patrols are being skipped behind an approval
	// card. Display data: a failed read leaves the rows unmarked.
	ids := make([]string, 0, len(out))
	for i := range out {
		if out[i].Status == store.ChatScheduleStatusActive {
			ids = append(ids, out[i].ID)
		}
	}
	if skips, err := s.store.PatrolSkips(r.Context(), ids); err == nil {
		for i := range out {
			out[i].Skipped = skips[out[i].ID]
		}
	}
	// Teammates' patrols, in chats shared with the caller. The store reads
	// them from the same set of chats the sidebar lists as shared
	// (ListSharedAndMultiplayerConversations), so nothing is listed here that
	// the caller could not already open.
	shared, err := s.store.ListSharedPatrols(r.Context(), user, patrolsIndexMax)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out = append(out, shared...)
	writeJSON(w, r, map[string]any{"patrols": out})
}

// handlePatrolByID backs GET /patrols/{id}: one patrol and its recent runs,
// for the Patrols page's open row. The caller's own patrol comes with its
// prompt. A teammate's patrol comes without it (what each run is asked to do
// is read in the chat, which the caller can open) and only when it is in a
// chat shared with the caller — the rule the index applies. Anything else is
// the 404 an unknown ID gets.
func (s *Server) handlePatrolByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	user := userFromCtx(r.Context())
	id := strings.TrimPrefix(r.URL.Path, "/patrols/")
	if id == "" || strings.Contains(id, "/") {
		http.Error(w, "patrol not found", http.StatusNotFound)
		return
	}
	p, err := s.store.GetUserPatrol(r.Context(), user, id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if p == nil {
		http.Error(w, "patrol not found", http.StatusNotFound)
		return
	}
	allowed := false
	switch {
	case p.Mine:
		// The caller's own patrol, while they can still open its chat.
		conv, err := s.getOwnedOrMultiplayerConversation(r.Context(), user, p.ConversationID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		allowed = conv != nil
	case p.OwnsChat:
		// A participant's patrol in the caller's own chat: listed by
		// ListSharedPatrols while that chat is live (not archived). Checked
		// on the one chat, not by scanning the capped listing, so a patrol
		// past the listing's cap still opens.
		conv, err := s.getOwnedOrMultiplayerConversation(r.Context(), user, p.ConversationID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		allowed = conv != nil && conv.ArchivedAt == nil
	default:
		// A teammate's patrol: its chat must be one the caller's shared list
		// holds — the same query, narrowed to that chat (one rule, no cap).
		ok, err := s.store.IsSharedOrMultiplayerConversation(r.Context(), user, p.ConversationID)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		allowed = ok
	}
	if !allowed {
		http.Error(w, "patrol not found", http.StatusNotFound)
		return
	}
	if p.Mine && p.Status == store.ChatScheduleStatusActive {
		if skips, err := s.store.PatrolSkips(r.Context(), []string{p.ID}); err == nil {
			p.Skipped = skips[p.ID]
		}
	}
	runs, err := s.store.ListPatrolRuns(r.Context(), p.ID, store.MaxPatrolRuns)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, r, map[string]any{"patrol": p, "runs": runs})
}
