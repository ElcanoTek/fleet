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
	"sync"

	"github.com/ElcanoTek/fleet/internal/agent"
	"github.com/ElcanoTek/fleet/internal/store"
)

// patrolDraftInFlight holds the users with a draft being synthesized. A
// draft is a model call of up to 45 seconds; one at a time per user is all
// the wizard needs, and anything more is spend nobody asked for. (The route's
// rateLimitMiddleware bounds the rate; this bounds concurrency.)
var patrolDraftInFlight sync.Map

// patrolDraftMaxBody bounds the request: a dozen goals, a notes box, a few
// dozen connector descriptions and memories fit comfortably.
const patrolDraftMaxBody = 64 << 10

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
	// actually receive: read them here and render them exactly as the turn
	// does (memoryContents — active only, pinned first, capped at 50, each
	// with its kind and validity window), so a time-scoped fact is not baked
	// into the standing instructions as timeless. The wizard's own list is
	// only a fallback for when the store cannot be read.
	if s.store != nil {
		if mems, err := s.store.ListMemories(r.Context(), user); err == nil {
			in.Memories = memoryContents(mems)
		}
	}
	if _, busy := patrolDraftInFlight.LoadOrStore(user, struct{}{}); busy {
		http.Error(w, "a draft is already being written; wait for it to finish", http.StatusTooManyRequests)
		return
	}
	defer patrolDraftInFlight.Delete(user)

	draft, err := s.agent.SuggestPatrolDraft(r.Context(), in)
	if err != nil || draft == nil {
		http.Error(w, "the assistant could not draft this patrol; try again or edit the draft yourself", http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]any{
		"title":   draft.Title,
		"prompt":  draft.Prompt,
		"summary": draft.Summary,
	})
}

// patrolsIndexMax bounds the Patrols index (a person with more live patrols
// still finds the rest in Chat); patrolsIndexMaxPages bounds how far it reads
// past rows the caller can no longer open.
const (
	patrolsIndexMax      = 100
	patrolsIndexMaxPages = 10
)

// handlePatrols backs GET /patrols: the caller's active and paused recurring
// patrols across their chats, for the Patrols hub in the navigation rail. A
// patrol lives in its chat; this is only the index of them.
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
				conv, err := s.getOwnedOrMultiplayerConversation(r.Context(), user, p.ConversationID)
				if err != nil {
					// An error is not "no access": answer 5xx so the rail
					// shows its load-failure state, never a false empty list.
					http.Error(w, err.Error(), http.StatusInternalServerError)
					return
				}
				if conv == nil {
					continue // access revoked
				}
			}
			out = append(out, p)
		}
		if len(items) < patrolsIndexMax {
			break // no more rows
		}
	}
	writeJSON(w, map[string]any{"patrols": out})
}
