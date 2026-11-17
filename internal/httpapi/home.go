package httpapi

// Each person's Home chat (docs/HOME-CHAT.md, ADR-0113):
//
//	GET    /me/home  → {"home": {...} | null}
//	PUT    /me/home  → {"conversation_id": "<id>"}: make it Home (replaces any other)
//	DELETE /me/home  → remove Home; the chat itself is untouched
//
// The default GET /conversations response carries the same "home" field, so a
// cold boot learns where to land from a request it already makes.
//
// The route sits behind auth+membership and rejectViewerWrites, like the other
// per-person settings (/connector-prefs, /me/team): a read-only viewer can read
// a Home but not choose one.

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/ElcanoTek/fleet/internal/logging"
	"github.com/ElcanoTek/fleet/internal/safe"
	"github.com/ElcanoTek/fleet/internal/store"
)

// homeResponse is the one shape every /me/home answer has. Home is nil when
// the person has none, or can no longer open the one they chose.
type homeResponse struct {
	Home *store.HomeConversation `json:"home"`
}

// handleMyHome serves GET, PUT and DELETE /me/home.
func (s *Server) handleMyHome(w http.ResponseWriter, r *http.Request) {
	user := userFromCtx(r.Context())
	switch r.Method {
	case http.MethodGet:
		home, err := s.store.HomeConversation(r.Context(), user)
		if err != nil {
			http.Error(w, "could not read your home chat", http.StatusInternalServerError)
			return
		}
		writeJSON(w, r, homeResponse{Home: home})
	case http.MethodPut:
		r.Body = http.MaxBytesReader(w, r.Body, 4<<10)
		var req struct {
			ConversationID string `json:"conversation_id"`
		}
		if !decodeJSONBody(w, r, &req) {
			return
		}
		id := strings.TrimSpace(req.ConversationID)
		if id == "" {
			http.Error(w, "conversation_id required", http.StatusBadRequest)
			return
		}
		home, err := s.store.SetHomeConversation(r.Context(), user, id)
		if errors.Is(err, store.ErrConversationNotFound) {
			http.Error(w, "conversation not found", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, "could not save your home chat", http.StatusInternalServerError)
			return
		}
		writeJSON(w, r, homeResponse{Home: home})
	case http.MethodDelete:
		if err := s.store.ClearHomeConversation(r.Context(), user); err != nil {
			http.Error(w, "could not clear your home chat", http.StatusInternalServerError)
			return
		}
		writeJSON(w, r, homeResponse{})
	default:
		w.Header().Set("Allow", "GET, PUT, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// homeForList is the "home" field of the default GET /conversations answer.
// It is best-effort: that list is on the chat's cold-boot critical path, and a
// failed Home lookup must cost the person their landing spot, not their chat
// list, so an error is logged and reported as no Home.
func (s *Server) homeForList(ctx context.Context, user string) *store.HomeConversation {
	home, err := s.store.HomeConversation(ctx, user)
	if err != nil {
		logging.From(ctx).Warn("home chat lookup failed; listing without it", "err", safe.LogSafe(err.Error()))
		return nil
	}
	return home
}
