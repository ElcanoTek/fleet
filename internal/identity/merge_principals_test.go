package identity_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/ElcanoTek/fleet/internal/identity"

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
	l.schedUser(uuid.New(), "Lee@X.test", "client", true)
	// Rows the account already has under its email (written since: a chat
	// member's prompt, a budget set for the email) collide with Lee's.
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := l.raw.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	prompt := `INSERT INTO sched.prompt_library (id, owner_username, name, content, visibility) VALUES ($1, $2, $3, 'body', 'private')`
	exec(prompt, uuid.New(), "Brad@X.test", "Daily brief")
	exec(prompt, uuid.New(), "lee@x.test", "Triage")
	exec(prompt, uuid.New(), "Lee@X.test", "triage") // same name, case-insensitively
	budgetSQL := `INSERT INTO sched.budgets (scope, principal_id, time_window, soft_usd, hard_usd, soft_tokens, hard_tokens) VALUES ('user', $1, $2, $3, $4, $5, $6)`
	exec(budgetSQL, "Brad@X.test", "day", nil, 0.0, nil, nil) // a hard stop
	exec(budgetSQL, "lee@x.test", "week", nil, 10.0, nil, 5000)
	exec(budgetSQL, "Lee@X.test", "week", 2.0, 20.0, nil, 1000)
	// Audit labels move too; an API key's label does not.
	taskID := uuid.New()
	exec(`INSERT INTO sched.tasks (id, prompt, priority, status, created_at) VALUES ($1, 'p', 0, $2, now())`, taskID, string(models.TaskStatusPending))
	exec(`INSERT INTO sched.task_feedback (task_id, rating, created_at, created_by) VALUES ($1, 'down', 1, 'Brad@X.test'), ($1, 'up', 2, 'api-key:ci')`, taskID)

	res := l.merge()
	if !res.Ran {
		t.Fatalf("merge = %+v", res)
	}
	rep := res.Report

	st := storage.New()
	st.SetDatabase(l.openSched())
	list, err := st.ListPromptLibrary(ctx, "brad@x.test")
	if err != nil || len(list) != 1 || list[0].Name != "Daily brief" {
		t.Fatalf("brad's prompts after the merge = %+v, %v", list, err)
	}
	enforcer := budget.New(budget.Config{Store: l.openSched()})
	var exceeded *budget.ExceededError
	if err := enforcer.CheckCreate(ctx, models.BudgetPrincipals{User: "brad@x.test"}); !errors.As(err, &exceeded) {
		t.Fatalf("brad's hard budget after the merge: CheckCreate = %v, want a refusal", err)
	}

	// Prompt collision: both kept, the moved one renamed.
	lee, err := st.ListPromptLibrary(ctx, "lee@x.test")
	if err != nil || len(lee) != 2 {
		t.Fatalf("lee's prompts = %+v, %v; want both", lee, err)
	}
	if len(rep.RenamedPrompts) != 1 || rep.RenamedPrompts[0].To != "triage (from Lee@X.test)" {
		t.Errorf("renamed prompts = %+v", rep.RenamedPrompts)
	}
	// Budget collision: one week budget, the stricter bound of each kind.
	var n int
	var soft, hard float64
	var hardTok int64
	if err := l.raw.QueryRow(`SELECT count(*), max(soft_usd), max(hard_usd), max(hard_tokens) FROM sched.budgets WHERE scope = 'user' AND principal_id = 'lee@x.test' AND time_window = 'week'`).Scan(&n, &soft, &hard, &hardTok); err != nil {
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

// TestMergeQuarantinesDuplicateUsernamesRows (Codex 4505664986 on #2083): two
// legacy usernames that normalize to one email are one account, and only the
// winner's rows move to it. The winner is the username that already is the
// email (else the oldest); the loser's private prompt and budget stay under
// its old username — never folded into the winner's account — and are
// reported as quarantined.
func TestMergeQuarantinesDuplicateUsernamesRows(t *testing.T) {
	l := newLegacy(t)
	ctx := context.Background()
	// The mixed-case one is older, but the exact email wins.
	l.schedUser(uuid.New(), " Pat@X.test", "client", true)
	l.schedUser(uuid.New(), "pat@x.test", "readonly", true)
	// And a group with no exact email: the oldest wins.
	l.schedUser(uuid.New(), "Kim@X.test", "client", true)
	l.schedUser(uuid.New(), "KIM@x.test", "client", true)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := l.raw.Exec(q, args...); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	prompt := `INSERT INTO sched.prompt_library (id, owner_username, name, content, visibility) VALUES ($1, $2, $3, 'body', 'private')`
	budgetSQL := `INSERT INTO sched.budgets (scope, principal_id, time_window, hard_usd) VALUES ('user', $1, 'day', $2)`
	for _, u := range []struct {
		name string
		hard float64
	}{{" Pat@X.test", 1}, {"pat@x.test", 50}, {"Kim@X.test", 2}, {"KIM@x.test", 60}} {
		exec(prompt, uuid.New(), u.name, "mine: "+u.name)
		exec(budgetSQL, u.name, u.hard)
	}

	res := l.merge()
	rep := res.Report
	st := storage.New()
	st.SetDatabase(l.openSched())
	for _, c := range []struct {
		email, ownPrompt, lostOwner string
		hard                        float64
	}{
		{"pat@x.test", "mine: pat@x.test", " Pat@X.test", 50},
		{"kim@x.test", "mine: Kim@X.test", "KIM@x.test", 2},
	} {
		list, err := st.ListPromptLibrary(ctx, c.email)
		if err != nil || len(list) != 1 || list[0].Name != c.ownPrompt {
			t.Errorf("%s's prompts = %+v, %v; want only %q", c.email, list, err, c.ownPrompt)
		}
		var hard float64
		var n int
		if err := l.raw.QueryRow(`SELECT count(*), coalesce(max(hard_usd), 0) FROM sched.budgets WHERE scope = 'user' AND principal_id = $1`, c.email).Scan(&n, &hard); err != nil || n != 1 || hard != c.hard {
			t.Errorf("%s's budgets: n=%d hard=%v (%v); want its own (%v)", c.email, n, hard, err, c.hard)
		}
		var lostPrompts, lostBudgets int
		if err := l.raw.QueryRow(`SELECT (SELECT count(*) FROM sched.prompt_library WHERE owner_username = $1), (SELECT count(*) FROM sched.budgets WHERE principal_id = $1)`, c.lostOwner).Scan(&lostPrompts, &lostBudgets); err != nil || lostPrompts != 1 || lostBudgets != 1 {
			t.Errorf("the loser %q's rows were moved: prompts=%d budgets=%d (%v)", c.lostOwner, lostPrompts, lostBudgets, err)
		}
	}
	got := map[string]identity.MergeEntry{}
	for _, d := range rep.DuplicateUsernames {
		got[d.SchedUsername] = d
	}
	for _, lost := range []string{" Pat@X.test", "KIM@x.test"} {
		if d, ok := got[lost]; !ok || d.QuarantinedPrompts != 1 || d.QuarantinedBudgets != 1 {
			t.Errorf("duplicate %q not reported as quarantined: %+v", lost, rep.DuplicateUsernames)
		}
	}
	if len(rep.MergedBudgets) != 0 || len(rep.RenamedPrompts) != 0 {
		t.Errorf("a loser's rows were folded in: merged=%+v renamed=%+v", rep.MergedBudgets, rep.RenamedPrompts)
	}
	// The exact email took the account: its UUID and role.
	if u := l.account("pat@x.test"); u.OpsScope != identity.OpsView {
		t.Errorf("pat@x.test = %+v, want the exact username's readonly access", u)
	}
}
