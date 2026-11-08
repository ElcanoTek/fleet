package handlers

import "net/http"

// MigrationStatus reports applied vs pending migrations of the one sequence
// (#256, ADR-0107), admin-gated by the route group. It is strictly read-only:
// it reads core.schema_versions and the embedded migration set, applying
// nothing. The chat server answers the same report at the same path.
func (h *Handlers) MigrationStatus(w http.ResponseWriter, r *http.Request) {
	report, err := h.storage.MigrationStatus(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to read migration status")
		return
	}
	writeJSON(w, http.StatusOK, report)
}
