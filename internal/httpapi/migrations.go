package httpapi

import (
	"net/http"
)

// handleMigrations reports applied vs pending migrations of the one sequence
// (#256, ADR-0107): one ledger, core.schema_versions, for the whole database.
// Admin-gated (wired with adminMiddleware in Routes). It is strictly
// read-only — it reads the ledger and the embedded migration set, applying
// nothing — so it is safe to call at any time. The orchestrator server
// answers the same report at the same path on its own port.
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
	writeJSON(w, report)
}
