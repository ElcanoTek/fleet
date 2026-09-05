package httpapi

import (
	"fmt"
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

// searchDefaultLimit / searchMaxLimit bound the search page: an absent limit
// is the default, anything present must be an integer in [1, searchMaxLimit].
const (
	searchDefaultLimit = 20
	searchMaxLimit     = 100
)

// search handles GET /search?q=…&type=conversations&limit=20&offset=0 — ranked
// full-text matches across the authenticated user's conversation titles and
// message content. Returns 404 when FLEET_SEARCH_ENABLED=false.
//
// Paging follows the scheduler's contract (sched/handlers/paging.go): absent
// limit/offset take their defaults; a present value that is not an integer,
// a limit outside [1, searchMaxLimit], or a negative offset is a 400 naming the
// accepted range. The old clamp-and-continue answered `?limit=abc` with the
// default page and `?limit=999` with 100 rows — a client with a typo got a
// page it never asked for and no signal that its parameter was ignored.
//
// Search is conversations-only: `type` accepts "conversations" (the default
// when absent) and nothing else. Any other value — including the retired
// "tasks" stub and its "all" alias, which used to answer 200 with a lying
// empty set (#1076) — is a 400, so a caller asking for an unindexed surface
// hears "no such surface" instead of "no hits".
func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	if s.cfg == nil || !s.cfg.SearchEnabled {
		http.Error(w, "search is disabled", http.StatusNotFound)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if t := r.URL.Query().Get("type"); t != "" && t != "conversations" {
		http.Error(w, `unknown search type (only "conversations" is indexed)`, http.StatusBadRequest)
		return
	}
	user := userFromCtx(r.Context())
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeJSON(w, searchResponse{Results: []store.SearchResult{}, Total: 0})
		return
	}

	limit, err := parseSearchLimit(r.URL.Query().Get("limit"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	offset, err := parseSearchOffset(r.URL.Query().Get("offset"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

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

// parseSearchLimit reads the optional ?limit= parameter: absent →
// searchDefaultLimit; otherwise an integer in [1, searchMaxLimit], else an
// error naming that range. The upper bound is a rejection, not a clamp, so a
// hostile or garbage value can neither request an unbounded page nor be
// silently answered with a different page size than it asked for.
func parseSearchLimit(raw string) (int, error) {
	if raw == "" {
		return searchDefaultLimit, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > searchMaxLimit {
		return 0, fmt.Errorf("limit must be an integer between 1 and %d", searchMaxLimit)
	}
	return n, nil
}

// parseSearchOffset reads the optional ?offset= parameter: absent → 0;
// otherwise a non-negative integer. A negative offset is rejected here rather
// than handed to Postgres, which refuses it as an opaque 500.
func parseSearchOffset(raw string) (int, error) {
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("offset must be a non-negative integer")
	}
	return n, nil
}
