package handlers

import (
	"net/http"

	"github.com/ElcanoTek/fleet/internal/clientconfig"
	"github.com/ElcanoTek/fleet/internal/logging"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// SetEmailTemplateCatalogProvider wires the bundle's live-read
// email_templates/ directory into the library. nil is a valid empty catalog.
func (h *Handlers) SetEmailTemplateCatalogProvider(p func() ([]clientconfig.EmailTemplate, []string)) {
	h.emailTemplateCatalog = p
}

// EmailTemplateLibrary handles GET /email-templates: the bundle's email
// templates, read-only. Like the prompt library it is shared with chat, so a
// signed-in member reads it by their Fleet role (isPromptLibraryPath).
//
// A template's problems are logged, never returned: they are for the bundle's
// author, and one bad file must not take the library away from its users.
func (h *Handlers) EmailTemplateLibrary(w http.ResponseWriter, r *http.Request) {
	if !h.principalFromRequest(r).hasPermission(models.PermissionViewTasks) {
		writeError(w, http.StatusForbidden, "view permission required")
		return
	}
	templates := []clientconfig.EmailTemplate{}
	if h.emailTemplateCatalog != nil {
		found, problems := h.emailTemplateCatalog()
		for _, problem := range problems {
			logging.From(r.Context()).Warn("email template library: problem", "problem", problem)
		}
		templates = append(templates, found...)
	}
	writeJSON(w, r, http.StatusOK, templates)
}
