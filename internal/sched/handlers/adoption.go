package handlers

// GET /admin/usage/adoption: the per-user AI-adoption read model behind the
// Operations Center's executive Adoption view. Where GET /admin/usage answers
// "where did the spend go" one dimension at a time, this report answers "who
// is actually using the agents, how often, and is that growing" — per-user
// merged totals with per-day series (active days, sparklines), an
// equal-length previous window for trend deltas, a daily activity trend, and
// the provisioned-account roster so "who hasn't adopted yet" is a first-class
// answer.
//
// Like usage.go this is STRICTLY a read model over the metering the governed
// core already persists — task-side from task_iterations ⋈ tasks (sched DB),
// chat-side and the account roster through seams into the chat store. No new
// accounting path is introduced anywhere in this file.

import (
	"context"
	"encoding/csv"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// adoptionNote extends the usage report's pricing caveat (#289) with the
// honest-scope framing this view needs: the report measures activity, and
// activity is an adoption signal, not a performance grade.
const adoptionNote = usagePricingNote +
	" Token volume measures how much someone uses the agents, not the quality of what" +
	" they produce with them — read this as an adoption signal, not a performance grade."

// ChatUserDayUsageProvider aggregates the chat store's turn_metrics over
// [from, to) by (user, UTC day). Injected by cmd/fleet via
// SetChatUserDayUsageProvider — same separate-database rationale as
// ChatUsageProvider. nil = chat metering unavailable (the report's sources
// list says so instead of silently under-counting).
type ChatUserDayUsageProvider func(ctx context.Context, from, to time.Time) ([]models.UserDayUsage, error)

// ChatAccountsProvider lists the chat store's provisioned accounts — the
// seat denominator for the adoption rate and the "not yet active" roster.
// nil = roster unavailable; the report omits inactive seats and its sources
// list omits "accounts".
type ChatAccountsProvider func(ctx context.Context) ([]models.AdoptionSeat, error)

// SetChatUserDayUsageProvider wires the chat-side per-user-per-day usage
// aggregation (see ChatUserDayUsageProvider). Call before serving traffic.
func (h *Handlers) SetChatUserDayUsageProvider(fn ChatUserDayUsageProvider) {
	h.chatUserDayUsage = fn
}

// SetChatAccountsProvider wires the chat-side account roster (see
// ChatAccountsProvider). Call before serving traffic.
func (h *Handlers) SetChatAccountsProvider(fn ChatAccountsProvider) {
	h.chatAccounts = fn
}

// GetAdoptionReport handles GET /admin/usage/adoption?from=&to=&format=.
//
// Admin-only with the exact /admin/usage posture: registered behind
// AdminOrUserAuthMiddleware (so the web proxy path resolves a principal) and
// gated HERE on PermissionAdmin — the report is global across all users'
// activity. from/to default to the trailing 30 days, to is exclusive, and the
// window is clamped to usageMaxWindow; the comparison window is the
// equal-length period immediately before from.
func (h *Handlers) GetAdoptionReport(w http.ResponseWriter, r *http.Request) {
	if !h.principalFromRequest(r).hasPermission(models.PermissionAdmin) {
		writeError(w, http.StatusForbidden, "Admin access required")
		return
	}

	to := time.Now().UTC()
	if raw := r.URL.Query().Get("to"); raw != "" {
		t, ok := parseUsageTime(raw)
		if !ok {
			writeError(w, http.StatusBadRequest, "Invalid to (want RFC 3339 or YYYY-MM-DD)")
			return
		}
		to = t
	}
	from := to.Add(-30 * 24 * time.Hour)
	if raw := r.URL.Query().Get("from"); raw != "" {
		t, ok := parseUsageTime(raw)
		if !ok {
			writeError(w, http.StatusBadRequest, "Invalid from (want RFC 3339 or YYYY-MM-DD)")
			return
		}
		from = t
	}
	if !from.Before(to) {
		writeError(w, http.StatusBadRequest, "from must be before to")
		return
	}
	if to.Sub(from) > usageMaxWindow {
		from = to.Add(-usageMaxWindow)
	}
	prevFrom := from.Add(-to.Sub(from))

	ctx := r.Context()
	taskCur, err := h.storage.TaskUsageByUserDay(ctx, from, to)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to aggregate task usage")
		return
	}
	taskPrev, err := h.storage.TaskUsageByUserDay(ctx, prevFrom, from)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to aggregate task usage")
		return
	}
	sources := []string{"tasks"}

	var chatCur, chatPrev []models.UserDayUsage
	if h.chatUserDayUsage != nil {
		if chatCur, err = h.chatUserDayUsage(ctx, from, to); err != nil {
			writeError(w, http.StatusInternalServerError, "Failed to aggregate chat usage")
			return
		}
		if chatPrev, err = h.chatUserDayUsage(ctx, prevFrom, from); err != nil {
			writeError(w, http.StatusInternalServerError, "Failed to aggregate chat usage")
			return
		}
		sources = append(sources, "chat")
	}

	var seats []models.AdoptionSeat
	if h.chatAccounts != nil {
		// A roster failure is a 500, not a silently absent denominator — an
		// exec reading "12 active of 0 seats" would draw the wrong conclusion.
		if seats, err = h.chatAccounts(ctx); err != nil {
			writeError(w, http.StatusInternalServerError, "Failed to list accounts")
			return
		}
		sources = append(sources, "accounts")
	}

	out := buildAdoptionReport(from, to, prevFrom, taskCur, chatCur, taskPrev, chatPrev, seats, sources)

	switch strings.TrimSpace(strings.ToLower(r.URL.Query().Get("format"))) {
	case "", "json":
		writeJSON(w, http.StatusOK, out)
	case "csv":
		writeAdoptionCSV(w, out)
	default:
		writeError(w, http.StatusBadRequest, "Invalid format (want json|csv)")
	}
}

// adoptionDayAxis returns every UTC day (YYYY-MM-DD) whose day-start falls in
// [day-start-of-from, to) — the shared x-axis every per-user series and the
// daily trend are index-aligned to. Bounded by usageMaxWindow upstream.
func adoptionDayAxis(from, to time.Time) []string {
	f := from.UTC()
	days := []string{}
	for d := time.Date(f.Year(), f.Month(), f.Day(), 0, 0, 0, 0, time.UTC); d.Before(to); d = d.AddDate(0, 0, 1) {
		days = append(days, d.Format("2006-01-02"))
	}
	return days
}

// adoptionAccum threads one user's in-progress row with the per-day activity
// flags that become ActiveDays/LastActive (and the daily active-user counts).
type adoptionAccum struct {
	row    *models.AdoptionUser
	active []bool
}

// buildAdoptionReport merges the four per-user-per-day row sets (current +
// previous window, task + chat meter) and the seat roster into the wire
// report. Pure so the merge/trend/roster logic is unit-testable without HTTP.
func buildAdoptionReport(
	from, to, prevFrom time.Time,
	taskCur, chatCur, taskPrev, chatPrev []models.UserDayUsage,
	seats []models.AdoptionSeat,
	sources []string,
) *models.AdoptionReport {
	days := adoptionDayAxis(from, to)
	dayIdx := make(map[string]int, len(days))
	for i, d := range days {
		dayIdx[d] = i
	}

	users := map[string]*adoptionAccum{}
	get := func(user string) *adoptionAccum {
		if a, ok := users[user]; ok {
			return a
		}
		a := &adoptionAccum{
			row:    &models.AdoptionUser{User: user, DailyTokens: make([]int64, len(days))},
			active: make([]bool, len(days)),
		}
		users[user] = a
		return a
	}
	add := func(rows []models.UserDayUsage, task bool) {
		for _, r := range rows {
			idx, ok := dayIdx[r.Day]
			if !ok {
				continue // defensive: a row outside the axis can't misalign a series
			}
			a := get(r.User)
			a.row.PromptTokens += r.PromptTokens
			a.row.CompletionTokens += r.CompletionTokens
			a.row.DailyTokens[idx] += r.PromptTokens + r.CompletionTokens
			a.active[idx] = true
			if task {
				a.row.TaskCostUSD += r.CostUSD
				a.row.TaskIterations += r.Units
			} else {
				a.row.ChatCostUSD += r.CostUSD
				a.row.ChatTurns += r.Units
				a.row.CachedTokens += r.CachedTokens
			}
		}
	}
	add(taskCur, true)
	add(chatCur, false)

	// Previous-window comparators: totals only — the trend delta needs no
	// daily resolution. The set of non-empty previous users feeds
	// PrevActiveUsers/NewActiveUsers.
	type prevTotal struct {
		cost   float64
		tokens int64
	}
	prev := map[string]prevTotal{}
	for _, rows := range [][]models.UserDayUsage{taskPrev, chatPrev} {
		for _, r := range rows {
			p := prev[r.User]
			p.cost += r.CostUSD
			p.tokens += r.PromptTokens + r.CompletionTokens
			prev[r.User] = p
		}
	}

	totals := models.AdoptionTotals{RegisteredUsers: int64(len(seats))}
	daily := make([]models.AdoptionDay, len(days))
	for i, d := range days {
		daily[i].Day = d
	}
	rows := make([]models.AdoptionUser, 0, len(users))
	for user, a := range users {
		for i, on := range a.active {
			if !on {
				continue
			}
			a.row.ActiveDays++
			a.row.LastActive = days[i]
			daily[i].ActiveUsers++
		}
		a.row.CostUSD = a.row.TaskCostUSD + a.row.ChatCostUSD
		p := prev[user]
		a.row.PrevCostUSD = p.cost
		a.row.PrevTokens = p.tokens
		for i, tok := range a.row.DailyTokens {
			daily[i].Tokens += tok
		}
		totals.CostUSD += a.row.CostUSD
		totals.Tokens += a.row.PromptTokens + a.row.CompletionTokens
		totals.CachedTokens += a.row.CachedTokens
		totals.ChatTurns += a.row.ChatTurns
		totals.TaskIterations += a.row.TaskIterations
		// The empty user is unattributed spend (deleted-user task rows): it is
		// listed and totaled so no spend disappears, but it isn't a person, so
		// it never counts toward the adoption headcounts.
		if user != "" {
			totals.ActiveUsers++
			if _, was := prev[user]; !was {
				totals.NewActiveUsers++
			}
		}
		rows = append(rows, *a.row)
	}
	// Daily cost + actions come straight from the source rows (cheaper than
	// widening the accumulator, and cost has no per-user daily field to reuse).
	for _, r := range taskCur {
		if idx, ok := dayIdx[r.Day]; ok {
			daily[idx].CostUSD += r.CostUSD
			daily[idx].Actions += r.Units
		}
	}
	for _, r := range chatCur {
		if idx, ok := dayIdx[r.Day]; ok {
			daily[idx].CostUSD += r.CostUSD
			daily[idx].Actions += r.Units
		}
	}
	for user, p := range prev {
		if user != "" {
			totals.PrevActiveUsers++
		}
		totals.PrevCostUSD += p.cost
		totals.PrevTokens += p.tokens
	}

	// Adoption is read on token volume first: tokens are the
	// coverage-independent meter (#289 — unpriced runs cost $0 but still count
	// tokens), so unlike the usage report's cost-first ordering the leaderboard
	// sorts tokens, then cost, then user for determinism.
	sort.Slice(rows, func(i, j int) bool {
		ti := rows[i].PromptTokens + rows[i].CompletionTokens
		tj := rows[j].PromptTokens + rows[j].CompletionTokens
		if ti != tj {
			return ti > tj
		}
		if rows[i].CostUSD != rows[j].CostUSD {
			return rows[i].CostUSD > rows[j].CostUSD
		}
		return rows[i].User < rows[j].User
	})

	inactive := []models.AdoptionSeat{}
	for _, s := range seats {
		if _, ok := users[s.Email]; !ok {
			inactive = append(inactive, s)
		}
	}
	sort.Slice(inactive, func(i, j int) bool { return inactive[i].Email < inactive[j].Email })

	return &models.AdoptionReport{
		From:          from,
		To:            to,
		PrevFrom:      prevFrom,
		Days:          days,
		Users:         rows,
		InactiveUsers: inactive,
		Daily:         daily,
		Totals:        totals,
		Sources:       sources,
		Note:          adoptionNote,
	}
}

// writeAdoptionCSV streams the per-user leaderboard as a CSV attachment — the
// exec view's Download CSV button. One row per user plus a trailing TOTAL row;
// the daily series stay JSON-only (a per-day matrix doesn't belong in a
// per-user CSV).
func writeAdoptionCSV(w http.ResponseWriter, out *models.AdoptionReport) {
	stamp := time.Now().UTC().Format("20060102")
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition",
		fmt.Sprintf("attachment; filename=%q", fmt.Sprintf("fleet-adoption-%s.csv", stamp)))
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{
		"user", "tokens", "prompt_tokens", "completion_tokens", "cached_tokens",
		"cost_usd", "task_cost_usd", "chat_cost_usd", "task_iterations", "chat_turns",
		"active_days", "last_active", "prev_tokens", "prev_cost_usd",
	})
	for _, u := range out.Users {
		_ = cw.Write([]string{
			u.User,
			strconv.FormatInt(u.PromptTokens+u.CompletionTokens, 10),
			strconv.FormatInt(u.PromptTokens, 10),
			strconv.FormatInt(u.CompletionTokens, 10),
			strconv.FormatInt(u.CachedTokens, 10),
			strconv.FormatFloat(u.CostUSD, 'f', 4, 64),
			strconv.FormatFloat(u.TaskCostUSD, 'f', 4, 64),
			strconv.FormatFloat(u.ChatCostUSD, 'f', 4, 64),
			strconv.FormatInt(u.TaskIterations, 10),
			strconv.FormatInt(u.ChatTurns, 10),
			strconv.FormatInt(u.ActiveDays, 10),
			u.LastActive,
			strconv.FormatInt(u.PrevTokens, 10),
			strconv.FormatFloat(u.PrevCostUSD, 'f', 4, 64),
		})
	}
	t := out.Totals
	_ = cw.Write([]string{
		"TOTAL",
		strconv.FormatInt(t.Tokens, 10), "", "",
		strconv.FormatInt(t.CachedTokens, 10),
		strconv.FormatFloat(t.CostUSD, 'f', 4, 64), "", "",
		strconv.FormatInt(t.TaskIterations, 10),
		strconv.FormatInt(t.ChatTurns, 10),
		"", "",
		strconv.FormatInt(t.PrevTokens, 10),
		strconv.FormatFloat(t.PrevCostUSD, 'f', 4, 64),
	})
	cw.Flush()
}
