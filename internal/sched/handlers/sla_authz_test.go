package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// TestSLAReportAdminGate locks in the #458 fix: GET /sla-report is reachable from
// the web proxy (so it sits behind AdminOrUserAuthMiddleware, not the bare
// admin-API-key gate) but is admin-only — gated in-handler on PermissionAdmin.
// The admin API key OR an admin-role user passes; a non-admin member gets 403;
// an unauthenticated request gets 401 from the middleware. Before the fix the
// route was admin-API-key-only and the proxy never sent X-API-Key, so it was a
// guaranteed "Invalid API key" for every dashboard user.
func TestSLAReportAdminGate(t *testing.T) {
	h, store := setupTest(t)

	addUser := func(username, role, token string) {
		t.Helper()
		hash := models.HashToken(token)
		u := &models.User{
			ID:           uuid.New(),
			Username:     username,
			Role:         role,
			Scopes:       []string{},
			CreatedAt:    time.Now(),
			SessionToken: &hash,
		}
		if _, err := store.AddUser(u); err != nil {
			t.Fatalf("AddUser(%s): %v", username, err)
		}
	}
	addUser("sla-admin", "admin", "sla-admin-token")
	addUser("sla-client", "client", "sla-client-token")

	gated := h.AdminOrUserAuthMiddleware(http.HandlerFunc(h.GetSLAReport))

	cases := []struct {
		name   string
		header func(*http.Request)
		want   int
	}{
		{name: "no auth → 401", header: func(*http.Request) {}, want: http.StatusUnauthorized},
		{
			name:   "non-admin member → 403",
			header: func(r *http.Request) { r.Header.Set("Authorization", "Bearer sla-client-token") },
			want:   http.StatusForbidden,
		},
		{
			name:   "admin-role user → 200",
			header: func(r *http.Request) { r.Header.Set("Authorization", "Bearer sla-admin-token") },
			want:   http.StatusOK,
		},
		{
			name:   "admin API key → 200",
			header: func(r *http.Request) { r.Header.Set("X-API-Key", "admin-key") },
			want:   http.StatusOK,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest("GET", "/sla-report", nil)
			tc.header(req)
			w := httptest.NewRecorder()
			gated.ServeHTTP(w, req)
			if w.Code != tc.want {
				t.Fatalf("GET /sla-report: got %d, want %d (body: %s)", w.Code, tc.want, w.Body.String())
			}
		})
	}
}
