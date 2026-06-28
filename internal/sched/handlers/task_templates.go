// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

package handlers

import (
	"net/http"
	"regexp"
	"sort"

	"github.com/ElcanoTek/fleet/internal/clientconfig"
)

// taskTemplateVarPattern matches a single {token} placeholder in a template
// prompt. A token is one or more word characters (letters, digits, underscore) —
// e.g. {repo_path}, {date}, {user_name}. The braces are NOT nested and a bare
// "{}" or "{ }" is not a placeholder. The UI uses the extracted names to drive a
// variable-fill dialog before pre-filling the form; the backend only surfaces
// them, it does not substitute (substitution is a UI-side, per-user concern).
var taskTemplateVarPattern = regexp.MustCompile(`\{(\w+)\}`)

// extractTemplateVars returns the de-duplicated, sorted set of {token} variable
// names referenced in a template prompt, so the UI can prompt for them before
// applying the template. Sorted for a stable response (and stable tests); an
// empty/placeholder-free prompt yields an empty slice (never nil — the JSON must
// be [] so the UI can branch on length without a null guard).
func extractTemplateVars(prompt string) []string {
	matches := taskTemplateVarPattern.FindAllStringSubmatch(prompt, -1)
	seen := make(map[string]struct{}, len(matches))
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		name := m[1]
		if _, dup := seen[name]; dup {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// taskTemplateProvider returns the bundle's task-template catalog. Injected by
// cmd/fleet from the loaded client bundle (Bundle.TaskTemplates); nil → empty.
type taskTemplateProvider func() []clientconfig.TaskTemplate

// SetTaskTemplateProvider wires the read-only task-template catalog the
// orchestrator serves to its task-create UI. cmd/fleet builds it once from the
// loaded client bundle and injects it here, keeping the handlers package
// decoupled from clientconfig's load path (mirrors SetMCPCatalogProvider). A nil
// provider → an empty catalog, so a bundle that ships no templates simply
// returns [] and the UI suppresses the template section.
func (h *Handlers) SetTaskTemplateProvider(p func() []clientconfig.TaskTemplate) {
	h.taskTemplates = p
}

// wireTaskTemplate is the response shape for one template: the bundle entry plus
// the {variable} names extracted from its prompt. The embedded TaskTemplate
// carries the partial-TaskCreate `task` payload verbatim (opaque to the backend);
// `variables` is the only enrichment.
type wireTaskTemplate struct {
	clientconfig.TaskTemplate
	Variables []string `json:"variables"`
}

// ListTaskTemplates handles GET /task-templates. It returns the bundle's
// read-only task-template catalog (each entry's name/description/icon, its
// partial-TaskCreate `task` payload, and the {variable} names found in the
// prompt). It is purely a config read — no database access, no task is created.
// Returns [] (never null) when no templates are configured. Same member-auth gate
// as the other orchestrator reads (registered in cmd/fleet's admin-or-user group).
func (h *Handlers) ListTaskTemplates(w http.ResponseWriter, _ *http.Request) {
	var templates []clientconfig.TaskTemplate
	if h.taskTemplates != nil {
		templates = h.taskTemplates()
	}
	out := make([]wireTaskTemplate, 0, len(templates))
	for _, t := range templates {
		out = append(out, wireTaskTemplate{
			TaskTemplate: t,
			Variables:    extractTemplateVars(t.Task.Prompt),
		})
	}
	writeJSON(w, http.StatusOK, out)
}
