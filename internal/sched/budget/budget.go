// Copyright (c) 2025 ElcanoTek
// SPDX-License-Identifier: MIT

// Package budget enforces per-principal rolling budgets (#601 part 2).
//
// A budget ({scope: user|key|project, principal, window: day|week|month,
// soft/hard bounds in dollars AND tokens}) is evaluated at TASK-CREATE time by
// the ONE Enforcer every create path shares — POST /tasks, POST /tasks/batch,
// and the chat schedule_task approval seam — mirroring the priorityCapError
// shared-helper discipline so the paths cannot drift.
//
// Governance invariants this package preserves:
//
//   - No second accounting path. The principal's current-window spend is
//     recomputed from the metering the governed core already persists — the
//     #601 part-1 usage read model (task_iterations ⋈ tasks via Store.TaskUsage,
//     chat turn_metrics via the ChatUsage seam). The budgets table stores only
//     configuration + the soft-alert dedup marker.
//   - Budgets only NARROW. The gate can only refuse work the global ceilings
//     would have allowed, never permit more: a budget's hard bounds are clamped
//     at check time against the LIVE global ceilings (Ceilings reads
//     FLEET_MAX_COST_USD / FLEET_MAX_TOTAL_TOKENS through the #286 hot-reload
//     accessors), so a budget row can never be more permissive than the
//     box-wide ceiling. The per-run enforcement inside agentcore is untouched.
//   - Nil/absent budget = today's behavior. With no budget rows matching a
//     create's principals the gate returns nil after one indexed SELECT; no
//     usage aggregation runs and nothing is refused.
//   - Exactly one soft alert per window crossing. The crossing marker is
//     persisted (budgets.soft_alert_window_start) and claimed with a
//     conditional UPDATE, so concurrent creates race to one alert and a
//     process restart cannot re-alert.
package budget

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/notify"
	"github.com/ElcanoTek/fleet/internal/safe"
	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// Store is the persistence surface the Enforcer needs, satisfied by
// *storage.Storage. Kept an interface so the enforcement logic is testable
// against a fake and the package stays free of a storage import.
type Store interface {
	// BudgetsFor returns the budgets matching a create's principals; empty
	// arguments match nothing.
	BudgetsFor(ctx context.Context, user, key, project string) ([]models.Budget, error)
	// ListBudgets returns every configured budget (for GET /admin/budgets).
	ListBudgets(ctx context.Context) ([]models.Budget, error)
	// TaskUsage is the part-1 aggregation over task_iterations ⋈ tasks.
	TaskUsage(ctx context.Context, from, to time.Time, groupBy string) ([]models.UsageBucket, error)
	// MarkBudgetSoftAlert claims the once-per-window soft alert.
	MarkBudgetSoftAlert(ctx context.Context, id uuid.UUID, windowStart time.Time) (bool, error)
}

// ChatUsage aggregates the chat store's turn_metrics over [from, to) for one
// group_by value — the same seam shape as handlers.ChatUsageProvider, injected
// by cmd/fleet because the chat store is a separate database. nil = chat
// metering unavailable; budgets then bound task-side spend only (and the docs
// say so).
type ChatUsage func(ctx context.Context, from, to time.Time, groupBy string) ([]models.UsageBucket, error)

// Notifier is the alert seam (email/webhook/Web Push fan-out today), satisfied
// by *notify.Notifier. A nil Notifier still claims the crossing marker — the
// "one alert per window" state must not depend on delivery being configured.
type Notifier interface {
	Notify(ctx context.Context, ev notify.Event) error
}

// Config wires an Enforcer.
type Config struct {
	Store     Store
	ChatUsage ChatUsage
	Notifier  Notifier
	// Ceilings returns the LIVE global ceilings (FLEET_MAX_COST_USD,
	// FLEET_MAX_TOTAL_TOKENS) — cmd/fleet passes the #286 hot-reload accessors
	// so a reload is honored on the very next check. 0 = that ceiling is
	// disabled (no clamp). nil Ceilings = no clamp at all.
	Ceilings func() (maxCostUSD float64, maxTotalTokens int)
	// Now overrides the clock (tests: window rollover). nil = time.Now.
	Now func() time.Time
	// SyncNotify makes the soft alert fire synchronously inside CheckCreate
	// instead of from a detached goroutine. For deterministic tests only; the
	// production default keeps SMTP latency out of the create request.
	SyncNotify bool
}

// Enforcer is the shared budget gate. Safe for concurrent use: all mutable
// state (the alert marker) lives in the database.
type Enforcer struct {
	cfg Config
}

// New builds the Enforcer. Store must be non-nil; the other seams degrade as
// documented on Config.
func New(cfg Config) *Enforcer {
	return &Enforcer{cfg: cfg}
}

// ExceededError is the hard-refusal: the principal's current-window spend has
// reached the budget's (globally-clamped) hard bound. Create paths map it to
// HTTP 402 with Error() as the body and WindowEnd driving Retry-After.
type ExceededError struct {
	Budget      models.Budget
	SpendUSD    float64
	SpendTokens int64
	// LimitUSD/LimitTokens are the EFFECTIVE bounds that refused (after the
	// global-ceiling clamp); nil = that measure did not refuse.
	LimitUSD    *float64
	LimitTokens *int64
	// WindowEnd is when the window rolls over and new work is accepted again.
	WindowEnd time.Time
}

func (e *ExceededError) Error() string {
	var reasons []string
	if e.LimitUSD != nil {
		reasons = append(reasons, fmt.Sprintf("spent $%.4f of the $%.2f hard cap", e.SpendUSD, *e.LimitUSD))
	}
	if e.LimitTokens != nil {
		reasons = append(reasons, fmt.Sprintf("used %d of the %d-token hard cap", e.SpendTokens, *e.LimitTokens))
	}
	return fmt.Sprintf("budget exceeded for %s %q (%s window): %s; new work is refused until %s",
		e.Budget.Scope, e.Budget.PrincipalID, e.Budget.Window,
		strings.Join(reasons, "; "), e.WindowEnd.UTC().Format(time.RFC3339))
}

// windowStart returns the UTC start of the calendar window containing now.
// Weeks start Monday 00:00 UTC, matching date_trunc('week') in the part-1
// usage queries, so enforcement and the report agree on boundaries.
func windowStart(now time.Time, window string) time.Time {
	n := now.UTC()
	day := time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.UTC)
	switch window {
	case models.BudgetWindowWeek:
		return day.AddDate(0, 0, -((int(day.Weekday()) + 6) % 7))
	case models.BudgetWindowMonth:
		return time.Date(n.Year(), n.Month(), 1, 0, 0, 0, 0, time.UTC)
	default: // models.BudgetWindowDay — the DB CHECK constrains the set.
		return day
	}
}

// windowEnd returns the exclusive end of the window starting at start.
func windowEnd(start time.Time, window string) time.Time {
	switch window {
	case models.BudgetWindowWeek:
		return start.AddDate(0, 0, 7)
	case models.BudgetWindowMonth:
		return start.AddDate(0, 1, 0)
	default:
		return start.AddDate(0, 0, 1)
	}
}

// groupByForScope maps a budget scope to the part-1 usage group_by whose bucket
// keys carry that principal id.
func groupByForScope(scope string) string {
	switch scope {
	case models.BudgetScopeKey:
		return "key"
	case models.BudgetScopeProject:
		return "project"
	default:
		return "user"
	}
}

func (e *Enforcer) now() time.Time {
	if e.cfg.Now != nil {
		return e.cfg.Now()
	}
	return time.Now()
}

// spendCache memoizes one CheckCreate/Statuses call's usage aggregations by
// (group_by, window start): several budgets for the same principal set often
// share a window, and each aggregation is two queries (tasks + chat).
type spendCache map[string]map[string][2]float64 // cacheKey → bucketKey → {usd, tokens}

// spendFor sums the principal's current-window spend from BOTH persisted
// meters: dollars and tokens (prompt + completion — the coverage-independent
// measure, since native-provider runs accrue $0 without a pricing override).
func (e *Enforcer) spendFor(ctx context.Context, cache spendCache, groupBy string, ws, we time.Time, principalID string) (float64, int64, error) {
	// The end is part of the key: a day and a week window can share a start
	// (any Monday) while aggregating different ranges.
	key := groupBy + "|" + ws.Format(time.RFC3339) + "|" + we.Format(time.RFC3339)
	buckets, ok := cache[key]
	if !ok {
		buckets = map[string][2]float64{}
		taskRows, err := e.cfg.Store.TaskUsage(ctx, ws, we, groupBy)
		if err != nil {
			return 0, 0, fmt.Errorf("aggregate task usage: %w", err)
		}
		var chatRows []models.UsageBucket
		if e.cfg.ChatUsage != nil {
			chatRows, err = e.cfg.ChatUsage(ctx, ws, we, groupBy)
			if err != nil {
				return 0, 0, fmt.Errorf("aggregate chat usage: %w", err)
			}
		}
		for _, r := range append(taskRows, chatRows...) {
			agg := buckets[r.Key]
			// Task rows carry TaskCostUSD, chat rows ChatCostUSD; summing both
			// fields per row is source-agnostic and never double-counts.
			agg[0] += r.TaskCostUSD + r.ChatCostUSD
			agg[1] += float64(r.PromptTokens + r.CompletionTokens)
			buckets[r.Key] = agg
		}
		cache[key] = buckets
	}
	agg := buckets[principalID]
	return agg[0], int64(agg[1]), nil
}

// effectiveHard clamps a budget's hard bounds against the live global ceilings:
// min(budget, ceiling) when both are set, so a budget can never authorize more
// than the box-wide ceiling permits (fail-safe — the clamp only narrows).
// Soft bounds are NOT clamped: they only time an alert, never permit work.
func (e *Enforcer) effectiveHard(b models.Budget) (usd *float64, tokens *int64) {
	usd, tokens = b.HardUSD, b.HardTokens
	if e.cfg.Ceilings == nil {
		return usd, tokens
	}
	maxCost, maxTokens := e.cfg.Ceilings()
	if usd != nil && maxCost > 0 && maxCost < *usd {
		usd = &maxCost
	}
	if tokens != nil && maxTokens > 0 && int64(maxTokens) < *tokens {
		mt := int64(maxTokens)
		tokens = &mt
	}
	return usd, tokens
}

// CheckCreate is the shared task-create gate. It looks up every budget matching
// the create's principals, recomputes each budget's current-window spend from
// the persisted metering, fires the once-per-window soft alert on a crossing,
// and returns *ExceededError when a hard bound (clamped to the live global
// ceilings) is already reached. A nil return admits the create; any other
// non-ExceededError error is an infrastructure failure the caller should
// surface as a 500 (fail closed: an unverifiable budget does not admit work).
func (e *Enforcer) CheckCreate(ctx context.Context, p models.BudgetPrincipals) error {
	if e == nil {
		return nil
	}
	user := strings.TrimSpace(p.User)
	key := strings.TrimSpace(p.Key)
	project := strings.TrimSpace(p.Project)
	if user == "" && key == "" && project == "" {
		return nil
	}
	budgets, err := e.cfg.Store.BudgetsFor(ctx, user, key, project)
	if err != nil {
		return fmt.Errorf("look up budgets: %w", err)
	}
	if len(budgets) == 0 {
		return nil // no budget configured — today's behavior, no aggregation runs
	}
	now := e.now()
	cache := spendCache{}
	for _, b := range budgets {
		ws := windowStart(now, b.Window)
		we := windowEnd(ws, b.Window)
		spendUSD, spendTokens, err := e.spendFor(ctx, cache, groupByForScope(b.Scope), ws, we, b.PrincipalID)
		if err != nil {
			return err
		}
		e.maybeSoftAlert(ctx, b, ws, spendUSD, spendTokens)
		hardUSD, hardTokens := e.effectiveHard(b)
		exceeded := &ExceededError{Budget: b, SpendUSD: spendUSD, SpendTokens: spendTokens, WindowEnd: we}
		if hardUSD != nil && spendUSD >= *hardUSD {
			exceeded.LimitUSD = hardUSD
		}
		if hardTokens != nil && spendTokens >= *hardTokens {
			exceeded.LimitTokens = hardTokens
		}
		if exceeded.LimitUSD != nil || exceeded.LimitTokens != nil {
			return exceeded
		}
	}
	return nil
}

// maybeSoftAlert fires the once-per-window soft alert when a soft bound is
// crossed. The persisted marker is claimed FIRST (conditional UPDATE — see
// db.MarkBudgetSoftAlert), then delivery fires; alerting is best-effort and
// must never block or fail a create, so claim/delivery errors are logged only.
func (e *Enforcer) maybeSoftAlert(ctx context.Context, b models.Budget, ws time.Time, spendUSD float64, spendTokens int64) {
	crossedUSD := b.SoftUSD != nil && spendUSD >= *b.SoftUSD
	crossedTokens := b.SoftTokens != nil && spendTokens >= *b.SoftTokens
	if !crossedUSD && !crossedTokens {
		return
	}
	if b.SoftAlertWindowStart != nil && b.SoftAlertWindowStart.Equal(ws) {
		return // already alerted for this window (fast path off the loaded row)
	}
	won, err := e.cfg.Store.MarkBudgetSoftAlert(ctx, b.ID, ws)
	if err != nil {
		log.Printf("budget: failed to claim soft alert for budget %s: %v", b.ID, err)
		return
	}
	if !won || e.cfg.Notifier == nil {
		return
	}
	ev := notify.Event{
		TaskID:  b.ID.String(),
		Name:    fmt.Sprintf("Budget alert: %s %s (%s window)", b.Scope, b.PrincipalID, b.Window),
		Status:  notify.StatusProgress,
		CostUSD: fmt.Sprintf("%.4f", spendUSD),
		Message: softAlertMessage(b, spendUSD, spendTokens),
	}
	if b.Scope == models.BudgetScopeUser {
		// The user principal is the sched username — an email on a standard
		// deployment — so the per-user Web Push channel can route to them. The
		// deployment-wide email/webhook channels ignore Audience entirely.
		ev.Audience = b.PrincipalID
	}
	if e.cfg.SyncNotify {
		if err := e.cfg.Notifier.Notify(ctx, ev); err != nil {
			log.Printf("budget: soft alert delivery for budget %s: %v", b.ID, err)
		}
		return
	}
	notifier := e.cfg.Notifier
	safe.Go("budget.soft-alert", func() {
		nctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if err := notifier.Notify(nctx, ev); err != nil {
			log.Printf("budget: soft alert delivery for budget %s: %v", b.ID, err)
		}
	})
}

// softAlertMessage renders the human line for the soft-crossing notification.
// It reports both measures — dollars depend on pricing coverage (#289), tokens
// do not — plus what happens at the hard bound.
func softAlertMessage(b models.Budget, spendUSD float64, spendTokens int64) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "Spend for %s %q in the current %s window has crossed its soft budget: $%.4f",
		b.Scope, b.PrincipalID, b.Window, spendUSD)
	if b.SoftUSD != nil {
		fmt.Fprintf(&sb, " (soft $%.2f)", *b.SoftUSD)
	}
	fmt.Fprintf(&sb, ", %d tokens", spendTokens)
	if b.SoftTokens != nil {
		fmt.Fprintf(&sb, " (soft %d)", *b.SoftTokens)
	}
	sb.WriteString(".")
	if b.HardUSD != nil || b.HardTokens != nil {
		sb.WriteString(" New task creation will be refused at the hard bound")
		if b.HardUSD != nil {
			fmt.Fprintf(&sb, " $%.2f", *b.HardUSD)
		}
		if b.HardTokens != nil {
			fmt.Fprintf(&sb, " %d tokens", *b.HardTokens)
		}
		sb.WriteString(".")
	}
	return sb.String()
}

// Statuses evaluates every configured budget for GET /admin/budgets: current
// window bounds, live spend from the persisted metering, the effective
// (globally-clamped) hard bounds, and whether this window's soft alert fired.
// Read-only — it never claims the alert marker.
func (e *Enforcer) Statuses(ctx context.Context) ([]models.BudgetStatus, error) {
	budgets, err := e.cfg.Store.ListBudgets(ctx)
	if err != nil {
		return nil, fmt.Errorf("list budgets: %w", err)
	}
	now := e.now()
	cache := spendCache{}
	out := make([]models.BudgetStatus, 0, len(budgets))
	for _, b := range budgets {
		ws := windowStart(now, b.Window)
		we := windowEnd(ws, b.Window)
		spendUSD, spendTokens, err := e.spendFor(ctx, cache, groupByForScope(b.Scope), ws, we, b.PrincipalID)
		if err != nil {
			return nil, err
		}
		hardUSD, hardTokens := e.effectiveHard(b)
		out = append(out, models.BudgetStatus{
			Budget:              b,
			WindowStart:         ws,
			WindowEnd:           we,
			SpendUSD:            spendUSD,
			SpendTokens:         spendTokens,
			EffectiveHardUSD:    hardUSD,
			EffectiveHardTokens: hardTokens,
			SoftAlerted:         b.SoftAlertWindowStart != nil && b.SoftAlertWindowStart.Equal(ws),
		})
	}
	return out, nil
}
