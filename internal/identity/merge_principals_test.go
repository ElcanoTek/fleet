package identity_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/sched/budget"
	"github.com/ElcanoTek/fleet/internal/sched/models"
	"github.com/ElcanoTek/fleet/internal/sched/storage"
)

// TestMergeMovesUsernameKeyedRows (Codex 4505481859 on #2083): a legacy
// username the merge normalizes ("Brad@X.test" → brad@x.test) takes its
// username-keyed scheduler rows with it. Its private prompt is still listed
// for the account, and its hard budget still blocks a task create. Where the
// account already had a prompt of the same name, the moved one is renamed
// (both kept); where it already had a budget for the window, the two are
// merged keeping the stricter bound of each kind.
func TestMergeMovesUsernameKeyedRows(t *testing.T) {
	l := newLegacy(t)
	ctx := context.Background()
	l.schedUser(uuid.New(), "Brad@X.test", "client", true)
	l.schedUser(uuid.New(), " Ops@X.test", "client", true)
	l.schedUser(uuid.New(), "ops@x.test", "readonly", true) // case/space duplicate of " Ops@X.test"
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := l.raw.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	prompt := `INSERT INTO sched.prompt_library (id, owner_username, name, content, visibility) VALUES ($1, $2, $3, 'body', 'private')`
	exec(prompt, uuid.New(), "Brad@X.test", "Daily brief")
	exec(prompt, uuid.New(), "ops@x.test", "Triage")
	exec(prompt, uuid.New(), " Ops@X.test", "triage") // same name, case-insensitively
	budgetSQL := `INSERT INTO sched.budgets (scope, principal_id, time_window, soft_usd, hard_usd, soft_tokens, hard_tokens) VALUES ('user', $1, $2, $3, $4, $5, $6)`
	exec(budgetSQL, "Brad@X.test", "day", nil, 0.0, nil, nil) // a hard stop
	exec(budgetSQL, "ops@x.test", "week", nil, 10.0, nil, 5000)
	exec(budgetSQL, " Ops@X.test", "week", 2.0, 20.0, nil, 1000)
	// Audit labels move too; an API key's label does not.
	taskID := uuid.New()
	if err := l.sched.AddTask(ctx, &models.Task{ID: taskID, Prompt: "p", Status: models.TaskStatusPending}); err != nil {
		t.Fatal(err)
	}
	exec(`INSERT INTO sched.task_feedback (task_id, rating, created_at, created_by) VALUES ($1, 'down', 1, 'Brad@X.test'), ($1, 'up', 2, 'api-key:ci')`, taskID)

	res := l.merge()
	if !res.Ran {
		t.Fatalf("merge = %+v", res)
	}
	rep := res.Report

	st := storage.New()
	st.SetDatabase(l.sched)
	list, err := st.ListPromptLibrary(ctx, "brad@x.test")
	if err != nil || len(list) != 1 || list[0].Name != "Daily brief" {
		t.Fatalf("brad's prompts after the merge = %+v, %v", list, err)
	}
	enforcer := budget.New(budget.Config{Store: l.sched})
	var exceeded *budget.ExceededError
	if err := enforcer.CheckCreate(ctx, models.BudgetPrincipals{User: "brad@x.test"}); !errors.As(err, &exceeded) {
		t.Fatalf("brad's hard budget after the merge: CheckCreate = %v, want a refusal", err)
	}

	// Prompt collision: both kept, the moved one renamed.
	ops, err := st.ListPromptLibrary(ctx, "ops@x.test")
	if err != nil || len(ops) != 2 {
		t.Fatalf("ops prompts = %+v, %v; want both", ops, err)
	}
	if len(rep.RenamedPrompts) != 1 || rep.RenamedPrompts[0].To != "triage (from  Ops@X.test)" {
		t.Errorf("renamed prompts = %+v", rep.RenamedPrompts)
	}
	// Budget collision: one week budget, the stricter bound of each kind.
	var n int
	var soft, hard float64
	var hardTok int64
	if err := l.raw.QueryRow(`SELECT count(*), max(soft_usd), max(hard_usd), max(hard_tokens) FROM sched.budgets WHERE scope = 'user' AND principal_id = 'ops@x.test' AND time_window = 'week'`).Scan(&n, &soft, &hard, &hardTok); err != nil {
		t.Fatal(err)
	}
	if n != 1 || soft != 2 || hard != 10 || hardTok != 1000 {
		t.Errorf("merged week budget: n=%d soft=%v hard=%v hard_tokens=%v; want 1, 2, 10, 1000", n, soft, hard, hardTok)
	}
	if len(rep.MergedBudgets) != 1 {
		t.Errorf("merged budgets = %+v", rep.MergedBudgets)
	}
	var left int
	if err := l.raw.QueryRow(`SELECT count(*) FROM sched.task_feedback WHERE created_by IN ('Brad@X.test')`).Scan(&left); err != nil || left != 0 {
		t.Errorf("feedback label not moved: %d (%v)", left, err)
	}
	if err := l.raw.QueryRow(`SELECT count(*) FROM sched.task_feedback WHERE created_by = 'api-key:ci'`).Scan(&left); err != nil || left != 1 {
		t.Errorf("an API key label was touched: %d (%v)", left, err)
	}
	if rep.RewrittenPrincipals == 0 {
		t.Errorf("report counts no rewritten rows")
	}
}
