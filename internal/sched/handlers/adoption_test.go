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

// TestAdoptionReportAdminGate mirrors TestUsageReportAdminGate: the adoption
// report is global across all users' activity, so it carries the exact same
// posture — AdminOrUserAuthMiddleware for proxy reachability, PermissionAdmin
// enforced in-handler.
func TestAdoptionReportAdminGate(t *testing.T) {
	h, store := setupTest(t)

	addUser := func(username, role, token string) {
		t.Helper()
		hash := models.HashToken(token)
		u := &models.User{
			ID:           uuid.New(),
			Username:     username,
			Role:         role,
			CreatedAt:    time.Now(),
			SessionToken: &hash,
		}
		if _, err := store.AddUser(u); err != nil {
			t.Fatalf("AddUser(%s): %v", username, err)
		}
	}
	addUser("adopt-admin", "admin", "adopt-admin-token")
	addUser("adopt-client", "client", "adopt-client-token")

	gated := h.AdminOrUserAuthMiddleware(http.HandlerFunc(h.GetAdoptionReport))

	cases := []struct {
		name   string
		header func(*http.Request)
		want   int
	}{
		{name: "no auth → 401", header: func(*http.Request) {}, want: http.StatusUnauthorized},
		{
			name:   "non-admin member → 403",
			header: func(r *http.Request) { r.Header.Set("Authorization", "Bearer adopt-client-token") },
			want:   http.StatusForbidden,
		},
		{
			name:   "admin-role user → 200",
			header: func(r *http.Request) { r.Header.Set("Authorization", "Bearer adopt-admin-token") },
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
			req := httptest.NewRequest("GET", "/admin/usage/adoption", nil)
			tc.header(req)
			w := httptest.NewRecorder()
			gated.ServeHTTP(w, req)
			if w.Code != tc.want {
				t.Fatalf("GET /admin/usage/adoption: got %d, want %d (body: %s)", w.Code, tc.want, w.Body.String())
			}
		})
	}
}

// TestAdoptionReportParamValidation covers the query-param contract: the two
// timestamp formats, from<to, the defaults, and the 366-day clamp — the
// /admin/usage rules verbatim (there is no group_by here; the grouping is
// fixed at user × day).
func TestAdoptionReportParamValidation(t *testing.T) {
	h, _ := setupTest(t)

	do := func(query string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/admin/usage/adoption"+query, nil)
		req.Header.Set("X-API-Key", "admin-key")
		w := httptest.NewRecorder()
		h.GetAdoptionReport(w, req)
		return w
	}

	bad := []struct{ name, query string }{
		{"bad from", "?from=yesterday"},
		{"bad to", "?to=2026-13-99"},
		{"from equals to", "?from=2026-06-01&to=2026-06-01"},
		{"from after to", "?from=2026-06-02&to=2026-06-01"},
		{"unknown format", "?format=xlsx"},
	}
	for _, tc := range bad {
		t.Run(tc.name+" → 400", func(t *testing.T) {
			if w := do(tc.query); w.Code != http.StatusBadRequest {
				t.Fatalf("got %d, want 400 (body: %s)", w.Code, w.Body.String())
			}
		})
	}

	t.Run("defaults → trailing 30 days, prev window immediately before", func(t *testing.T) {
		w := do("")
		if w.Code != http.StatusOK {
			t.Fatalf("got %d, want 200 (body: %s)", w.Code, w.Body.String())
		}
		var rep models.AdoptionReport
		if err := json.Unmarshal(w.Body.Bytes(), &rep); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got := rep.To.Sub(rep.From); got != 30*24*time.Hour {
			t.Errorf("default window = %v, want 720h", got)
		}
		if got := rep.From.Sub(rep.PrevFrom); got != 30*24*time.Hour {
			t.Errorf("prev window = %v, want 720h", got)
		}
		// Chat + accounts seams not wired → tasks-only, and the report says so.
		if len(rep.Sources) != 1 || rep.Sources[0] != "tasks" {
			t.Errorf("sources = %v, want [tasks]", rep.Sources)
		}
		if rep.Note == "" || !strings.Contains(rep.Note, "adoption signal") {
			t.Errorf("honest-scope note missing or incomplete: %q", rep.Note)
		}
		// The day axis spans the window even with no activity at all.
		if len(rep.Days) < 30 || len(rep.Days) > 31 {
			t.Errorf("day axis = %d entries, want 30-31", len(rep.Days))
		}
	})

	t.Run("window clamped to 366 days", func(t *testing.T) {
		w := do("?from=2020-01-01&to=2026-06-01T00:00:00Z")
		if w.Code != http.StatusOK {
			t.Fatalf("got %d, want 200 (body: %s)", w.Code, w.Body.String())
		}
		var rep models.AdoptionReport
		if err := json.Unmarshal(w.Body.Bytes(), &rep); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if got := rep.To.Sub(rep.From); got != usageMaxWindow {
			t.Errorf("clamped window = %v, want %v", got, usageMaxWindow)
		}
		if got := len(rep.Days); got != 366 {
			t.Errorf("day axis = %d entries, want 366", got)
		}
	})
}

// TestAdoptionReportMergesSources exercises the full merge: task metering from
// the sched DB, chat metering + the account roster through their seams,
// per-user daily series, previous-window comparators, and the inactive-seat
// roster.
func TestAdoptionReportMergesSources(t *testing.T) {
	h, store := setupTest(t)
	ctx := context.Background()

	// Fixed window: June 2026 (30 days), prev window = May 3 – June 1.
	const query = "?from=2026-06-01&to=2026-07-01"

	// Task side: alice runs one iteration on June 2 — $2, 200/20 tokens.
	alice := uuid.New()
	if err := store.DB().AddUser(ctx, &models.User{ID: alice, Username: "alice@example.com", Role: "client", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatalf("AddUser: %v", err)
	}
	task := &models.Task{ID: uuid.New(), Prompt: "t", Status: models.TaskStatusSuccess, CreatedAt: time.Now().UTC(), CreatedBy: &alice}
	if err := store.DB().AddTask(ctx, task); err != nil {
		t.Fatalf("AddTask: %v", err)
	}
	it := &models.TaskIteration{ID: uuid.New(), TaskID: task.ID, IterationNumber: 1, StartedAt: time.Date(2026, 6, 2, 12, 0, 0, 0, time.UTC), CostUSD: 2.0, PromptTokens: 200, CompletionTokens: 20, Status: "completed"}
	if err := store.DB().AddTaskIteration(ctx, it); err != nil {
		t.Fatalf("AddTaskIteration: %v", err)
	}

	// Chat side: alice chats June 1 + June 2, bob chats June 3 only. In the
	// previous window alice and carol were active (carol has since churned).
	h.SetChatUserDayUsageProvider(func(_ context.Context, from, _ time.Time) ([]models.UserDayUsage, error) {
		if from.Format("2006-01-02") == "2026-06-01" {
			return []models.UserDayUsage{
				{User: "alice@example.com", Day: "2026-06-01", CostUSD: 1.0, PromptTokens: 100, CompletionTokens: 10, CachedTokens: 5, Units: 1},
				{User: "alice@example.com", Day: "2026-06-02", CostUSD: 0.5, PromptTokens: 50, CompletionTokens: 5, Units: 1},
				{User: "bob@example.com", Day: "2026-06-03", CostUSD: 4.0, PromptTokens: 400, CompletionTokens: 40, Units: 2},
			}, nil
		}
		return []models.UserDayUsage{
			{User: "alice@example.com", Day: "2026-05-10", CostUSD: 3.0, PromptTokens: 300, CompletionTokens: 30, Units: 2},
			{User: "carol@example.com", Day: "2026-05-12", CostUSD: 1.0, PromptTokens: 100, CompletionTokens: 10, Units: 1},
		}, nil
	})
	// Roster: the three actives-or-formers plus dave, who has never used it.
	h.SetChatAccountsProvider(func(context.Context) ([]models.AdoptionSeat, error) {
		return []models.AdoptionSeat{
			{Email: "dave@example.com", CreatedAt: time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)},
			{Email: "alice@example.com", CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
			{Email: "bob@example.com", CreatedAt: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)},
			{Email: "carol@example.com", CreatedAt: time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)},
		}, nil
	})

	req := httptest.NewRequest("GET", "/admin/usage/adoption"+query, nil)
	req.Header.Set("X-API-Key", "admin-key")
	w := httptest.NewRecorder()
	h.GetAdoptionReport(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("got %d (body: %s)", w.Code, w.Body.String())
	}
	var rep models.AdoptionReport
	if err := json.Unmarshal(w.Body.Bytes(), &rep); err != nil {
		t.Fatalf("decode: %v", err)
	}

	if want := []string{"tasks", "chat", "accounts"}; strings.Join(rep.Sources, ",") != strings.Join(want, ",") {
		t.Errorf("sources = %v, want %v", rep.Sources, want)
	}
	if len(rep.Days) != 30 || rep.Days[0] != "2026-06-01" || rep.Days[29] != "2026-06-30" {
		t.Fatalf("day axis wrong: %d entries, first %q last %q", len(rep.Days), rep.Days[0], rep.Days[len(rep.Days)-1])
	}
	assertAdoptionUsers(t, &rep)
	assertAdoptionDailyAndTotals(t, &rep)

	// Inactive roster: carol (churned) and dave (never active), sorted.
	if len(rep.InactiveUsers) != 2 || rep.InactiveUsers[0].Email != "carol@example.com" || rep.InactiveUsers[1].Email != "dave@example.com" {
		t.Errorf("inactive roster wrong: %+v", rep.InactiveUsers)
	}
}

// assertAdoptionUsers checks the per-user rows of the merged fixture:
// token-first ordering, the task/chat splits, activity fields, the aligned
// daily series, and the previous-window comparators.
func assertAdoptionUsers(t *testing.T, rep *models.AdoptionReport) {
	t.Helper()
	// Leaderboard sorts token volume first: bob (440) before alice (385).
	if len(rep.Users) != 2 {
		t.Fatalf("want 2 user rows, got %+v", rep.Users)
	}
	if rep.Users[0].User != "bob@example.com" || rep.Users[1].User != "alice@example.com" {
		t.Fatalf("user order wrong: %s, %s", rep.Users[0].User, rep.Users[1].User)
	}

	a := rep.Users[1]
	if a.CostUSD != 3.5 || a.TaskCostUSD != 2.0 || a.ChatCostUSD != 1.5 {
		t.Errorf("alice cost split wrong: %+v", a)
	}
	if a.PromptTokens != 350 || a.CompletionTokens != 35 || a.CachedTokens != 5 {
		t.Errorf("alice token merge wrong: %+v", a)
	}
	if a.TaskIterations != 1 || a.ChatTurns != 2 {
		t.Errorf("alice unit counts wrong: %+v", a)
	}
	// Active June 1 (chat) and June 2 (chat + task) → 2 active days.
	if a.ActiveDays != 2 || a.LastActive != "2026-06-02" {
		t.Errorf("alice activity wrong: %+v", a)
	}
	// Daily series is index-aligned: June 1 = 110 chat tokens; June 2 = 55
	// chat + 220 task = 275; everything else 0.
	if len(a.DailyTokens) != 30 || a.DailyTokens[0] != 110 || a.DailyTokens[1] != 275 || a.DailyTokens[2] != 0 {
		t.Errorf("alice daily series wrong: %v", a.DailyTokens)
	}
	// Previous-window comparators.
	if a.PrevCostUSD != 3.0 || a.PrevTokens != 330 {
		t.Errorf("alice prev comparators wrong: %+v", a)
	}
	b := rep.Users[0]
	if b.PrevCostUSD != 0 || b.PrevTokens != 0 || b.ActiveDays != 1 || b.LastActive != "2026-06-03" {
		t.Errorf("bob row wrong: %+v", b)
	}
}

// assertAdoptionDailyAndTotals checks the daily trend rows and the report
// roll-up of the merged fixture.
func assertAdoptionDailyAndTotals(t *testing.T, rep *models.AdoptionReport) {
	t.Helper()
	// Daily trend: June 2 carries alice's chat + task activity.
	d2 := rep.Daily[1]
	if d2.Day != "2026-06-02" || d2.Tokens != 275 || d2.CostUSD != 2.5 || d2.Actions != 2 || d2.ActiveUsers != 1 {
		t.Errorf("June 2 daily trend wrong: %+v", d2)
	}
	d3 := rep.Daily[2]
	if d3.ActiveUsers != 1 || d3.Actions != 2 || d3.Tokens != 440 {
		t.Errorf("June 3 daily trend wrong: %+v", d3)
	}

	tot := rep.Totals
	if tot.ActiveUsers != 2 || tot.PrevActiveUsers != 2 || tot.NewActiveUsers != 1 {
		t.Errorf("headcounts wrong: %+v", tot)
	}
	if tot.RegisteredUsers != 4 {
		t.Errorf("registered = %d, want 4", tot.RegisteredUsers)
	}
	if tot.CostUSD != 7.5 || tot.Tokens != 825 || tot.PrevCostUSD != 4.0 || tot.PrevTokens != 440 {
		t.Errorf("totals wrong: %+v", tot)
	}
	if tot.ChatTurns != 4 || tot.TaskIterations != 1 || tot.CachedTokens != 5 {
		t.Errorf("unit totals wrong: %+v", tot)
	}
}

// TestAdoptionReportSeamFailures: a failing chat or roster seam is a 500 —
// never a silent under-count or a zero seat denominator.
func TestAdoptionReportSeamFailures(t *testing.T) {
	h, _ := setupTest(t)

	do := func() int {
		req := httptest.NewRequest("GET", "/admin/usage/adoption", nil)
		req.Header.Set("X-API-Key", "admin-key")
		w := httptest.NewRecorder()
		h.GetAdoptionReport(w, req)
		return w.Code
	}

	t.Run("chat-side failure → 500, not a silent under-count", func(t *testing.T) {
		h.SetChatUserDayUsageProvider(func(context.Context, time.Time, time.Time) ([]models.UserDayUsage, error) {
			return nil, errors.New("chat store down")
		})
		if code := do(); code != http.StatusInternalServerError {
			t.Fatalf("got %d, want 500", code)
		}
	})

	t.Run("roster failure → 500, not a zero denominator", func(t *testing.T) {
		h.SetChatUserDayUsageProvider(nil)
		h.SetChatAccountsProvider(func(context.Context) ([]models.AdoptionSeat, error) {
			return nil, errors.New("chat store down")
		})
		if code := do(); code != http.StatusInternalServerError {
			t.Fatalf("got %d, want 500", code)
		}
	})
}

// TestAdoptionUnattributedSpend: the empty user (deleted-user task rows) is
// listed and totaled — spend never disappears — but never counts toward the
// adoption headcounts.
func TestAdoptionUnattributedSpend(t *testing.T) {
	from := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 6, 8, 0, 0, 0, 0, time.UTC)
	rep := buildAdoptionReport(from, to, from.AddDate(0, 0, -7),
		[]models.UserDayUsage{
			{User: "", Day: "2026-06-02", CostUSD: 8.0, PromptTokens: 800, CompletionTokens: 80, Units: 1},
			{User: "alice@example.com", Day: "2026-06-02", CostUSD: 1.0, PromptTokens: 100, CompletionTokens: 10, Units: 1},
		},
		nil,
		[]models.UserDayUsage{{User: "", Day: "2026-05-28", CostUSD: 2.0, PromptTokens: 200, Units: 1}},
		nil, nil, []string{"tasks"})

	if len(rep.Users) != 2 || rep.Users[0].User != "" {
		t.Fatalf("unattributed row missing or misplaced: %+v", rep.Users)
	}
	if rep.Totals.ActiveUsers != 1 || rep.Totals.PrevActiveUsers != 0 || rep.Totals.NewActiveUsers != 1 {
		t.Errorf("headcounts must exclude the unattributed bucket: %+v", rep.Totals)
	}
	if rep.Totals.CostUSD != 9.0 || rep.Totals.PrevCostUSD != 2.0 {
		t.Errorf("unattributed spend must still be totaled: %+v", rep.Totals)
	}
}

// TestAdoptionReportCSVExport: ?format=csv returns a text/csv attachment with
// the per-user header row + a trailing TOTAL row.
func TestAdoptionReportCSVExport(t *testing.T) {
	h, _ := setupTest(t)
	req := httptest.NewRequest("GET", "/admin/usage/adoption?format=csv", nil)
	req.Header.Set("X-API-Key", "admin-key")
	w := httptest.NewRecorder()
	h.GetAdoptionReport(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("csv: got %d, want 200 (body: %s)", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "text/csv; charset=utf-8" {
		t.Errorf("Content-Type = %q", ct)
	}
	cd := w.Header().Get("Content-Disposition")
	if !strings.Contains(cd, "attachment; filename=") || !strings.Contains(cd, "fleet-adoption-") {
		t.Errorf("Content-Disposition = %q", cd)
	}
	body := w.Body.String()
	if !strings.HasPrefix(body, "user,tokens,") {
		t.Errorf("csv header = %q", strings.SplitN(body, "\n", 2)[0])
	}
	if !strings.Contains(body, "TOTAL") {
		t.Errorf("csv should end with a TOTAL row, got:\n%s", body)
	}
}
