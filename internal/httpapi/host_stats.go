package httpapi

import (
	"net/http"

	"github.com/ElcanoTek/fleet/internal/hoststats"
)

// handleServerStats is the admin-only, read-only host snapshot used by
// Settings → Admin → Server. Authorization is applied at route registration;
// this handler deliberately exposes no process list, command line, environment,
// addresses, or filesystem names beyond the root mount.
func (s *Server) handleServerStats(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.hostStats == nil {
		writeJSON(w, hoststats.Snapshot{Warnings: []string{"host statistics collector unavailable"}})
		return
	}
	writeJSON(w, s.hostStats.Collect())
}
