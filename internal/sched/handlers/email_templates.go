package handlers

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/ElcanoTek/fleet/internal/clientconfig"
	"github.com/ElcanoTek/fleet/internal/logging"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// SetEmailTemplateCatalogProvider wires the bundle's live-read
// email_templates/ directory into the library: the list, and one template
// with its preview. nil providers are a valid empty catalog.
func (h *Handlers) SetEmailTemplateCatalogProvider(
	list func() ([]clientconfig.EmailTemplate, []string),
	byID func(id string) (clientconfig.EmailTemplate, bool, []string),
) {
	h.emailTemplateCatalog = list
	h.emailTemplateByID = byID
}

func isEmailTemplatePath(path string) bool {
	return path == "/email-templates" || strings.HasPrefix(path, "/email-templates/")
}

func (h *Handlers) logEmailTemplateProblems(r *http.Request, problems []string) {
	for _, problem := range problems {
		logging.From(r.Context()).Warn("email template library: problem", "problem", problem)
	}
}

// EmailTemplateLibrary handles GET /email-templates: the bundle's email
// templates, read-only, without their previews. Like the prompt library it is
// shared with chat, so a signed-in member reads it by their Fleet role
// (isPromptLibraryPath).
//
// The list is what the composer asks to decide whether to show its menu
// entry, so it stays small: a preview (up to 1 MiB each) is served only by
// EmailTemplateLibraryItem, for the one template a person is looking at.
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
		h.logEmailTemplateProblems(r, problems)
		templates = append(templates, found...)
	}
	writeJSON(w, r, http.StatusOK, templates)
}

// EmailTemplateLibraryItem handles GET /email-templates/{template_id}: one
// template with preview_html, the bundle's rendered sample.
func (h *Handlers) EmailTemplateLibraryItem(w http.ResponseWriter, r *http.Request) {
	if !h.principalFromRequest(r).hasPermission(models.PermissionViewTasks) {
		writeError(w, http.StatusForbidden, "view permission required")
		return
	}
	if h.emailTemplateByID != nil {
		template, ok, problems := h.emailTemplateByID(chi.URLParam(r, "template_id"))
		h.logEmailTemplateProblems(r, problems)
		if ok {
			writeJSON(w, r, http.StatusOK, template)
			return
		}
	}
	writeError(w, http.StatusNotFound, "email template not found")
}
