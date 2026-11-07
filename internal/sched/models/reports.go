package models

import (
	"time"
)

// SLAReportTask is one row of the SLA report (#274): the actual-duration
// distribution (p50/p95) and breach rate for a (prompt, expected_duration)
// bucket over the report window. Tasks without an expected_duration_minutes
// are excluded; the bucket key is the task's prompt (fleet has no separate
// `name` column, so prompt is the closest stable grouping key — recurring
// tasks repeat the same prompt verbatim).
type SLAReportTask struct {
	TaskName          string  `json:"task_name"`
	ExpectedMinutes   int     `json:"expected_minutes"`
	P50ActualMinutes  float64 `json:"p50_actual_minutes"`
	P95ActualMinutes  float64 `json:"p95_actual_minutes"`
	BreachRatePercent float64 `json:"breach_rate_pct"`
	SampleCount       int     `json:"sample_count"`
}

// SLAReport is the GET /admin/sla-report response (#274): the per-bucket SLA
// actuals for the report window. Period is the human label ("last_7_days");
// WindowDays is the numeric bound (default 7, capped at 90).
type SLAReport struct {
	Period     string          `json:"period"`
	WindowDays int             `json:"window_days"`
	Tasks      []SLAReportTask `json:"tasks"`
}

// UsageBucket is one aggregated row of the usage report (#601 part 1): the
// cost/token roll-up for a single group_by key over the requested window.
// Key is the grouping value (a user email/username, an API key id, a project
// id, a model slug, or a YYYY-MM-DD bucket start for day/week grouping); the
// empty key collects rows without that dimension (e.g. tasks have no project,
// chat turns have no API key). Label is a human-friendly name when one exists
// (today: the project name for group_by=project).
//
// The per-source splits (Task*/Chat*) are kept alongside the combined totals
// on purpose: the two sources meter different surfaces (scheduled-task
// iterations vs interactive chat turns) with different dimensional coverage,
// so an honest UI can show where each number came from. CachedTokens is
// chat-only — task_iterations does not persist a cached-token count.
type UsageBucket struct {
	Key              string  `json:"key"`
	Label            string  `json:"label,omitempty"`
	CostUSD          float64 `json:"cost_usd"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	CachedTokens     int64   `json:"cached_tokens"`
	ChatPromptTokens int64   `json:"chat_prompt_tokens,omitempty"`
	UncachedTokens   int64   `json:"uncached_tokens"`
	CacheHitRatePct  float64 `json:"cache_hit_rate_pct"`
	TaskCostUSD      float64 `json:"task_cost_usd"`
	ChatCostUSD      float64 `json:"chat_cost_usd"`
	TaskIterations   int64   `json:"task_iterations"`
	ChatTurns        int64   `json:"chat_turns"`
}

// UsageReport is the GET /admin/usage response (#601 part 1): cost + token
// totals over [From, To) grouped by GroupBy (user|key|project|model|day|week),
// sourced from the already-persisted metering (task_iterations ⋈ tasks, and
// the chat turn_metrics log) — never a parallel accounting path. Sources lists
// which meters contributed ("tasks" always; "chat" only when the chat store is
// wired). Note carries the honest-scope caveat that dollar figures depend on
// pricing config (#289): native-provider runs accrue $0 unless a pricing
// override is set, so token totals are the coverage-independent signal.
type UsageReport struct {
	GroupBy string        `json:"group_by"`
	From    time.Time     `json:"from"`
	To      time.Time     `json:"to"`
	Buckets []UsageBucket `json:"buckets"`
	Totals  UsageBucket   `json:"totals"`
	Sources []string      `json:"sources"`
	Note    string        `json:"note"`
}

// UserDayUsage is one meter's per-user-per-day aggregation row, the raw
// material of the adoption report (GET /admin/usage/adoption). Both meters
// return this shape — task-side from task_iterations ⋈ tasks, chat-side
// through the ChatUserDayUsageProvider seam — and Units means "that meter's
// work unit" (task iterations or chat turns respectively; the handler knows
// which source a slice came from). Never serialized: the wire types below
// carry the merged result.
type UserDayUsage struct {
	User             string
	Day              string // YYYY-MM-DD, UTC
	CostUSD          float64
	PromptTokens     int64
	CompletionTokens int64
	CachedTokens     int64 // chat only; task iterations persist no cached count
	Units            int64
}

// AdoptionUser is one user's row of the adoption report: their merged spend
// and activity over the window, the previous-window comparators, and a
// per-day token series aligned to AdoptionReport.Days (the sparkline). The
// empty User collects unattributed spend (deleted-user task rows) — it is
// listed so no spend disappears, but never counted as an active user.
type AdoptionUser struct {
	User             string  `json:"user"`
	CostUSD          float64 `json:"cost_usd"`
	TaskCostUSD      float64 `json:"task_cost_usd"`
	ChatCostUSD      float64 `json:"chat_cost_usd"`
	PromptTokens     int64   `json:"prompt_tokens"`
	CompletionTokens int64   `json:"completion_tokens"`
	CachedTokens     int64   `json:"cached_tokens"`
	TaskIterations   int64   `json:"task_iterations"`
	ChatTurns        int64   `json:"chat_turns"`
	// ActiveDays counts the distinct UTC days in the window with any metered
	// activity; LastActive is the latest such day (YYYY-MM-DD).
	ActiveDays int64  `json:"active_days"`
	LastActive string `json:"last_active,omitempty"`
	// PrevCostUSD/PrevTokens are this user's totals over the equal-length
	// window immediately before From — the trend comparators.
	PrevCostUSD float64 `json:"prev_cost_usd"`
	PrevTokens  int64   `json:"prev_tokens"`
	// DailyTokens is prompt+completion tokens per day, index-aligned to
	// AdoptionReport.Days.
	DailyTokens []int64 `json:"daily_tokens"`
}

// AdoptionSeat is one provisioned account in the adoption report's roster —
// the denominator side. Seats come from the chat store's user table via the
// ChatAccountsProvider seam; seats with no metered activity in the window are
// listed under InactiveUsers so "who hasn't adopted yet" is a first-class
// answer, not an inference.
type AdoptionSeat struct {
	Email     string    `json:"email"`
	CreatedAt time.Time `json:"created_at"`
}

// AdoptionDay is one day of the report-level activity trend: merged cost,
// prompt+completion tokens, work units (task iterations + chat turns), and
// how many distinct users were active that day.
type AdoptionDay struct {
	Day         string  `json:"day"`
	CostUSD     float64 `json:"cost_usd"`
	Tokens      int64   `json:"tokens"`
	Actions     int64   `json:"actions"`
	ActiveUsers int64   `json:"active_users"`
}

// AdoptionTotals is the adoption report's roll-up. RegisteredUsers is 0 when
// the accounts roster isn't wired — Sources says whether "accounts"
// contributed, so a 0 is distinguishable from "roster unavailable".
type AdoptionTotals struct {
	ActiveUsers     int64   `json:"active_users"`
	PrevActiveUsers int64   `json:"prev_active_users"`
	NewActiveUsers  int64   `json:"new_active_users"`
	RegisteredUsers int64   `json:"registered_users"`
	CostUSD         float64 `json:"cost_usd"`
	PrevCostUSD     float64 `json:"prev_cost_usd"`
	Tokens          int64   `json:"tokens"`
	PrevTokens      int64   `json:"prev_tokens"`
	CachedTokens    int64   `json:"cached_tokens"`
	ChatTurns       int64   `json:"chat_turns"`
	TaskIterations  int64   `json:"task_iterations"`
}

// AdoptionReport is the GET /admin/usage/adoption response: the per-user
// AI-adoption read model for the executive audit view. Like UsageReport it is
// strictly an aggregation of the already-persisted metering (task_iterations
// ⋈ tasks plus the chat turn_metrics log) — no new accounting path. Days is
// the full UTC day axis of [From, To) so per-user DailyTokens series and the
// Daily trend need no client-side date math; PrevFrom marks the start of the
// equal-length comparison window feeding the Prev* fields. Sources lists the
// meters that contributed ("tasks" always; "chat" and "accounts" when their
// seams are wired). Note carries the honest-scope caveats.
type AdoptionReport struct {
	From          time.Time      `json:"from"`
	To            time.Time      `json:"to"`
	PrevFrom      time.Time      `json:"prev_from"`
	Days          []string       `json:"days"`
	Users         []AdoptionUser `json:"users"`
	InactiveUsers []AdoptionSeat `json:"inactive_users"`
	Daily         []AdoptionDay  `json:"daily"`
	Totals        AdoptionTotals `json:"totals"`
	Sources       []string       `json:"sources"`
	Note          string         `json:"note"`
}
