package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/ElcanoTek/fleet/internal/logging"
	"github.com/ElcanoTek/fleet/internal/safe"
	"github.com/ElcanoTek/fleet/internal/sidebar"
	"github.com/ElcanoTek/fleet/internal/store"
)

// Saved left-rail ("sidebar") layouts. Each user may save their own layout
// (/sidebar); an admin may save a workspace default (/admin/sidebar) that
// applies to everyone who has not customized. The client resolves the
// effective layout (user ?? workspace default ?? its built-in default); the
// server stores and holds every write to the internal/sidebar contract.
//
// A layout is a preference, not an authority: a shortcut names a chat,
// project or task by id, and opening it still goes through that resource's own
// access checks.

// maxSidebarBody caps one layout PUT. A full 60-item layout is a few KiB.
const maxSidebarBody = 64 << 10

// decodeSidebarBody reads and normalizes a PUT body, answering 400 itself on
// bad JSON or a layout that breaks the contract.
func decodeSidebarBody(w http.ResponseWriter, r *http.Request) (sidebar.Layout, bool) {
	r.Body = http.MaxBytesReader(w, r.Body, maxSidebarBody)
	var in sidebar.Layout
	if !decodeJSONBody(w, r, &in) {
		return sidebar.Layout{}, false
	}
	l, err := sidebar.Normalize(in)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return sidebar.Layout{}, false
	}
	return l, true
}

// storedSidebarLayout decodes a stored row. A row that no longer passes the
// contract (saved by another build) reads as absent, with a warning, so one
// bad row never breaks the rail.
func storedSidebarLayout(ctx context.Context, raw []byte, whose string) *sidebar.Layout {
	l, err := sidebar.Parse(raw)
	if err != nil {
		logging.From(ctx).Warn("sidebar: ignoring a stored layout that no longer validates", "layout", whose, "err", safe.LogSafe(err.Error()))
		return nil
	}
	return &l
}

// workspaceSidebarDefault returns the workspace default layout and its row,
// nil when none is saved (or the saved one no longer validates).
func (s *Server) workspaceSidebarDefault(ctx context.Context) (*sidebar.Layout, store.WorkspaceSidebarLayout, error) {
	row, err := s.store.WorkspaceSidebarLayout(ctx)
	if errors.Is(err, store.ErrSidebarLayoutNotFound) {
		return nil, store.WorkspaceSidebarLayout{}, nil
	}
	if err != nil {
		return nil, store.WorkspaceSidebarLayout{}, err
	}
	return storedSidebarLayout(ctx, row.Layout, "workspace"), row, nil
}

// handleSidebar serves /sidebar, the caller's own layout:
//
//	GET    → {"layout": <layout|null>, "workspace_default": <layout|null>}
//	PUT    → body is a layout; saved normalized → {"layout": <normalized>}
//	DELETE → drops the caller's layout → {"layout": null}
func (s *Server) handleSidebar(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	user := userFromCtx(ctx)
	switch r.Method {
	case http.MethodGet:
		var mine *sidebar.Layout
		raw, err := s.store.SidebarLayout(ctx, user)
		switch {
		case err == nil:
			mine = storedSidebarLayout(ctx, raw, "user")
		case !errors.Is(err, store.ErrSidebarLayoutNotFound):
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		ws, _, err := s.workspaceSidebarDefault(ctx)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		// Members get the default without the captured target titles
		// (sidebar.Layout.ForMembers): an admin's pin to their own chat must
		// not name that chat to everyone.
		if ws != nil {
			redacted := ws.ForMembers()
			ws = &redacted
		}
		writeJSON(w, r, map[string]any{"layout": mine, "workspace_default": ws})
	case http.MethodPut:
		l, ok := decodeSidebarBody(w, r)
		if !ok {
			return
		}
		raw, err := json.Marshal(l)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if err := s.store.SetSidebarLayout(ctx, user, raw); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, r, map[string]any{"layout": l})
	case http.MethodDelete:
		if err := s.store.DeleteSidebarLayout(ctx, user); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		writeJSON(w, r, map[string]any{"layout": nil})
	default:
		w.Header().Set("Allow", "GET, PUT, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// adminSidebarResponse is /admin/sidebar's answer.
type adminSidebarResponse struct {
	Layout    *sidebar.Layout `json:"layout"`
	UpdatedBy string          `json:"updated_by"`
	UpdatedAt int64           `json:"updated_at"`
}

// handleAdminSidebar serves /admin/sidebar, the workspace default layout
// (admin-gated by the route):
//
//	GET    → {"layout": <layout|null>, "updated_by": "...", "updated_at": <unix s>}
//	PUT    → body is a layout; saved normalized → the same shape
//	DELETE → removes the default → {"layout": null, "updated_by": "", "updated_at": 0}
func (s *Server) handleAdminSidebar(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	switch r.Method {
	case http.MethodGet:
		l, row, err := s.workspaceSidebarDefault(ctx)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		resp := adminSidebarResponse{Layout: l}
		if l != nil {
			resp.UpdatedBy, resp.UpdatedAt = row.UpdatedBy, row.UpdatedAt
		}
		writeJSON(w, r, resp)
	case http.MethodPut:
		l, ok := decodeSidebarBody(w, r)
		if !ok {
			return
		}
		if err := sidebar.CheckWorkspace(l); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		raw, err := json.Marshal(l)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		actor := userFromCtx(ctx)
		row, err := s.store.SetWorkspaceSidebarLayout(ctx, raw, actor)
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		logging.From(ctx).Info("workspace sidebar layout: saved", "items", len(l.Items), "updated_by", safe.LogSafe(actor))
		writeJSON(w, r, adminSidebarResponse{Layout: &l, UpdatedBy: row.UpdatedBy, UpdatedAt: row.UpdatedAt})
	case http.MethodDelete:
		if err := s.store.DeleteWorkspaceSidebarLayout(ctx); err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		logging.From(ctx).Info("workspace sidebar layout: reset to the built-in default", "updated_by", safe.LogSafe(userFromCtx(ctx)))
		writeJSON(w, r, adminSidebarResponse{})
	default:
		w.Header().Set("Allow", "GET, PUT, DELETE")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
