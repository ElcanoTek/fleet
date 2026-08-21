package db

import (
	"database/sql"
	"encoding/json"
	"log"
	"strings"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/models"
)

// sourceTaskIDValue maps the optional source-task lineage pointer (#270) to a
// nullable column value: nil → SQL NULL, set → the UUID string.
func sourceTaskIDValue(id *uuid.UUID) any {
	if id == nil {
		return nil
	}
	return id.String()
}

// createdByTaskIDValue maps the optional spawned-by-task lineage pointer (#277)
// to a nullable column value: nil → SQL NULL, set → the UUID string. Mirrors
// sourceTaskIDValue; the two columns carry distinct lineage (re-run vs spawn).
func createdByTaskIDValue(id *uuid.UUID) any {
	if id == nil {
		return nil
	}
	return id.String()
}

// recurrenceSpawnedInsertValue derives the recurrence spawn-settlement flag
// (#1116, migration 065) for a freshly INSERTED row. A row born success/error
// — restored history from `fleet import` (#713 preserves status/recurrence/
// completed_at verbatim), or any future insert of an already-completed row —
// must land SETTLED: its successor question was answered in the deployment it
// came from, and an unsettled flag would make the reconciliation sweep treat
// every restored occurrence of a recurring chain as a lost spawn and
// mass-spawn duplicate successors. Rows born in any other status stay FALSE:
// live rows settle through the normal spawn on their own success/error
// transition, cancelled rows never spawn (the sweep never selects them), and
// dead_lettered rows deliberately stay unsettled so a DLQ replay can continue
// the chain (see ReplayDeadLetteredTask). Like effective_priority, the column
// is insert-only here — it is excluded from the upsert/UpdateTaskTx so a
// status write can never clobber the spawn claim.
func recurrenceSpawnedInsertValue(t *models.Task) bool {
	return t.Status == models.TaskStatusSuccess || t.Status == models.TaskStatusError
}

// marshalTags serializes task tags for the JSONB column, ALWAYS as a JSON array
// (never the bare "null" marshalJSON emits for a nil slice) so the tags catalogue
// query's jsonb_array_elements_text never hits a scalar. Empty → "[]".
func marshalTags(tags []string) string {
	if len(tags) == 0 {
		return "[]"
	}
	return marshalJSON(tags)
}

// serializationKeyValue maps the optional mutual-exclusion key (#709) to a
// nullable column value: nil/empty/whitespace-only → SQL NULL (unserialized),
// else the trimmed key. NewTask already normalizes, but re-normalizing here
// defends the claim gate against a directly-constructed Task (internal caller
// or test seed) that bypassed it — a NULL column can never serialize on "".
func serializationKeyValue(p *string) any {
	if p == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*p)
	if trimmed == "" {
		return nil
	}
	return trimmed
}

// recurrenceRemainingValue maps the optional remaining-runs counter to a
// nullable column value: nil → SQL NULL (unbounded), set → the int.
func recurrenceRemainingValue(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

// thinkingBudgetValue maps the optional per-task thinking budget (#220) to a
// nullable column value: nil → SQL NULL (inherit the global default), else the
// int. Mirrors expectedDurationValue.
func thinkingBudgetValue(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

// expectedDurationValue maps the optional expected-duration pointer (#274) to a
// nullable column value: nil → SQL NULL (no SLA), set → the int.
func expectedDurationValue(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

// slaMultiplierValue defends the NOT NULL multiplier columns against a
// directly-constructed Task that bypassed NewTask (e.g. an internal caller or a
// test seed), where the field would otherwise be the zero value. 0/negative
// maps to the supplied default (matching NewTask's normalization); a positive
// value passes through verbatim.
func slaMultiplierValue(v, def float64) float64 {
	if v <= 0 {
		return def
	}
	return v
}

// maybeComputeActualDuration sets task.ActualDurationSeconds from
// CompletedAt - StartedAt (whole seconds) when both are present and the field
// is not already populated (#274). It never overwrites a caller-set value so a
// test seed or an explicit write is preserved.
func maybeComputeActualDuration(task *models.Task) {
	if task.ActualDurationSeconds != nil {
		return
	}
	if task.StartedAt == nil || task.CompletedAt == nil {
		return
	}
	secs := int(task.CompletedAt.Sub(*task.StartedAt).Seconds())
	if secs < 0 {
		secs = 0
	}
	task.ActualDurationSeconds = &secs
}

// deref returns the pointed-to string, or "" for a nil pointer. Paired with
// nullableString so a nil/empty DeadLetterReason persists as SQL NULL (#253).
func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// deadLetterAttemptsValue maps the dead-letter attempt count (#253) to a nullable
// column value: 0 (the not-quarantined sentinel) → SQL NULL, >0 → the int. Keeps
// non-dead-lettered rows NULL in the column rather than a misleading 0.
func deadLetterAttemptsValue(n int) any {
	if n <= 0 {
		return nil
	}
	return n
}

// workspacePathValue maps the optional per-run workspace path (#287) to a
// nullable column value: nil/empty → SQL NULL, set → the path string.
func workspacePathValue(p *string) any {
	if p == nil || strings.TrimSpace(*p) == "" {
		return nil
	}
	return *p
}

// triggerTypeOrCron defends the NOT NULL trigger_type column against a
// directly-constructed Task that bypassed NewTask, where the field would
// otherwise be the empty string.
func triggerTypeOrCron(t models.TriggerType) string {
	if t == "" {
		return string(models.TriggerTypeCron)
	}
	return string(t)
}

// triggerTypeOrCronStr normalizes a scanned trigger_type, defaulting an empty
// value to "cron" (the column is NOT NULL DEFAULT 'cron', so this only guards
// against a stray empty string).
func triggerTypeOrCronStr(s string) string {
	if s == "" {
		return string(models.TriggerTypeCron)
	}
	return s
}

// taskTimezoneOrUTC defends the NOT NULL timezone column against a
// directly-constructed Task that bypassed NewTask (e.g. an internal caller),
// where the field would otherwise be the empty string.
func taskTimezoneOrUTC(tz string) string {
	if tz == "" {
		return "UTC"
	}
	return tz
}

func mcpSelectionOrEmpty(s models.MCPSelection) models.MCPSelection {
	if s == nil {
		return models.MCPSelection{}
	}
	return s
}

// effectivePriorityValue defends the NOT NULL effective_priority column (#230)
// against a directly-constructed Task that bypassed NewTask, where the field
// would be the Go zero value 0 — which under the ASC claim ordering is the MOST
// urgent. It falls back to the submitted Priority, then to Normal, so such a
// task is never silently dispatched ahead of everything else.
func effectivePriorityValue(t *models.Task) int {
	if t.EffectivePriority > 0 {
		return t.EffectivePriority
	}
	if t.Priority > 0 {
		return t.Priority
	}
	return models.PriorityNormal
}

// marshalCredentialAllowlist serializes the allowlist for the nullable JSONB
// column, PRESERVING the nil-vs-empty distinction: nil → SQL NULL ("inherit
// global"), a non-nil (possibly empty) list → its JSON ("[]" = deny all).
func marshalCredentialAllowlist(al models.CredentialAllowlist) any {
	if al == nil {
		return nil
	}
	return marshalJSON(al)
}

// unmarshalCredentialAllowlist reads the nullable JSONB column back. A NULL/empty
// column is nil ("inherit global"); "[]" decodes to a non-nil empty slice
// ("deny all"), so the distinction round-trips.
func unmarshalCredentialAllowlist(ns sql.NullString) models.CredentialAllowlist {
	if !ns.Valid || ns.String == "" {
		return nil
	}
	var result models.CredentialAllowlist
	if err := json.Unmarshal([]byte(ns.String), &result); err != nil {
		log.Printf("Warning: failed to unmarshal credential_allowlist: %v (input: %.100s)", err, ns.String)
		return nil
	}
	return result
}

// marshalLoopConfig serializes the optional loop config for the nullable JSONB
// column: nil → SQL NULL (an ordinary one-shot task), non-nil → its JSON.
func marshalLoopConfig(lc *models.LoopConfig) any {
	if lc == nil {
		return nil
	}
	return marshalJSON(lc)
}

// unmarshalLoopConfig reads the nullable loop_config column back. NULL/empty → nil.
func unmarshalLoopConfig(ns sql.NullString) *models.LoopConfig {
	if !ns.Valid || ns.String == "" {
		return nil
	}
	var lc models.LoopConfig
	if err := json.Unmarshal([]byte(ns.String), &lc); err != nil {
		log.Printf("Warning: failed to unmarshal loop_config: %v (input: %.100s)", err, ns.String)
		return nil
	}
	return &lc
}

// marshalWorktreeConfig serializes the optional worktree config for the nullable
// JSONB column: nil → SQL NULL (shared-workspace task), non-nil → its JSON (#180).
func marshalWorktreeConfig(wc *models.WorktreeConfig) any {
	if wc == nil {
		return nil
	}
	return marshalJSON(wc)
}

// unmarshalWorktreeConfig reads the nullable worktree_config column back. NULL/empty → nil.
func unmarshalWorktreeConfig(ns sql.NullString) *models.WorktreeConfig {
	if !ns.Valid || ns.String == "" {
		return nil
	}
	var wc models.WorktreeConfig
	if err := json.Unmarshal([]byte(ns.String), &wc); err != nil {
		log.Printf("Warning: failed to unmarshal worktree_config: %v (input: %.100s)", err, ns.String)
		return nil
	}
	return &wc
}

// marshalSandboxLimits serializes the optional per-task sandbox limits for the
// nullable JSONB column: nil → SQL NULL (use the global ceilings), non-nil → its
// JSON (#205).
func marshalSandboxLimits(l *models.TaskSandboxLimits) any {
	if l == nil {
		return nil
	}
	return marshalJSON(l)
}

// unmarshalSandboxLimits reads the nullable sandbox_limits column back. NULL/empty → nil.
func unmarshalSandboxLimits(ns sql.NullString) *models.TaskSandboxLimits {
	if !ns.Valid || ns.String == "" {
		return nil
	}
	var l models.TaskSandboxLimits
	if err := json.Unmarshal([]byte(ns.String), &l); err != nil {
		log.Printf("Warning: failed to unmarshal sandbox_limits: %v (input: %.100s)", err, ns.String)
		return nil
	}
	return &l
}

// marshalRawJSON serializes an optional raw-JSON column value (output_schema /
// output_json, #244) for the nullable JSONB column: nil/empty → SQL NULL,
// non-nil → the bytes verbatim (already valid JSON — validated upstream).
func marshalRawJSON(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	return []byte(raw)
}

// unmarshalRawJSON reads a nullable raw-JSON column back. NULL/empty → nil so the
// omitempty field stays absent (#244).
func unmarshalRawJSON(ns sql.NullString) json.RawMessage {
	if !ns.Valid || ns.String == "" {
		return nil
	}
	return json.RawMessage(ns.String)
}

// marshalRetryPolicy serializes the optional retry policy for the nullable JSONB
// column: nil → SQL NULL (legacy policy), non-nil → its JSON (#201).
func marshalRetryPolicy(rp *models.RetryPolicy) any {
	if rp == nil {
		return nil
	}
	return marshalJSON(rp)
}

// unmarshalRetryPolicy reads the nullable retry_policy column back. NULL/empty → nil.
func unmarshalRetryPolicy(ns sql.NullString) *models.RetryPolicy {
	if !ns.Valid || ns.String == "" {
		return nil
	}
	var rp models.RetryPolicy
	if err := json.Unmarshal([]byte(ns.String), &rp); err != nil {
		log.Printf("Warning: failed to unmarshal retry_policy: %v (input: %.100s)", err, ns.String)
		return nil
	}
	return &rp
}

// marshalRunIf serializes the optional pre-run shell gate (#269) for the nullable
// JSONB column: nil → SQL NULL (the legacy unconditional promotion path), non-nil
// → its JSON.
func marshalRunIf(r *models.RunIf) any {
	if r == nil {
		return nil
	}
	return marshalJSON(r)
}

// unmarshalRunIf reads the nullable run_if column back. NULL/empty → nil.
func unmarshalRunIf(ns sql.NullString) *models.RunIf {
	if !ns.Valid || ns.String == "" {
		return nil
	}
	var r models.RunIf
	if err := json.Unmarshal([]byte(ns.String), &r); err != nil {
		log.Printf("Warning: failed to unmarshal run_if: %v (input: %.100s)", err, ns.String)
		return nil
	}
	return &r
}

func nullableString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
