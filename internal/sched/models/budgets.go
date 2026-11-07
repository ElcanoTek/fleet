package models

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Budget scopes (#601 part 2): WHO a budget bounds. The closed set doubles as
// the validation gate — an unknown scope is rejected before it reaches SQL.
const (
	// BudgetScopeUser bounds a user principal, keyed by the sched username
	// (an email on a standard deployment, matching the usage report's
	// group_by=user bucket key on both meters).
	BudgetScopeUser = "user"
	// BudgetScopeKey bounds a scoped API key, keyed by the key id
	// (tasks.created_by_key_id).
	BudgetScopeKey = "key"
)

// Budget windows (#601 part 2): the UTC calendar window spend is summed over.
// All windows are calendar-aligned (a "day" is the UTC day, a "week" starts
// Monday 00:00 UTC — matching date_trunc('week') in the part-1 usage queries —
// and a "month" is the UTC calendar month), so the enforcement window and the
// usage report's time buckets agree on boundaries.
const (
	BudgetWindowDay   = "day"
	BudgetWindowWeek  = "week"
	BudgetWindowMonth = "month"
)

// budgetScopes / budgetWindows are the closed validation sets for BudgetCreate.
var (
	budgetScopes  = map[string]bool{BudgetScopeUser: true, BudgetScopeKey: true}
	budgetWindows = map[string]bool{BudgetWindowDay: true, BudgetWindowWeek: true, BudgetWindowMonth: true}
)

// Budget is one per-principal rolling budget row (#601 part 2): soft/hard
// bounds in dollars AND tokens over a calendar window for one {scope,
// principal}. Nil bounds are unconfigured. Spend is never accumulated on the
// row — enforcement recomputes the current-window spend from the persisted
// metering (the part-1 usage read model) at each task-create, so this can
// never drift into a second accounting path. SoftAlertWindowStart is the
// persisted dedup marker: the window start the one soft alert was fired for.
type Budget struct {
	ID          uuid.UUID `json:"id"`
	Scope       string    `json:"scope"`
	PrincipalID string    `json:"principal_id"`
	Window      string    `json:"window"`
	SoftUSD     *float64  `json:"soft_usd,omitempty"`
	HardUSD     *float64  `json:"hard_usd,omitempty"`
	SoftTokens  *int64    `json:"soft_tokens,omitempty"`
	HardTokens  *int64    `json:"hard_tokens,omitempty"`
	// SoftAlertWindowStart/SoftAlertAt record the once-per-window soft alert:
	// the window it fired for and when. Cleared on upsert so editing a budget
	// re-arms its alert for the current window.
	SoftAlertWindowStart *time.Time `json:"soft_alert_window_start,omitempty"`
	SoftAlertAt          *time.Time `json:"soft_alert_at,omitempty"`
	CreatedAt            time.Time  `json:"created_at"`
	UpdatedAt            time.Time  `json:"updated_at"`
}

// BudgetCreate is the POST /admin/budgets body (#601 part 2). It upserts on
// (scope, principal_id, window) — one budget per principal per window kind.
type BudgetCreate struct {
	Scope       string   `json:"scope"`
	PrincipalID string   `json:"principal_id"`
	Window      string   `json:"window"`
	SoftUSD     *float64 `json:"soft_usd,omitempty"`
	HardUSD     *float64 `json:"hard_usd,omitempty"`
	SoftTokens  *int64   `json:"soft_tokens,omitempty"`
	HardTokens  *int64   `json:"hard_tokens,omitempty"`
}

// maxBudgetPrincipalIDLen bounds the principal id (an email, key id, or project
// id — none legitimately longer than this) so a runaway body can't bloat the row.
const maxBudgetPrincipalIDLen = 320

// Validate rejects a malformed budget: unknown scope/window, empty principal,
// negative bounds, no bounds at all, or a soft bound above its hard bound (an
// alert threshold you can never reach before refusal is a misconfiguration).
func (bc *BudgetCreate) Validate() error {
	if !budgetScopes[bc.Scope] {
		return fmt.Errorf("scope must be one of user|key (got %q)", bc.Scope)
	}
	if !budgetWindows[bc.Window] {
		return fmt.Errorf("window must be one of day|week|month (got %q)", bc.Window)
	}
	bc.PrincipalID = strings.TrimSpace(bc.PrincipalID)
	if bc.PrincipalID == "" {
		return fmt.Errorf("principal_id is required")
	}
	if len(bc.PrincipalID) > maxBudgetPrincipalIDLen {
		return fmt.Errorf("principal_id exceeds %d characters", maxBudgetPrincipalIDLen)
	}
	for name, v := range map[string]*float64{"soft_usd": bc.SoftUSD, "hard_usd": bc.HardUSD} {
		if v != nil && (*v < 0 || math.IsNaN(*v) || math.IsInf(*v, 0)) {
			return fmt.Errorf("%s must be a non-negative finite number", name)
		}
	}
	for name, v := range map[string]*int64{"soft_tokens": bc.SoftTokens, "hard_tokens": bc.HardTokens} {
		if v != nil && *v < 0 {
			return fmt.Errorf("%s must be non-negative", name)
		}
	}
	if bc.SoftUSD == nil && bc.HardUSD == nil && bc.SoftTokens == nil && bc.HardTokens == nil {
		return fmt.Errorf("at least one of soft_usd, hard_usd, soft_tokens, hard_tokens is required")
	}
	if bc.SoftUSD != nil && bc.HardUSD != nil && *bc.SoftUSD > *bc.HardUSD {
		return fmt.Errorf("soft_usd must not exceed hard_usd")
	}
	if bc.SoftTokens != nil && bc.HardTokens != nil && *bc.SoftTokens > *bc.HardTokens {
		return fmt.Errorf("soft_tokens must not exceed hard_tokens")
	}
	return nil
}

// BudgetStatus is one row of GET /admin/budgets: the configured budget plus its
// live evaluation — the current window's [start, end), the spend recomputed
// from the persisted metering, the EFFECTIVE hard bounds after the fail-safe
// clamp against the live global ceilings (#286; a budget can only narrow, never
// exceed FLEET_MAX_COST_USD / FLEET_MAX_TOTAL_TOKENS), and whether the
// once-per-window soft alert has fired for the current window.
type BudgetStatus struct {
	Budget
	WindowStart         time.Time `json:"window_start"`
	WindowEnd           time.Time `json:"window_end"`
	SpendUSD            float64   `json:"spend_usd"`
	SpendTokens         int64     `json:"spend_tokens"`
	EffectiveHardUSD    *float64  `json:"effective_hard_usd,omitempty"`
	EffectiveHardTokens *int64    `json:"effective_hard_tokens,omitempty"`
	SoftAlerted         bool      `json:"soft_alerted"`
}

// BudgetPrincipals names the principals one task-create carries, so the budget
// gate can look up every budget that applies. Empty fields = the create path
// has no such principal (e.g. an admin-key submission carries neither a user
// nor a scoped key and is therefore not budget-gated — the admin key is the
// box operator, not a meterable principal).
type BudgetPrincipals struct {
	User string
	Key  string
}
