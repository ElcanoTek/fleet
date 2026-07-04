package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// TestUsageReportAdminGate mirrors TestSLAReportAdminGate: GET /admin/usage is
// registered behind AdminOrUserAuthMiddleware (so the web proxy path resolves a
// principal) but is admin-only, gated in-handler on PermissionAdmin. The admin
// API key OR an admin-role user passes; a non-admin member gets 403; an
// unauthenticated request gets 401 from the middleware.
func TestUsageReportAdminGate(t *testing.T) {
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
	addUser("usage-admin", "admin", "usage-admin-token")
	addUser("usage-client", "client", "usage-client-token")

	gated := h.AdminOrUserAuthMiddleware(http.HandlerFunc(h.GetUsageReport))

	cases := []struct {
		name   string
		header func(*http.Request)
		want   int
	}{
		{name: "no auth → 401", header: func(*http.Request) {}, want: http.StatusUnauthorized},
		{
			name:   "non-admin member → 403",
			header: func(r *http.Request) { r.Header.Set("Authorization", "Bearer usage-client-token") },
			want:   http.StatusForbidden,
		},
		{
			name:   "admin-role user → 200",
			header: func(r *http.Request) { r.Header.Set("Authorization", "Bearer usage-admin-token") },
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
			req := httptest.NewRequest("GET", "/admin/usage", nil)
			tc.header(req)
			w := httptest.NewRecorder()
			gated.ServeHTTP(w, req)
			if w.Code != tc.want {
				t.Fatalf("GET /admin/usage: got %d, want %d (body: %s)", w.Code, tc.want, w.Body.String())
			}
		})
	}
}

// TestUsageReportParamValidation covers the query-param contract: the closed
// group_by set, the two timestamp formats, from<to, and the defaults.
func TestUsageReportParamValidation(t *testing.T) {
	h, _ := setupTest(t)

	do := func(query string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/admin/usage"+query, nil)
		req.Header.Set("X-API-Key", "admin-key")
		w := httptest.NewRecorder()
		h.GetUsageReport(w, req)
		return w
	}

	bad := []struct{ name, query string }{
		{"unknown group_by", "?group_by=prompt"},
		{"sql in group_by", "?group_by=user%3B--"},
		{"bad from", "?from=yesterday"},
		{"bad to", "?to=2026-13-99"},
		{"from equals to", "?from=2026-06-01&to=2026-06-01"},
		{"from after to", "?from=2026-06-02&to=2026-06-01"},
	}
	for _, tc := range bad {
		t.Run(tc.name+" → 400", func(t *testing.T) {
			if w := do(tc.query); w.Code != http.StatusBadRequest {
				t.Fatalf("got %d, want 400 (body: %s)", w.Code, w.Body.String())
			}
		})
	}

	t.Run("defaults → user grouping over the trailing 30 days", func(t *testing.T) {
		w := do("")
		if w.Code != http.StatusOK {
			t.Fatalf("got %d, want 200 (body: %s)", w.Code, w.Body.String())
		}
		var rep models.UsageReport
		if err := json.Unmarshal(w.Body.Bytes(), &rep); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if rep.GroupBy != "user" {
			t.Errorf("default group_by = %q, want user", rep.GroupBy)
		}
		if got := rep.To.Sub(rep.From); got != 30*24*time.Hour {
			t.Errorf("default window = %v, want 720h", got)
		}
		// Chat provider not wired → tasks-only, and the report says so.
		if len(rep.Sources) != 1 || rep.Sources[0] != "tasks" {
			t.Errorf("sources = %v, want [tasks]", rep.Sources)
		}
		if rep.Note == "" {
			t.Error("honest-scope pricing note missing")
		}
	})

	t.Run("date-only and RFC3339 both accepted; window clamped to 366 days", func(t *testing.T) {
		w := do("?group_by=day&from=2020-01-01&to=2026-06-01T00:00:00Z")
		if w.Code != http.StatusOK {
			t.Fatalf("got %d, want 200 (body: %s)", w.Code, w.Body.String())
		}
		var rep models.UsageReport
		if err := json.Unmarshal(w.Body.Bytes(), &rep); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got := rep.To.Sub(rep.From); got != usageMaxWindow {
			t.Errorf("clamped window = %v, want %v", got, usageMaxWindow)
		}
	})
}

// TestUsageReportMergesChatSource exercises the ChatUsageProvider seam: chat
// rows merge into the task buckets by key, per-source subtotals survive, and a
// chat-side failure is a 500 (never a silent under-count).
func TestUsageReportMergesChatSource(t *testing.T) {
	h, store := setupTest(t)
	ctx := context.Background()

	// Task side: one iteration by alice, $2, 200/20 tokens.
	alice := uuid.New()
	if err := store.DB().AddUser(ctx, &models.User{ID: alice, Username: "alice@example.com", Role: "client", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("AddUser: %v", err)
	}
	task := &models.Task{ID: uuid.New(), Prompt: "t", Status: models.TaskStatusSuccess, CreatedAt: time.Now().UTC(), CreatedBy: &alice}
	if err := store.DB().AddTask(ctx, task); err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	it := &models.TaskIteration{ID: uuid.New(), TaskID: task.ID, IterationNumber: 1, StartedAt: time.Now().UTC().Add(-time.Hour), CostUSD: 2.0, PromptTokens: 200, CompletionTokens: 20, Status: "completed"}
	if err := store.DB().AddTaskIteration(ctx, it); err != nil {
		t.Fatalf("AddTaskIteration: %v", err)
	}

	// Chat side: same principal ($1, 100/10, 5 cached) plus a chat-only one.
	h.SetChatUsageProvider(func(_ context.Context, _, _ time.Time, groupBy string) ([]models.UsageBucket, error) {
		if groupBy != "user" {
			t.Errorf("chat provider called with group_by %q, want user", groupBy)
		}
		return []models.UsageBucket{
			{Key: "alice@example.com", ChatCostUSD: 1.0, PromptTokens: 100, CompletionTokens: 10, CachedTokens: 5, ChatTurns: 1},
			{Key: "bob@example.com", ChatCostUSD: 4.0, PromptTokens: 400, CompletionTokens: 40, ChatTurns: 2},
		}, nil
	})

	req := httptest.NewRequest("GET", "/admin/usage?group_by=user", nil)
	req.Header.Set("X-API-Key", "admin-key")
	w := httptest.NewRecorder()
	h.GetUsageReport(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d (body: %s)", w.Code, w.Body.String())
	}
	var rep models.UsageReport
	if err := json.Unmarshal(w.Body.Bytes(), &rep); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(rep.Sources) != 2 || rep.Sources[1] != "chat" {
		t.Errorf("sources = %v, want [tasks chat]", rep.Sources)
	}
	if len(rep.Buckets) != 2 {
		t.Fatalf("want 2 buckets, got %+v", rep.Buckets)
	}
	// Sorted by combined cost desc: bob ($4) before alice ($3).
	if rep.Buckets[0].Key != "bob@example.com" || rep.Buckets[1].Key != "alice@example.com" {
		t.Fatalf("bucket order wrong: %+v", rep.Buckets)
	}
	a := rep.Buckets[1]
	if a.CostUSD != 3.0 || a.TaskCostUSD != 2.0 || a.ChatCostUSD != 1.0 {
		t.Errorf("alice cost split wrong: %+v", a)
	}
	if a.PromptTokens != 300 || a.CompletionTokens != 30 || a.CachedTokens != 5 {
		t.Errorf("alice token merge wrong: %+v", a)
	}
	if a.TaskIterations != 1 || a.ChatTurns != 1 {
		t.Errorf("alice unit counts wrong: %+v", a)
	}
	if rep.Totals.CostUSD != 7.0 || rep.Totals.TaskIterations != 1 || rep.Totals.ChatTurns != 3 {
		t.Errorf("totals wrong: %+v", rep.Totals)
	}

	t.Run("chat-side failure → 500, not a silent under-count", func(t *testing.T) {
		h.SetChatUsageProvider(func(context.Context, time.Time, time.Time, string) ([]models.UsageBucket, error) {
			return nil, errors.New("chat store down")
		})
		req := httptest.NewRequest("GET", "/admin/usage", nil)
		req.Header.Set("X-API-Key", "admin-key")
		w := httptest.NewRecorder()
		h.GetUsageReport(w, req)
		if w.Code != http.StatusInternalServerError {
			t.Fatalf("got %d, want 500", w.Code)
		}
	})
}

// TestUsageReportCSVExport: ?format=csv returns a text/csv attachment with the
// header row + a trailing TOTAL row; an unknown format is a 400.
func TestUsageReportCSVExport(t *testing.T) {
	h, _ := setupTest(t)
	do := func(query string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/admin/usage"+query, nil)
		req.Header.Set("X-API-Key", "admin-key")
		w := httptest.NewRecorder()
		h.GetUsageReport(w, req)
		return w
	}

	w := do("?format=csv&group_by=user")
	if w.Code != http.StatusOK {
		t.Fatalf("csv: got %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/csv; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	cd := w.Header().Get("Content-Disposition")
	if !strings.Contains(cd, "attachment; filename=") || !strings.Contains(cd, "fleet-usage-user-") {
		t.Errorf("Content-Disposition = %q", cd)
	}
	body := w.Body.String()
	// Header row names the group dimension + token columns; TOTAL row present.
	if !strings.HasPrefix(body, "user,label,cost_usd,") {
		t.Errorf("csv header = %q", strings.SplitN(body, "\n", 2)[0])
	}
	if !strings.Contains(body, "TOTAL") {
		t.Errorf("csv should end with a TOTAL row, got:\n%s", body)
	}

	// Unknown format → 400.
	if w := do("?format=xlsx"); w.Code != http.StatusBadRequest {
		t.Fatalf("unknown format: got %d, want 400", w.Code)
	}
	// Explicit json still works.
	if w := do("?format=json"); w.Code != http.StatusOK || w.Header().Get("Content-Type") == "text/csv; charset=utf-8" {
		t.Fatalf("json format should stay JSON, got %d ct=%q", w.Code, w.Header().Get("Content-Type"))
	}
}
