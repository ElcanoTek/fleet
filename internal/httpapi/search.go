package httpapi

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/ElcanoTek/fleet/internal/store"
)

// searchResponse is the GET /search payload (#308).
type searchResponse struct {
	Results []store.SearchResult `json:"results"`
	Total   int                  `json:"total"`
}

// search handles GET /search?q=…&type=conversations&limit=20&offset=0 — ranked
// full-text matches across the authenticated user's conversation titles and
// message content. Returns 404 when FLEET_SEARCH_ENABLED=false.
//
// The `type` filter accepts "conversations" (default) and "all" (currently an
// alias — task-log search is a documented follow-up); "tasks" returns an empty
// set rather than pretending to search a surface that isn't indexed yet.
func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	if s.cfg == nil || !s.cfg.SearchEnabled {
		http.Error(w, "search is disabled", http.StatusNotFound)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	user := userFromCtx(r.Context())
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeJSON(w, searchResponse{Results: []store.SearchResult{}, Total: 0})
		return
	}

	if t := r.URL.Query().Get("type"); t == "tasks" {
		// Task / session-log search is not indexed yet (see PR notes). Be honest:
		// return an empty set rather than silently returning conversation hits.
		writeJSON(w, searchResponse{Results: []store.SearchResult{}, Total: 0})
		return
	}

	limit := clampSearchInt(r.URL.Query().Get("limit"), 20, 1, 100)
	offset := clampSearchInt(r.URL.Query().Get("offset"), 0, 0, 1_000_000)

	results, total, err := s.store.SearchConversations(r.Context(), user, q, limit, offset)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if results == nil {
		results = []store.SearchResult{}
	}
	writeJSON(w, searchResponse{Results: results, Total: total})
}

// clampSearchInt parses a query-param int, falling back to def and clamping to
// [lo, hi] so a hostile/garbage value can't request an unbounded page.
func clampSearchInt(raw string, def, lo, hi int) int {
	v := def
	if raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			v = n
		}
	}
	if v < lo {
		v = lo
	}
	if v > hi {
		v = hi
	}
	return v
}
