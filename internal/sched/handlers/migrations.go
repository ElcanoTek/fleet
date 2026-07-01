package handlers

import "net/http"

// MigrationStatus reports the orchestrator (sched) DB's applied vs pending
// migrations (#256), admin-gated by the route group. It is strictly read-only:
// it reads golang-migrate's tracking row and the embedded migration set,
// applying nothing.
func (h *Handlers) MigrationStatus(w http.ResponseWriter, _ *http.Request) {
	report, err := h.storage.MigrationStatus()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to read migration status")
		return
	}
	writeJSON(w, http.StatusOK, report)
}
