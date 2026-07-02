package handlers

// GET /admin/usage (#601 part 1): the usage-analytics read model. Rolls the
// ALREADY-persisted metering up by principal/project/model/time-bucket over a
// requested window — task-side from task_iterations ⋈ tasks (sched DB),
// chat-side from turn_metrics via the ChatUsageProvider seam (chat store DB).
// The two databases are separate, so the merge happens here, keyed on the
// bucket key, with per-source subtotals kept so the report stays honest about
// where each number came from. No new accounting path is introduced anywhere
// in this file — it only reads what the governed core recorded.

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// usageGroupByValues is the closed set GET /admin/usage accepts. It doubles as
// the validation gate: group_by never reaches SQL as raw caller input (the DB
// layers each map these to fixed expressions).
var usageGroupByValues = map[string]bool{
	"user": true, "key": true, "project": true, "model": true, "day": true, "week": true,
}

// usageMaxWindow caps the report window so an arbitrary from= can't turn the
// aggregation into an unbounded table scan. One year + a leap day, mirroring
// the SLA report's clamp-don't-error convention.
const usageMaxWindow = 366 * 24 * time.Hour

// usagePricingNote is the honest-scope caveat (#289) every response carries:
// dollar coverage depends on pricing configuration, token totals do not.
const usagePricingNote = "Dollar figures cover only runs with model pricing available; " +
	"native-provider runs accrue $0 cost unless a pricing override is configured (#289). " +
	"Token totals are complete regardless of pricing configuration."

// ChatUsageProvider aggregates the chat store's turn_metrics over [from, to)
// for one group_by value. Injected by cmd/fleet via SetChatUsageProvider —
// the chat store is a separate database, so handlers reach it through this
// seam rather than a second DSN. nil = chat metering unavailable (the report
// says so via its sources list instead of silently under-counting).
type ChatUsageProvider func(ctx context.Context, from, to time.Time, groupBy string) ([]models.UsageBucket, error)

// SetChatUsageProvider wires the chat-side usage aggregation (see
// ChatUsageProvider). Call before serving traffic.
func (h *Handlers) SetChatUsageProvider(fn ChatUsageProvider) {
	h.chatUsage = fn
}

// parseUsageTime accepts RFC 3339 ("2026-07-01T00:00:00Z") or a bare date
// ("2026-07-01", read as midnight UTC) for the from=/to= query params.
func parseUsageTime(raw string) (time.Time, bool) {
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t.UTC(), true
	}
	if t, err := time.Parse("2006-01-02", raw); err == nil {
		return t.UTC(), true
	}
	return time.Time{}, false
}

// GetUsageReport handles GET /admin/usage?group_by=&from=&to= (#601 part 1).
//
// Admin-only, but reachable from the web UI: registered behind
// AdminOrUserAuthMiddleware and gated HERE on PermissionAdmin, exactly like
// GET /sla-report (#458) — the Next proxy can never send the admin X-API-Key,
// so the bare admin-key middleware would make the panel unreachable for every
// dashboard admin. The report is GLOBAL (all principals' spend), so exposing
// it to a non-admin member would leak other users' activity — hence 403 for
// anyone without the admin permission.
//
// group_by defaults to "user"; from/to default to the trailing 30 days and
// the window is clamped to usageMaxWindow. to is exclusive.
func (h *Handlers) GetUsageReport(w http.ResponseWriter, r *http.Request) {
	if !h.principalFromRequest(r).hasPermission(models.PermissionAdmin) {
		writeError(w, http.StatusForbidden, "Admin access required")
		return
	}

	groupBy := strings.TrimSpace(r.URL.Query().Get("group_by"))
	if groupBy == "" {
		groupBy = "user"
	}
	if !usageGroupByValues[groupBy] {
		writeError(w, http.StatusBadRequest, "Invalid group_by (want user|key|project|model|day|week)")
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

	ctx := r.Context()
	taskRows, err := h.storage.TaskUsage(ctx, from, to, groupBy)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "Failed to aggregate task usage")
		return
	}
	sources := []string{"tasks"}
	var chatRows []models.UsageBucket
	if h.chatUsage != nil {
		chatRows, err = h.chatUsage(ctx, from, to, groupBy)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "Failed to aggregate chat usage")
			return
		}
		sources = append(sources, "chat")
	}

	report := mergeUsage(taskRows, chatRows)
	sortUsageBuckets(report, groupBy)

	out := &models.UsageReport{
		GroupBy: groupBy,
		From:    from,
		To:      to,
		Buckets: report,
		Totals:  usageTotals(report),
		Sources: sources,
		Note:    usagePricingNote,
	}
	writeJSON(w, http.StatusOK, out)
}

// mergeUsage folds the two meters' rows into one bucket list keyed on the
// group value. Chat rows carry their spend in CostUSD/ChatCostUSD and the
// chat-only CachedTokens; task rows carry TaskCostUSD/TaskIterations. Neither
// side's subtotal is ever recomputed from the other — they are summed as
// recorded.
func mergeUsage(taskRows, chatRows []models.UsageBucket) []models.UsageBucket {
	byKey := map[string]*models.UsageBucket{}
	order := []string{}
	get := func(key string) *models.UsageBucket {
		if b, ok := byKey[key]; ok {
			return b
		}
		b := &models.UsageBucket{Key: key}
		byKey[key] = b
		order = append(order, key)
		return b
	}
	for _, r := range taskRows {
		b := get(r.Key)
		b.TaskCostUSD += r.TaskCostUSD
		b.TaskIterations += r.TaskIterations
		b.CostUSD += r.TaskCostUSD
		b.PromptTokens += r.PromptTokens
		b.CompletionTokens += r.CompletionTokens
	}
	for _, r := range chatRows {
		b := get(r.Key)
		b.ChatCostUSD += r.ChatCostUSD
		b.ChatTurns += r.ChatTurns
		b.CostUSD += r.ChatCostUSD
		b.PromptTokens += r.PromptTokens
		b.CompletionTokens += r.CompletionTokens
		b.CachedTokens += r.CachedTokens
		if b.Label == "" {
			b.Label = r.Label
		}
	}
	out := make([]models.UsageBucket, 0, len(order))
	for _, key := range order {
		out = append(out, *byKey[key])
	}
	return out
}

// sortUsageBuckets orders time buckets chronologically (their key is the
// YYYY-MM-DD bucket start, so a string sort is a date sort) and every other
// grouping by spend — cost first, then tokens (the coverage-independent
// signal when pricing isn't configured, #289), then key for determinism.
func sortUsageBuckets(buckets []models.UsageBucket, groupBy string) {
	if groupBy == "day" || groupBy == "week" {
		sort.Slice(buckets, func(i, j int) bool { return buckets[i].Key < buckets[j].Key })
		return
	}
	sort.Slice(buckets, func(i, j int) bool {
		if buckets[i].CostUSD != buckets[j].CostUSD {
			return buckets[i].CostUSD > buckets[j].CostUSD
		}
		ti := buckets[i].PromptTokens + buckets[i].CompletionTokens
		tj := buckets[j].PromptTokens + buckets[j].CompletionTokens
		if ti != tj {
			return ti > tj
		}
		return buckets[i].Key < buckets[j].Key
	})
}

// usageTotals sums the bucket list into the report-level roll-up.
func usageTotals(buckets []models.UsageBucket) models.UsageBucket {
	var t models.UsageBucket
	for _, b := range buckets {
		t.CostUSD += b.CostUSD
		t.PromptTokens += b.PromptTokens
		t.CompletionTokens += b.CompletionTokens
		t.CachedTokens += b.CachedTokens
		t.TaskCostUSD += b.TaskCostUSD
		t.ChatCostUSD += b.ChatCostUSD
		t.TaskIterations += b.TaskIterations
		t.ChatTurns += b.ChatTurns
	}
	return t
}
