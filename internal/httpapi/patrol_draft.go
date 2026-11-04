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
)

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

// handlePatrols backs GET /patrols: the caller's active and paused recurring
// patrols across their chats, for the Patrols hub in the navigation rail. A
// patrol lives in its chat; this is only the index of them.
func (s *Server) handlePatrols(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	items, err := s.store.ListUserPatrols(r.Context(), userFromCtx(r.Context()))
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]any{"patrols": items})
}
