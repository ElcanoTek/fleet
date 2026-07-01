package httpapi

import (
	"encoding/json"
	"net/http"
)

// handleMigrations reports the chat DB's applied vs pending migrations (#256).
// Admin-gated (wired with adminMiddleware in Routes). It is strictly read-only —
// it queries schema_migrations and the embedded migration set, applying nothing —
// so it is safe to call at any time. The orchestrator server exposes the
// equivalent for the sched DB at the same path on its own port.
func (s *Server) handleMigrations(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	report, err := s.store.MigrationStatus(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(report)
}
